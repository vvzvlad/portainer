package containerautomation

import (
	"context"
	"fmt"
	"sync"

	portainer "github.com/portainer/portainer/api"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

// callSeq is a shared, ordered recorder the daemon-path fakes write to, so a test
// can assert the SEQUENCE of operations across the docker client, the recreator
// and the notifier on a single timeline (e.g. cleanup strictly after the healthy
// gate, "updated" held until health is confirmed).
type callSeq struct {
	mu    sync.Mutex
	calls []string
}

func (s *callSeq) record(entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, entry)
}

// snapshot returns a copy of the recorded calls in order.
func (s *callSeq) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// indexOf returns the position of the first recorded call equal to entry, or -1.
func (s *callSeq) indexOf(entry string) int {
	for i, c := range s.snapshot() {
		if c == entry {
			return i
		}
	}
	return -1
}

// fakeDockerClient is a programmable, call-recording implementation of the
// dockerClient seam. Inspect responses/errors are keyed by container id so the
// pre-update inspect (old id) and the health-gate poll (new id) can return
// different states within one update.
type fakeDockerClient struct {
	seq *callSeq

	inspectByID    map[string]container.InspectResponse
	inspectErrByID map[string]error

	// inspectHook, when set, runs on every ContainerInspect before the programmed
	// response is returned. It lets a test observe service state at the exact moment
	// the health gate polls the new container, which is the only window where the
	// auto-update hold has to be observable from the outside.
	inspectHook func(containerID string)

	imageTagErr    error
	imageRemoveErr error
	restartErrByID map[string]error
}

func newFakeDockerClient(seq *callSeq) *fakeDockerClient {
	return &fakeDockerClient{
		seq:            seq,
		inspectByID:    map[string]container.InspectResponse{},
		inspectErrByID: map[string]error{},
		restartErrByID: map[string]error{},
	}
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, containerID string) (container.InspectResponse, error) {
	f.seq.record("inspect:" + containerID)
	if f.inspectHook != nil {
		f.inspectHook(containerID)
	}
	if err := f.inspectErrByID[containerID]; err != nil {
		return container.InspectResponse{}, err
	}
	resp, ok := f.inspectByID[containerID]
	if !ok {
		return container.InspectResponse{}, fmt.Errorf("fake: no inspect programmed for %q", containerID)
	}
	return resp, nil
}

func (f *fakeDockerClient) ContainerRestart(_ context.Context, containerID string, _ container.StopOptions) error {
	f.seq.record("restart:" + containerID)
	return f.restartErrByID[containerID]
}

func (f *fakeDockerClient) ImageTag(_ context.Context, source, target string) error {
	f.seq.record("imagetag:" + source + "->" + target)
	return f.imageTagErr
}

func (f *fakeDockerClient) ImageRemove(_ context.Context, imageID string, _ image.RemoveOptions) ([]image.DeleteResponse, error) {
	f.seq.record("imageremove:" + imageID)
	if f.imageRemoveErr != nil {
		return nil, f.imageRemoveErr
	}
	return []image.DeleteResponse{{Deleted: imageID}}, nil
}

// recreateCall records the salient arguments of a single Recreate invocation.
type recreateCall struct {
	containerID    string
	forcePullImage bool
}

// fakeRecreator is a programmable, call-recording implementation of the
// containerRecreator seam. It returns the same result for every call; the
// standalone rollback path recreates a second time (on the previous image) and
// ignores that return value.
type fakeRecreator struct {
	seq    *callSeq
	result *types.ContainerJSON
	err    error
	calls  []recreateCall

	// recreateHook, when set, runs on every Recreate before the programmed result is
	// returned. It mirrors inspectHook on fakeDockerClient: a recreate is the only
	// window in which the holds taken around it are observable from the outside — the
	// one on the ORIGINAL container (which stays up, and unhealthy, for the whole
	// image pull the real Recreate does first), and the one on the container name
	// (which must still be held when the rollback recreates under it).
	recreateHook func(containerID string)
}

func (f *fakeRecreator) Recreate(_ context.Context, _ *portainer.Endpoint, containerID string, forcePullImage bool, _, _ string) (*types.ContainerJSON, error) {
	f.seq.record("recreate:" + containerID)
	if f.recreateHook != nil {
		f.recreateHook(containerID)
	}
	f.calls = append(f.calls, recreateCall{containerID: containerID, forcePullImage: forcePullImage})
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// seqNotifier records each event onto the shared timeline (as "event:<kind>") and
// keeps the full events for content assertions.
type seqNotifier struct {
	seq    *callSeq
	events []Event
}

func (n *seqNotifier) Notify(event Event) {
	n.seq.record("event:" + string(event.Kind))
	n.events = append(n.events, event)
}

// only returns the single recorded event of the given kind, requiring exactly one.
func (n *seqNotifier) only(kind EventKind) (Event, int) {
	var found Event
	count := 0
	for _, e := range n.events {
		if e.Kind == kind {
			found = e
			count++
		}
	}
	return found, count
}
