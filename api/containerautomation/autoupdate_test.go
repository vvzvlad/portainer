package containerautomation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/docker"
	dockerclient "github.com/portainer/portainer/api/docker/client"
	"github.com/portainer/portainer/api/docker/images"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

// TestUpdateEndpointRecreatesComposeStackMemberIndividually is the regression
// guard for the reported bug: a member of a PORTAINER-MANAGED compose stack that
// is in scope and outdated must be recreated INDIVIDUALLY via the standalone
// recreate path (like Watchtower), never redeployed as a whole stack.
//
// The test store is seeded with a real Portainer Docker Compose stack whose name
// matches the container's com.docker.compose.project label and endpoint, so the
// container is a genuine managed stack member — exactly the case that used to
// trigger a whole-stack redeploy under the old routing. Under the fix it flows
// straight to updateStandalone, which calls containerService.Recreate for that
// single container.
//
// It drives the real updateEndpoint against a mock Docker daemon (the production
// ClientFactory reaches loopback because SSRF filtering is inactive when the
// global dialer is unconfigured, as in a plain unit test). The individual recreate
// is deliberately failed at the image pull (the mock daemon rejects
// POST /images/create), which keeps the test fully offline and avoids faking the
// entire successful recreate sequence. That is sufficient to lock in the routing:
// updateStandalone emits EventUpdateFailed ONLY after it has invoked Recreate for
// this container — so the single per-container event proves the managed stack
// member reached the individual recreate path rather than a whole-stack redeploy.
func TestUpdateEndpointRecreatesComposeStackMemberIndividually(t *testing.T) {
	const (
		containerID = "web-1"
		imageID     = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	// Mock Docker daemon: enough of the API to let updateEndpoint list one
	// compose-labelled container, resolve its status, and begin an individual
	// recreate whose image pull is rejected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			// API version negotiation performed by the production client.
			w.Header().Set("Api-Version", "1.41")
			w.Header().Set("Ostype", "linux")
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/create"):
			// The forced re-pull inside Recreate: reject it so the individual
			// recreate fails deterministically without contacting a real registry.
			http.Error(w, `{"message":"pull rejected in test"}`, http.StatusInternalServerError)

		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			// ContainerList: a single running, compose-stack-labelled container.
			_ = json.NewEncoder(w).Encode([]container.Summary{{
				ID:      containerID,
				Names:   []string{"/" + containerID},
				Image:   "nginx:latest",
				ImageID: imageID,
				Labels: map[string]string{
					"com.docker.compose.project": "regression-stack",
					"io.portainer.update.enable": "true",
				},
			}})

		case strings.HasSuffix(r.URL.Path, "/containers/"+containerID+"/json"):
			// ContainerInspect (status check) and ContainerInspectWithRaw (recreate).
			// The sha256 Image lets the status check resolve the local image id; the
			// Config.Image is a parseable reference so Recreate proceeds to the pull.
			_ = json.NewEncoder(w).Encode(container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    containerID,
					Name:  "/" + containerID,
					Image: imageID,
				},
				Config: &container.Config{Image: "nginx:latest"},
			})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// Seed the status cache keyed by the local image id so the status check reports
	// Outdated without any registry lookup, making the candidate eligible.
	images.CacheResourceImageStatus(imageID, images.Outdated)
	t.Cleanup(func() { images.EvictImageStatus(imageID) })

	_, store := datastore.MustNewTestStore(t, true, false)

	// Seed a real Portainer-managed Docker Compose stack whose name matches the
	// container's compose project label on this endpoint, so the container is a
	// genuine managed stack member (the case that used to trigger a whole-stack
	// redeploy), not an externally-managed compose container.
	require.NoError(t, store.Stack().Create(&portainer.Stack{
		ID: 1, Name: "regression-stack", Type: portainer.DockerComposeStack, EndpointID: 1,
	}))

	// One ClientFactory shared by updateEndpoint, the digest client and the
	// container service, all pointed at the mock daemon via the endpoint URL.
	factory := dockerclient.NewClientFactory(nil, nil)
	rec := &recordingNotifier{}
	s := &Service{
		baseCtx:          context.Background(),
		dataStore:        store,
		clientFactory:    factory,
		digestClient:     images.NewClientWithRegistry(images.NewRegistryClient(store), factory),
		containerService: docker.NewContainerService(factory, store),
		notifier:         rec,
		rolledBack:       make(map[string]rolledBackTarget),
	}

	endpoint := &portainer.Endpoint{ID: 1, Name: "nebula.lc", URL: srv.URL, Type: portainer.DockerEnvironment}

	s.updateEndpoint(endpoint, ScopeAll, updateOptions{})

	// Exactly one per-container event, and it is the standalone recreate outcome for
	// this managed stack member. Under the old routing this member would have gone to
	// the whole-stack redeploy path (a different flow), so the single per-container
	// recreate event is the real discriminator; StackID staying zero is a secondary
	// confirmation that no stack-scoped redeploy was taken.
	require.Len(t, rec.events, 1, "the managed stack member is recreated individually, emitting one per-container event")
	require.Equal(t, EventUpdateFailed, rec.events[0].Kind)
	require.Equal(t, containerID, rec.events[0].ContainerName)
	require.Contains(t, rec.events[0].Message, "recreate", "the event reflects the individual recreate path")
	require.Zero(t, rec.events[0].StackID, "no whole-stack redeploy path is taken for a managed stack member")
}
