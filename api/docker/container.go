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
// runs on a Docker client and a context of its own (see pullImage). The bound the
// client factory knows how to set is http.Client.Timeout, and that covers the
// WHOLE exchange, the reading of the response body included — while a pull holds
// its progress stream open for the entire download. Under the factory's 60s
// default that makes every image taking longer than a minute to come down fail
// mid-download, however generous the caller's own deadline is: auto-update hands
// the recreate ten minutes and still could not pull a large image over a slow
// link.
//
// The hour is chosen from what the images this bound has to carry actually weigh,
// not from taste. Take an image of 6-7 GB, the order the largest auto-updated
// images run to: pulling it inside the old 60s meant about 112 MB/s sustained end
// to end, which is LAN speed and not what these endpoints sit behind — so for a
// whole class of images the old ceiling did not make failure likely, it made it
// certain before the request was even sent. An hour errs the other way by the
// same arithmetic: to exceed it, an image of that size would have to average under
// about 1.9 MB/s for the entire download, which is not a slow link but a broken
// one.
//
// What this bound is NOT is the thing that notices a wedged pull. A pull that has
// gone silent — a half-open connection, a registry that stopped answering — is
// caught a minute after its last byte by defaultPullStallTimeout in
// api/docker/images, which bounds the ABSENCE OF PROGRESS instead of the elapsed
// time and so asks nothing about how big the image is or how fast the link is.
// The hour is the absolute backstop behind it: what still ends a pull that never
// stops dribbling bytes, which is the one failure a stall detector cannot see. The
// two answer different questions and neither replaces the other.
//
// The same magnitude is written down elsewhere in the tree — the stack deployer
// builds its unpacker client with 3600s (see stackDeployer.createDockerClient) —
// but that is a recorded intent rather than a running example to point at: in CE
// the unpacker path is unreachable, since the only callers of the remote-stack
// deployments sit behind stackutils.IsRelativePathStack, which returns a
// hardcoded false.
//
// One kind of endpoint does not get the hour, and cannot be given it from here.
// An Edge tunnel is closed once it has been idle longer than chisel's
// activeTimeout (4m30s, see checkTunnels), and nothing refreshes the tunnel's
// LastActivity while a pull streams — the Docker SDK client this service builds
// talks past the proxy transport that would have. The pull does at least begin
// with a fresh window: building its client goes through TunnelAddr, which calls
// UpdateLastActivity before it hands the address back, so the idle counter is
// reset immediately before the download starts. What an Edge pull gets is
// therefore a worst case rather than a ceiling — as little as ~4m30s, and more
// only because checkTunnels samples on an interval instead of at the instant the
// timeout passes. Lifting it would mean changing the tunnel service; stating it
// honestly here is the alternative.
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
// the image pull for everything the recreate does after it. The discipline is the
// one the restore shares above already follow, applied to the step that escaped
// it: the pull runs first and is by far the longest, and until it returns nothing
// has been touched — but the moment it returns, the recreate starts taking the
// workload down. A pull that spent the caller's whole window and then SUCCEEDED
// would hand the stop, the rename, the create and the start a context with
// nothing left in it, and a stop whose ANSWER is lost is the worst outcome of the
// lot: the engine may carry it out regardless, which puts the recreate in the
// restore path — a brief real outage of the workload — where a pull that simply
// ran out of time gives the clean "the update did not happen, nothing was
// touched". Auto-update bounds a whole recreate with ten minutes, so a sixth held
// back leaves the pull 8m20s and the handful of short control-plane calls that
// follow 1m40s, which is ample for calls that answer in milliseconds against an
// engine that answers at all.
const pullReserveShare = 6

// ClientFactory creates Docker clients for a given environment.
//
// ContainerService holds the interface rather than the concrete
// *client.ClientFactory so a test can see the timeout each client is ASKED for.
// For the pull that argument is the whole of the fix in this file — it is what
// keeps the factory's 60s default off the pull client on tcp and agent endpoints,
// which is most of them — and it cannot be observed any other way: the pull's own
// context deadline is armed before the request goes out, so it always expires
// first and the client's timeout never gets to fire. Pinning it by timing would
// take a test that sits through the whole minute.
//
// It is declared here rather than imported from api/docker/images, which declares
// its own identical one for the same reason: the interface belongs to the package
// that consumes it, not to the package that implements it.
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

// pullBudget is the whole time budget of one image pull, and the only reading of
// pullTimeout there is: both bounds derived from it — the http.Client.Timeout of
// the client the pull gets and the deadline of the context it runs on — go
// through here.
//
// A ContainerService built without NewContainerService carries a zero budget, and
// a zero timeout is an already-expired context: every forced pull would fail
// before it left the process, so no recreate that pulls could ever succeed.
// Falling back to the default keeps that from ever being a silent switch-off.
//
// A caller that brought a deadline of its own has the budget capped to what is
// left of that deadline less pullReserveShare of it, so the pull cannot spend the
// whole window and leave the destructive steps after it running on a context with
// nothing in it. The cap only ever shortens the budget — the smaller of the two
// wins, and a caller with no deadline at all (the recreate HTTP handler passes
// context.TODO()) is capped by nothing. A caller whose deadline has ALREADY
// passed keeps its non-positive remainder rather than being handed the default:
// the pull it gets is one that has already expired, never an unbounded one — and
// pullImage refuses such a budget outright rather than building a client on it.
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
// It cannot share the client the rest of Recreate uses. That one serves the short
// control-plane calls — the inspects, the stop, the renames, the create, the
// start, the removals and every call of the restore — which want a SHORT
// http.Client.Timeout so an unresponsive engine cannot pin the caller for long;
// one caller (the recreate HTTP handler) hands Recreate a context with no
// deadline at all, so for it that timeout is the only bound there is. A pull wants
// the opposite, and one client cannot be both.
//
// nodeName is the same one the recreate's own client carries. On an agent cluster
// it is what routes the request to a particular node, and dropping it would pull
// the image on whichever node the agent happened to pick — leaving the node the
// container is actually recreated on to create it from the image it already has,
// silently running the old one.
//
// Of the two bounds cut from the one budget, the CONTEXT is the one that
// actually fires. It is armed before the request goes out, so on every kind of
// endpoint it expires before the client's own http.Client.Timeout could — and on
// an endpoint reached over a unix socket or a named pipe it is the only bound
// there is at all, since createLocalClient ignores the timeout argument entirely
// and such a client carries no http.Client.Timeout. What the client's timeout is
// for is that the factory's 60s default cannot come back silently: asking for the
// client without one is exactly the bug this function exists to undo, and the
// timeout is the bound that would remain if the context here were ever taken
// away. A caller with a tighter deadline of its own still wins, since
// context.WithTimeout keeps the earlier of the two — and pullBudget holds a share
// of that deadline back for the steps after the pull.
func (c *ContainerService) pullImage(ctx context.Context, endpoint *portainer.Endpoint, nodeName string, img images.Image) error {
	budget := c.pullBudget(ctx)

	// A budget that is not positive is a caller whose deadline has already passed,
	// and there is no pull to be had out of no time. Building a client for it would
	// not merely be wasted work: net/http reads a non-positive http.Client.Timeout as
	// NO timeout at all, so the factory would hand back precisely the unbounded
	// client the argument below exists to prevent, while the context derived from
	// this same budget is already dead — the one case where NEITHER bound would
	// exist. Returning here is what keeps the timeout handed to the factory positive
	// by construction, and the claim above — that the client's timeout is the bound
	// that would remain if the context were taken away — true on every path.
	//
	// The error is not ctx.Err() on its own because ctx.Err() can still be nil for
	// the instant after a deadline passes, and errors.Wrap of a nil error is nil,
	// which the caller would read as a pull that succeeded.
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
// through here. A ContainerService built without NewContainerService carries a
// zero budget, and a zero timeout is an already-expired context: every restore
// call would fail before it left the process, leaving the original renamed,
// disconnected and stopped. Falling back to the default keeps that from ever
// being a silent switch-off, in the derived budgets as much as in the whole.
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
// observed is nil when the state could not be read at all. The plan then falls
// back to every step that was ATTEMPTED, which errs towards doing too much — and
// harmlessly so: a superfluous step is refused by the engine ("already in that
// state") and cannot spoil the verdict, which is read off the container at the
// end of the restore rather than off which calls came back clean.
//
// The observed state says what is to be undone, but only among the steps this
// recreate attempted: a name that does not match while no rename was even issued
// is somebody ELSE's rename, and putting the container back under the name this
// recreate happens to remember is not a mandate the restore has.
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
// The rollback is not taken on trust, and it is not judged by which of its calls
// came back clean either: the original is inspected at the end, and the verdict is
// what that inspect says — running, carrying its own name, attached to the
// networks it was detached from. A restore that did not fully land is reported to
// the caller as a *RestoreError — never silently logged away — which says whether
// the original is running again (a degraded but serving workload: a name or a
// network that did not come back) or is left down (an outage).
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
	// a step taken needlessly is refused by the engine without changing anything.
	//
	// stopSaidGone is the exception that is read off the ANSWER rather than raised
	// before the call: a stop refused with "no such container" is the one answer
	// that proves this recreate did not take the container down, because there was
	// nothing there to take down. It is what tells a --rm container the engine
	// reaped on OUR stop from one that had already exited and been reaped before we
	// asked (the ordinary way a --rm container dies, and none of our doing).
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
