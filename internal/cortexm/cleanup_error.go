package cortexm

import "fmt"

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

func PrimaryError(err error) error {
	for {
		e, ok := err.(*cleanupError)
		if !ok {
			return err
		}
		err = e.primary
	}
}
