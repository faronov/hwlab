package pluginsdk

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DebugCleanupNeeded distinguishes operational failures from rejected requests.
// Rejecting an invalid request must not discard triggers installed earlier.
func DebugCleanupNeeded(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied:
		return false
	}
	return true
}
