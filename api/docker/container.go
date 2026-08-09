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
	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// restoreTimeout bounds the best-effort restore of the original container after a
// failed recreate. The restore runs on a context detached from the caller's (see
// restoreContext), so it needs a deadline of its own: long enough for a rename, a
// handful of network connects and a start against a healthy engine, short enough
// that an unresponsive engine cannot pin the caller's goroutine for minutes.
const restoreTimeout = 30 * time.Second

type ContainerService struct {
	factory   *dockerclient.ClientFactory
	dataStore dataservices.DataStore
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

// restoreContext derives the context used by the restore/teardown defers. It is
// deliberately detached from the caller's context: the most common recreate
// failure is the caller's own deadline or cancellation (auto-update bounds a
// recreate with recreateTimeout), and reusing that dead context would make every
// restore call fail instantly, leaving the original container renamed,
// disconnected and stopped. The derived context keeps the caller's values but
// gets its own restoreTimeout deadline.
func restoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
}

// Recreate a container.
//
// The original container is kept until the new one has actually started, so a
// failure at any step before that rolls back to it. When that rollback itself
// fails the workload is left down, which is reported to the caller as a
// *RestoreError wrapping the original failure — never silently logged away.
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

	// 2. stop the current container
	log.Debug().Str("container_id", containerId).Msg("starting to stop the container")
	if err := cli.ContainerStop(ctx, containerId, dockercontainer.StopOptions{}); err != nil {
		return nil, errors.Wrap(err, "stop container error")
	}

	// 3. rename the current container
	log.Debug().Str("container_id", containerId).Msg("starting to rename the container")
	if err := cli.ContainerRename(ctx, containerId, container.Name+"-old"); err != nil {
		return nil, errors.Wrap(err, "rename container error")
	}

	initialNetwork := network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}

	// 4. disconnect all networks from the current container
	for name, network := range container.NetworkSettings.Networks {
		// This allows new container to use the same IP address if specified
		if err := cli.NetworkDisconnect(ctx, network.NetworkID, containerId, true); err != nil {
			return nil, errors.Wrap(err, "disconnect network from old container error")
		}

		// 5. get the first network attached to the current container
		if len(initialNetwork.EndpointsConfig) == 0 {
			// Retrieve the first network that is linked to the present container, which
			// will be utilized when creating the container.
			initialNetwork.EndpointsConfig[name] = network
		}
	}

	restore := true

	// restoreErrs collects everything that goes wrong while putting the original
	// container back in service, across both defers below. A non-empty slice means
	// the workload is DOWN, which the restore defer reports as a *RestoreError
	// instead of swallowing it into a Warn nobody acts on.
	var restoreErrs []error

	// Restore of the original container. Registered FIRST, so LIFO runs it LAST,
	// after the teardown defer registered below has removed the new container: the
	// new container owns the original name, so it has to be gone before the
	// original can be renamed back.
	defer func() {
		if !restore {
			return
		}

		restoreCtx, cancel := restoreContext(ctx)
		defer cancel()

		log.Debug().Str("container_id", containerId).Str("container", container.Name).Msg("restoring the container")
		if err := cli.ContainerRename(restoreCtx, containerId, container.Name); err != nil {
			restoreErrs = append(restoreErrs, errors.Wrap(err, "rename container back error"))
		}

		for _, network := range container.NetworkSettings.Networks {
			if err := cli.NetworkConnect(restoreCtx, network.NetworkID, containerId, network); err != nil {
				restoreErrs = append(restoreErrs, errors.Wrapf(err, "connect container to network %s error", network.NetworkID))
			}
		}

		if err := cli.ContainerStart(restoreCtx, containerId, dockercontainer.StartOptions{}); err != nil {
			restoreErrs = append(restoreErrs, errors.Wrap(err, "start container error"))
		} else {
			// A successful start call is not proof of service: the container may have
			// exited immediately. Only a running state means the original is back.
			restored, _, err := cli.ContainerInspectWithRaw(restoreCtx, containerId, false)
			switch {
			case err != nil:
				restoreErrs = append(restoreErrs, errors.Wrap(err, "inspect restored container error"))
			case restored.ContainerJSONBase == nil || restored.State == nil || !restored.State.Running:
				restoreErrs = append(restoreErrs, errors.New("restored container is not running"))
			}
		}

		if len(restoreErrs) == 0 {
			return
		}

		log.Error().Err(err).Errs("restore_errors", restoreErrs).
			Str("container_id", containerId).
			Str("container", strings.TrimPrefix(container.Name, "/")).
			Int("endpoint_id", int(endpoint.ID)).
			Str("endpoint", endpoint.Name).
			Msg("recreate failed and the original container could not be restored, it is left down")

		err = &RestoreError{ContainerID: containerId, Name: container.Name, Cause: err, Errs: restoreErrs}
	}()

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

		restoreCtx, cancel := restoreContext(ctx)
		defer cancel()

		log.Debug().Str("container_id", create.ID).Msg("removing the new container")

		// A stop failure is not fatal on its own, the forced removal below kills the
		// container anyway.
		if err := cli.ContainerStop(restoreCtx, create.ID, dockercontainer.StopOptions{}); err != nil {
			log.Warn().Err(err).Str("container_id", create.ID).Msg("failure to stop container")
		}

		// Forced: the new container holds the original name, so a removal refused
		// because it is still running would also make the rename of the original
		// back to that name fail.
		if err := cli.ContainerRemove(restoreCtx, create.ID, dockercontainer.RemoveOptions{Force: true}); err != nil {
			restoreErrs = append(restoreErrs, errors.Wrap(err, "remove new container error"))
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
