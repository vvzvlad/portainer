package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	portainer "github.com/portainer/portainer/api"
	dockerclient "github.com/portainer/portainer/api/docker/client"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/require"
)

func TestApplyVersionConstraint(t *testing.T) {
	t.Parallel()
	initialNet := network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			"key1": {
				MacAddress: "mac1",
				EndpointID: "endpointID1",
			},
			"key2": {
				MacAddress: "mac2",
				EndpointID: "endpointID2",
			},
		},
	}

	f := func(currentVer string, constraint string, success, emptyMac bool) {
		t.Helper()

		transformedNet, err := applyVersionConstraint(currentVer, constraint, initialNet, clearMacAddrs)
		if success {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}

		require.Len(t, transformedNet.EndpointsConfig, len(initialNet.EndpointsConfig))

		for k := range initialNet.EndpointsConfig {
			if emptyMac {
				require.NotEqual(t, initialNet.EndpointsConfig[k], transformedNet.EndpointsConfig[k])
				require.Empty(t, transformedNet.EndpointsConfig[k].MacAddress)

				continue
			}

			require.Equal(t, initialNet.EndpointsConfig[k], transformedNet.EndpointsConfig[k])
		}
	}

	f("1.45", "< 1.44", true, false)  // No transformation
	f("1.43", "< 1.44", true, true)   // Transformation
	f("a.b.", "< 1.44", true, false)  // Invalid current version
	f("1.45", "z 1.44", false, false) // Invalid version constraint
}

const (
	// standAPIVersion is what the stand advertises on /_ping. It is deliberately
	// below the SDK's own maximum so version negotiation settles on it, and at or
	// above 1.44 so Recreate keeps the per-endpoint MAC addresses.
	standAPIVersion = "1.47"

	standOldID      = "old-id"
	standNewID      = "new-id"
	standName       = "/web"
	standNetworkID  = "net-bridge"
	standNetwork    = "bridge"
	standImage      = "nginx:1.21"
	standStartError = "new container refused to start"
)

// standOp is a decoded Docker Engine API call the stand understands. Splitting
// the decoding from the response keeps the routing in one place, so a test can
// script a failure or a hook against the same identity the call log reports.
type standOp struct {
	verb      string // inspect, stop, start, rename, remove, create, connect, disconnect
	id        string // container id, or network id for connect/disconnect
	container string // container id, for connect/disconnect
	name      string // rename/create target name
	force     bool   // removal force flag
}

// key is the coarse identity a test scripts a failure or a hook against.
func (o standOp) key() string {
	switch {
	case o.container != "":
		return o.verb + ":" + o.id + ":" + o.container
	case o.id == "":
		return o.verb
	default:
		return o.verb + ":" + o.id
	}
}

// detail is what lands on the call log: the key plus whatever a test needs to
// tell two calls of the same verb apart (rename target, forced removal).
func (o standOp) detail() string {
	switch {
	case o.verb == "rename":
		return o.key() + "->" + o.name
	case o.verb == "create":
		return o.key() + ":" + o.name
	case o.verb == "remove" && o.force:
		return o.key() + ":force"
	default:
		return o.key()
	}
}

// dockerStand is a scriptable stand-in for the Docker Engine API, serving just
// the calls Recreate makes. It is driven through the production ClientFactory
// and the real Docker SDK client (version negotiation included), so the test
// exercises the actual request/response wiring rather than a hand-rolled seam.
type dockerStand struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    []string
	failures map[string]string // call key -> daemon error message
	hooks    map[string]func() // call key -> side effect run before answering
	running  map[string]bool   // container id -> state reported by inspect
	inert    map[string]bool   // ids whose start succeeds but leaves them stopped
}

// newRecreateStand wires a ContainerService to a fresh stand. The data store is
// nil on purpose: it is only reached by the image puller, and every test here
// recreates without a forced pull.
func newRecreateStand(t *testing.T) (*ContainerService, *dockerStand, *portainer.Endpoint) {
	t.Helper()

	stand := &dockerStand{
		failures: map[string]string{},
		hooks:    map[string]func(){},
		running:  map[string]bool{standOldID: true},
		inert:    map[string]bool{},
	}

	stand.srv = httptest.NewServer(stand)
	t.Cleanup(stand.srv.Close)

	endpoint := &portainer.Endpoint{
		ID:   1,
		Name: "stand",
		Type: portainer.DockerEnvironment,
		URL:  "tcp://" + stand.srv.Listener.Addr().String(),
	}

	return NewContainerService(dockerclient.NewClientFactory(nil, nil), nil), stand, endpoint
}

// failCall makes every call matching key fail with a daemon-style error.
func (s *dockerStand) failCall(key, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures[key] = message
}

// hookCall runs fn just before the stand answers a call matching key.
func (s *dockerStand) hookCall(key string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.hooks[key] = fn
}

// startsInert makes a container's start succeed while the container stays
// stopped, i.e. it exits immediately after being started.
func (s *dockerStand) startsInert(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inert[id] = true
}

// recorded returns the calls the stand answered, in order.
func (s *dockerStand) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.calls...)
}

func (s *dockerStand) isRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.running[id]
}

func (s *dockerStand) inspectResponse(id string) container.InspectResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:         id,
			Name:       standName,
			Image:      "sha256:" + strings.Repeat("a", 64),
			State:      &container.State{Running: s.running[id]},
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: standImage},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				standNetwork: {NetworkID: standNetworkID},
			},
		},
	}
}

// stripAPIVersion drops the "/v1.xx" prefix the SDK puts on versioned paths. The
// ping is unversioned and passes through untouched.
func stripAPIVersion(path string) string {
	if !strings.HasPrefix(path, "/v") {
		return path
	}

	if idx := strings.Index(path[1:], "/"); idx >= 0 {
		return path[idx+1:]
	}

	return path
}

func decodeStandOp(r *http.Request, path string) (standOp, bool) {
	switch {
	case r.Method == http.MethodPost && path == "/containers/create":
		return standOp{verb: "create", name: r.URL.Query().Get("name")}, true

	case strings.HasPrefix(path, "/containers/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/containers/"), "/")

		switch {
		case r.Method == http.MethodDelete && action == "":
			return standOp{verb: "remove", id: id, force: r.URL.Query().Get("force") == "1"}, true
		case action == "json":
			return standOp{verb: "inspect", id: id}, true
		case action == "stop", action == "start":
			return standOp{verb: action, id: id}, true
		case action == "rename":
			return standOp{verb: "rename", id: id, name: r.URL.Query().Get("name")}, true
		}

	case strings.HasPrefix(path, "/networks/"):
		netID, action, _ := strings.Cut(strings.TrimPrefix(path, "/networks/"), "/")
		if action != "connect" && action != "disconnect" {
			break
		}

		var body struct{ Container string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return standOp{}, false
		}

		return standOp{verb: action, id: netID, container: body.Container}, true
	}

	return standOp{}, false
}

func (s *dockerStand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := stripAPIVersion(r.URL.Path)

	if path == "/_ping" {
		w.Header().Set("Api-Version", standAPIVersion)
		w.Header().Set("Ostype", "linux")
		w.WriteHeader(http.StatusOK)

		return
	}

	op, ok := decodeStandOp(r, path)
	if !ok {
		// Not an assertion failure: this runs in the server goroutine, so it is
		// reported to the client, which surfaces it as a recreate error.
		http.Error(w, `{"message":"stand: unexpected `+r.Method+" "+path+`"}`, http.StatusNotFound)

		return
	}

	s.mu.Lock()
	s.calls = append(s.calls, op.detail())
	message, failed := s.failures[op.key()]
	hook := s.hooks[op.key()]
	s.mu.Unlock()

	if hook != nil {
		hook()
	}

	if failed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":` + strconv.Quote(message) + `}`))

		return
	}

	switch op.verb {
	case "inspect":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.inspectResponse(op.id))

	case "create":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: standNewID})

	case "start":
		s.mu.Lock()
		s.running[op.id] = !s.inert[op.id]
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case "stop":
		s.mu.Lock()
		s.running[op.id] = false
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// callIndex returns the position of a recorded call, failing the test when it is
// absent so an ordering assertion can never pass vacuously.
func callIndex(t *testing.T, calls []string, call string) int {
	t.Helper()

	for i, c := range calls {
		if c == call {
			return i
		}
	}

	require.FailNowf(t, "missing call", "%q was never issued, recorded calls: %v", call, calls)

	return -1
}

func countCalls(calls []string, call string) int {
	n := 0

	for _, c := range calls {
		if c == call {
			n++
		}
	}

	return n
}

// TestRecreateReplacesTheOriginalContainer pins the happy-path sequence: the
// original is stopped, renamed aside and disconnected, the new container takes
// its name and starts, and only then is the original removed. Nothing is
// restored, and the caller gets the new container.
func TestRecreateReplacesTheOriginalContainer(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)

	newContainer, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.NoError(t, err)
	require.NotNil(t, newContainer)
	require.Equal(t, standNewID, newContainer.ID)

	require.Equal(t, []string{
		"inspect:" + standOldID,
		"stop:" + standOldID,
		"rename:" + standOldID + "->" + standName + "-old",
		"disconnect:" + standNetworkID + ":" + standOldID,
		"create:" + standName,
		"start:" + standNewID,
		"remove:" + standOldID,
		"inspect:" + standNewID,
	}, stand.recorded())
}

// TestRecreateFailedStartRestoresTheOriginal covers the ordinary rollback: the
// new container will not start, the original is put back in service, and the
// caller gets the ORIGINAL failure. A *RestoreError here would wrongly tell the
// caller the workload is down.
func TestRecreateFailedStartRestoresTheOriginal(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)

	newContainer, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.Nil(t, newContainer)
	require.ErrorContains(t, err, standStartError)

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the restore succeeded, so the caller must see the plain recreate failure")

	calls := stand.recorded()
	removed := callIndex(t, calls, "remove:"+standNewID+":force")
	renamed := callIndex(t, calls, "rename:"+standOldID+"->"+standName)
	callIndex(t, calls, "connect:"+standNetworkID+":"+standOldID)
	callIndex(t, calls, "start:"+standOldID)

	// The new container holds the original name, so it has to be gone (forcibly,
	// it may still be running) before the original can be renamed back.
	require.Less(t, removed, renamed, "the new container must be removed before the original is renamed back")
	require.True(t, stand.isRunning(standOldID), "the original container is running again")
}

// TestRecreateReportsRestoreErrorWhenTheOriginalCannotBeStarted is the core of
// the fix: when the restore ITSELF fails the workload is left down, and that
// must reach the caller as a *RestoreError instead of being logged away. The
// failure that triggered the restore stays reachable through Unwrap, and Errs
// says what could not be put back.
func TestRecreateReportsRestoreErrorWhenTheOriginalCannotBeStarted(t *testing.T) {
	t.Parallel()

	const restoreStartError = "original container refused to start"

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	stand.failCall("start:"+standOldID, restoreStartError)

	newContainer, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.Nil(t, newContainer)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.Equal(t, standOldID, restoreErr.ContainerID)
	require.Equal(t, standName, restoreErr.Name)

	require.ErrorIs(t, err, restoreErr.Cause, "the recreate failure stays reachable through Unwrap")
	require.ErrorContains(t, restoreErr.Cause, "start container error")
	require.ErrorContains(t, restoreErr.Cause, standStartError)

	require.Len(t, restoreErr.Errs, 1, "only the restore start failed")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)
	require.ErrorContains(t, err, "left down", "the message must say the workload is down, not merely that a recreate failed")

	require.False(t, stand.isRunning(standOldID))
}

// TestRecreateReportsRestoreErrorWhenTheOriginalDoesNotStayUp guards the check
// that a successful start call is not proof of service: the original is started
// but exits immediately, which the post-start inspect must catch and report as a
// *RestoreError like any other failed restore.
func TestRecreateReportsRestoreErrorWhenTheOriginalDoesNotStayUp(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	stand.startsInert(standOldID)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.Len(t, restoreErr.Errs, 1)
	require.ErrorContains(t, restoreErr.Errs[0], "not running")

	calls := stand.recorded()
	callIndex(t, calls, "start:"+standOldID)
	require.Equal(t, 2, countCalls(calls, "inspect:"+standOldID),
		"the restore must verify the original is actually running, on top of the initial inspect")
}

// TestRecreateRestoresAfterTheCallerContextIsCancelled is the second half of the
// fix: the restore runs on a context detached from the caller's. Auto-update
// bounds a recreate with its own timeout, and the most common recreate failure
// is that timeout firing — reusing the dead caller context would make every
// restore call fail instantly and leave the original renamed, disconnected and
// stopped. Here the caller's context is cancelled exactly when the new container
// is started, and every restore step must still reach the daemon.
func TestRecreateRestoresAfterTheCallerContextIsCancelled(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The stand kills the caller's context while answering the start, and fails
	// the call as well so the recreate fails deterministically either way.
	stand.hookCall("start:"+standNewID, cancel)
	stand.failCall("start:"+standNewID, standStartError)

	_, err := svc.Recreate(ctx, endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled, "the caller context must be dead by the time the defers run")

	calls := stand.recorded()
	callIndex(t, calls, "remove:"+standNewID+":force")
	callIndex(t, calls, "rename:"+standOldID+"->"+standName)
	callIndex(t, calls, "connect:"+standNetworkID+":"+standOldID)
	callIndex(t, calls, "start:"+standOldID)

	require.True(t, stand.isRunning(standOldID), "the original container is running again")

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the restore itself succeeded despite the dead caller context")
}
