package containerautomation

import (
	"context"
	"strings"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/docker/images"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

const (
	// 64-hex content-addressable image ids for the pre/post-update identities.
	oldImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newImageID = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// preUpdateInspect is the pre-update ContainerInspect the standalone path issues
// on the OLD container to capture its original image ref + healthcheck (so the
// rollback health gate is armed).
func preUpdateInspect(id, ref string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, Image: oldImageID},
		Config: &container.Config{
			Image:       ref,
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}},
		},
	}
}

// healthInspect is the health-gate ContainerInspect on the NEW container,
// reporting the given health status.
func healthInspect(id string, status container.HealthStatus) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:    id,
			State: &container.State{Running: true, Health: &container.Health{Status: status}},
		},
		Config: &container.Config{},
	}
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// TestUpdateStandaloneHappyPathCleansUpAfterHealthGate locks in the happy-path
// wiring #20 calls out: pull/recreate -> health gate confirms healthy -> the
// "updated" event is emitted only AFTER health is confirmed -> the old-image
// cleanup runs strictly AFTER the healthy gate (so a rollback target could never
// be deleted before the update is confirmed good).
func TestUpdateStandaloneHappyPathCleansUpAfterHealthGate(t *testing.T) {
	const (
		oldID = "old-id"
		newID = "new-id"
		ref   = "nginx:1.21"
	)

	seq := &callSeq{}
	notif := &seqNotifier{seq: seq}

	cli := newFakeDockerClient(seq)
	cli.inspectByID[oldID] = preUpdateInspect(oldID, ref)
	cli.inspectByID[newID] = healthInspect(newID, container.Healthy)

	rec := &fakeRecreator{seq: seq, result: &types.ContainerJSON{
		ContainerJSONBase: &container.ContainerJSONBase{ID: newID, Image: newImageID},
		Config:            &container.Config{Image: ref},
	}}

	s := &Service{
		baseCtx:          context.Background(),
		containerService: rec,
		notifier:         notif,
		rolledBack:       map[string]rolledBackTarget{},
	}

	endpoint := &portainer.Endpoint{ID: 1}
	c := UpdateCandidate{ID: oldID, Name: "web", ImageID: oldImageID, Image: ref}
	opts := updateOptions{cleanup: true, rollback: true, rollbackTimeout: 60 * time.Second}

	s.updateStandalone(cli, endpoint, c, opts)

	calls := seq.snapshot()
	iRecreate := seq.indexOf("recreate:" + oldID)
	iGate := seq.indexOf("inspect:" + newID)
	iUpdated := seq.indexOf("event:" + string(EventUpdated))
	iCleanup := seq.indexOf("imageremove:" + oldImageID)

	require.NotEqual(t, -1, iRecreate, "recreate must run")
	require.NotEqual(t, -1, iGate, "the health gate must poll the new container")
	require.NotEqual(t, -1, iUpdated, "an updated event must be emitted")
	require.NotEqual(t, -1, iCleanup, "cleanup must remove the old image")

	require.Less(t, iRecreate, iGate, "recreate must happen before the health gate")
	require.Less(t, iGate, iUpdated, "the updated event must be held until health is confirmed")
	require.Less(t, iUpdated, iCleanup, "cleanup must run strictly AFTER the healthy gate/updated event")

	require.Equal(t, oldImageID, calls[iCleanup][len("imageremove:"):], "cleanup targets the OLD image, never the new/rollback target")

	updated, n := notif.only(EventUpdated)
	require.Equal(t, 1, n, "exactly one updated event")
	require.Equal(t, newID, updated.ContainerID)
	require.Equal(t, oldImageID, updated.OldDigest)
	require.Equal(t, newImageID, updated.NewDigest)

	_, rollbacks := notif.only(EventRollback)
	require.Zero(t, rollbacks, "no rollback on the happy path")
}

// TestUpdateStandaloneRollbackPreservesTarget locks in the rollback-path wiring:
// a new container that fails the health gate is rolled back to the previous image
// (re-tag -> recreate on the old image with NO pull), EventRollback (not
// EventUpdated) is emitted, and the old-image cleanup NEVER runs — so the rollback
// target is never deleted early.
func TestUpdateStandaloneRollbackPreservesTarget(t *testing.T) {
	_, store := datastore.MustNewTestStore(t, true, false)

	const (
		oldID = "old-id"
		newID = "new-id"
		// A registry ref that resolves to a fast connection-refused so the loop-guard's
		// best-effort remote-digest resolution fails immediately (offline-safe) without
		// blocking; the record is still stored with an empty digest.
		ref = "localhost:1/web:v1"
	)

	seq := &callSeq{}
	notif := &seqNotifier{seq: seq}

	cli := newFakeDockerClient(seq)
	cli.inspectByID[oldID] = preUpdateInspect(oldID, ref)
	cli.inspectByID[newID] = healthInspect(newID, container.Unhealthy)

	rec := &fakeRecreator{seq: seq, result: &types.ContainerJSON{
		ContainerJSONBase: &container.ContainerJSONBase{ID: newID, Image: newImageID},
		Config:            &container.Config{Image: ref},
	}}

	s := &Service{
		baseCtx:          context.Background(),
		containerService: rec,
		digestClient:     images.NewClientWithRegistry(images.NewRegistryClient(store), nil),
		notifier:         notif,
		rolledBack:       map[string]rolledBackTarget{},
	}

	endpoint := &portainer.Endpoint{ID: 1}
	c := UpdateCandidate{ID: oldID, Name: "web", ImageID: oldImageID, Image: ref}
	// cleanup enabled to prove it is NOT run on the rollback path.
	opts := updateOptions{cleanup: true, rollback: true, rollbackTimeout: 60 * time.Second}

	s.updateStandalone(cli, endpoint, c, opts)

	calls := seq.snapshot()
	iGate := seq.indexOf("inspect:" + newID)
	iTag := seq.indexOf("imagetag:" + oldImageID + "->" + ref)
	iRollbackRecreate := seq.indexOf("recreate:" + newID)
	iRollbackEvent := seq.indexOf("event:" + string(EventRollback))

	require.NotEqual(t, -1, iGate, "the health gate must poll the new container")
	require.NotEqual(t, -1, iTag, "rollback must re-tag the previous image onto the original ref")
	require.NotEqual(t, -1, iRollbackRecreate, "rollback must recreate on the previous image")
	require.NotEqual(t, -1, iRollbackEvent, "a rollback event must be emitted")

	require.Less(t, iGate, iTag, "rollback happens after the failed gate")
	require.Less(t, iTag, iRollbackRecreate, "re-tag the old image before recreating on it")
	require.Less(t, iRollbackRecreate, iRollbackEvent, "the rollback event follows the rollback recreate")

	require.Zero(t, countPrefix(calls, "imageremove:"), "cleanup must NOT run on the rollback path (rollback target preserved)")

	_, updates := notif.only(EventUpdated)
	require.Zero(t, updates, "no updated event on the rollback path")

	rollback, n := notif.only(EventRollback)
	require.Equal(t, 1, n, "exactly one rollback event")
	require.Equal(t, ref, rollback.Image, "the rollback event carries the restored original ref")

	// The recreate seam saw two calls: the initial pull-recreate on the old container,
	// then the rollback recreate on the new container WITHOUT a pull (resolves the
	// re-tagged previous image).
	require.Len(t, rec.calls, 2)
	require.Equal(t, recreateCall{containerID: oldID, forcePullImage: true}, rec.calls[0])
	require.Equal(t, recreateCall{containerID: newID, forcePullImage: false}, rec.calls[1])
}

// TestHealContainersRestartsUnhealthyThenRespectsCooldown locks in the auto-heal
// restart loop: an unhealthy container is restarted (restart precedes the heal
// event), and a second immediate pass is suppressed by the restart cooldown/loop
// guard so a flapping container is not restart-stormed.
func TestHealContainersRestartsUnhealthyThenRespectsCooldown(t *testing.T) {
	seq := &callSeq{}
	notif := &seqNotifier{seq: seq}
	cli := newFakeDockerClient(seq)

	s := &Service{
		baseCtx:  context.Background(),
		notifier: notif,
		retries:  map[string]retryState{},
	}

	endpoint := &portainer.Endpoint{ID: 1}
	containers := []container.Summary{{ID: "c1", Names: []string{"/web"}}}

	// First pass: the unhealthy container is restarted and a heal event is emitted.
	s.healContainers(cli, endpoint, ScopeAll, containers)

	require.Equal(t, []string{"restart:c1", "event:" + string(EventHealRestarted)}, seq.snapshot(),
		"the restart must precede the heal event")
	require.Equal(t, 1, s.getRetry("c1").attempts, "the restart is accounted against the retry budget")

	ev, n := notif.only(EventHealRestarted)
	require.Equal(t, 1, n)
	require.Equal(t, "c1", ev.ContainerID)
	require.Equal(t, "web", ev.ContainerName)

	// Second immediate pass: within the restart cooldown, the loop guard suppresses a
	// second restart (no new restart call, no new event).
	s.healContainers(cli, endpoint, ScopeAll, containers)

	require.Equal(t, 1, countPrefix(seq.snapshot(), "restart:"), "no second restart within the cooldown")
	_, n2 := notif.only(EventHealRestarted)
	require.Equal(t, 1, n2, "the flap is suppressed by the restart cooldown")
}
