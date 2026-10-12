package cortexm

import (
	"errors"
	"fmt"
)

type cleanupError struct{ primary, cleanup error }

func (e *cleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup failed: %v", e.primary, e.cleanup)
}

func (e *cleanupError) Unwrap() []error { return []error{e.primary, e.cleanup} }

// WithCleanupError preserves the operation error separately for classification,
// while errors.Is/As can still inspect both failures.
func WithCleanupError(primary, cleanup error) error {
	if primary == nil {
		return cleanup
	}
	if cleanup == nil {
		return primary
	}
	return &cleanupError{primary: primary, cleanup: cleanup}
}

// PrimaryError returns the operation error and whether cleanup failures were
// separated from it. The flag avoids comparing error identities to detect this.
func PrimaryError(err error) (error, bool) {
	separated := false
	for {
		var e *cleanupError
		if !errors.As(err, &e) {
			return err, separated
		}
		separated = true
		err = e.primary
	}
}
