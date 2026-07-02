package stacks

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/filesystem"
	gittypes "github.com/portainer/portainer/api/git/types"
	"github.com/portainer/portainer/api/internal/testhelpers"

	"encoding/json"

	"github.com/stretchr/testify/require"
)

func TestStackFile_GitPendingRedeploy_Returns409(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	_, err := mockCreateUser(store)
	require.NoError(t, err)

	endpoint, err := mockCreateEndpoint(store)
	require.NoError(t, err)

	tempDir := t.TempDir()
	fileService, err := filesystem.NewService(tempDir, "")
	require.NoError(t, err)

	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.FileService = fileService
	handler.DataStore = store

	const stackID = portainer.StackID(1)

	src := &portainer.Source{
		Type: portainer.SourceTypeGit,
		Git: &gittypes.RepoConfig{
			URL:            "https://github.com/portainer/portainer.git",
			ConfigFilePath: "docker-compose.yml",
		},
	}
	require.NoError(t, store.Source().Create(src))

	wf := &portainer.Workflow{Artifacts: []portainer.Artifact{{
		StackID: stackID,
		Files:   []portainer.ArtifactFile{{SourceID: src.ID}},
	}}}
	require.NoError(t, store.Workflow().Create(wf))

	stack := &portainer.Stack{
		ID:         stackID,
		EndpointID: endpoint.ID,
		Type:       portainer.DockerComposeStack,
		WorkflowID: wf.ID,
		CurrentDeploymentInfo: &portainer.StackDeploymentInfo{
			RepositoryURL:  "https://github.com/portainer/old-repo.git",
			ConfigFilePath: "docker-compose.yml",
		},
	}
	require.NoError(t, store.Stack().Create(stack))

	req := mockCreateStackRequestWithSecurityContext(
		http.MethodGet,
		"/stacks/"+strconv.Itoa(int(stack.ID))+"/file",
		nil,
	)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusConflict, rr.Code)
}

// setupFileVersionStackTest wires a handler over a real datastore + filesystem and creates a
// file-based (non-git) stack together with the given on-disk file versions. It returns the handler
// and the created stack so version-selection cases can be exercised through the HTTP handler.
func setupFileVersionStackTest(t *testing.T, stack *portainer.Stack, versionContent map[int]string) *Handler {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, false, true)

	_, err := mockCreateUser(store)
	require.NoError(t, err)

	endpoint, err := mockCreateEndpoint(store)
	require.NoError(t, err)

	fileService, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.FileService = fileService
	handler.DataStore = store

	stackFolder := strconv.Itoa(int(stack.ID))
	for v, content := range versionContent {
		_, err := fileService.StoreStackFileFromBytesByVersion(stackFolder, stack.EntryPoint, v, []byte(content))
		require.NoError(t, err)
	}

	stack.EndpointID = endpoint.ID
	require.NoError(t, store.Stack().Create(stack))

	return handler
}

// requestStackFile performs a GET /stacks/{id}/file request (optionally with a raw query string).
func requestStackFile(t *testing.T, handler *Handler, stackID portainer.StackID, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()

	target := "/stacks/" + strconv.Itoa(int(stackID)) + "/file"
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	req := mockCreateStackRequestWithSecurityContext(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	return rr
}

// TestStackFile_VersionParam exercises the ?version= selector on a file-based stack: rejects a
// negative version, rejects an out-of-range version, and returns the selected version's content.
func TestStackFile_VersionParam(t *testing.T) {
	t.Parallel()

	newHandlerAndStack := func(t *testing.T) (*Handler, portainer.StackID) {
		stack := &portainer.Stack{
			ID:               10,
			Type:             portainer.DockerComposeStack,
			EntryPoint:       "docker-compose.yml",
			StackFileVersion: 3,
			Versions: []portainer.StackFileVersionInfo{
				{Version: 1}, {Version: 2}, {Version: 3},
			},
		}
		handler := setupFileVersionStackTest(t, stack, map[int]string{
			1: "V1-CONTENT", 2: "V2-CONTENT", 3: "V3-CONTENT",
		})
		// Point ProjectPath at the current version directory (as the versioning code does).
		stack.ProjectPath = handler.FileService.GetStackProjectPathByVersion("10", 3, "")
		require.NoError(t, handler.DataStore.Stack().Update(stack.ID, stack))

		return handler, stack.ID
	}

	t.Run("negative version returns 400", func(t *testing.T) {
		handler, id := newHandlerAndStack(t)
		rr := requestStackFile(t, handler, id, "version=-1")
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("out-of-range version returns 400", func(t *testing.T) {
		handler, id := newHandlerAndStack(t)
		rr := requestStackFile(t, handler, id, "version=99")
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("valid version returns that version content", func(t *testing.T) {
		handler, id := newHandlerAndStack(t)
		rr := requestStackFile(t, handler, id, "version=2")
		require.Equal(t, http.StatusOK, rr.Code)

		var resp stackFileResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.Equal(t, "V2-CONTENT", resp.StackFileContent)
	})
}

// TestStackFile_VersionParam_LegacyFallback covers the fallback branch for stacks predating the
// version history seed: when Versions[] is empty, an in-range version (1..StackFileVersion) is
// accepted and served from its v{N} directory.
func TestStackFile_VersionParam_LegacyFallback(t *testing.T) {
	t.Parallel()

	stack := &portainer.Stack{
		ID:               11,
		Type:             portainer.DockerComposeStack,
		EntryPoint:       "docker-compose.yml",
		StackFileVersion: 2,
		// Versions intentionally empty: legacy stack without a recorded history.
	}
	handler := setupFileVersionStackTest(t, stack, map[int]string{
		1: "LEGACY-V1", 2: "LEGACY-V2",
	})
	stack.ProjectPath = handler.FileService.GetStackProjectPathByVersion("11", 2, "")
	require.NoError(t, handler.DataStore.Stack().Update(stack.ID, stack))

	rr := requestStackFile(t, handler, stack.ID, "version=1")
	require.Equal(t, http.StatusOK, rr.Code)

	var resp stackFileResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "LEGACY-V1", resp.StackFileContent)
}

func TestStackFile_MatchingGitSettings_ReturnsFileContent(t *testing.T) {
	t.Parallel()
	_, store := datastore.MustNewTestStore(t, false, true)

	_, err := mockCreateUser(store)
	require.NoError(t, err)

	endpoint, err := mockCreateEndpoint(store)
	require.NoError(t, err)

	tempDir := t.TempDir()
	fileService, err := filesystem.NewService(tempDir, "")
	require.NoError(t, err)

	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.FileService = fileService
	handler.DataStore = store

	const repoURL = "https://github.com/portainer/portainer.git"
	const configPath = "docker-compose.yml"
	const fileContent = "version: '3'\nservices:\n  web:\n    image: nginx\n"

	require.NoError(t, os.WriteFile(filesystem.JoinPaths(tempDir, configPath), []byte(fileContent), 0o644))

	stack := &portainer.Stack{
		ID:          2,
		EndpointID:  endpoint.ID,
		Type:        portainer.DockerComposeStack,
		ProjectPath: tempDir,
		EntryPoint:  configPath,
		CurrentDeploymentInfo: &portainer.StackDeploymentInfo{
			RepositoryURL:  repoURL,
			ConfigFilePath: configPath,
		},
		GitConfig: &gittypes.RepoConfig{
			URL:            repoURL,
			ConfigFilePath: configPath,
		},
	}
	require.NoError(t, store.Stack().Create(stack))

	req := mockCreateStackRequestWithSecurityContext(
		http.MethodGet,
		"/stacks/"+strconv.Itoa(int(stack.ID))+"/file",
		nil,
	)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp stackFileResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, fileContent, resp.StackFileContent)
}
