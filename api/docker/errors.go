package docker

import (
	"errors"
	"fmt"
	"strings"
)

// Docker errors
var (
	ErrUnableToPingEndpoint = errors.New("Unable to communicate with the environment")
)

// RestoreError reports that a failed recreate could not fully put the original
// container back the way it was. It is deliberately distinct from a plain
// recreate failure: the latter ends with the workload serving exactly as before
// and needs nobody, this one always needs a human.
//
// OriginalRunning splits it into the two outcomes an operator has to act on
// differently, and it is the field to branch on — the wording of Error() is not
// machine-readable:
//
//   - false: nothing is running. The original was torn down and could not be
//     started again, which is an outage.
//   - true: the original is serving again, but not as it was — it may still carry
//     the "-old" name (so a stack peer resolving it by name does not find it, and
//     the next auto-update pass would recreate the wrong container) or be missing
//     a network. Degraded, and it stays degraded until someone fixes it.
//
// Recreate builds one whenever the restore did not fully land, and NOT for a
// recreate that merely left something untidy without touching the original (a
// new container that could not be removed while everything else came back).
type RestoreError struct {
	// ContainerID is the original container that could not be restored.
	ContainerID string
	// Name is the original container name as reported by Docker (e.g. "/web").
	Name string
	// Cause is the recreate failure that triggered the restore. It is never nil:
	// Recreate only wraps a failure it is already returning.
	Cause error
	// Errs is everything that went wrong while restoring, including a new container
	// that could not be removed when one was left behind (holding the original
	// name, it is a common reason the rename back could not land).
	Errs []error
	// OriginalRunning reports whether the original container was observed running
	// again at the end of the restore. See the type comment: it is what tells an
	// outage apart from a degraded-but-serving workload.
	OriginalRunning bool
}

func (e *RestoreError) Error() string {
	reasons := make([]string, 0, len(e.Errs))
	for _, err := range e.Errs {
		reasons = append(reasons, err.Error())
	}

	name := strings.TrimPrefix(e.Name, "/")

	if e.OriginalRunning {
		return fmt.Sprintf("recreate failed and the original container %s (%s) is running again, but the restore was incomplete, these did not roll back: %s (recreate failure: %s)",
			name, e.ContainerID, strings.Join(reasons, "; "), e.Cause)
	}

	return fmt.Sprintf("recreate failed and the original container %s (%s) was NOT restored, it is left down: %s (restore errors: %s)",
		name, e.ContainerID, e.Cause, strings.Join(reasons, "; "))
}

// Unwrap returns the failure that triggered the restore, so callers that already
// match on the recreate error with errors.Is/errors.As keep matching once it is
// wrapped in a RestoreError.
func (e *RestoreError) Unwrap() error { return e.Cause }
