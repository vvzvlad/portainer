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
	"time"

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
	timeout   string // stop grace period, as sent on the wire ("" when unset)
}

// containerID is the container a call targets, empty for one that targets none
// (a create, which has no container yet).
func (o standOp) containerID() string {
	switch {
	case o.container != "":
		return o.container
	case o.verb == "create":
		return ""
	default:
		return o.id
	}
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
	case o.verb == "stop" && o.timeout != "":
		return o.key() + ":t=" + o.timeout
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
//
// It keeps the state Recreate reasons about — which containers exist, the name
// each one carries and the networks it is attached to — so a restore that is
// planned from what the engine REPORTS is tested against a moving state rather
// than a fixed inspect. A name is a resource here as it is on a real engine:
// exactly one container can hold it, and taking one that is in use is refused.
type dockerStand struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      []string
	failures   map[string]standFailure         // call key or detail -> scripted daemon error
	nth        map[string]map[int]standFailure // verb -> 1-based call number -> scripted error
	seen       map[string]int                  // verb -> calls of it answered so far
	hooks      map[string]func()               // call key or detail -> side effect run before answering
	blocked    map[string]bool                 // call key or detail -> hang until the caller gives up
	blockedNth map[string]map[int]bool         // verb -> 1-based call number -> hang
	running    map[string]bool                 // container id -> state reported by inspect
	inert      map[string]bool                 // ids whose start succeeds but leaves them stopped
	names      map[string]string               // container id -> name, and the set of live containers
	networks   map[string]string               // network name -> id
	attached   map[string]map[string]bool      // container id -> network ids it is attached to
	autoRemove bool                            // the original runs with --rm
}

// newRecreateStand wires a ContainerService to a fresh stand. The data store is
// nil on purpose: it is only reached by the image puller, and every test here
// recreates without a forced pull.
func newRecreateStand(t *testing.T) (*ContainerService, *dockerStand, *portainer.Endpoint) {
	t.Helper()

	stand := &dockerStand{
		failures:   map[string]standFailure{},
		nth:        map[string]map[int]standFailure{},
		seen:       map[string]int{},
		hooks:      map[string]func(){},
		blocked:    map[string]bool{},
		blockedNth: map[string]map[int]bool{},
		running:    map[string]bool{standOldID: true},
		inert:      map[string]bool{},
		names:      map[string]string{standOldID: standName},
		networks:   map[string]string{standNetwork: standNetworkID},
		attached:   map[string]map[string]bool{standOldID: {standNetworkID: true}},
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

// failNthCallStatus makes the n-th call of a verb (1-based, counted across every
// object) fail. It is how a test scripts "the second disconnect, whichever
// network that turns out to be" or "the restore's own inspect but not the one
// that opened the recreate", neither of which can be keyed by id.
func (s *dockerStand) failNthCallStatus(verb string, n int, message string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nth[verb] == nil {
		s.nth[verb] = map[int]standFailure{}
	}

	s.nth[verb][n] = standFailure{message: message, status: status}
}

// failNthCall is failNthCallStatus with the daemon-style server error.
func (s *dockerStand) failNthCall(verb string, n int, message string) {
	s.failNthCallStatus(verb, n, message, http.StatusInternalServerError)
}

// hookCall runs fn just before the stand answers a call matching key. It runs
// before a scripted failure is served too, which is how a test reproduces the
// most awkward engine outcome: the call was carried out and the answer was lost.
func (s *dockerStand) hookCall(key string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.hooks[key] = fn
}

// blockCall makes every call matching key hang until the caller gives up on it,
// i.e. until the context bounding that call expires. It is how a test pins the
// restore's time budget against a wedged engine without needing a real one.
func (s *dockerStand) blockCall(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.blocked[key] = true
}

// blockNthCall is blockCall for the n-th call of a verb, for the calls a test
// cannot name — the restore's own inspect looks exactly like the one that opened
// the recreate.
func (s *dockerStand) blockNthCall(verb string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.blockedNth[verb] == nil {
		s.blockedNth[verb] = map[int]bool{}
	}

	s.blockedNth[verb][n] = true
}

// withAutoRemove makes the original container report HostConfig.AutoRemove, i.e.
// it runs with --rm and the engine reaps it the moment it stops.
func (s *dockerStand) withAutoRemove() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.autoRemove = true
}

// withNetworks replaces the networks the original container is attached to. It
// must be called before the recreate under test, while nothing is being served.
func (s *dockerStand) withNetworks(networks map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.networks = networks
	s.attached[standOldID] = map[string]bool{}
	for _, id := range networks {
		s.attached[standOldID][id] = true
	}
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

// setRunning is how a hook reproduces an engine that carried a stop out and then
// failed to say so.
func (s *dockerStand) setRunning(id string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.running[id] = running
}

// nameOf is the name the stand currently reports for a container, i.e. what an
// operator would see in `docker ps` once the recreate is over.
func (s *dockerStand) nameOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.names[id]
}

// setName is how a hook reproduces an engine that carried a rename out and then
// failed to say so.
func (s *dockerStand) setName(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.names[id] = name
}

// forget drops a container the way a removal does: everything it held goes,
// its name first of all, since the original cannot be renamed back while another
// container still carries that name. It is also how a hook reproduces a container
// the engine removed on its own (a --rm container it reaped, a removal whose
// answer was lost).
func (s *dockerStand) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.names, id)
	delete(s.running, id)
	delete(s.attached, id)
}

// exists reports whether the stand still knows the container, i.e. whether it
// holds a name. Everything else about it is gone once it is removed.
func (s *dockerStand) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, live := s.names[id]

	return live
}

// nameHolder returns the container currently carrying name, if any. Called with
// the lock held.
func (s *dockerStand) nameHolder(name string) (string, bool) {
	for id, held := range s.names {
		if held == name {
			return id, true
		}
	}

	return "", false
}

func (s *dockerStand) inspectResponse(id string) container.InspectResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	networks := make(map[string]*network.EndpointSettings, len(s.networks))
	for name, netID := range s.networks {
		if s.attached[id][netID] {
			networks[name] = &network.EndpointSettings{NetworkID: netID}
		}
	}

	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:         id,
			Name:       s.names[id],
			Image:      "sha256:" + strings.Repeat("a", 64),
			State:      &container.State{Running: s.running[id]},
			HostConfig: &container.HostConfig{AutoRemove: id == standOldID && s.autoRemove},
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
		case action == "stop":
			return standOp{verb: action, id: id, timeout: r.URL.Query().Get("t")}, true
		case action == "start":
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
	s.seen[op.verb]++
	// The detail is looked up first so a test can single out one call of a verb
	// (the rename BACK, say) without also scripting its sibling; the count-based
	// script is last, for the calls a test cannot name.
	failure, failed := s.failures[op.detail()]
	if !failed {
		failure, failed = s.failures[op.key()]
	}
	if !failed {
		failure, failed = s.nth[op.verb][s.seen[op.verb]]
	}
	hook, hooked := s.hooks[op.detail()]
	if !hooked {
		hook = s.hooks[op.key()]
	}
	blocked := s.blocked[op.detail()] || s.blocked[op.key()] || s.blockedNth[op.verb][s.seen[op.verb]]
	s.mu.Unlock()

	if hook != nil {
		hook()
	}

	if blocked {
		<-r.Context().Done()

		return
	}

	if failed {
		writeStandError(w, failure.message, failure.status)

		return
	}

	// A container the stand no longer knows is answered like a container the engine
	// no longer knows: gone for good, and no call renames or starts it back into
	// existence.
	if id := op.containerID(); id != "" && !s.exists(id) {
		writeStandError(w, "No such container: "+id, http.StatusNotFound)

		return
	}

	switch op.verb {
	case "inspect":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.inspectResponse(op.id))

	case "create":
		s.mu.Lock()
		holder, taken := s.nameHolder(op.name)
		if !taken {
			s.names[standNewID] = op.name
		}
		s.mu.Unlock()

		if taken {
			writeStandError(w, nameInUse(op.name, holder), http.StatusConflict)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: standNewID})

	case "remove":
		s.forget(op.id)
		w.WriteHeader(http.StatusNoContent)

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

	case "rename":
		// A name is a resource: the engine refuses a rename to the name the container
		// already carries, and refuses one to a name ANOTHER container holds — which
		// is what a new container that could not be removed does to the rename back.
		s.mu.Lock()
		unchanged := s.names[op.id] == op.name
		holder, taken := s.nameHolder(op.name)
		if !unchanged && !taken {
			s.names[op.id] = op.name
		}
		s.mu.Unlock()

		switch {
		case unchanged:
			writeStandError(w, "Renaming a container with the same name as its current name", http.StatusBadRequest)
		case taken:
			writeStandError(w, nameInUse(op.name, holder), http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNoContent)
		}

	case "connect":
		// Same for a network the container is already attached to.
		s.mu.Lock()
		alreadyAttached := s.attached[op.container][op.id]
		if !alreadyAttached {
			if s.attached[op.container] == nil {
				s.attached[op.container] = map[string]bool{}
			}
			s.attached[op.container][op.id] = true
		}
		s.mu.Unlock()

		if alreadyAttached {
			writeStandError(w, "endpoint with name web already exists in network "+op.id, http.StatusForbidden)

			return
		}

		w.WriteHeader(http.StatusNoContent)

	case "disconnect":
		s.mu.Lock()
		delete(s.attached[op.container], op.id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// nameInUse is the engine's wording for a name that another container holds.
func nameInUse(name, holder string) string {
	return `Conflict. The container name "` + name + `" is already in use by container "` + holder +
		`". You have to remove (or rename) that container to be able to reuse that name.`
}

// writeStandError serves a daemon-style error body, the shape the Docker SDK
// turns into a typed error.
func writeStandError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":` + strconv.Quote(message) + `}`))
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

// callsWithPrefix returns the recorded calls that start with prefix, in order.
// It is how a test asserts on a set of calls whose exact identity is decided by
// map iteration (which network was reached first).
func callsWithPrefix(calls []string, prefix string) []string {
	var found []string

	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			found = append(found, c)
		}
	}

	return found
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

	require.Len(t, restoreErr.Errs, 2, "the refused start, and the state it left the container in")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)
	require.ErrorContains(t, restoreErr.Errs[1], "not running")
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
	require.Equal(t, 3, countCalls(calls, "inspect:"+standOldID),
		"the original is inspected to open the recreate, to plan the restore, and to verify it is actually running afterwards")
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
	// Map iteration order decides WHICH network is reached first, so the failure is
	// scripted by call number instead of by network: the first disconnect always
	// lands, the second is always refused. Failing a named network would leave the
	// invariant below checked against an empty set in about half the runs.
	stand.failNthCall("disconnect", 2, disconnectError)

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

	// Exactly one network was detached, and exactly that one comes back:
	// reconnecting the network that is still attached is refused by the engine with
	// "already exists in network", which would be misread as a broken restore.
	disconnects := callsWithPrefix(calls, "disconnect:")
	require.Len(t, disconnects, 2, "both networks are attempted, the second one is refused")
	require.Equal(t,
		[]string{strings.Replace(disconnects[0], "disconnect:", "connect:", 1)},
		callsWithPrefix(calls, "connect:"),
		"only the network that was actually disconnected may be reconnected")
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
	require.Len(t, restoreErr.Errs, 2, "the refused start, and the state it left the container in")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)
	require.ErrorContains(t, restoreErr.Errs[1], "not running")

	require.False(t, stand.isRunning(standOldID))
}

// TestRecreateJudgesALeftoverNewContainerByTheOriginalsState pins what the
// verdict is built from: the state of the ORIGINAL, not the tidiness of the
// rollback. A removal that fails is not itself a verdict — it matters exactly as
// far as it keeps the original from coming back.
func TestRecreateJudgesALeftoverNewContainerByTheOriginalsState(t *testing.T) {
	t.Parallel()

	const removeError = "removal refused by the daemon"

	t.Run("a removal that did land under a lost answer is not a failed restore", func(t *testing.T) {
		t.Parallel()

		svc, stand, endpoint := newRecreateStand(t)
		stand.failCall("start:"+standNewID, standStartError)
		// The engine removed it, the client only ever saw the failure — so the name is
		// free and the original comes back whole.
		stand.hookCall("remove:"+standNewID, func() { stand.forget(standNewID) })
		stand.failCall("remove:"+standNewID, removeError)

		_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
		require.Error(t, err)
		require.ErrorContains(t, err, standStartError)

		var restoreErr *RestoreError
		require.NotErrorAs(t, err, &restoreErr, "the original is back exactly as it was, whatever the removal answered")

		calls := stand.recorded()
		callIndex(t, calls, "remove:"+standNewID+":force")
		require.True(t, stand.isRunning(standOldID), "the original container is running again")
		require.Equal(t, standName, stand.nameOf(standOldID), "under its own name")
	})

	t.Run("a leftover holding the original name leaves it degraded", func(t *testing.T) {
		t.Parallel()

		svc, stand, endpoint := newRecreateStand(t)
		stand.failCall("start:"+standNewID, standStartError)
		stand.failCall("remove:"+standNewID, removeError)

		_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
		require.Error(t, err)

		// The new container survives holding "/web", so the engine refuses the rename
		// back with a conflict and the original is left serving under "-old".
		var restoreErr *RestoreError
		require.ErrorAs(t, err, &restoreErr, "a container answering under the wrong name is not a plain recreate failure")
		require.True(t, restoreErr.OriginalRunning, "it is serving, so this is a degradation and not an outage")
		require.ErrorContains(t, err, removeError, "the leftover explains the refused rename")
		require.ErrorContains(t, err, "already in use")

		require.True(t, stand.isRunning(standOldID), "the original container is running again")
		require.Equal(t, standName+"-old", stand.nameOf(standOldID))
	})
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
	// Gone for real, so its name is free again — and answered with a 404 all the
	// same, which is exactly what the engine does for a container it reaped itself.
	stand.hookCall("remove:"+standNewID, func() { stand.forget(standNewID) })
	stand.failCallStatus("remove:"+standNewID, "No such container: "+standNewID, http.StatusNotFound)
	stand.failCall("start:"+standOldID, restoreStartError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.Len(t, restoreErr.Errs, 2, "a container that is already gone is not a restore failure")
	require.ErrorContains(t, restoreErr.Errs[0], restoreStartError)
	require.ErrorContains(t, restoreErr.Errs[1], "not running")
	require.NotContains(t, err.Error(), "remove new container error")
	require.Equal(t, standName, stand.nameOf(standOldID), "the freed name did go back to the original")
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

	require.Len(t, restoreErr.Errs, 5, "three calls that failed, and the two things the container ended up without")
	require.ErrorContains(t, err, removeError, "the leftover new container explains the refused rename")
	require.ErrorContains(t, err, renameBackError)
	require.ErrorContains(t, err, restoreStartError)
	require.ErrorContains(t, err, "the original container is not running")
	require.ErrorContains(t, err, "is named "+standName+"-old")

	require.False(t, stand.isRunning(standOldID))
}

// TestRecreateReportsAnIncompleteRestoreWhileTheOriginalRuns is the honesty of
// the verdict at its edge: the original is serving again, so nothing is "down",
// but it did not come back the way it was. A container still carrying "-old" is
// not reachable under its own name by a stack peer, and the next auto-update
// pass would find and recreate THAT container instead; a container that came
// back without a network is running blind. Both must reach the caller as a
// *RestoreError that says the workload is up, not as a plain failed recreate
// that says everything is fine.
func TestRecreateReportsAnIncompleteRestoreWhileTheOriginalRuns(t *testing.T) {
	t.Parallel()

	const (
		renameBackError = "rename back refused by the daemon"
		connectError    = "connect refused by the daemon"
	)

	tests := []struct {
		name       string
		script     func(*dockerStand)
		wantReason string
		wantName   string
	}{
		{
			name: "the original is left under the -old name",
			script: func(stand *dockerStand) {
				stand.failCall("rename:"+standOldID+"->"+standName, renameBackError)
			},
			wantReason: renameBackError,
			wantName:   standName + "-old",
		},
		{
			name: "the original is left off one of its networks",
			script: func(stand *dockerStand) {
				stand.failCall("connect:"+standNetworkID+":"+standOldID, connectError)
			},
			wantReason: connectError,
			wantName:   standName,
		},
		{
			// A conflict is also how the engine says the container is ALREADY attached,
			// so a verdict built from the answers to the restore calls would have to
			// guess which conflict this is — and reading it as "already attached" turns
			// a container left off its network into a clean rollback. The verdict is
			// read off the container instead, which cannot be talked into it.
			name: "the original is refused its network with a conflict",
			script: func(stand *dockerStand) {
				stand.failCallStatus("connect:"+standNetworkID+":"+standOldID,
					"container "+standOldID+" is marked for removal and cannot be connected to network "+standNetworkID,
					http.StatusConflict)
			},
			wantReason: "marked for removal",
			wantName:   standName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, stand, endpoint := newRecreateStand(t)
			stand.failCall("start:"+standNewID, standStartError)
			tt.script(stand)

			_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
			require.Error(t, err)

			var restoreErr *RestoreError
			require.ErrorAs(t, err, &restoreErr, "an incomplete restore is not a plain recreate failure")
			require.True(t, restoreErr.OriginalRunning, "the original is serving again")
			require.ErrorIs(t, err, restoreErr.Cause, "the recreate failure stays reachable through Unwrap")

			require.ErrorContains(t, err, "restore was incomplete")
			require.ErrorContains(t, err, tt.wantReason, "the message says what did not roll back")
			require.NotContains(t, err.Error(), "left down", "the workload is up, the message must not claim otherwise")

			require.True(t, stand.isRunning(standOldID), "the original container is running again")
			require.Equal(t, tt.wantName, stand.nameOf(standOldID))
		})
	}
}

// TestRecreateKeepsBudgetForTheOriginalWhenAnEarlyStepHangs guards the split of
// the restore budget. Everything in a restore shares one deadline, but the
// teardown of the new container and the inspect that plans the rollback both run
// BEFORE the calls that put the original back: either one wedged on a stuck runc
// or an unresponsive engine would otherwise spend the whole window and leave the
// rename back, the reconnects and the start — the calls that decide whether the
// workload is down — without a single attempt, turning a recoverable failure into
// an outage.
func TestRecreateKeepsBudgetForTheOriginalWhenAnEarlyStepHangs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script func(*dockerStand)
		// The wedged teardown never removes the new container, which goes on holding
		// the original name, so the original comes back up but under "-old": degraded,
		// and still not the outage it would be without a budget of its own.
		wantDegraded bool
	}{
		{
			name:         "the teardown of the new container hangs",
			script:       func(stand *dockerStand) { stand.blockCall("stop:" + standNewID) },
			wantDegraded: true,
		},
		{
			// The second inspect of the run is the restore's own: the recreate opens
			// with one, and the verdict takes another at the end. Losing it costs
			// nothing but precision — the plan falls back to every attempted step.
			name:   "the inspect that plans the rollback hangs",
			script: func(stand *dockerStand) { stand.blockNthCall("inspect", 2) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, stand, endpoint := newRecreateStand(t)
			// Same shares as in production, in milliseconds instead of seconds.
			svc.restoreTimeout = 900 * time.Millisecond

			stand.failCall("start:"+standNewID, standStartError)
			tt.script(stand)

			_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
			require.Error(t, err)
			require.ErrorContains(t, err, standStartError)

			// The invariant is that the original got its calls and came back up — not
			// that the whole rollback was tidy, which the wedged teardown decides.
			calls := stand.recorded()
			callIndex(t, calls, "rename:"+standOldID+"->"+standName)
			callIndex(t, calls, "connect:"+standNetworkID+":"+standOldID)
			callIndex(t, calls, "start:"+standOldID)

			require.True(t, stand.isRunning(standOldID), "the original container is running again")

			var restoreErr *RestoreError
			if !tt.wantDegraded {
				require.NotErrorAs(t, err, &restoreErr, "the original came back exactly as it was")
				require.Equal(t, standName, stand.nameOf(standOldID), "under its own name")

				return
			}

			require.ErrorAs(t, err, &restoreErr)
			require.True(t, restoreErr.OriginalRunning,
				"an early step spending its own share is a degradation at worst, never an outage")
		})
	}
}

// TestRecreateStopsTheNewContainerWithoutGrace pins the teardown's stop having no
// grace period. The engine's default is 10s, the same order as the whole teardown
// share, so a container that ignores SIGTERM — most of them — would spend the
// share waiting here and leave the forced removal to run on an expired context,
// keeping the original name and turning a clean rollback into a degraded one. The
// stop is best-effort anyway: the forced removal right after it kills whatever is
// left.
func TestRecreateStopsTheNewContainerWithoutGrace(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	callIndex(t, stand.recorded(), "stop:"+standNewID+":t=0")
}

// TestRecreateUndoesAStepWhoseAnswerWasLost covers the most common way a rollback
// goes wrong: the engine carried the call out and the answer never came back (a
// client deadline firing mid-flight). Bookkeeping that only counts confirmed
// steps would leave the original named "-old" for good, so the rollback is
// planned from what the engine REPORTS about the container instead.
func TestRecreateUndoesAStepWhoseAnswerWasLost(t *testing.T) {
	t.Parallel()

	const lostAnswer = "connection reset while renaming"

	svc, stand, endpoint := newRecreateStand(t)
	// The rename lands on the engine, the caller only ever sees the failure.
	stand.hookCall("rename:"+standOldID+"->"+standName+"-old", func() {
		stand.setName(standOldID, standName+"-old")
	})
	stand.failCall("rename:"+standOldID+"->"+standName+"-old", lostAnswer)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "rename container error")

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the rename was undone, so the original is back exactly as it was")

	calls := stand.recorded()
	callIndex(t, calls, "rename:"+standOldID+"->"+standName)
	require.Equal(t, standName, stand.nameOf(standOldID), "the original carries its own name again")
	require.True(t, stand.isRunning(standOldID), "the original container is running again")
}

// TestRecreateTreatsAnAlreadyRestoredStepAsRestored is the other side of
// planning the rollback from the observed state: when the state cannot be read
// the plan falls back to every step that was ATTEMPTED, which necessarily
// includes steps that never landed. Undoing those is refused by the engine
// ("already in that state"), and the restore must read that as done rather than
// as a failure — otherwise a perfectly restored container is reported as broken.
func TestRecreateTreatsAnAlreadyRestoredStepAsRestored(t *testing.T) {
	t.Parallel()

	const (
		inspectError    = "inspect refused by the daemon"
		renameError     = "rename refused by the daemon"
		disconnectError = "disconnect refused by the daemon"
	)

	tests := []struct {
		name   string
		script func(*dockerStand)
		undone string
	}{
		{
			name: "a rename that never landed is not renamed back twice",
			script: func(stand *dockerStand) {
				stand.failCall("rename:"+standOldID+"->"+standName+"-old", renameError)
			},
			undone: "rename:" + standOldID + "->" + standName,
		},
		{
			name: "a disconnect that never landed is not reconnected twice",
			script: func(stand *dockerStand) {
				stand.failCall("disconnect:"+standNetworkID+":"+standOldID, disconnectError)
			},
			undone: "connect:" + standNetworkID + ":" + standOldID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, stand, endpoint := newRecreateStand(t)
			// The second inspect of the run is the restore's own: the recreate opens
			// with one, and the verdict takes another after the start.
			stand.failNthCall("inspect", 2, inspectError)
			tt.script(stand)

			_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
			require.Error(t, err)

			var restoreErr *RestoreError
			require.NotErrorAs(t, err, &restoreErr,
				"the original is exactly as it was, an already-satisfied step is not a failed restore")

			calls := stand.recorded()
			callIndex(t, calls, tt.undone)
			require.True(t, stand.isRunning(standOldID), "the original container is running again")
			require.Equal(t, standName, stand.nameOf(standOldID))
		})
	}
}

// TestRecreateRestoresAfterAFailedStop is the #36 symptom itself: the stop comes
// back with an error, and the engine went on to carry it out anyway. Treating a
// failed stop as "nothing happened" leaves the container in Exited with nothing
// restoring it, which is precisely the reported outage.
func TestRecreateRestoresAfterAFailedStop(t *testing.T) {
	t.Parallel()

	const stopError = "connection reset while stopping"

	svc, stand, endpoint := newRecreateStand(t)
	// The engine stops the container, the caller only ever sees the failure.
	stand.hookCall("stop:"+standOldID, func() { stand.setRunning(standOldID, false) })
	stand.failCall("stop:"+standOldID, stopError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "stop container error")
	require.ErrorContains(t, err, stopError)

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr, "the original is running again, so this is a plain recreate failure")

	calls := stand.recorded()
	callIndex(t, calls, "start:"+standOldID)
	// Nothing beyond the stop was reached, so there is nothing else to put back.
	requireNoCall(t, calls, "rename:"+standOldID+"->"+standName)
	requireNoCall(t, calls, "create:"+standName)

	require.True(t, stand.isRunning(standOldID), "the original container is running again")
}

// TestRecreateDoesNotReportAVanishedOriginalAsAnOutage is the guard on the
// restore now covering the stop: a container that is already gone and does NOT
// run with --rm was not taken down by this recreate — nothing removes a container
// on a stop, so an operator or another tool removed it — and there is nothing to
// put back. Reporting it as a workload left down would page someone over a
// container nobody is missing. The --rm case is the opposite and is covered by
// TestRecreateReportsAnAutoRemovedOriginalAsAnOutage.
func TestRecreateDoesNotReportAVanishedOriginalAsAnOutage(t *testing.T) {
	t.Parallel()

	const gone = "No such container: " + standOldID

	tests := []struct {
		name      string
		script    func(*dockerStand)
		wantStart bool
	}{
		{
			name: "gone before the restore looks at it",
			script: func(stand *dockerStand) {
				stand.failNthCallStatus("inspect", 2, gone, http.StatusNotFound)
			},
		},
		{
			name: "gone between the look and the start",
			script: func(stand *dockerStand) {
				// Really gone: the start is refused because the container is not there
				// any more, and the inspect that takes the verdict will not find it either.
				stand.hookCall("start:"+standOldID, func() { stand.forget(standOldID) })
				stand.failCallStatus("start:"+standOldID, gone, http.StatusNotFound)
			},
			wantStart: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, stand, endpoint := newRecreateStand(t)
			stand.failCallStatus("stop:"+standOldID, gone, http.StatusNotFound)
			tt.script(stand)

			_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
			require.Error(t, err)
			require.ErrorContains(t, err, "stop container error")

			var restoreErr *RestoreError
			require.NotErrorAs(t, err, &restoreErr,
				"a container that no longer exists is not an outage this recreate caused")

			calls := stand.recorded()
			if tt.wantStart {
				callIndex(t, calls, "start:"+standOldID)

				return
			}

			requireNoCall(t, calls, "start:"+standOldID)
		})
	}
}

// TestRecreateReportsAnAutoRemovedOriginalAsAnOutage is the other side of the
// test above: a container running with --rm that is gone after OUR stop is gone
// BECAUSE of our stop. The engine reaps such a container the moment it stops, the
// restore is armed before the stop, and no new container has taken over — so
// nothing is running, nothing ever will be, and calling that a third party's
// doing would hand the operator an ordinary recreate failure over an outage.
func TestRecreateReportsAnAutoRemovedOriginalAsAnOutage(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)
	stand.withAutoRemove()
	// Stopping a --rm container is the engine's cue to remove it: the stop itself is
	// answered, and the container is gone by the time the next call reaches it.
	stand.hookCall("rename:"+standOldID+"->"+standName+"-old", func() { stand.forget(standOldID) })

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr, "our own stop taking the workload down is an outage this recreate caused")
	require.False(t, restoreErr.OriginalRunning)
	require.False(t, restoreErr.StateUnknown, "the state is known: the container is gone")
	require.ErrorContains(t, err, "left down")
	require.ErrorContains(t, err, "--rm", "the message has to say why there is nothing left to restore")

	requireNoCall(t, stand.recorded(), "start:"+standOldID)
}

// TestRecreateDoesNotBlameAStopThatFoundNothingToStop is the line between the two
// tests above, drawn where --rm makes it easy to get wrong: a --rm container that
// was ALREADY gone when the stop reached it exited on its own and was reaped for
// it — the ordinary way such a container dies — between the scan that picked it
// as a candidate and this recreate. The stop saying "no such container" is the
// proof, and without it the operator is paged for an outage this recreate had no
// part in.
func TestRecreateDoesNotBlameAStopThatFoundNothingToStop(t *testing.T) {
	t.Parallel()

	svc, stand, endpoint := newRecreateStand(t)
	stand.withAutoRemove()
	// Gone before the stop lands, so the stand answers the stop itself — and every
	// call the restore makes afterwards — with "no such container".
	stand.hookCall("stop:"+standOldID, func() { stand.forget(standOldID) })

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "stop container error")

	var restoreErr *RestoreError
	require.NotErrorAs(t, err, &restoreErr,
		"a --rm container that had already exited is not an outage this recreate caused")

	requireNoCall(t, stand.recorded(), "start:"+standOldID)
}

// TestRecreateDoesNotUndoARenameItNeverMade guards the restore's mandate: it puts
// back what THIS recreate took away, and nothing else. A recreate that failed at
// the stop never renamed anything, so a name that does not match by the time the
// restore looks was set by somebody else — and writing the remembered name over
// it would be this recreate renaming a container on its own account.
func TestRecreateDoesNotUndoARenameItNeverMade(t *testing.T) {
	t.Parallel()

	const (
		stopError = "connection reset while stopping"
		byHand    = "/web-by-hand"
	)

	svc, stand, endpoint := newRecreateStand(t)
	// The stop fails before anything is renamed, and somebody renames the container
	// while this recreate is unwinding.
	stand.hookCall("stop:"+standOldID, func() { stand.setName(standOldID, byHand) })
	stand.failCall("stop:"+standOldID, stopError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "stop container error")

	requireNoCall(t, stand.recorded(), "rename:"+standOldID+"->"+standName)
	require.Equal(t, byHand, stand.nameOf(standOldID), "the name somebody else set is left alone")

	// Reported, since the container is not the one the recreate started from, but as
	// a running container rather than an outage.
	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.True(t, restoreErr.OriginalRunning)
}

// TestRecreateReportsAnUnreadableOriginalStateAsUnknown covers the verdict having
// no observation to build on: the inspect that decides it is refused. Nothing can
// be claimed then — the container may be serving or may be down — so the caller
// is told exactly that, and told it conservatively (OriginalRunning false, which
// callers act on as an outage). What it must NOT get is the flat "it is left
// down" of a real outage, which here would be a page over a running container.
func TestRecreateReportsAnUnreadableOriginalStateAsUnknown(t *testing.T) {
	t.Parallel()

	const inspectError = "inspect refused by the daemon"

	svc, stand, endpoint := newRecreateStand(t)
	stand.failCall("start:"+standNewID, standStartError)
	// The third inspect of the run is the verdict's: the recreate opens with one
	// and the restore plans the rollback with another.
	stand.failNthCall("inspect", 3, inspectError)

	_, err := svc.Recreate(t.Context(), endpoint, standOldID, false, "", "")
	require.Error(t, err)

	var restoreErr *RestoreError
	require.ErrorAs(t, err, &restoreErr)
	require.True(t, restoreErr.StateUnknown)
	require.False(t, restoreErr.OriginalRunning, "an unread state is not a running container")
	require.ErrorContains(t, err, "could not be read")
	require.ErrorContains(t, err, inspectError)
	require.NotContains(t, err.Error(), "left down", "the container is in fact running, the message must not claim otherwise")

	require.True(t, stand.isRunning(standOldID), "the restore did put it back, only the reading of it failed")
}

// observedContainer is an inspect response as the restore reads one: the name the
// container carries and the networks it is attached to, which is all the plan and
// the verdict are built from.
func observedContainer(name string, attachedTo ...string) *container.InspectResponse {
	networks := make(map[string]*network.EndpointSettings, len(attachedTo))
	for _, id := range attachedTo {
		networks[id] = &network.EndpointSettings{NetworkID: id}
	}

	return &container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: name},
		NetworkSettings:   &container.NetworkSettings{Networks: networks},
	}
}

// TestPlanRestore pins the rule the rollback is planned by: what the engine
// reports about the original decides what has to be undone, and the steps that
// were attempted are only the fallback for when that cannot be read.
func TestPlanRestore(t *testing.T) {
	t.Parallel()

	attempted := []*network.EndpointSettings{{NetworkID: "net-a"}, {NetworkID: "net-b"}}

	tests := []struct {
		name            string
		observed        *container.InspectResponse
		renameAttempted bool
		wantRename      bool
		wantNetworks    []string
	}{
		{
			name:            "a container already back where it started needs nothing",
			observed:        observedContainer(standName, "net-a", "net-b"),
			renameAttempted: true,
		},
		{
			name:     "a rename whose answer was lost is undone all the same",
			observed: observedContainer(standName+"-old", "net-a", "net-b"),
			// The call came back as a failure, so nothing was "confirmed".
			renameAttempted: true,
			wantRename:      true,
		},
		{
			name:         "only the networks the original is actually missing come back",
			observed:     observedContainer(standName, "net-a"),
			wantNetworks: []string{"net-b"},
		},
		{
			// Not this recreate's doing and not this recreate's to undo: writing the
			// remembered name over it would take a name away from whoever set it.
			name:            "a name this recreate never touched is left alone",
			observed:        observedContainer("/web-by-hand", "net-a", "net-b"),
			renameAttempted: false,
		},
		{
			name:            "an unreadable state falls back to every attempted step",
			observed:        nil,
			renameAttempted: true,
			wantRename:      true,
			wantNetworks:    []string{"net-a", "net-b"},
		},
		{
			name:            "an unreadable state undoes nothing that was never attempted",
			observed:        nil,
			renameAttempted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attemptedNetworks := attempted
			if !tt.renameAttempted && tt.observed == nil {
				attemptedNetworks = nil
			}

			plan := planRestore(tt.observed, standName, tt.renameAttempted, attemptedNetworks)
			require.Equal(t, tt.wantRename, plan.rename)

			ids := make([]string, 0, len(plan.networks))
			for _, endpointSettings := range plan.networks {
				ids = append(ids, endpointSettings.NetworkID)
			}
			require.ElementsMatch(t, tt.wantNetworks, ids)
		})
	}
}

// TestMissingNetworks pins what "attached" means to the restore, which asks this
// twice: of the state it starts from, to plan the reconnects, and of the state it
// leaves behind, to decide whether the container is whole again. A state it could
// not read — nil, or an engine answer carrying no network settings at all — has
// to count as attached to NOTHING: reading it the other way would skip every
// reconnect and then pronounce the container fully restored.
func TestMissingNetworks(t *testing.T) {
	t.Parallel()

	attempted := []*network.EndpointSettings{{NetworkID: "net-a"}, {NetworkID: "net-b"}}

	tests := []struct {
		name      string
		observed  *container.InspectResponse
		attempted []*network.EndpointSettings
		want      []string
	}{
		{
			name:      "a state that could not be read is attached to nothing",
			observed:  nil,
			attempted: attempted,
			want:      []string{"net-a", "net-b"},
		},
		{
			name:      "an answer without network settings is attached to nothing",
			observed:  &container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Name: standName}},
			attempted: attempted,
			want:      []string{"net-a", "net-b"},
		},
		{
			name:      "only what the container is actually off is missing",
			observed:  observedContainer(standName, "net-a"),
			attempted: attempted,
			want:      []string{"net-b"},
		},
		{
			name:      "a container back on every network is missing nothing",
			observed:  observedContainer(standName, "net-a", "net-b"),
			attempted: attempted,
		},
		{
			name:      "a network nothing was detached from is not reconnected",
			observed:  observedContainer(standName),
			attempted: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ids := make([]string, 0, len(tt.want))
			for _, endpointSettings := range missingNetworks(tt.observed, tt.attempted) {
				ids = append(ids, endpointSettings.NetworkID)
			}

			require.ElementsMatch(t, tt.want, ids)
		})
	}
}

// TestRestoreContextFallsBackToTheDefaultBudget guards the zero value: a
// ContainerService built without NewContainerService carries no restore budget,
// and a zero timeout is an already-expired context — every restore call would
// fail before it left the process, switching the restore off without a word.
func TestRestoreContextFallsBackToTheDefaultBudget(t *testing.T) {
	t.Parallel()

	ctx, cancel := (&ContainerService{}).restoreContext(t.Context())
	defer cancel()

	deadline, ok := ctx.Deadline()
	require.True(t, ok, "the restore is always bounded")
	require.Positive(t, time.Until(deadline), "a zero budget must not leave the restore no time at all")
	require.LessOrEqual(t, time.Until(deadline), defaultRestoreTimeout)
}
