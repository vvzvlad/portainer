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

// standFailure is a scripted daemon error: the message plus the HTTP status it
// is served with. The status is part of the script because the Docker SDK turns
// it into a typed error, and Recreate treats a 404 (the object is already gone)
// differently from a 500.
type standFailure struct {
	message string
	status  int
}

// dockerStand is a scriptable stand-in for the Docker Engine API, serving just
// the calls Recreate makes. It is driven through the production ClientFactory
// and the real Docker SDK client (version negotiation included), so the test
// exercises the actual request/response wiring rather than a hand-rolled seam.
type dockerStand struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    []string
	failures map[string]standFailure // call key or detail -> scripted daemon error
	hooks    map[string]func()       // call key or detail -> side effect run before answering
	running  map[string]bool         // container id -> state reported by inspect
	inert    map[string]bool         // ids whose start succeeds but leaves them stopped
	networks map[string]string       // network name -> id, as reported by inspect
}

// newRecreateStand wires a ContainerService to a fresh stand. The data store is
// nil on purpose: it is only reached by the image puller, and every test here
// recreates without a forced pull.
func newRecreateStand(t *testing.T) (*ContainerService, *dockerStand, *portainer.Endpoint) {
	t.Helper()

	stand := &dockerStand{
		failures: map[string]standFailure{},
		hooks:    map[string]func(){},
		running:  map[string]bool{standOldID: true},
		inert:    map[string]bool{},
		networks: map[string]string{standNetwork: standNetworkID},
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

// failCall makes every call matching key fail with a daemon-style server error.
// The key is matched against both the coarse call key and the finer detail, so a
// test can fail every rename of a container or just the rename to one name.
func (s *dockerStand) failCall(key, message string) {
	s.failCallStatus(key, message, http.StatusInternalServerError)
}

// failCallStatus is failCall with a chosen HTTP status, for the cases where the
// status is what is under test (a 404 meaning "already gone").
func (s *dockerStand) failCallStatus(key, message string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures[key] = standFailure{message: message, status: status}
}

// hookCall runs fn just before the stand answers a call matching key.
func (s *dockerStand) hookCall(key string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.hooks[key] = fn
}

// withNetworks replaces the networks the original container reports. It must be
// called before the recreate under test, while nothing is being served.
func (s *dockerStand) withNetworks(networks map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.networks = networks
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

	networks := make(map[string]*network.EndpointSettings, len(s.networks))
	for name, id := range s.networks {
		networks[name] = &network.EndpointSettings{NetworkID: id}
	}

	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:         id,
			Name:       standName,
			Image:      "sha256:" + strings.Repeat("a", 64),
			State:      &container.State{Running: s.running[id]},
			HostConfig: &container.HostConfig{},
		},
		Config:          &container.Config{Image: standImage},
		NetworkSettings: &container.NetworkSettings{Networks: networks},
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
	// The detail is looked up first so a test can single out one call of a verb
	// (the rename BACK, say) without also scripting its sibling.
	failure, failed := s.failures[op.detail()]
	if !failed {
		failure, failed = s.failures[op.key()]
	}
	hook, hooked := s.hooks[op.detail()]
	if !hooked {
		hook = s.hooks[op.key()]
	}
	s.mu.Unlock()

	if hook != nil {
		hook()
	}

	if failed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(failure.status)
		_, _ = w.Write([]byte(`{"message":` + strconv.Quote(failure.message) + `}`))

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

// requireNoCall fails the test when a call was issued at all. It is the mirror of
// callIndex, for the steps the restore must NOT take.
func requireNoCall(t *testing.T, calls []string, call string) {
	t.Helper()

	require.NotContains(t, calls, call, "%q must not have been issued, recorded calls: %v", call, calls)
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

// TestRecreateRestoresAfterAFailedRename covers a failure in the teardown itself
// rather than in the new container: the rename aside is refused, which leaves the
// original stopped. That is just as much an outage as a failed start, so the
// restore has to run here too — and it must NOT try to rename a container that
// was never renamed, since Docker refuses a rename to the name already held.
func TestRecreateRestoresAfterAFailedRename(t *testing.T) {
	t.Parallel()

	const renameError = "rename refused by the daemon"

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("rename:"+standOldID+"->"+standName+"-old", renameError)

	newContainer, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.Nil(t, newContainer)
	require.ErrorContains(t, err, "rename container error")
	require.ErrorContains(t, err, renameError)

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the original is running again, so this is a plain recreate failure")

	calls := stand.recorded()
	callIndex(t, calls, "start:"+standOldID)
	requireNoCall(t, calls, "rename:"+standOldID+"->"+standName)
	requireNoCall(t, calls, "connect:"+standNetworkID+":"+standOldID)
	requireNoCall(t, calls, "create:"+standName)

	require.True(t, stand.isRunning(standOldID), "the original container is running again")
}

// TestRecreateRestoresAfterAFailedNetworkDisconnect is the same for the network
// teardown, with the extra wrinkle that it fails halfway: the original is left
// stopped, renamed aside, and detached from SOME of its networks. The restore
// must reconnect exactly the networks it disconnected — reconnecting one that is
// still attached fails with "already exists" and would be misread as a broken
// restore.
func TestRecreateRestoresAfterAFailedNetworkDisconnect(t *testing.T) {
	t.Parallel()

	const (
		disconnectError = "disconnect refused by the daemon"
		netAID          = "net-a"
		netBID          = "net-b"
	)

	svc, stand, endpoint := newRecreateStand(t)
	stand.withNetworks(map[string]string{"a": netAID, "b": netBID})
	stand.failCall("disconnect:"+netBID+":"+standOldID, disconnectError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "disconnect network from old container error")
	require.ErrorContains(t, err, disconnectError)

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the original is running again, so this is a plain recreate failure")

	calls := stand.recorded()
	callIndex(t, calls, "rename:"+standOldID+"->"+standName)
	callIndex(t, calls, "start:"+standOldID)
	require.True(t, stand.isRunning(standOldID), "the original container is running again")

	// Map iteration decides whether net-a was reached before net-b failed, so the
	// assertion is on the invariant rather than on a fixed sequence: every network
	// is reconnected exactly as often as it was successfully disconnected.
	requireNoCall(t, calls, "connect:"+netBID+":"+standOldID)
	require.Equal(t,
		countCalls(calls, "disconnect:"+netAID+":"+standOldID),
		countCalls(calls, "connect:"+netAID+":"+standOldID),
		"only the networks that were actually disconnected may be reconnected")
}

// TestRecreateReportsRestoreErrorWhenATeardownFailureLeavesTheOriginalDown is the
// other half of the two tests above: covering the teardown with a restore is only
// worth anything if a restore that does not work is still reported. The
// disconnect fails, and so does the start of the original — nothing is running,
// which the caller has to hear as a *RestoreError.
func TestRecreateReportsRestoreErrorWhenATeardownFailureLeavesTheOriginalDown(t *testing.T) {
	t.Parallel()

	const (
		disconnectError   = "disconnect refused by the daemon"
		restoreStartError = "original container refused to start"
	)

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("disconnect:"+standNetworkID+":"+standOldID, disconnectError)
	stand.failCall("start:"+standOldID, restoreStartError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.ErrorContains(t, restoreErr.Cause, disconnectError, "the teardown failure stays the cause")
	require.Len(t, restoreErr.Errs, 1, "only the start of the original failed")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)

	require.False(t, stand.isRunning(standOldID))
}

// TestRecreateKeepsTheVerdictWhenTheNewContainerCannotBeRemoved pins what the
// verdict is built from: the state of the ORIGINAL, not the tidiness of the
// rollback. The new container cannot be removed (an engine hiccup, a client
// timeout on a removal that did land), but the original is back and running, so
// the caller must get the plain recreate failure. Reporting a *RestoreError here
// would tell an operator the workload is down while it is serving.
func TestRecreateKeepsTheVerdictWhenTheNewContainerCannotBeRemoved(t *testing.T) {
	t.Parallel()

	const removeError = "removal refused by the daemon"

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	stand.failCall("remove:"+standNewID, removeError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, standStartError)

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the original is running, so the workload is not down")

	calls := stand.recorded()
	callIndex(t, calls, "remove:"+standNewID+":force")
	require.True(t, stand.isRunning(standOldID), "the original container is running again")
}

// TestRecreateIgnoresAnAlreadyRemovedNewContainer covers the removal answering
// "no such container": the original may have run with --rm, in which case the
// engine reaps the new container on its own. That is the outcome the removal was
// after, so it must not show up among the restore failures. The original is kept
// down here only to make the reported failures observable.
func TestRecreateIgnoresAnAlreadyRemovedNewContainer(t *testing.T) {
	t.Parallel()

	const restoreStartError = "original container refused to start"

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	stand.failCallStatus("remove:"+standNewID, "No such container: "+standNewID, http.StatusNotFound)
	stand.failCall("start:"+standOldID, restoreStartError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.Len(t, restoreErr.Errs, 1, "a container that is already gone is not a restore failure")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)
	require.NotContains(t, err.Error(), "remove new container error")
}

// TestRecreateReportsRestoreErrorWhenTheLeftoverNewContainerBlocksTheOriginal is
// the regression guard on the other side of the verdict: dropping the removal
// failure out of the verdict must not drop it out of the REPORT. Here the new
// container survives, keeps the original name so the rename back is refused, and
// the original does not come up — the workload is down, and the operator needs to
// read why in one place.
func TestRecreateReportsRestoreErrorWhenTheLeftoverNewContainerBlocksTheOriginal(t *testing.T) {
	t.Parallel()

	const (
		removeError       = "removal refused by the daemon"
		renameBackError   = "container name is already in use"
		restoreStartError = "original container refused to start"
	)

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	stand.failCall("remove:"+standNewID, removeError)
	stand.failCall("rename:"+standOldID+"->"+standName, renameBackError)
	stand.failCall("start:"+standOldID, restoreStartError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.ErrorContains(t, err, "left down")

	require.Len(t, restoreErr.Errs, 3)
	require.ErrorContains(t, err, removeError, "the leftover new container explains the refused rename")
	require.ErrorContains(t, err, renameBackError)
	require.ErrorContains(t, err, restoreStartError)

	require.False(t, stand.isRunning(standOldID))
}
