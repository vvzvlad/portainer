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

// RestoreError reports that a failed recreate could not put the original
// container back into service, so the workload is left down. It is deliberately
// distinct from a plain recreate failure: the latter ends with the original
// container running again, this one ends with nothing running and needs a human.
type RestoreError struct {
	// ContainerID is the original container that could not be restored.
	ContainerID string
	// Name is the original container name as reported by Docker (e.g. "/web").
	Name string
	// Cause is the recreate failure that triggered the restore.
	Cause error
	// Errs is everything that went wrong while restoring.
	Errs []error
}

func (e *RestoreError) Error() string {
	reasons := make([]string, 0, len(e.Errs))
	for _, err := range e.Errs {
		reasons = append(reasons, err.Error())
	}

	return fmt.Sprintf("recreate failed and the original container %s (%s) was NOT restored, it is left down: %s (restore errors: %s)",
		strings.TrimPrefix(e.Name, "/"), e.ContainerID, e.Cause, strings.Join(reasons, "; "))
}

// Unwrap returns the failure that triggered the restore, so callers that already
// match on the recreate error with errors.Is/errors.As keep matching once it is
// wrapped in a RestoreError.
func (e *RestoreError) Unwrap() error { return e.Cause }
