package containerautomation

import (
	"context"

	portainer "github.com/portainer/portainer/api"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

// dockerClient is the minimal subset of the Docker SDK client
// (*github.com/docker/docker/client.Client) that the auto-update and auto-heal
// apply paths call. Threading this interface — instead of the concrete client —
// through the daemon paths lets their wiring/sequencing be exercised with a fake
// in tests, while the real client returned by ClientFactory.CreateClient
// satisfies it unchanged at the call sites.
type dockerClient interface {
	// ContainerInspect backs the pre-update image-identity capture
	// (updateStandalone) and the health-gate poll (healthGate).
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	// ContainerRestart backs the auto-heal restart of an unhealthy container.
	ContainerRestart(ctx context.Context, containerID string, options container.StopOptions) error
	// ImageTag backs the rollback re-tag of the previous image onto the original ref.
	ImageTag(ctx context.Context, source, target string) error
	// ImageRemove backs the conservative cleanup of the dangling old image.
	ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error)
}

// containerRecreator is the minimal seam over *docker.ContainerService used by the
// standalone update and rollback paths to recreate a container (pull + stop +
// create + start). The concrete *docker.ContainerService satisfies it; threading
// the interface lets the recreate step be faked so the surrounding sequencing
// (recreate -> health gate -> cleanup / rollback) is testable without a live
// engine.
type containerRecreator interface {
	Recreate(ctx context.Context, endpoint *portainer.Endpoint, containerID string, forcePullImage bool, imageTag, nodeName string) (*types.ContainerJSON, error)
}
