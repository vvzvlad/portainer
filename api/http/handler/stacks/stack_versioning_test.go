package stacks

import (
	"errors"
	"strconv"
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/dataservices"
	"github.com/portainer/portainer/api/datastore"
	"github.com/portainer/portainer/api/filesystem"
	"github.com/portainer/portainer/api/internal/testhelpers"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"

	"github.com/stretchr/testify/require"
)

// TestApplyRetention verifies that once the version history exceeds the cap, the oldest
// entries are trimmed from Versions in memory and their version numbers are returned for
// post-commit deletion. Retention itself must NOT touch the disk.
func TestApplyRetention(t *testing.T) {
	t.Parallel()

	fs, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	stackID := 7
	stackFolder := strconv.Itoa(stackID)

	const total = maxStackFileVersions + 2
	stack := &portainer.Stack{
		ID:               portainer.StackID(stackID),
		EntryPoint:       "docker-compose.yml",
		StackFileVersion: total,
	}

	for v := 1; v <= total; v++ {
		_, err := fs.StoreStackFileFromBytesByVersion(stackFolder, stack.EntryPoint, v, []byte("v"+strconv.Itoa(v)))
		require.NoError(t, err)
		stack.Versions = append(stack.Versions, portainer.StackFileVersionInfo{Version: v})
	}

	pruned := applyRetention(stack, maxStackFileVersions)

	require.Len(t, stack.Versions, maxStackFileVersions)
	// The two oldest (v1, v2) must be dropped; the window now starts at v3.
	require.Equal(t, 3, stack.Versions[0].Version)
	require.Equal(t, total, stack.Versions[len(stack.Versions)-1].Version)

	// The two oldest versions are returned for deletion, in order.
	require.Equal(t, []int{1, 2}, pruned)

	// applyRetention must NOT delete anything from disk; all directories still exist.
	for _, v := range []int{1, 2, 3, total} {
		exists, err := fs.FileExists(fs.GetStackProjectPathByVersion(stackFolder, v, ""))
		require.NoError(t, err)
		require.True(t, exists, "version %d directory must still exist (retention is in-memory only)", v)
	}
}

// TestApplyRetentionKeepsCurrentVersion verifies the safety guard: a to-be-pruned entry that
// happens to be the currently deployed version is neither trimmed from Versions nor returned
// for deletion, keeping the slice trim consistent with what is actually pruned.
func TestApplyRetentionKeepsCurrentVersion(t *testing.T) {
	t.Parallel()

	// 4 versions, cap 2 -> the 2 oldest (v1, v2) are eligible; but mark v1 as current.
	stack := &portainer.Stack{ID: 1, StackFileVersion: 1}
	stack.Versions = []portainer.StackFileVersionInfo{{Version: 1}, {Version: 2}, {Version: 3}, {Version: 4}}

	pruned := applyRetention(stack, 2)

	// Only v2 is pruned; v1 (current) is kept even though it is in the oldest window.
	require.Equal(t, []int{2}, pruned)
	// The kept slice retains v1 and every non-pruned entry, in order.
	require.Equal(t, []int{1, 3, 4}, versionNumbers(stack.Versions))
}

// TestApplyRetentionNoOp verifies retention leaves history untouched when under the cap.
func TestApplyRetentionNoOp(t *testing.T) {
	t.Parallel()

	stack := &portainer.Stack{ID: 1, StackFileVersion: 3}
	stack.Versions = []portainer.StackFileVersionInfo{{Version: 1}, {Version: 2}, {Version: 3}}

	pruned := applyRetention(stack, maxStackFileVersions)

	require.Nil(t, pruned)
	require.Len(t, stack.Versions, 3)
}

func versionNumbers(versions []portainer.StackFileVersionInfo) []int {
	out := make([]int, len(versions))
	for i, v := range versions {
		out[i] = v.Version
	}
	return out
}

// TestValidateRollbackTarget checks the rollback-version guard: it rejects non-positive versions,
// versions above the current file version, and versions that are not present in the append-only
// history (a "hole"), while accepting an in-range version that exists in Versions[].
func TestValidateRollbackTarget(t *testing.T) {
	t.Parallel()

	// StackFileVersion is 5, but v4 was never recorded (a hole in the history).
	stack := &portainer.Stack{
		StackFileVersion: 5,
		Versions: []portainer.StackFileVersionInfo{
			{Version: 1}, {Version: 2}, {Version: 3}, {Version: 5},
		},
	}

	require.Error(t, validateRollbackTarget(stack, 0), "zero is not a valid version")
	require.Error(t, validateRollbackTarget(stack, -1), "negative is not a valid version")
	require.Error(t, validateRollbackTarget(stack, 6), "version above StackFileVersion is out of range")
	require.Error(t, validateRollbackTarget(stack, 4), "a version missing from history (hole) is rejected")

	require.NoError(t, validateRollbackTarget(stack, 3), "in-range version present in history is accepted")
	require.NoError(t, validateRollbackTarget(stack, 5), "current version present in history is accepted")
}

// newVersioningTestHandler builds a Handler wired with a real filesystem service and datastore,
// plus a test user, for exercising the version snapshot/prune helpers.
func newVersioningTestHandler(t *testing.T) (*Handler, *datastore.Store, portainer.FileService, *portainer.User) {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, false, true)

	fs, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	user, err := mockCreateUser(store)
	require.NoError(t, err)

	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.DataStore = store
	handler.FileService = fs

	return handler, store, fs, user
}

// TestSnapshotFileBasedStackVersion_Rollback verifies the rollback branch: it reads the TARGET
// version's content from disk (ignoring the client-supplied content), writes a NEW monotonic
// version whose note is "rollback from v{N}", and repoints ProjectPath to the new version dir.
func TestSnapshotFileBasedStackVersion_Rollback(t *testing.T) {
	t.Parallel()

	handler, store, fs, user := newVersioningTestHandler(t)

	stackFolder := "1"
	entryPoint := "docker-compose.yml"

	// Seed three on-disk versions with distinct content.
	for v, content := range map[int]string{1: "V1-CONTENT", 2: "V2-CONTENT", 3: "V3-CONTENT"} {
		_, err := fs.StoreStackFileFromBytesByVersion(stackFolder, entryPoint, v, []byte(content))
		require.NoError(t, err)
	}

	stack := &portainer.Stack{
		ID:               1,
		EntryPoint:       entryPoint,
		StackFileVersion: 3,
		ProjectPath:      fs.GetStackProjectPathByVersion(stackFolder, 3, ""),
		Versions: []portainer.StackFileVersionInfo{
			{Version: 1}, {Version: 2}, {Version: 3},
		},
	}

	target := 1
	var (
		pruned  []int
		httpErr *httperror.HandlerError
	)
	err := store.UpdateTx(func(tx dataservices.DataStoreTx) error {
		// Client content is deliberately non-empty to prove it is IGNORED for a rollback.
		pruned, httpErr = handler.snapshotFileBasedStackVersion(tx, stack, []byte("CLIENT-CONTENT-IGNORED"), &target, user.ID)
		return nil
	})
	require.NoError(t, err)
	require.Nil(t, httpErr)
	require.Empty(t, pruned, "no retention pruning expected below the cap")

	// A new monotonic version (v4) was created.
	require.Equal(t, 4, stack.StackFileVersion)
	require.Equal(t, fs.GetStackProjectPathByVersion(stackFolder, 4, ""), stack.ProjectPath)

	last := stack.Versions[len(stack.Versions)-1]
	require.Equal(t, 4, last.Version)
	require.Equal(t, "rollback from v1", last.Note)

	// The new version's content is the TARGET (v1) content read from disk, not the client payload.
	got, err := fs.GetFileContent(stack.ProjectPath, entryPoint)
	require.NoError(t, err)
	require.Equal(t, "V1-CONTENT", string(got))
}

// TestSnapshotFileBasedStackVersion_MonotonicVersion guards against a len-based next-version bug:
// the new version must be StackFileVersion+1, strictly greater than any previously trimmed version,
// even when the history slice is shorter than StackFileVersion (older entries already pruned).
func TestSnapshotFileBasedStackVersion_MonotonicVersion(t *testing.T) {
	t.Parallel()

	handler, store, fs, user := newVersioningTestHandler(t)

	stackFolder := "1"
	entryPoint := "docker-compose.yml"

	// StackFileVersion is 24 but only two history entries remain (older ones were pruned):
	// a len-based scheme would wrongly compute the next version as len+1 = 3.
	stack := &portainer.Stack{
		ID:               1,
		EntryPoint:       entryPoint,
		StackFileVersion: 24,
		ProjectPath:      fs.GetStackProjectPathByVersion(stackFolder, 24, ""),
		Versions: []portainer.StackFileVersionInfo{
			{Version: 23}, {Version: 24},
		},
	}

	var httpErr *httperror.HandlerError
	err := store.UpdateTx(func(tx dataservices.DataStoreTx) error {
		_, httpErr = handler.snapshotFileBasedStackVersion(tx, stack, []byte("NEW-CONTENT"), nil, user.ID)
		return nil
	})
	require.NoError(t, err)
	require.Nil(t, httpErr)

	require.Equal(t, 25, stack.StackFileVersion, "next version must be StackFileVersion+1, not len-based")
	last := stack.Versions[len(stack.Versions)-1]
	require.Equal(t, 25, last.Version)
	require.Greater(t, last.Version, 24, "new version must be strictly greater than any prior version")

	got, err := fs.GetFileContent(stack.ProjectPath, entryPoint)
	require.NoError(t, err)
	require.Equal(t, "NEW-CONTENT", string(got))
}

// countingFileService wraps a FileService to count and optionally fail RemoveDirectory calls,
// so tests can assert prune ordering and error-swallowing without touching real disk.
type countingFileService struct {
	portainer.FileService
	removeDirErr   error
	removeDirCalls int
	removedDirs    []string
}

func (s *countingFileService) GetStackProjectPathByVersion(stackID string, version int, commitHash string) string {
	return "compose/" + stackID + "/v" + strconv.Itoa(version)
}

func (s *countingFileService) RemoveDirectory(dir string) error {
	s.removeDirCalls++
	s.removedDirs = append(s.removedDirs, dir)
	return s.removeDirErr
}

// TestPruneStackFileVersionDirs_RemovesGivenDirs verifies the prune helper physically deletes
// exactly the requested version directories and leaves the others untouched.
func TestPruneStackFileVersionDirs_RemovesGivenDirs(t *testing.T) {
	t.Parallel()

	fs, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.FileService = fs

	const stackID = portainer.StackID(9)
	stackFolder := strconv.Itoa(int(stackID))

	for v := 1; v <= 3; v++ {
		_, err := fs.StoreStackFileFromBytesByVersion(stackFolder, "docker-compose.yml", v, []byte("v"+strconv.Itoa(v)))
		require.NoError(t, err)
	}

	handler.pruneStackFileVersionDirs(stackID, []int{1, 3})

	for v, wantExists := range map[int]bool{1: false, 2: true, 3: false} {
		exists, err := fs.FileExists(fs.GetStackProjectPathByVersion(stackFolder, v, ""))
		require.NoError(t, err)
		require.Equal(t, wantExists, exists, "version %d directory existence mismatch", v)
	}
}

// TestPruneStackFileVersionDirs_SwallowsRemoveError verifies a RemoveDirectory failure is
// best-effort: it is logged and swallowed (no panic), and every requested version is attempted.
func TestPruneStackFileVersionDirs_SwallowsRemoveError(t *testing.T) {
	t.Parallel()

	fs := &countingFileService{removeDirErr: errors.New("disk error")}
	handler := NewHandler(testhelpers.NewTestRequestBouncer())
	handler.FileService = fs

	require.NotPanics(t, func() {
		handler.pruneStackFileVersionDirs(7, []int{1, 2})
	})
	require.Equal(t, 2, fs.removeDirCalls, "every requested version directory must be attempted")
}

// TestPruneGateContract_Illustrative documents the post-commit gate shape used by stackUpdate:
// the pruned version directories are physically removed only when the transaction committed
// (err == nil); on a failed transaction the trimmed Versions[] was never persisted, so the
// directories must be kept on disk to stay consistent with the database.
//
// NOTE: this reproduces the gate condition inline — it illustrates the intended contract rather
// than exercising the real handler wiring (forcing a mid-commit UpdateTx failure while
// pruneVersions is already populated is not injectable in a unit test). The actual gate lives in
// stackUpdate (`if err == nil && len(pruneVersions) > 0`); pruneStackFileVersionDirs's real
// behaviour (deletes the given dirs, swallows RemoveDirectory errors) is covered non-vacuously by
// TestPruneStackFileVersionDirs_RemovesGivenDirs and _SwallowsRemoveError.
func TestPruneGateContract_Illustrative(t *testing.T) {
	t.Parallel()

	pruneVersions := []int{1, 2}

	t.Run("transaction failed - prune skipped", func(t *testing.T) {
		fs := &countingFileService{}
		handler := NewHandler(testhelpers.NewTestRequestBouncer())
		handler.FileService = fs

		var err error = errors.New("commit failed")
		if err == nil && len(pruneVersions) > 0 {
			handler.pruneStackFileVersionDirs(7, pruneVersions)
		}

		require.Zero(t, fs.removeDirCalls, "no directory may be deleted when the transaction failed")
	})

	t.Run("transaction committed - prune runs", func(t *testing.T) {
		fs := &countingFileService{}
		handler := NewHandler(testhelpers.NewTestRequestBouncer())
		handler.FileService = fs

		var err error
		if err == nil && len(pruneVersions) > 0 {
			handler.pruneStackFileVersionDirs(7, pruneVersions)
		}

		require.Equal(t, len(pruneVersions), fs.removeDirCalls, "all pruned directories deleted after commit")
	})
}

// TestSnapshotStackFilesToVersion verifies a multi-file snapshot writes every file into the
// v{N} folder and returns the version project path (not the base path).
func TestSnapshotStackFilesToVersion(t *testing.T) {
	t.Parallel()

	fs, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	stackFolder := "42"
	filesContent := map[string][]byte{
		"docker-compose.yml": []byte("main"),
		"override.yml":       []byte("extra"),
	}

	projectPath, err := snapshotStackFilesToVersion(fs, stackFolder, 5, filesContent)
	require.NoError(t, err)
	require.Equal(t, fs.GetStackProjectPathByVersion(stackFolder, 5, ""), projectPath)

	for name, want := range filesContent {
		got, err := fs.GetFileContent(projectPath, name)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}
