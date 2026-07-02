package migrator

import (
	"os"
	"path/filepath"
	"strconv"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/filesystem"

	"github.com/rs/zerolog/log"
)

// migrateStackFileVersions_2_44_0 introduces the on-disk file version history for existing
// file-based (non-git) Compose/Swarm stacks. For each such stack it ensures the entrypoint and
// every additional file live under a versioned folder compose/{id}/v{N}/<files>, repoints
// ProjectPath to that folder, records StackFileVersion=N and seeds the append-only Versions
// history with a single "migrated" entry.
//
// The version N to ensure on disk is the stack's CURRENT DB StackFileVersion, or 1 for a fresh,
// never-versioned stack (StackFileVersion==0). This is what heals stacks that already carry
// StackFileVersion>0 in the DB (from EE↔CE edition switches, DB restores/imports or older
// builds) yet still have their files sitting ONLY at the flat base path compose/{id}/<files>
// with no v{N} directory: GET /stacks/{id}/file?version=N would 500 for those because it reads
// compose/{id}/v{N}/<files>. The migration materializes the missing v{N} folder from the base
// files.
//
// The migration is idempotent (a COMPLETE current v{N} folder on disk is adopted without moving
// files or touching data) and resilient: git/kubernetes stacks and stacks whose base files are
// missing on disk are logged and skipped without failing the whole migration. It never writes a
// partial v{N} (all files are read up-front) and never adopts an incomplete pre-existing v{N}.
func (m *Migrator) migrateStackFileVersions_2_44_0() error {
	log.Info().Msg("migrating file-based stacks to versioned file storage")

	stacks, err := m.stackService.ReadAll()
	if err != nil {
		return err
	}

	for i := range stacks {
		stack := stacks[i]

		// Only file-based (non-git) Compose/Swarm stacks get a file version history.
		if stack.WorkflowID != 0 {
			continue
		}
		if stack.Type != portainer.DockerComposeStack && stack.Type != portainer.DockerSwarmStack {
			continue
		}
		if stack.ProjectPath == "" || stack.EntryPoint == "" {
			continue
		}

		stackID := strconv.Itoa(int(stack.ID))

		// The version we must guarantee exists on disk. A stack already carrying a DB file
		// version targets THAT version (it may have been recorded by an EE↔CE edition switch,
		// a DB restore/import or an older build without the v-dir ever being created on disk);
		// a fresh, never-versioned stack targets v1.
		v := stack.StackFileVersion
		if v == 0 {
			v = 1
		}

		vDir := m.fileService.GetStackProjectPathByVersion(stackID, v, "")

		// The complete expected file set for this stack: entrypoint + every additional file.
		fileNames := append([]string{stack.EntryPoint}, stack.AdditionalFiles...)

		if exists, err := m.fileService.FileExists(vDir); err != nil {
			log.Warn().Err(err).Int("stack_id", int(stack.ID)).Msg("unable to check stack version directory; skipping")
			continue
		} else if exists {
			// v-dir already present (e.g. a normally-versioned stack, or one left by an
			// interrupted earlier migration). Only treat it as authoritative if it holds the
			// FULL expected file set; a partial v-dir must not be adopted, or we'd repoint the
			// stack to an incomplete directory.
			complete, err := m.stackVersionDirComplete(vDir, fileNames)
			if err != nil {
				log.Warn().Err(err).Int("stack_id", int(stack.ID)).Msg("unable to verify existing stack version directory; skipping")
				continue
			}
			if !complete {
				log.Warn().Int("stack_id", int(stack.ID)).Str("version_dir", vDir).Msg("existing stack version directory is incomplete; skipping version migration")
				continue
			}

			// Complete v-dir already on disk: nothing to move. Repoint metadata / seed the
			// history only if they aren't already consistent, so an already-migrated stack is a
			// true no-op (no double-write, no data change). We do NOT remove the base files here
			// because ProjectPath already reads from the v-dir and we never touch files in this
			// branch.
			if stack.ProjectPath == vDir && stack.StackFileVersion == v && len(stack.Versions) > 0 {
				continue
			}
			m.seedStackVersionMetadata(&stack, vDir, v)
			if err := m.stackService.Update(stack.ID, &stack); err != nil {
				return err
			}
			continue
		}

		// v-dir missing on disk: materialize it from the flat base files. This is the shared
		// heal path used by both the fresh (v==0→v1) migration and the repair of a stack whose
		// DB StackFileVersion>0 but whose v{N} directory was never created (the file?version=N
		// 500 bug). If the base files are also missing the stack is left untouched.
		materialized, err := m.materializeStackVersion(&stack, stackID, v, vDir, fileNames)
		if err != nil {
			return err
		}
		if !materialized {
			continue
		}

		if err := m.stackService.Update(stack.ID, &stack); err != nil {
			return err
		}
	}

	return nil
}

// materializeStackVersion reads the stack's current files from the flat base location
// (compose/{id}/<files>) and writes them into the versioned folder compose/{id}/v{v}, then
// repoints the stack metadata to that folder. It is the single materialize/heal path shared by
// both the fresh (StackFileVersion==0 → v1) migration and the repair of a stack whose DB
// StackFileVersion>0 but whose v-dir is missing on disk.
//
// It is all-or-nothing: every file is read up-front and only written once the whole set is
// present. If any base file is missing/unreadable it writes NOTHING, leaves the base files
// intact and returns false so the caller skips the stack (never creating a partial v{v} that a
// re-run could later adopt). The base-level copies are removed only after ProjectPath has been
// repointed to the v-dir, so a deploy that still reads the base path is never broken mid-way.
func (m *Migrator) materializeStackVersion(stack *portainer.Stack, stackID string, v int, vDir string, fileNames []string) (bool, error) {
	basePath := m.fileService.GetStackProjectPath(stackID)

	// Pre-flight: read the entrypoint and every additional file into memory BEFORE writing
	// anything to v{v}. If any file is missing/unreadable we skip the whole stack (leaving the
	// base files intact) rather than create a partial v{v} that a re-run could later adopt.
	contents := make(map[string][]byte, len(fileNames))
	for _, fileName := range fileNames {
		content, err := m.fileService.GetFileContent(basePath, fileName)
		if err != nil {
			log.Warn().Err(err).Int("stack_id", int(stack.ID)).Str("file", fileName).Str("base_path", basePath).Msg("stack file missing or unreadable on disk; skipping version migration")
			return false, nil
		}

		contents[fileName] = content
	}

	// All files read successfully; now write them (all-or-nothing) into the v{v} folder.
	for _, fileName := range fileNames {
		if _, err := m.fileService.StoreStackFileFromBytesByVersion(stackID, fileName, v, contents[fileName]); err != nil {
			return false, err
		}
	}

	// Repoint metadata to the v-dir and seed the history. Done before removing the base copies
	// so we only ever delete the base-level files once the stack actually reads from the v-dir.
	m.seedStackVersionMetadata(stack, vDir, v)

	// Remove the old base-level copies now that they live under v{v} and ProjectPath points there.
	for _, fileName := range fileNames {
		// Clamp the path to the trusted base root, consistent with the clamped read
		// (GetFileContent) and write (StoreStackFileFromBytesByVersion) above, so an
		// attacker-influenceable fileName (stack.EntryPoint/AdditionalFiles via DB
		// restore/import) cannot escape basePath in this file-deleting path.
		oldPath := filesystem.JoinPaths(basePath, fileName)
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			log.Warn().Err(err).Int("stack_id", int(stack.ID)).Str("file", fileName).Msg("unable to remove old stack file after version migration")
		}
	}

	return true, nil
}

// stackVersionDirComplete reports whether dir holds the stack's full expected file set
// (entrypoint + every additional file). Used to reject an incomplete v-dir left behind by an
// interrupted migration so it is never adopted as authoritative.
func (m *Migrator) stackVersionDirComplete(dir string, fileNames []string) (bool, error) {
	for _, fileName := range fileNames {
		exists, err := m.fileService.FileExists(filepath.Join(dir, fileName))
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}

	return true, nil
}

// seedStackVersionMetadata repoints a stack to its v{v} folder, records StackFileVersion=v and
// seeds the append-only version history with a single "migrated" entry when it is still empty.
// An existing history is preserved (never clobbered), so re-pointing an already-versioned stack
// keeps its real version records intact.
func (m *Migrator) seedStackVersionMetadata(stack *portainer.Stack, vDir string, v int) {
	createdAt := stack.CreationDate
	if createdAt == 0 {
		createdAt = stack.UpdateDate
	}

	stack.ProjectPath = vDir
	stack.StackFileVersion = v
	if len(stack.Versions) == 0 {
		stack.Versions = []portainer.StackFileVersionInfo{{
			Version:   v,
			CreatedAt: createdAt,
			CreatedBy: stack.CreatedBy,
			Note:      "migrated",
		}}
	}
}
