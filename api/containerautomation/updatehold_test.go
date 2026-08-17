package containerautomation

import (
	"context"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/docker/images"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

// TestUpdateStandaloneHoldsTheContainerItIsActingOn locks in the auto-update side
// of the auto-heal interlock. Three moments have to be covered, because auto-heal
// restarting the container at any of them fights the update for it — and mid-gate
// it also resets Docker health to "starting", so the gate waits instead of rolling
// back and a failed update can be accepted as a good one:
//
//   - the recreate itself: the original container is held by id, and it needs to
//     be, because Recreate pulls the image BEFORE it stops the container;
//   - the health gate over the new container, held by id;
//   - the whole pass, held by NAME — the only key that exists before the new
//     container does and that survives into the rollback recreate.
//
// And every hold is released by the time the pass returns, on the rollback path
// too, which returns early.
func TestUpdateStandaloneHoldsTheContainerItIsActingOn(t *testing.T) {
	const (
		oldID = "old-id"
		newID = "new-id"
		name  = "web"
		// A registry ref that resolves to a fast connection-refused, so the rollback
		// path's best-effort remote-digest resolution fails immediately (offline-safe).
		ref = "localhost:1/web:v1"
	)

	tests := []struct {
		name   string
		health container.HealthStatus
		// wantRollbackRecreate is whether the failed gate makes the pass recreate a
		// second time, under the same name, on the previous image.
		wantRollbackRecreate bool
	}{
		{name: "the healthy path releases every hold", health: container.Healthy},
		{name: "the rollback path releases every hold despite returning early", health: container.Unhealthy, wantRollbackRecreate: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, store := datastore.MustNewTestStore(t, true, false)

			seq := &callSeq{}
			cli := newFakeDockerClient(seq)
			cli.inspectByID[oldID] = preUpdateInspect(oldID, ref)
			cli.inspectByID[newID] = healthInspect(newID, tt.health)

			rec := &fakeRecreator{seq: seq, result: &types.ContainerJSON{
				ContainerJSONBase: &container.ContainerJSONBase{ID: newID, Image: newImageID},
				Config:            &container.Config{Image: ref},
			}}

			s := &Service{
				baseCtx:          context.Background(),
				containerService: rec,
				digestClient:     images.NewClientWithRegistry(images.NewRegistryClient(store), nil),
				notifier:         &seqNotifier{seq: seq},
				rolledBack:       map[string]rolledBackTarget{},
				updateHolds:      map[string]updateHold{},
			}

			endpoint := &portainer.Endpoint{ID: 1}

			// Recreate is where the holds around it are observable. The first call is the
			// update's own recreate of the ORIGINAL container (which the real Recreate
			// keeps running, and unhealthy, for the whole image pull); the second is the
			// rollback's recreate of the container the gate rejected, which creates a
			// third container under the SAME name — so the name hold must still be alive
			// there, at the worst possible moment to lose it.
			var (
				updateRecreates        int
				rollbackRecreates      int
				originalHeldAtRecreate bool
				nameHeldAtRecreate     bool
				nameHeldAtRollback     bool
			)
			rec.recreateHook = func(containerID string) {
				switch containerID {
				case oldID:
					updateRecreates++
					if _, held := s.heldByUpdate(endpoint.ID, oldID, ""); held {
						originalHeldAtRecreate = true
					}
					if _, held := s.heldByUpdate(endpoint.ID, "", name); held {
						nameHeldAtRecreate = true
					}
				case newID:
					rollbackRecreates++
					if _, held := s.heldByUpdate(endpoint.ID, "", name); held {
						nameHeldAtRollback = true
					}
				}
			}

			// The gate polls the new container through ContainerInspect, so the hook is
			// the only place the mid-gate state can be observed.
			gatePolls := 0
			heldDuringGate := false
			cli.inspectHook = func(containerID string) {
				if containerID != newID {
					return
				}

				gatePolls++
				if _, held := s.heldByUpdate(endpoint.ID, newID, ""); held {
					heldDuringGate = true
				}
			}

			c := UpdateCandidate{ID: oldID, Name: name, ImageID: oldImageID, Image: ref}
			s.updateStandalone(cli, endpoint, c, updateOptions{rollback: true, rollbackTimeout: 60 * time.Second})

			require.NotZero(t, updateRecreates, "the pass must recreate the original container, otherwise the holds around the recreate are untested")
			require.True(t, originalHeldAtRecreate,
				"the original container must be held while Recreate runs: it stays up, and unhealthy, for the whole image pull")
			require.True(t, nameHeldAtRecreate,
				"the name must be held from before the recreate, so the new container is covered from the moment Recreate starts it under that name")

			require.NotZero(t, gatePolls, "the health gate must poll the new container, otherwise the hold is untested")
			require.True(t, heldDuringGate, "the new container must be held while the health gate is open")

			if tt.wantRollbackRecreate {
				require.NotZero(t, rollbackRecreates, "a failed gate must roll back, otherwise the hold across the rollback is untested")
				require.True(t, nameHeldAtRollback,
					"the name must still be held while the rollback recreates under it: that container is born unhealthy by construction")
			}

			_, oldHeld := s.heldByUpdate(endpoint.ID, oldID, "")
			_, newHeld := s.heldByUpdate(endpoint.ID, newID, "")
			_, nameHeld := s.heldByUpdate(endpoint.ID, "", name)
			require.False(t, oldHeld, "the hold on the original container must be released when the pass returns")
			require.False(t, newHeld, "the hold on the new container must be released when the pass returns")
			require.False(t, nameHeld, "the hold on the container name must be released when the pass returns")
			require.Zero(t, heldCount(s), "holds are leases: none may outlive the pass that took them")
		})
	}
}

// heldCount reports how many holds the service currently carries, read under
// updateHoldMu. Tests go through it instead of reading s.updateHolds directly:
// the map is written by the update pass on another goroutine, and a bare read of
// it is exactly the kind of thing -race exists to catch.
func heldCount(s *Service) int {
	s.updateHoldMu.Lock()
	defer s.updateHoldMu.Unlock()

	return len(s.updateHolds)
}

// TestClaimFirstSkipLogIsOncePerHold pins the once-per-hold Info mechanic
// directly, because nothing else does: a hold outlives many heal ticks, so
// reporting every suppressed tick at Info would flood the log with one line per
// tick for the whole recreate plus gate window, and the flood would still leave
// every other test in this package green.
//
// The token belongs to the HOLD, not to the container: it is per key, and a fresh
// hold on the same key starts unclaimed, so the next update over that container is
// announced again rather than silenced by the previous one.
func TestClaimFirstSkipLogIsOncePerHold(t *testing.T) {
	const (
		endpointID  = portainer.EndpointID(1)
		containerID = "c1"
		otherID     = "c2"
		name        = "web"
	)

	newService := func() *Service { return &Service{updateHolds: map[string]updateHold{}} }

	t.Run("a hold is announced once and then reported at Debug", func(t *testing.T) {
		s := newService()
		s.acquireUpdateHold(endpointID, containerID)

		// heldByUpdate is a pure probe: asking whether the container is held, however
		// often, must not consume the single Info the hold is worth.
		for range 3 {
			_, held := s.heldByUpdate(endpointID, containerID, "")
			require.True(t, held, "the hold is taken, so the lookup must see it")
		}

		require.True(t, s.claimFirstSkipLog(endpointID, containerID, ""),
			"the first heal tick suppressed by this hold is the one that gets reported")
		require.False(t, s.claimFirstSkipLog(endpointID, containerID, ""),
			"every later tick of the same hold must fall back to Debug")
		require.False(t, s.claimFirstSkipLog(endpointID, containerID, ""),
			"the token is consumed once, not refreshed by later ticks")
	})

	t.Run("each hold carries its own token", func(t *testing.T) {
		s := newService()
		s.acquireUpdateHold(endpointID, containerID)
		s.acquireUpdateHold(endpointID, otherID)

		require.True(t, s.claimFirstSkipLog(endpointID, containerID, ""))
		require.True(t, s.claimFirstSkipLog(endpointID, otherID, ""),
			"one container's Info must not silence the skip of a different container")
	})

	t.Run("the id hold and the name hold of one container are separate holds", func(t *testing.T) {
		s := newService()
		s.acquireUpdateHold(endpointID, containerID)
		s.acquireUpdateHoldByName(endpointID, name)

		require.True(t, s.claimFirstSkipLog(endpointID, containerID, ""),
			"claiming the id hold must not touch the name hold")

		// Only the id token is spent at this point, which is what makes the key order
		// observable: asked about BOTH keys, the claim answers false only if it
		// resolves the id key first — resolving the name key first would find an
		// untouched token and answer true. It pins the order inside claimFirstSkipLog
		// only: heldByUpdate documents the same order but nothing here observes it,
		// since its key order shows up in held_for alone, which no test asserts on.
		require.False(t, s.claimFirstSkipLog(endpointID, containerID, name),
			"asked about both keys, the claim resolves the id first — and that one is already spent")

		require.True(t, s.claimFirstSkipLog(endpointID, "", name),
			"the name hold keeps its own token, unspent by either claim above")
	})

	t.Run("a fresh hold on the same key starts unclaimed", func(t *testing.T) {
		s := newService()
		s.acquireUpdateHold(endpointID, containerID)
		require.True(t, s.claimFirstSkipLog(endpointID, containerID, ""))

		s.releaseUpdateHold(endpointID, containerID)
		s.acquireUpdateHold(endpointID, containerID)

		require.True(t, s.claimFirstSkipLog(endpointID, containerID, ""),
			"the next update over this container is a new event and gets its own Info")
	})

	t.Run("a container that is not held claims nothing", func(t *testing.T) {
		s := newService()

		require.False(t, s.claimFirstSkipLog(endpointID, containerID, name),
			"with no hold there is no skip to announce")
	})
}
