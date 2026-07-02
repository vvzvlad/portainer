package migrator

import (
	"os"
	"path/filepath"
	"strconv"

	portainer "github.com/portainer/portainer/api"

	"github.com/rs/zerolog/log"
)

// migrateStackFileVersions_2_44_0 introduces the on-disk file version history for existing
// file-based (non-git) Compose/Swarm stacks. For each such stack it moves the entrypoint and
// every additional file from compose/{id}/<files> into compose/{id}/v1/<files>, repoints
// ProjectPath to the v1 folder, sets StackFileVersion=1 and seeds the append-only Versions
// history with a single "migrated" entry.
//
// The migration is idempotent (it skips stacks that already have StackFileVersion>0 or a
// COMPLETE v1 folder on disk) and resilient: git/kubernetes stacks and stacks whose files are
// missing on disk are logged and skipped without failing the whole migration. It never writes
// a partial v1 (all files are read up-front) and never adopts an incomplete pre-existing v1.
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

		// Idempotency: already migrated.
		if stack.StackFileVersion > 0 {
			continue
		}

		stackID := strconv.Itoa(int(stack.ID))
		v1Dir := m.fileService.GetStackProjectPathByVersion(stackID, 1, "")

		// The complete expected file set for this stack: entrypoint + every additional file.
		fileNames := append([]string{stack.EntryPoint}, stack.AdditionalFiles...)

		if exists, err := m.fileService.FileExists(v1Dir); err != nil {
			log.Warn().Err(err).Int("stack_id", int(stack.ID)).Msg("unable to check stack v1 directory; skipping")
			continue
		} else if exists {
			// v1 folder already present (e.g. from an interrupted earlier migration). Only
			// treat it as authoritative if it holds the FULL expected file set; a partial v1
			// must not be adopted, or we'd repoint the stack to an incomplete directory.
			complete, err := m.stackVersionDirComplete(v1Dir, fileNames)
			if err != nil {
				log.Warn().Err(err).Int("stack_id", int(stack.ID)).Msg("unable to verify existing stack v1 directory; skipping")
				continue
			}
			if !complete {
				log.Warn().Int("stack_id", int(stack.ID)).Str("v1_dir", v1Dir).Msg("existing stack v1 directory is incomplete; skipping version migration")
				continue
			}

			// Complete v1: repoint metadata if needed but don't move files.
			m.seedStackVersionMetadata(&stack, v1Dir)
			if err := m.stackService.Update(stack.ID, &stack); err != nil {
				return err
			}
			continue
		}

		// Pre-flight: read the entrypoint and every additional file into memory BEFORE writing
		// anything to v1. If any file is missing/unreadable we skip the whole stack (leaving the
		// base files intact) rather than create a partial v1 that a re-run could later adopt.
		contents := make(map[string][]byte, len(fileNames))
		missing := false
		for _, fileName := range fileNames {
			content, err := m.fileService.GetFileContent(stack.ProjectPath, fileName)
			if err != nil {
				log.Warn().Err(err).Int("stack_id", int(stack.ID)).Str("file", fileName).Str("project_path", stack.ProjectPath).Msg("stack file missing or unreadable on disk; skipping version migration")
				missing = true
				break
			}

			contents[fileName] = content
		}

		if missing {
			continue
		}

		// All files read successfully; now move them (all-or-nothing) into the v1 folder.
		for _, fileName := range fileNames {
			if _, err := m.fileService.StoreStackFileFromBytesByVersion(stackID, fileName, 1, contents[fileName]); err != nil {
				return err
			}
		}

		// Remove the old base-level copies now that they live under v1.
		for _, fileName := range fileNames {
			oldPath := filepath.Join(stack.ProjectPath, fileName)
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				log.Warn().Err(err).Int("stack_id", int(stack.ID)).Str("file", fileName).Msg("unable to remove old stack file after version migration")
			}
		}

		m.seedStackVersionMetadata(&stack, v1Dir)

		if err := m.stackService.Update(stack.ID, &stack); err != nil {
			return err
		}
	}

	return nil
}

// stackVersionDirComplete reports whether dir holds the stack's full expected file set
// (entrypoint + every additional file). Used to reject an incomplete v1 left behind by an
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

// seedStackVersionMetadata repoints a stack to its v1 folder and seeds the version history.
func (m *Migrator) seedStackVersionMetadata(stack *portainer.Stack, v1Dir string) {
	createdAt := stack.CreationDate
	if createdAt == 0 {
		createdAt = stack.UpdateDate
	}

	stack.ProjectPath = v1Dir
	stack.StackFileVersion = 1
	stack.Versions = []portainer.StackFileVersionInfo{{
		Version:   1,
		CreatedAt: createdAt,
		CreatedBy: stack.CreatedBy,
		Note:      "migrated",
	}}
}
