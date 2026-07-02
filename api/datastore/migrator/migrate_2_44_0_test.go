package migrator

import (
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/database/boltdb"
	"github.com/portainer/portainer/api/dataservices/stack"
	"github.com/portainer/portainer/api/filesystem"
	"github.com/portainer/portainer/api/logs"

	"github.com/stretchr/testify/require"
)

func newStackVersionTestMigrator(t *testing.T) (*Migrator, *stack.Service, portainer.FileService) {
	t.Helper()

	conn := &boltdb.DbConnection{Path: t.TempDir()}
	require.NoError(t, conn.Open())
	t.Cleanup(func() { logs.CloseAndLogErr(conn) })

	stackSvc, err := stack.NewService(conn)
	require.NoError(t, err)

	fileSvc, err := filesystem.NewService(t.TempDir(), "")
	require.NoError(t, err)

	m := NewMigrator(&MigratorParameters{
		StackService: stackSvc,
		FileService:  fileSvc,
	})

	return m, stackSvc, fileSvc
}

func TestMigrateStackFileVersions_2_44_0_MultiFileMove(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	// Seed the legacy on-disk layout: files live directly under compose/{id}/.
	_, err := fileSvc.StoreStackFileFromBytes(stackFolder, "docker-compose.yml", []byte("main-content"))
	require.NoError(t, err)
	_, err = fileSvc.StoreStackFileFromBytes(stackFolder, "override.yml", []byte("override-content"))
	require.NoError(t, err)

	fileStack := &portainer.Stack{
		ID:              1,
		Name:            "file-stack",
		Type:            portainer.DockerComposeStack,
		EntryPoint:      "docker-compose.yml",
		AdditionalFiles: []string{"override.yml"},
		ProjectPath:     fileSvc.GetStackProjectPath(stackFolder),
		CreationDate:    1234,
		CreatedBy:       "admin",
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	migrated, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 1, migrated.StackFileVersion)
	require.Equal(t, fileSvc.GetStackProjectPathByVersion(stackFolder, 1, ""), migrated.ProjectPath)
	require.Len(t, migrated.Versions, 1)
	require.Equal(t, 1, migrated.Versions[0].Version)
	require.Equal(t, int64(1234), migrated.Versions[0].CreatedAt)
	require.Equal(t, "admin", migrated.Versions[0].CreatedBy)
	require.Equal(t, "migrated", migrated.Versions[0].Note)

	// Files must have been moved into v1 (content preserved) and removed from the base folder.
	v1Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 1, "")
	got, err := fileSvc.GetFileContent(v1Path, "docker-compose.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("main-content"), got)
	got, err = fileSvc.GetFileContent(v1Path, "override.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("override-content"), got)

	basePath := fileSvc.GetStackProjectPath(stackFolder)
	exists, err := fileSvc.FileExists(basePath + "/docker-compose.yml")
	require.NoError(t, err)
	require.False(t, exists, "old base entrypoint should be removed")

	// Idempotency: a second run must not change anything and must not error.
	require.NoError(t, m.migrateStackFileVersions_2_44_0())
	again, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 1, again.StackFileVersion)
	require.Len(t, again.Versions, 1)
}

func TestMigrateStackFileVersions_2_44_0_SkipsGitAndOrphan(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	// Git stack: must be left untouched.
	gitStack := &portainer.Stack{
		ID:          1,
		Name:        "git-stack",
		Type:        portainer.DockerComposeStack,
		EntryPoint:  "docker-compose.yml",
		ProjectPath: fileSvc.GetStackProjectPath("1"),
		WorkflowID:  99,
	}
	require.NoError(t, stackSvc.Create(gitStack))

	// Orphan file-based stack: metadata present but no files on disk.
	orphanStack := &portainer.Stack{
		ID:          2,
		Name:        "orphan-stack",
		Type:        portainer.DockerSwarmStack,
		EntryPoint:  "docker-compose.yml",
		ProjectPath: fileSvc.GetStackProjectPath("2"),
	}
	require.NoError(t, stackSvc.Create(orphanStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	git, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 0, git.StackFileVersion)
	require.Empty(t, git.Versions)

	orphan, err := stackSvc.Read(2)
	require.NoError(t, err)
	require.Equal(t, 0, orphan.StackFileVersion)
	require.Empty(t, orphan.Versions)
}

// TestMigrateStackFileVersions_2_44_0_MissingAdditionalFile verifies that a stack whose
// entrypoint exists but whose additional file is missing is skipped entirely: it is left
// un-migrated, the base entrypoint stays intact, and NO partial v1 directory is created.
// A re-run must not accept the (absent) v1 either.
func TestMigrateStackFileVersions_2_44_0_MissingAdditionalFile(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	// Only the entrypoint is on disk; the declared additional file is missing.
	_, err := fileSvc.StoreStackFileFromBytes(stackFolder, "docker-compose.yml", []byte("main-content"))
	require.NoError(t, err)

	fileStack := &portainer.Stack{
		ID:              1,
		Name:            "partial-stack",
		Type:            portainer.DockerComposeStack,
		EntryPoint:      "docker-compose.yml",
		AdditionalFiles: []string{"override.yml"},
		ProjectPath:     fileSvc.GetStackProjectPath(stackFolder),
		CreationDate:    1234,
		CreatedBy:       "admin",
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	// The stack must be left un-migrated and consistent.
	skipped, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 0, skipped.StackFileVersion)
	require.Empty(t, skipped.Versions)
	require.Equal(t, fileSvc.GetStackProjectPath(stackFolder), skipped.ProjectPath)

	// No partial v1 directory may have been created, and base files stay intact.
	v1Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 1, "")
	exists, err := fileSvc.FileExists(v1Path)
	require.NoError(t, err)
	require.False(t, exists, "no partial v1 directory should be created when a file is missing")

	basePath := fileSvc.GetStackProjectPath(stackFolder)
	exists, err = fileSvc.FileExists(basePath + "/docker-compose.yml")
	require.NoError(t, err)
	require.True(t, exists, "base entrypoint must be left intact")

	// A re-run must remain a no-op (still un-migrated, still no v1 adopted).
	require.NoError(t, m.migrateStackFileVersions_2_44_0())
	again, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 0, again.StackFileVersion)
	require.Empty(t, again.Versions)
}

// TestMigrateStackFileVersions_2_44_0_HealFlatV1 covers the #30 bug: a stack that already
// carries StackFileVersion=1 in the DB but whose files still live ONLY at the flat base path
// compose/{id}/<files> (no v1/ directory) — as happens after EE↔CE edition switches, DB
// restores/imports or older builds. Such a stack was previously skipped by the blanket
// StackFileVersion>0 guard and left broken (file?version=1 → 500). The migration must now heal
// it: materialize v1 from the base files and repoint ProjectPath to the v1 folder.
func TestMigrateStackFileVersions_2_44_0_HealFlatV1(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	// Files live only at the flat base path; there is NO v1/ directory.
	_, err := fileSvc.StoreStackFileFromBytes(stackFolder, "docker-compose.yml", []byte("heal-content"))
	require.NoError(t, err)

	// The DB already claims version 1 (e.g. carried over from an EE build) while ProjectPath is
	// still the flat base folder — exactly the broken state from issue #30.
	fileStack := &portainer.Stack{
		ID:               1,
		Name:             "flat-versioned-stack",
		Type:             portainer.DockerComposeStack,
		EntryPoint:       "docker-compose.yml",
		ProjectPath:      fileSvc.GetStackProjectPath(stackFolder),
		StackFileVersion: 1,
		CreationDate:     1234,
		CreatedBy:        "admin",
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	migrated, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 1, migrated.StackFileVersion)
	require.Equal(t, fileSvc.GetStackProjectPathByVersion(stackFolder, 1, ""), migrated.ProjectPath)
	require.Len(t, migrated.Versions, 1)
	require.Equal(t, 1, migrated.Versions[0].Version)

	// v1 must now exist with identical content and be where retrieval-by-version looks.
	v1Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 1, "")
	got, err := fileSvc.GetFileContent(v1Path, "docker-compose.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("heal-content"), got)

	// The base copy must have been removed now that ProjectPath reads from v1.
	basePath := fileSvc.GetStackProjectPath(stackFolder)
	exists, err := fileSvc.FileExists(basePath + "/docker-compose.yml")
	require.NoError(t, err)
	require.False(t, exists, "old base entrypoint should be removed after heal")

	// Idempotency: a second run must be a no-op.
	require.NoError(t, m.migrateStackFileVersions_2_44_0())
	again, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 1, again.StackFileVersion)
	require.Len(t, again.Versions, 1)
}

// TestMigrateStackFileVersions_2_44_0_HealFlatV3 proves the generalization: a stack recorded at
// StackFileVersion=3 with its files only at the flat base path heals into v3 (not v1), with
// ProjectPath repointed to the v3 folder and the seeded history recorded at version 3.
func TestMigrateStackFileVersions_2_44_0_HealFlatV3(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	_, err := fileSvc.StoreStackFileFromBytes(stackFolder, "docker-compose.yml", []byte("v3-content"))
	require.NoError(t, err)
	_, err = fileSvc.StoreStackFileFromBytes(stackFolder, "override.yml", []byte("v3-override"))
	require.NoError(t, err)

	fileStack := &portainer.Stack{
		ID:               1,
		Name:             "flat-v3-stack",
		Type:             portainer.DockerSwarmStack,
		EntryPoint:       "docker-compose.yml",
		AdditionalFiles:  []string{"override.yml"},
		ProjectPath:      fileSvc.GetStackProjectPath(stackFolder),
		StackFileVersion: 3,
		CreationDate:     5678,
		CreatedBy:        "admin",
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	migrated, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 3, migrated.StackFileVersion, "must heal into the stack's own version, not v1")
	require.Equal(t, fileSvc.GetStackProjectPathByVersion(stackFolder, 3, ""), migrated.ProjectPath)
	require.Len(t, migrated.Versions, 1)
	require.Equal(t, 3, migrated.Versions[0].Version)

	// No v1 directory should be created — the stack heals into v3 exclusively.
	v1Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 1, "")
	exists, err := fileSvc.FileExists(v1Path)
	require.NoError(t, err)
	require.False(t, exists, "no v1 directory should be created for a v3 stack")

	// v3 holds both files with identical content.
	v3Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 3, "")
	got, err := fileSvc.GetFileContent(v3Path, "docker-compose.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("v3-content"), got)
	got, err = fileSvc.GetFileContent(v3Path, "override.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("v3-override"), got)
}

// TestMigrateStackFileVersions_2_44_0_CompleteExistingVersionNoDoubleWrite verifies the
// idempotency guarantee for the generalized guard: a stack already correctly versioned on disk
// (StackFileVersion=2, ProjectPath at v2, files present under v2/, no flat base copy) is left
// completely untouched — no double-write, ProjectPath unchanged, history unchanged.
func TestMigrateStackFileVersions_2_44_0_CompleteExistingVersionNoDoubleWrite(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	// Files already live under v2/ (the correct, already-migrated layout). No flat base copy.
	_, err := fileSvc.StoreStackFileFromBytesByVersion(stackFolder, "docker-compose.yml", 2, []byte("v2-content"))
	require.NoError(t, err)

	v2Path := fileSvc.GetStackProjectPathByVersion(stackFolder, 2, "")
	existing := []portainer.StackFileVersionInfo{{Version: 2, CreatedAt: 111, CreatedBy: "someone", Note: "real"}}
	fileStack := &portainer.Stack{
		ID:               1,
		Name:             "already-versioned-stack",
		Type:             portainer.DockerComposeStack,
		EntryPoint:       "docker-compose.yml",
		ProjectPath:      v2Path,
		StackFileVersion: 2,
		Versions:         existing,
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	after, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 2, after.StackFileVersion)
	require.Equal(t, v2Path, after.ProjectPath)
	// The real history must be preserved verbatim, not clobbered by a "migrated" entry.
	require.Len(t, after.Versions, 1)
	require.Equal(t, 2, after.Versions[0].Version)
	require.Equal(t, "real", after.Versions[0].Note)
	require.Equal(t, "someone", after.Versions[0].CreatedBy)

	// Content under v2 is unchanged.
	got, err := fileSvc.GetFileContent(v2Path, "docker-compose.yml")
	require.NoError(t, err)
	require.Equal(t, []byte("v2-content"), got)
}

// TestMigrateStackFileVersions_2_44_0_IncompleteExistingV1 verifies that a pre-existing but
// INCOMPLETE v1 directory (e.g. left by an interrupted earlier migration) is not adopted as
// authoritative: the stack stays un-migrated rather than being repointed to a partial v1.
func TestMigrateStackFileVersions_2_44_0_IncompleteExistingV1(t *testing.T) {
	t.Parallel()

	m, stackSvc, fileSvc := newStackVersionTestMigrator(t)

	stackFolder := "1"
	// Base files present.
	_, err := fileSvc.StoreStackFileFromBytes(stackFolder, "docker-compose.yml", []byte("main-content"))
	require.NoError(t, err)
	_, err = fileSvc.StoreStackFileFromBytes(stackFolder, "override.yml", []byte("override-content"))
	require.NoError(t, err)
	// A partial v1 exists: only the entrypoint was copied, the additional file is missing.
	_, err = fileSvc.StoreStackFileFromBytesByVersion(stackFolder, "docker-compose.yml", 1, []byte("main-content"))
	require.NoError(t, err)

	fileStack := &portainer.Stack{
		ID:              1,
		Name:            "interrupted-stack",
		Type:            portainer.DockerComposeStack,
		EntryPoint:      "docker-compose.yml",
		AdditionalFiles: []string{"override.yml"},
		ProjectPath:     fileSvc.GetStackProjectPath(stackFolder),
	}
	require.NoError(t, stackSvc.Create(fileStack))

	require.NoError(t, m.migrateStackFileVersions_2_44_0())

	// The incomplete v1 must NOT have been adopted; metadata stays un-migrated.
	skipped, err := stackSvc.Read(1)
	require.NoError(t, err)
	require.Equal(t, 0, skipped.StackFileVersion)
	require.Empty(t, skipped.Versions)
	require.Equal(t, fileSvc.GetStackProjectPath(stackFolder), skipped.ProjectPath)
}
