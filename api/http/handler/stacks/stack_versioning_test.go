package stacks

import (
	"strconv"
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/filesystem"

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
