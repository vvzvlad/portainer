package webhooks

import (
	"net/http"

	"github.com/portainer/portainer/api/dataservices"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/http/security"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"

	"github.com/gorilla/mux"
)

// ContainerAutomationTrigger triggers an immediate container auto-update pass. It
// is a minimal interface (mirroring the settings handler's
// ContainerAutomationReloader) so the webhooks handler does not depend on the
// concrete container-automation service. It is satisfied by
// *containerautomation.Service.
type ContainerAutomationTrigger interface {
	TriggerUpdate()
}

// Handler is the HTTP handler used to handle webhook operations.
type Handler struct {
	*mux.Router
	requestBouncer      security.BouncerService
	DataStore           dataservices.DataStore
	DockerClientFactory *dockerclient.ClientFactory
	// ContainerAutomationService receives the inbound registry-push kick that
	// triggers an immediate auto-update pass. It may be nil (the endpoint then
	// reports the feature as unavailable rather than panicking).
	ContainerAutomationService ContainerAutomationTrigger
}

// NewHandler creates a handler to manage webhooks operations.
func NewHandler(bouncer security.BouncerService) *Handler {
	h := &Handler{
		Router:         mux.NewRouter(),
		requestBouncer: bouncer,
	}
	h.Handle("/webhooks",
		bouncer.AuthenticatedAccess(httperror.LoggerHandler(h.webhookCreate))).Methods(http.MethodPost)
	h.Handle("/webhooks/{id}",
		bouncer.AuthenticatedAccess(httperror.LoggerHandler(h.webhookUpdate))).Methods(http.MethodPut)
	h.Handle("/webhooks",
		bouncer.AuthenticatedAccess(httperror.LoggerHandler(h.webhookList))).Methods(http.MethodGet)
	h.Handle("/webhooks/{id}",
		bouncer.AuthenticatedAccess(httperror.LoggerHandler(h.webhookDelete))).Methods(http.MethodDelete)
	h.Handle("/webhooks/{token}",
		bouncer.PublicAccess(httperror.LoggerHandler(h.webhookExecute))).Methods(http.MethodPost)
	// Inbound registry-push trigger for native container auto-update. The extra path
	// segment ("container-automation") keeps it distinct from the resource webhook
	// above, which matches a single {token} segment.
	h.Handle("/webhooks/container-automation/{token}",
		bouncer.PublicAccess(httperror.LoggerHandler(h.webhookContainerAutomation))).Methods(http.MethodPost)

	return h
}
