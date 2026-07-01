package containers

import (
	"net/http"

	"github.com/portainer/portainer/api/docker/images"
	"github.com/portainer/portainer/api/http/middlewares"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"
	"github.com/portainer/portainer/pkg/libhttp/request"
	"github.com/portainer/portainer/pkg/libhttp/response"

	"github.com/rs/zerolog/log"
)

// imageStatusResponse is the body returned by the image status endpoint.
type imageStatusResponse struct {
	// Status of the running container image. One of:
	// "outdated", "updated", "skipped", "processing", "preparing", "error".
	Status string `json:"Status"`
	// Message holds an optional human-readable detail, typically the detection error.
	Message string `json:"Message,omitempty"`
}

// @id ContainerImageStatus
// @summary Fetch the image status of a container
// @description Detect whether a newer image is available for the running container by
// @description comparing the local image digest against the remote registry digest.
// @description This is a read-only operation: it never pulls or recreates anything.
// @description Engine-level issues (container not found, registry unreachable, auth
// @description failure, ...) are not treated as API errors: they degrade gracefully to a
// @description 200 response carrying a "skipped" or "error" status. HTTP errors are only
// @description returned for request/authorization problems.
// @description **Access policy**: authenticated
// @tags docker
// @security ApiKeyAuth
// @security jwt
// @produce json
// @param id path int true "Environment identifier"
// @param containerId path string true "Container identifier"
// @param nodeName query string false "Node name for a Swarm/agent endpoint"
// @param force query bool false "Bypass the server-side status cache and recompute against the registry (manual re-check)"
// @success 200 {object} imageStatusResponse "Image status (also returned with a skipped/error status for engine-level issues)"
// @failure 400 "Invalid request: missing container identifier"
// @failure 403 "Permission denied to access the environment"
// @failure 404 "Environment not found"
// @router /docker/{id}/containers/{containerId}/image_status [get]
func (handler *Handler) imageStatus(w http.ResponseWriter, r *http.Request) *httperror.HandlerError {
	containerID, err := request.RetrieveRouteVariableValue(r, "containerId")
	if err != nil {
		return httperror.BadRequest("Invalid containerId", err)
	}

	// nodeName is optional and only relevant for Swarm/agent endpoints.
	nodeName, _ := request.RetrieveQueryParameter(r, "nodeName", true)

	// force is set by the UI's manual "re-check" action to bypass the ~5m server
	// cache and recompute against the registry. Absent (the default, used by the
	// per-row auto-badges) the cached value is served as before.
	force, _ := request.RetrieveBooleanQueryParameter(r, "force", true)

	endpoint, err := middlewares.FetchEndpoint(r)
	if err != nil {
		return httperror.NotFound("Unable to find an environment on request context", err)
	}

	if err := handler.bouncer.AuthorizedEndpointOperation(r, endpoint); err != nil {
		return httperror.Forbidden("Permission denied to access environment", err)
	}

	// The detection engine (zlib/CE) routes outbound registry calls through the
	// RegistryClient, which honors the encrypted credential store. It caches results
	// briefly and skips digest-pinned/local-only images. Note: the outbound registry
	// HEAD (RemoteDigest -> docker.GetDigest) is NOT run through an SSRF/AllowList
	// filter; this mirrors upstream ContainersImageStatus behaviour.
	digestClient := images.NewClientWithRegistry(images.NewRegistryClient(handler.dataStore), handler.dockerClientFactory)

	statusFn := digestClient.ContainerImageStatus
	if force {
		statusFn = digestClient.ContainerImageStatusForced
	}

	status, err := statusFn(r.Context(), containerID, endpoint, nodeName)
	if err != nil {
		// A detection failure (registry unreachable, auth failure, ...) is not an API
		// failure: degrade gracefully with a 200 + "error" status so the UI can render a
		// neutral badge instead of surfacing a hard error. The raw error is logged
		// server-side only; the response carries a generic message to avoid leaking
		// registry URLs or credential details to the client.
		log.Warn().Err(err).Str("containerId", containerID).Msg("unable to determine container image status")

		return response.JSON(w, &imageStatusResponse{Status: string(images.Error), Message: "unable to determine image status"})
	}

	return response.JSON(w, &imageStatusResponse{Status: string(status)})
}
