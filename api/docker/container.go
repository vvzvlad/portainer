package docker

import (
	"context"
	"strings"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/logs"

	"github.com/Masterminds/semver/v3"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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

// restoreTeardownShare is the fraction of the restore budget the teardown of the
// NEW container may spend. Both halves of the restore draw on ONE budget so a
// dead engine cannot pin the caller for twice as long, but the teardown must not
// be able to drink it dry: it runs FIRST (LIFO), and a stop or a forced removal
// that hangs — a wedged runc, a process in D state, a storage-driver hiccup —
// would otherwise leave the restore of the original, the part that decides
// whether the workload is down, without a single call, turning a recoverable
// failure into an outage. A third is ample for a stop plus a forced removal
// against an engine that answers at all, and leaves two thirds for the rename
// back, the reconnects, the start and the verifying inspect.
const restoreTeardownShare = 3

type ContainerService struct {
	factory   *dockerclient.ClientFactory
	dataStore dataservices.DataStore
	// restoreTimeout is the whole time budget of one restore, defaultRestoreTimeout
	// when unset. It is a field rather than a constant read at the call site so a
	// test can drive the budget-exhaustion paths in milliseconds without mutating a
	// package-level knob shared by every other (parallel) test.
	restoreTimeout time.Duration
}

func NewContainerService(factory *dockerclient.ClientFactory, dataStore dataservices.DataStore) *ContainerService {
	return &ContainerService{
		factory:   factory,
		dataStore: dataStore,
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

// restoreBudget is the total time one restore may spend, falling back to
// defaultRestoreTimeout for a service built without an explicit budget.
func (c *ContainerService) restoreBudget() time.Duration {
	if c.restoreTimeout > 0 {
		return c.restoreTimeout
	}

	return defaultRestoreTimeout
}

// restoreContext derives the context the restore runs on. It is deliberately
// detached from the caller's context: the most common recreate failure is the
// caller's own deadline or cancellation (auto-update bounds a recreate with
// recreateTimeout), and reusing that dead context would make every restore call
// fail instantly, leaving the original container renamed, disconnected and
// stopped. The derived context keeps the caller's values but gets its own
// restoreBudget deadline.
//
// context.WithoutCancel only detaches the restore from the caller; it promises
// nothing about process shutdown. If the daemon exits mid-restore the sequence
// is cut at whatever step it had reached, exactly as before.
func (c *ContainerService) restoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), c.restoreBudget())
}

// restorePlan is what putting the original back still requires: whether it has
// to be renamed back, and which of its networks have to be reconnected.
type restorePlan struct {
	rename   bool
	networks []*network.EndpointSettings
}

// planRestore works out what the restore still has to undo. It is driven by the
// original's OBSERVED state, not by which of the teardown calls returned nil: a
// call whose answer was lost (the caller's deadline firing on a request the
// engine went on to execute) did happen and has to be undone, and a call that
// never landed must not be undone twice.
//
// observed is nil when the state could not be read at all. The plan then falls
// back to every step that was ATTEMPTED, which errs towards doing too much — the
// restore treats an "already in that state" refusal as success, so an unnecessary
// step is not reported as a failed restore.
func planRestore(observed *dockercontainer.InspectResponse, name string, renameAttempted bool, disconnectAttempted []*network.EndpointSettings) restorePlan {
	if observed == nil || observed.ContainerJSONBase == nil {
		return restorePlan{rename: renameAttempted, networks: disconnectAttempted}
	}

	attached := make(map[string]bool)
	if observed.NetworkSettings != nil {
		for _, endpointSettings := range observed.NetworkSettings.Networks {
			attached[endpointSettings.NetworkID] = true
		}
	}

	plan := restorePlan{rename: observed.Name != name}
	for _, endpointSettings := range disconnectAttempted {
		if !attached[endpointSettings.NetworkID] {
			plan.networks = append(plan.networks, endpointSettings)
		}
	}

	return plan
}

// alreadyNamed reports whether a refused rename back is in fact the outcome the
// restore wanted: the container already carries the name. The engine answers a
// rename to the name a container already holds with an invalid-parameter error
// ("Renaming a container with the same name as its current name"), while a name
// held by ANOTHER container comes back as a conflict — only the former means the
// step is done, so the message is checked on top of the error class.
func alreadyNamed(err error) bool {
	return cerrdefs.IsInvalidArgument(err) &&
		strings.Contains(strings.ToLower(err.Error()), "same name as its current name")
}

// alreadyConnected reports whether a refused network connect is in fact the
// outcome the restore wanted: the container is already attached. Engines differ
// on the status they use for it (a conflict, or the forbidden "endpoint with
// name X already exists in network Y"), so both are recognised.
func alreadyConnected(err error) bool {
	return cerrdefs.IsConflict(err) ||
		strings.Contains(strings.ToLower(err.Error()), "already exists in network")
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
// The rollback is not taken on trust: the original is inspected afterwards and
// must report State.Running, and what the rollback had to undo is planned from
// the original's observed state rather than from which calls returned nil. A
// restore that did not fully land is reported to the caller as a *RestoreError —
// never silently logged away — which says whether the original is running again
// (a degraded but serving workload: a name or a network that did not come back)
// or is left down (an outage).
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
		puller := images.NewPuller(cli, images.NewRegistryClient(c.dataStore), c.dataStore)
		if err := puller.Pull(ctx, img); err != nil {
			return nil, errors.Wrapf(err, "pull image error %s", img.FullName())
		}
	}

	// The original container stops serving the moment the engine is ASKED to stop
	// it, so every exit path from here on has to put it back. That includes a stop
	// that comes back with an ERROR: the engine may have carried it out anyway (the
	// answer was lost on a cancelled request, the failure came after the SIGKILL
	// landed), and a container left in Exited (137) with nothing restoring it is
	// exactly the reported symptom. Restoring a container that never actually
	// stopped costs one call and no harm: the engine answers a start on a running
	// container with 304, which the SDK reports as success. restore is cleared only
	// once the new container has taken over for good.
	restore := true

	// What the restore may have to undo. Each flag is raised BEFORE its call rather
	// than after it succeeds: the most common failure is a lost ANSWER to a call
	// the engine did execute (the caller's deadline firing mid-flight), which would
	// otherwise be recorded as never having happened, leaving the original named
	// "-old" or off its networks for good. Doing too much is safe because the
	// restore is idempotent — it is planned from the original's observed state, and
	// where that cannot be read an "already in that state" refusal counts as
	// success.
	var (
		renameAttempted     bool
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

		// Read the original's state first and plan the rollback from it. Where it
		// cannot be read the plan falls back to every step that was attempted.
		var observed *dockercontainer.InspectResponse

		switch current, _, err := cli.ContainerInspectWithRaw(restoreCtx, containerId, false); {
		case err == nil:
			observed = &current
		case cerrdefs.IsNotFound(err):
			// The original is gone: the engine reaped it (it ran with --rm), or someone
			// removed it. There is nothing left to put back, and a container we did not
			// take down must not be reported as an outage this recreate caused.
			log.Warn().Str("container_id", containerId).Str("container", strings.TrimPrefix(container.Name, "/")).
				Msg("the original container no longer exists, there is nothing to restore")

			return
		default:
			log.Warn().Err(err).Str("container_id", containerId).
				Msg("unable to read the original container state, restoring every step that was attempted")
		}

		plan := planRestore(observed, container.Name, renameAttempted, disconnectAttempted)

		if plan.rename {
			if err := cli.ContainerRename(restoreCtx, containerId, container.Name); err != nil && !alreadyNamed(err) {
				restoreErrs = append(restoreErrs, errors.Wrap(err, "rename container back error"))
			}
		}

		for _, endpointSettings := range plan.networks {
			if err := cli.NetworkConnect(restoreCtx, endpointSettings.NetworkID, containerId, endpointSettings); err != nil && !alreadyConnected(err) {
				restoreErrs = append(restoreErrs, errors.Wrapf(err, "connect container to network %s error", endpointSettings.NetworkID))
			}
		}

		// Whether the workload is serving again is the ORIGINAL's observed state and
		// nothing else. A successful start call is not proof of service (the container
		// may have exited at once), and a state that could not be read is not proof
		// either.
		originalRunning := false

		switch err := cli.ContainerStart(restoreCtx, containerId, dockercontainer.StartOptions{}); {
		case err == nil:
			restored, _, err := cli.ContainerInspectWithRaw(restoreCtx, containerId, false)
			switch {
			case err != nil:
				restoreErrs = append(restoreErrs, errors.Wrap(err, "inspect restored container error"))
			case restored.ContainerJSONBase == nil || restored.State == nil || !restored.State.Running:
				restoreErrs = append(restoreErrs, errors.New("restored container is not running"))
			default:
				originalRunning = true
			}
		case cerrdefs.IsNotFound(err):
			// It vanished between the inspect above and this start. Same verdict as
			// above: nothing to restore, and no outage to pin on this recreate.
			log.Warn().Errs("restore_errors", restoreErrs).
				Str("container_id", containerId).Str("container", strings.TrimPrefix(container.Name, "/")).
				Msg("the original container no longer exists, there is nothing to restore")

			return
		default:
			restoreErrs = append(restoreErrs, errors.Wrap(err, "start container error"))
		}

		if originalRunning && len(restoreErrs) == 0 {
			// The workload is serving again exactly as before, so this is an ordinary
			// failed recreate and the caller keeps the original error. A new container
			// that could not be removed is loud, but it is a leftover to clean up rather
			// than an outage, and must not be dressed up as one.
			if newContainerErr != nil {
				log.Warn().Err(newContainerErr).Str("container_id", containerId).
					Msg("the new container could not be removed after a failed recreate, the original is running again but the leftover needs cleaning up")
			}

			return
		}

		// Something did not come back. A new container that could not be removed
		// belongs in the report: holding the original name, it is a common reason the
		// rename back could not land.
		errs := restoreErrs
		if newContainerErr != nil {
			errs = append([]error{newContainerErr}, restoreErrs...)
		}

		event := log.Error().Errs("restore_errors", errs).
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
		if originalRunning {
			// Serving, but not as it was: it may answer under the wrong name (a stack
			// peer resolving "web" would not find it) or be missing a network. Loud, and
			// still not an outage.
			event.Msg("recreate failed and the original container is running again, but the restore was incomplete")
		} else {
			event.Msg("recreate failed and the original container could not be restored, it is left down")
		}

		err = &RestoreError{ContainerID: containerId, Name: container.Name, Cause: err, Errs: errs, OriginalRunning: originalRunning}
	}()

	// 2. stop the current container
	log.Debug().Str("container_id", containerId).Msg("starting to stop the container")
	if err := cli.ContainerStop(ctx, containerId, dockercontainer.StopOptions{}); err != nil {
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
		// container anyway.
		if err := cli.ContainerStop(restoreCtx, create.ID, dockercontainer.StopOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
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
