package stacks

import (
	"strconv"

	portainer "github.com/portainer/portainer/api"

	"github.com/rs/zerolog/log"
)

// maxStackFileVersions caps the number of on-disk file versions kept per file-based stack.
// Once the history exceeds this cap, the oldest versions are pruned from disk and history.
const maxStackFileVersions = 20

// snapshotStackFilesToVersion writes every stack file into the v{version} folder of the given
// stack. filesContent maps a file name (relative to the stack root) to its content and must
// include the entrypoint (multi-file: all AdditionalFiles are copied too). It returns the
// versioned project path, which the caller must assign to stack.ProjectPath (the underlying
// StoreStackFileFromBytesByVersion returns the base path, not the version path).
func snapshotStackFilesToVersion(fileService portainer.FileService, stackID string, version int, filesContent map[string][]byte) (string, error) {
	for fileName, content := range filesContent {
		if _, err := fileService.StoreStackFileFromBytesByVersion(stackID, fileName, version, content); err != nil {
			return "", err
		}
	}

	return fileService.GetStackProjectPathByVersion(stackID, version, ""), nil
}

// applyRetention trims the oldest entries from stack.Versions once the history exceeds
// maxVersions and returns the version numbers whose on-disk v{N} directories should be
// deleted. It never selects the currently deployed version for deletion, and the in-memory
// trim is kept consistent with what is actually pruned (an entry whose directory is
// intentionally kept stays in stack.Versions).
//
// The physical directory deletion is deliberately NOT performed here: the caller must run
// it (see (*Handler).pruneStackFileVersionDirs) only AFTER the enclosing DB transaction has
// committed. Deleting inside the transaction would leave dangling history if the tx later
// rolled back (the persisted Versions[] would still reference now-deleted directories).
func applyRetention(stack *portainer.Stack, maxVersions int) []int {
	if maxVersions <= 0 || len(stack.Versions) <= maxVersions {
		return nil
	}

	removeCount := len(stack.Versions) - maxVersions

	pruned := make([]int, 0, removeCount)
	kept := make([]portainer.StackFileVersionInfo, 0, len(stack.Versions))
	for i, info := range stack.Versions {
		// Only the oldest removeCount entries are eligible for pruning, and never the
		// currently deployed version (safety guard). Everything else is kept.
		if i < removeCount && info.Version != stack.StackFileVersion {
			pruned = append(pruned, info.Version)
			continue
		}

		kept = append(kept, info)
	}

	stack.Versions = kept

	return pruned
}

// pruneStackFileVersionDirs physically deletes the given file-version directories for a stack.
// It is best-effort: a failed deletion is logged and does not fail the request. This MUST be
// called only after the DB transaction that trimmed stack.Versions has committed successfully,
// so a rolled-back transaction never leaves history referencing deleted directories.
func (handler *Handler) pruneStackFileVersionDirs(stackID portainer.StackID, versions []int) {
	stackFolder := strconv.Itoa(int(stackID))
	for _, v := range versions {
		dir := handler.FileService.GetStackProjectPathByVersion(stackFolder, v, "")
		if err := handler.FileService.RemoveDirectory(dir); err != nil {
			log.Warn().Err(err).Int("version", v).Int("stack_id", int(stackID)).Msg("unable to remove old stack file version directory")
		}
	}
}

// collectStackFilesContent builds the file-name -> content map for a version snapshot: the
// entrypoint is taken from entryPointContent (the new/target content), while every additional
// file is read from srcProjectPath so the whole file set is carried into the new version.
func collectStackFilesContent(fileService portainer.FileService, stack *portainer.Stack, srcProjectPath string, entryPointContent []byte) (map[string][]byte, error) {
	filesContent := map[string][]byte{
		stack.EntryPoint: entryPointContent,
	}

	for _, additionalFile := range stack.AdditionalFiles {
		content, err := fileService.GetFileContent(srcProjectPath, additionalFile)
		if err != nil {
			return nil, err
		}

		filesContent[additionalFile] = content
	}

	return filesContent, nil
}
