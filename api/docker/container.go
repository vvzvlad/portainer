package docker

import (
	"context"
	"strings"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/logs"

	"github.com/Masterminds/semver/v3"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	sdkclient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// defaultRestoreTimeout bounds the best-effort restore of the original container
// after a failed recreate. The restore runs on a context detached from the
// caller's (see restoreContext), so it needs a deadline of its own: long enough
// for a rename, a handful of network connects and a start against a healthy
// engine, short enough that an unresponsive engine cannot pin the caller's
// goroutine for minutes.
const defaultRestoreTimeout = 30 * time.Second

// defaultPullTimeout bounds the image pull a forced recreate begins with, which
// runs on a Docker client and a context of its own (see pullImage). The client's
// http.Client.Timeout covers reading the whole progress stream, so the factory's
// 60s default would cut any pull longer than a minute.
//
// An hour lets a 6-7 GB image through at about 1.9 MB/s. It is only the absolute
// backstop: a pull that goes silent is caught by defaultPullStallTimeout in
// api/docker/images, which bounds the absence of progress instead.
//
// Edge endpoints do not get the hour: the tunnel closes after chisel's
// activeTimeout (4m30s, see checkTunnels) because nothing refreshes LastActivity
// while a pull streams; TunnelAddr only resets it once, before the pull starts.
const defaultPullTimeout = time.Hour

// Fractions of the restore budget reserved for the two steps that run BEFORE the
// calls that put the original back. Everything in a restore draws on ONE budget
// so a dead engine cannot pin the caller for one deadline per step, which means
// an early step that hangs would otherwise leave the rename back, the reconnects
// and the start — the calls that decide whether the workload is down — running on
// an expired context.
//
//   - restoreTeardownShare bounds the teardown of the NEW container, which runs
//     first (LIFO). A third is ample for a stop plus a forced removal against an
//     engine that answers at all.
//   - restorePlanShare bounds the single inspect that plans the rollback.
const (
	restoreTeardownShare = 3
	restorePlanShare     = 6
)

// pullReserveShare is the fraction of the caller's REMAINING time held back from
// the image pull for the destructive steps after it (stop, rename, create,
// start), so a pull that uses the whole window cannot leave them an expired
// context. Against a ten-minute recreate, a sixth leaves the pull 8m20s and the
// control-plane calls 1m40s.
const pullReserveShare = 6

// DefaultRecreateTimeout (72m): pullBudget holds back a sixth of it, so the pull keeps its hour (less 5/6 of the time the steps before it took) and the steps after it get the 12m reserve.
const DefaultRecreateTimeout = defaultPullTimeout * pullReserveShare / (pullReserveShare - 1)

// ClientFactory creates Docker clients for a given environment.
//
// ContainerService holds the interface rather than the concrete
// *client.ClientFactory so a test can see the timeout each client is asked for;
// the pull's context always expires first, so that timeout cannot be observed by
// timing. api/docker/images declares its own identical interface.
type ClientFactory interface {
	CreateClient(endpoint *portainer.Endpoint, nodeName string, timeout *time.Duration) (*sdkclient.Client, error)
}

type ContainerService struct {
	factory   ClientFactory
	dataStore dataservices.DataStore
	// restoreTimeout is the whole time budget of one restore. It is a field rather
	// than a constant read at the call site so a test can drive the
	// budget-exhaustion paths in milliseconds without mutating a package-level knob
	// shared by every other (parallel) test. Read it through restoreBudget rather
	// than directly, so the zero value cannot switch a restore off.
	restoreTimeout time.Duration
	// pullTimeout is the whole time budget of one image pull, both the client's own
	// http.Client.Timeout and the deadline of the context the pull runs on. Like
	// restoreTimeout it is a field rather than a constant read at the call site so a
	// test can drive a pull that outlives its budget in milliseconds without mutating
	// a package-level knob shared by every other (parallel) test. Read it through
	// pullBudget rather than directly, so the zero value cannot turn every pull into
	// an already-expired context.
	pullTimeout time.Duration
}

func NewContainerService(factory ClientFactory, dataStore dataservices.DataStore) *ContainerService {
	return &ContainerService{
		factory:        factory,
		dataStore:      dataStore,
		restoreTimeout: defaultRestoreTimeout,
		pullTimeout:    defaultPullTimeout,
	}
}

// applyVersionConstraint uses the version to apply a transformation function to
// the value when the constraint is satisfied
func applyVersionConstraint[T any](currentVersion, versionConstraint string, value T, transform func(T) T) (T, error) {
	newValue := value

	constraint, err := semver.NewConstraint(versionConstraint)
	if err != nil {
		return newValue, errors.New("invalid version constraint specified")
	}

	currentVer, err := semver.NewVersion(currentVersion)
	if err != nil {
		log.Warn().Err(err).Msg("Unable to parse the Docker client version")

		return newValue, nil
	}

	if satisfiesConstraint, _ := constraint.Validate(currentVer); satisfiesConstraint {
		newValue = transform(value)
	}

	return newValue, nil
}

func clearMacAddrs(n network.NetworkingConfig) network.NetworkingConfig {
	netConfig := network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}

	for k := range n.EndpointsConfig {
		endpointConfig := n.EndpointsConfig[k].Copy()
		endpointConfig.MacAddress = ""
		netConfig.EndpointsConfig[k] = endpointConfig
	}

	return netConfig
}

// pullBudget is the whole time budget of one image pull; both the pull client's
// http.Client.Timeout and its context deadline derive from it.
//
// A zero pullTimeout (a ContainerService built without NewContainerService) falls
// back to the default, since a zero timeout is an already-expired context.
//
// A caller deadline caps the budget to its remainder less pullReserveShare of it;
// the cap only shortens. A deadline that has already passed yields a non-positive
// budget, which pullImage refuses.
func (c *ContainerService) pullBudget(ctx context.Context) time.Duration {
	budget := c.pullTimeout
	if budget <= 0 {
		budget = defaultPullTimeout
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		return budget
	}

	remaining := time.Until(deadline)

	return min(budget, remaining-remaining/pullReserveShare)
}

// pullImage pulls the image a recreate is about to build its new container from,
// on a Docker client and a deadline of its own.
//
// The rest of Recreate uses a client with a short http.Client.Timeout so an
// unresponsive engine cannot pin the caller; a pull needs a long one, so it gets
// its own client.
//
// nodeName routes the pull to the node the container is recreated on in an agent
// cluster; without it the image could be pulled on another node.
//
// The context deadline is the bound that fires, and the only one on unix-socket
// and named-pipe endpoints (createLocalClient ignores the timeout). The client
// timeout keeps the factory's 60s default off the pull client.
func (c *ContainerService) pullImage(ctx context.Context, endpoint *portainer.Endpoint, nodeName string, img images.Image) error {
	budget := c.pullBudget(ctx)

	// A non-positive budget means the caller's deadline has passed. It must not
	// reach the factory: net/http reads a non-positive http.Client.Timeout as no
	// timeout at all.
	//
	// ctx.Err() can still be nil just after a deadline passes, and errors.Wrap of
	// nil is nil, which the caller would read as a successful pull.
	if budget <= 0 {
		err := ctx.Err()
		if err == nil {
			err = context.DeadlineExceeded
		}

		return errors.Wrap(err, "no time left to pull the image")
	}

	cli, err := c.factory.CreateClient(endpoint, nodeName, &budget)
	if err != nil {
		return errors.Wrap(err, "create client error")
	}
	defer logs.CloseAndLogErr(cli)

	pullCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	return images.NewPuller(cli, images.NewRegistryClient(c.dataStore), c.dataStore).Pull(pullCtx, img)
}

// restoreBudget is the whole time budget of one restore, and the only reading of
// restoreTimeout there is: every budget derived from it — the restore context
// itself and the shares the teardown and the planning inspect run on — goes
// through here. A zero restoreTimeout (a ContainerService built without
// NewContainerService) falls back to the default, since a zero timeout is an
// already-expired context.
func (c *ContainerService) restoreBudget() time.Duration {
	if c.restoreTimeout <= 0 {
		return defaultRestoreTimeout
	}

	return c.restoreTimeout
}

// restoreContext derives the context the restore runs on. It is deliberately
// detached from the caller's context: the most common recreate failure is the
// caller's own deadline or cancellation (auto-update bounds a recreate with
// recreateTimeout), and reusing that dead context would make every restore call
// fail instantly, leaving the original container renamed, disconnected and
// stopped. The derived context keeps the caller's values but gets its own
// restoreTimeout deadline.
//
// If the process exits mid-restore, the sequence is cut at whatever step it had
// reached.
func (c *ContainerService) restoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), c.restoreBudget())
}

// restorePlan is what putting the original back still requires: whether it has
// to be renamed back, and which of its networks have to be reconnected.
type restorePlan struct {
	rename   bool
	networks []*network.EndpointSettings
}

// missingNetworks returns the networks among attempted that observed is NOT
// attached to. The restore asks it twice, of two different observations: of the
// state it starts from, which is what it still has to reconnect, and of the state
// it leaves behind, which is what it failed to. A state that could not be read
// counts as attached to nothing, never as a container that is fine.
func missingNetworks(observed *dockercontainer.InspectResponse, attempted []*network.EndpointSettings) []*network.EndpointSettings {
	if observed == nil || observed.NetworkSettings == nil {
		return attempted
	}

	attached := make(map[string]bool, len(observed.NetworkSettings.Networks))
	for _, endpointSettings := range observed.NetworkSettings.Networks {
		attached[endpointSettings.NetworkID] = true
	}

	var missing []*network.EndpointSettings

	for _, endpointSettings := range attempted {
		if !attached[endpointSettings.NetworkID] {
			missing = append(missing, endpointSettings)
		}
	}

	return missing
}

// planRestore works out what the restore still has to undo. It is driven by the
// original's OBSERVED state, not by which of the teardown calls returned nil: a
// call whose answer was lost (the caller's deadline firing on a request the
// engine went on to execute) did happen and has to be undone, and a call that
// never landed must not be undone twice.
//
// observed is nil when the state could not be read; the plan then falls back to
// every attempted step, which is harmless because the engine refuses a
// superfluous step and the verdict is read off the container afterwards.
//
// Only steps this recreate attempted are undone: a mismatched name with no rename
// issued is someone else's rename.
func planRestore(observed *dockercontainer.InspectResponse, name string, renameAttempted bool, disconnectAttempted []*network.EndpointSettings) restorePlan {
	if observed == nil || observed.ContainerJSONBase == nil {
		return restorePlan{rename: renameAttempted, networks: disconnectAttempted}
	}

	return restorePlan{
		rename:   renameAttempted && observed.Name != name,
		networks: missingNetworks(observed, disconnectAttempted),
	}
}

// Recreate a container.
//
// From the moment the engine is ASKED to stop the original container, every
// failure path rolls back to it: the rename aside is undone, the networks it was
// detached from are reconnected, the new container (if one was created already)
// is removed, and the original is started again. A failure before that point has
// not touched the original, so there is nothing to roll back — but a stop that
// merely FAILS is not such a point, since the engine may have carried it out
// anyway.
//
// The rollback's verdict comes from inspecting the original at the end: running,
// carrying its own name, attached to its networks. A restore that did not fully
// land is returned as a *RestoreError saying whether the original is running
// again (degraded) or left down (an outage).
func (c *ContainerService) Recreate(ctx context.Context, endpoint *portainer.Endpoint, containerId string, forcePullImage bool, imageTag, nodeName string) (newContainer *types.ContainerJSON, err error) {
	cli, err := c.factory.CreateClient(endpoint, nodeName, nil)
	if err != nil {
		return nil, errors.Wrap(err, "create client error")
	}
	defer logs.CloseAndLogErr(cli)

	log.Debug().Str("container_id", containerId).Msg("starting to fetch container information")

	container, _, err := cli.ContainerInspectWithRaw(ctx, containerId, true)
	if err != nil {
		return nil, errors.Wrap(err, "fetch container information error")
	}

	log.Debug().Str("image", container.Config.Image).Msg("starting to parse image")
	img, err := images.ParseImage(images.ParseImageOptions{
		Name: container.Config.Image,
	})
	if err != nil {
		return nil, errors.Wrap(err, "parse image error")
	}

	if imageTag != "" {
		if err := img.WithTag(imageTag); err != nil {
			return nil, errors.Wrapf(err, "set image tag error %s", imageTag)
		}

		log.Debug().Str("image", container.Config.Image).Msg("new image with tag")

		container.Config.Image = img.FullName()
	}

	// 1. pull image if you need force pull
	if forcePullImage {
		if err := c.pullImage(ctx, endpoint, nodeName, img); err != nil {
			return nil, errors.Wrapf(err, "pull image error %s", img.FullName())
		}
	}

	// The original container stops serving the moment the engine is ASKED to stop
	// it, so every exit path from here on has to put it back, including a stop that
	// returns an error (the engine may have carried it out anyway). Starting a
	// container that never stopped is harmless: the engine answers 304, which the
	// SDK reports as success. restore is cleared only once the new container has
	// taken over.
	restore := true

	// What the restore may have to undo. Each flag is raised BEFORE its call,
	// because a call whose answer was lost may still have been executed; the
	// restore is planned from observed state, so doing too much is safe.
	//
	// stopSaidGone is read off the answer instead: a stop refused with "no such
	// container" proves this recreate did not take the container down, which tells
	// a --rm container reaped on our stop from one reaped before we asked.
	var (
		renameAttempted     bool
		stopSaidGone        bool
		disconnectAttempted []*network.EndpointSettings
	)

	// restoreCtx is ONE time budget for the whole restore, shared by both defers
	// below, so a dead engine can pin the caller's goroutine for the restore budget
	// in total rather than once per defer. It is built lazily: a recreate that never
	// rolls back must not pay for a context it does not use.
	var (
		sharedRestoreCtx context.Context
		stopRestore      context.CancelFunc
	)

	useRestoreCtx := func() context.Context {
		if sharedRestoreCtx == nil {
			sharedRestoreCtx, stopRestore = c.restoreContext(ctx)
		}

		return sharedRestoreCtx
	}

	// Registered before both restore defers, so LIFO releases the shared context
	// only once they are done with it.
	defer func() {
		if stopRestore != nil {
			stopRestore()
		}
	}()

	// restoreErrs collects what went wrong putting the ORIGINAL container back in
	// service. The teardown of the NEW container reports through newContainerErr
	// instead: a new container that could not be removed is a leftover to clean up,
	// not by itself proof that the workload is down.
	var (
		restoreErrs     []error
		newContainerErr error
	)

	// Restore of the original container. Registered FIRST, so LIFO runs it LAST,
	// after the teardown defer registered below has removed the new container: the
	// new container owns the original name, so it has to be gone before the
	// original can be renamed back.
	defer func() {
		if !restore {
			return
		}

		restoreCtx := useRestoreCtx()

		log.Debug().Str("container_id", containerId).Str("container", container.Name).Msg("restoring the container")

		// Read the original's state first and plan the rollback from it, on a small
		// share of the budget of its own: this inspect runs before every call that
		// puts the container back, so one that hangs must not leave them to run on an
		// expired context. Where the state cannot be read the plan falls back to every
		// step that was attempted.
		var (
			observed *dockercontainer.InspectResponse
			gone     bool
		)

		planCtx, stopPlan := context.WithTimeout(restoreCtx, c.restoreBudget()/restorePlanShare)

		switch current, _, err := cli.ContainerInspectWithRaw(planCtx, containerId, false); {
		case err == nil:
			observed = &current
		case cerrdefs.IsNotFound(err):
			gone = true
		default:
			log.Warn().Err(err).Str("container_id", containerId).
				Msg("unable to read the original container state, restoring every step that was attempted")
		}

		stopPlan()

		if !gone {
			plan := planRestore(observed, container.Name, renameAttempted, disconnectAttempted)

			if plan.rename {
				if err := cli.ContainerRename(restoreCtx, containerId, container.Name); err != nil {
					restoreErrs = append(restoreErrs, errors.Wrap(err, "rename container back error"))
				}
			}

			for _, endpointSettings := range plan.networks {
				if err := cli.NetworkConnect(restoreCtx, endpointSettings.NetworkID, containerId, endpointSettings); err != nil {
					restoreErrs = append(restoreErrs, errors.Wrapf(err, "connect container to network %s error", endpointSettings.NetworkID))
				}
			}

			if err := cli.ContainerStart(restoreCtx, containerId, dockercontainer.StartOptions{}); err != nil {
				restoreErrs = append(restoreErrs, errors.Wrap(err, "start container error"))
			}
		}

		// The verdict is what the engine reports about the original at the end, and
		// nothing else. Which restore calls failed says little about it: a refused
		// call may have been refused precisely because the step was already in place
		// ("already in that state", a name the container already carries), and a call
		// that returned nil is no proof either — a container can exit the moment it is
		// started. restoreErrs is diagnostics for the report, never an input here.
		var (
			restored     *dockercontainer.InspectResponse
			stateUnknown bool
		)

		if !gone {
			switch final, _, err := cli.ContainerInspectWithRaw(restoreCtx, containerId, false); {
			case cerrdefs.IsNotFound(err):
				gone = true
			case err != nil:
				restoreErrs = append(restoreErrs, errors.Wrap(err, "inspect the original container error"))
				stateUnknown = true
			case final.ContainerJSONBase == nil || final.State == nil:
				restoreErrs = append(restoreErrs, errors.New("the engine reported no state for the original container"))
				stateUnknown = true
			default:
				restored = &final
			}
		}

		// A new container that could not be removed belongs in every report below:
		// holding the original name, it is a common reason a rename back could not
		// land, and it is a leftover somebody has to clean up in any case.
		if newContainerErr != nil {
			restoreErrs = append([]error{newContainerErr}, restoreErrs...)
		}

		// A --rm container disappearing is not a third party's doing: the engine reaps
		// it the moment it stops, and the restore is armed BEFORE the stop. Unless the
		// stop itself said the container was already gone — then it exited and was
		// reaped on its own, which is simply how a --rm container ends, and this
		// recreate never touched it.
		autoRemoved := gone && !stopSaidGone && container.HostConfig != nil && container.HostConfig.AutoRemove
		originalRunning := false

		switch {
		case gone && !autoRemoved:
			// A container this recreate did not take down: nothing removes a container on
			// a stop unless it runs with --rm, so somebody else removed this one (or it
			// was already gone before the stop). There is nothing to put back, and no
			// outage to pin on this recreate.
			event := log.Warn().Errs("restore_errors", restoreErrs).
				Str("container_id", containerId).Str("container", strings.TrimPrefix(container.Name, "/"))

			if newContainerErr != nil {
				// Nothing to restore, but not nothing to do: the leftover is holding the
				// name the vanished original had, so it answers to it.
				event.Msg("the original container no longer exists, there is nothing to restore, but the new container could not be removed and is left holding the original name")

				return
			}

			event.Msg("the original container no longer exists, there is nothing to restore")

			return
		case autoRemoved:
			restoreErrs = append(restoreErrs, errors.New("the original container ran with --rm, so the engine removed it when this recreate stopped it and there is nothing left to restore"))
		case stateUnknown:
			// Conservative: a state that could not be read is not a running container.
			// restoreErrs already says why it could not be read.
		default:
			originalRunning = restored.State.Running
			nameBack := restored.Name == container.Name
			missing := missingNetworks(restored, disconnectAttempted)

			if originalRunning && nameBack && len(missing) == 0 {
				// The workload is serving again exactly as before, so this is an ordinary
				// failed recreate and the caller keeps the original error. A new container
				// that could not be removed is loud, but it is a leftover to clean up rather
				// than an outage, and must not be dressed up as one.
				if newContainerErr != nil {
					log.Warn().Err(newContainerErr).Str("container_id", containerId).
						Msg("the new container could not be removed after a failed recreate, the original is running again but the leftover needs cleaning up")
				}

				// Refused calls that turned out not to matter: the step was already in
				// place, or a later one made up for it. Debug material, not an operator's.
				if len(restoreErrs) > 0 {
					log.Debug().Errs("restore_errors", restoreErrs).Str("container_id", containerId).
						Msg("some restore calls failed, but the original came back exactly as it was")
				}

				return
			}

			// What the container itself says did not come back. The call errors are no
			// substitute: a step can be refused and still be in place, and a step can be
			// reported as done and not be.
			if !originalRunning {
				restoreErrs = append(restoreErrs, errors.New("the original container is not running"))
			}

			if !nameBack {
				restoreErrs = append(restoreErrs, errors.Errorf("the original container is named %s, not %s", restored.Name, container.Name))
			}

			for _, endpointSettings := range missing {
				restoreErrs = append(restoreErrs, errors.Errorf("the original container is not attached to network %s", endpointSettings.NetworkID))
			}
		}

		// Something did not come back, or could not be read. Degraded is a warning,
		// nothing running (or nothing known) is an error — the same split the
		// container-automation notifier uses, so one incident reads at one level.
		level := log.Warn
		if !originalRunning {
			level = log.Error
		}

		event := level().Errs("restore_errors", restoreErrs).
			Str("container_id", containerId).
			Str("container", strings.TrimPrefix(container.Name, "/")).
			Int("endpoint_id", int(endpoint.ID)).
			Str("endpoint", endpoint.Name)

		if err == nil {
			// Not reachable on a normal return: restore is only cleared on the path that
			// returns the new container. A nil error here means Recreate is unwinding
			// through a panic, and it is the panic — not this named return — that the
			// caller will see, so there is nothing to wrap.
			event.Bool("original_running", originalRunning).
				Msg("recreate panicked and the original container could not be fully restored")

			return
		}

		event = event.Err(err)

		switch {
		case stateUnknown:
			event.Msg("recreate failed and the state of the original container could not be read, whether it is running again is unknown")
		case originalRunning:
			// Serving, but not as it was: it may answer under the wrong name (a stack
			// peer resolving "web" would not find it) or be missing a network. Loud, and
			// still not an outage.
			event.Msg("recreate failed and the original container is running again, but the restore was incomplete")
		default:
			event.Msg("recreate failed and the original container could not be restored, it is left down")
		}

		err = &RestoreError{
			ContainerID:     containerId,
			Name:            container.Name,
			Cause:           err,
			Errs:            restoreErrs,
			OriginalRunning: originalRunning,
			StateUnknown:    stateUnknown,
		}
	}()

	// 2. stop the current container
	log.Debug().Str("container_id", containerId).Msg("starting to stop the container")
	if err := cli.ContainerStop(ctx, containerId, dockercontainer.StopOptions{}); err != nil {
		stopSaidGone = cerrdefs.IsNotFound(err)

		return nil, errors.Wrap(err, "stop container error")
	}

	// 3. rename the current container
	log.Debug().Str("container_id", containerId).Msg("starting to rename the container")

	renameAttempted = true
	if err := cli.ContainerRename(ctx, containerId, container.Name+"-old"); err != nil {
		return nil, errors.Wrap(err, "rename container error")
	}

	initialNetwork := network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}

	// 4. disconnect all networks from the current container
	for name, endpointSettings := range container.NetworkSettings.Networks {
		disconnectAttempted = append(disconnectAttempted, endpointSettings)

		// This allows new container to use the same IP address if specified
		if err := cli.NetworkDisconnect(ctx, endpointSettings.NetworkID, containerId, true); err != nil {
			return nil, errors.Wrap(err, "disconnect network from old container error")
		}

		// 5. get the first network attached to the current container
		if len(initialNetwork.EndpointsConfig) == 0 {
			// Retrieve the first network that is linked to the present container, which
			// will be utilized when creating the container.
			initialNetwork.EndpointsConfig[name] = endpointSettings
		}
	}

	log.Debug().Str("container", strings.Split(container.Name, "/")[1]).Msg("starting to create a new container")

	// 6. create a new container
	// when a container is created without a network, docker connected it by default to the
	// bridge network with a random IP, also it can only connect to one network on creation.
	// to retain the same network settings we have to connect on creation to one of the old
	// container's networks, and connect to the other networks after creation.
	// see: https://portainer.atlassian.net/browse/EE-5448

	// Docker API < 1.44 does not support specifying MAC addresses
	// https://github.com/moby/moby/blob/6aea26b431ea152a8b085e453da06ea403f89886/client/container_create.go#L44-L46
	initialNetwork, err = applyVersionConstraint(cli.ClientVersion(), "< 1.44", initialNetwork, clearMacAddrs)
	if err != nil {
		return nil, err
	}

	create, err := cli.ContainerCreate(ctx, container.Config, container.HostConfig, &initialNetwork, nil, container.Name)
	if err != nil {
		return nil, errors.Wrap(err, "create container error")
	}

	// Teardown of the new container. Registered AFTER the restore defer, so LIFO
	// runs it FIRST and the original name is free by the time the restore defer
	// renames the original back.
	defer func() {
		if !restore {
			return
		}

		// Only a share of the shared budget: this defer runs FIRST (LIFO), so a stop
		// or a removal that hangs must not be able to spend the whole restore window
		// and leave the original with nothing. See restoreTeardownShare.
		restoreCtx, stopTeardown := context.WithTimeout(useRestoreCtx(), c.restoreBudget()/restoreTeardownShare)
		defer stopTeardown()

		log.Debug().Str("container_id", create.ID).Msg("removing the new container")

		// A stop failure is not fatal on its own, the forced removal below kills the
		// container anyway — which is also why it is given no grace period at all.
		// The engine's default grace is 10s, the same order as this whole teardown
		// share, so a container ignoring SIGTERM (common enough) would spend the share
		// waiting here and leave the forced removal to run on an expired context, the
		// new container holding the original name and the rename back refused.
		noGrace := 0
		if err := cli.ContainerStop(restoreCtx, create.ID, dockercontainer.StopOptions{Timeout: &noGrace}); err != nil && !cerrdefs.IsNotFound(err) {
			log.Warn().Err(err).Str("container_id", create.ID).Msg("failure to stop container")
		}

		// Forced: the new container holds the original name, so a removal refused
		// because it is still running would also make the rename of the original
		// back to that name fail. A container that is already gone — the engine
		// removed it itself because the original ran with --rm, or the removal did
		// land and only the answer was lost — is the outcome we wanted, not an error.
		if err := cli.ContainerRemove(restoreCtx, create.ID, dockercontainer.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			newContainerErr = errors.Wrap(err, "remove new container error")
		}
	}()

	newContainerId := create.ID

	// 7. connect to networks
	// docker can connect to only one network at creation, so we need to connect to networks after creation
	// see https://github.com/moby/moby/issues/17750
	log.Debug().Str("container_id", newContainerId).Msg("connecting networks to container")
	networks := container.NetworkSettings.Networks
	for key, network := range networks {
		if _, ok := initialNetwork.EndpointsConfig[key]; ok {
			// skip the network that is used during container creation
			continue
		}

		if err := cli.NetworkConnect(ctx, network.NetworkID, newContainerId, network); err != nil {
			return nil, errors.Wrap(err, "connect container network error")
		}
	}

	// 8. start the new container
	log.Debug().Str("container_id", newContainerId).Msg("starting the new container")
	if err := cli.ContainerStart(ctx, newContainerId, dockercontainer.StartOptions{}); err != nil {
		return nil, errors.Wrap(err, "start container error")
	}

	// 9. delete the old container
	log.Debug().Str("container_id", containerId).Msg("starting to remove the old container")
	_ = cli.ContainerRemove(ctx, containerId, dockercontainer.RemoveOptions{})

	restore = false

	created, _, err := cli.ContainerInspectWithRaw(ctx, newContainerId, true)
	if err != nil {
		return nil, errors.Wrap(err, "fetch container information error")
	}

	return &created, nil
}
