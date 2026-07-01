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
	"github.com/portainer/portainer/api/internal/testhelpers"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

// newStackInspectClient builds a Docker client wired to a test server that answers
// ContainerInspect by name, returning the given new image id. It is the seam the
// post-redeploy best-effort "new digest" re-inspect uses.
func newStackInspectClient(t *testing.T, newImageIDByName map[string]string) *dockerclient.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, imageID := range newImageIDByName {
			if strings.HasSuffix(r.URL.Path, "/containers/"+name+"/json") {
				_ = json.NewEncoder(w).Encode(container.InspectResponse{
					ContainerJSONBase: &container.ContainerJSONBase{ID: name, Image: imageID},
					Config:            &container.Config{},
				})
				return
			}
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.WithHost(srv.URL),
		dockerclient.WithHTTPClient(http.DefaultClient),
	)
	require.NoError(t, err)

	return cli
}

// TestUpdateStackEmitsPerContainerEvents proves the maintainer's requirement: a
// (file) stack redeploy emits one EventUpdated PER updated member container, each
// carrying the compose stack name (from the container's label, not a Stack().Read)
// and a best-effort post-redeploy new image id — never a single aggregate stack
// event.
func TestUpdateStackEmitsPerContainerEvents(t *testing.T) {
	_, store := datastore.MustNewTestStore(t, true, false)

	// A stack author must exist for registry resolution; an admin resolves to the
	// (empty) registry set without needing endpoint/team wiring.
	require.NoError(t, store.User().Create(&portainer.User{ID: 1, Username: "auto", Role: portainer.AdministratorRole}))

	endpoint := &portainer.Endpoint{ID: 1, Name: "nebula.lc"}
	require.NoError(t, store.Endpoint().Create(endpoint))

	require.NoError(t, store.Stack().Create(&portainer.Stack{
		ID: 7, EndpointID: 1, Name: "cache-demo", Type: portainer.DockerComposeStack, CreatedBy: "auto",
	}))

	const (
		oldEsphome = "sha256:59b94983c73a000000000000000000000000000000000000000000000000aaaa"
		newEsphome = "sha256:2231ca5d676d000000000000000000000000000000000000000000000000bbbb"
		oldOther   = "sha256:1111111111110000000000000000000000000000000000000000000000000000"
		newOther   = "sha256:2222222222220000000000000000000000000000000000000000000000000000"
	)

	cli := newStackInspectClient(t, map[string]string{
		"esphome": newEsphome,
		"other":   newOther,
	})

	rec := &recordingNotifier{}
	s := &Service{
		baseCtx:       context.Background(),
		dataStore:     store,
		stackDeployer: testhelpers.NewTestStackDeployer(),
		notifier:      rec,
	}

	st := StackUpdate{
		StackID: 7,
		IsGit:   false,
		Containers: []UpdateCandidate{
			{Name: "esphome", ImageID: oldEsphome, Image: "esphome/esphome:latest", Labels: map[string]string{composeProjectLabel: "cache-demo"}},
			{Name: "other", ImageID: oldOther, Image: "redis:7", Labels: map[string]string{composeProjectLabel: "cache-demo"}},
		},
	}

	s.updateStack(cli, endpoint, st)

	require.Len(t, rec.events, 2, "one EventUpdated per updated member container, not one aggregate stack event")

	byContainer := map[string]Event{}
	for _, e := range rec.events {
		require.Equal(t, EventUpdated, e.Kind)
		require.Equal(t, "cache-demo", e.StackName, "each per-container event carries the compose stack name")
		require.Equal(t, 7, e.StackID)
		byContainer[e.ContainerName] = e
	}

	esphome, ok := byContainer["esphome"]
	require.True(t, ok, "expected a per-container event for esphome")
	require.Equal(t, oldEsphome, esphome.OldDigest)
	require.Equal(t, newEsphome, esphome.NewDigest, "the new image id is recovered by re-inspecting the container after redeploy")

	other, ok := byContainer["other"]
	require.True(t, ok, "expected a per-container event for other")
	require.Equal(t, oldOther, other.OldDigest)
	require.Equal(t, newOther, other.NewDigest)
}

// TestUpdateStackGitIsDetectOnly guards that a git stack stays detect-only: it is
// not redeployed and emits no notification (its image update lands on the next git
// change or a manual update).
func TestUpdateStackGitIsDetectOnly(t *testing.T) {
	_, store := datastore.MustNewTestStore(t, true, false)

	endpoint := &portainer.Endpoint{ID: 1, Name: "nebula.lc"}
	require.NoError(t, store.Endpoint().Create(endpoint))

	deployer := testhelpers.NewTestStackDeployer()
	rec := &recordingNotifier{}
	s := &Service{
		baseCtx:       context.Background(),
		dataStore:     store,
		stackDeployer: deployer,
		notifier:      rec,
	}

	cli := newStackInspectClient(t, nil)

	s.updateStack(cli, endpoint, StackUpdate{
		StackID: 9, IsGit: true,
		Containers: []UpdateCandidate{{Name: "esphome", Labels: map[string]string{composeProjectLabel: "cache-demo"}}},
	})

	require.Empty(t, rec.events, "a git stack is detect-only, no per-container notification")
	require.Zero(t, deployer.DeployComposeCallCount, "a git stack must not be redeployed here")
}
