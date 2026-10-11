// Package apperr gives service-layer errors a stable, machine-readable code.
// Frontends (CLI, MCP) render the code; they never parse messages.
package apperr

import (
	"errors"
	"fmt"
)

// Codes returned to clients. Probe and target conditions that plugins
// report as gRPC statuses or message prefixes are mapped onto the same set
// by app.Classify.
const (
	InvalidArgument         = "INVALID_ARGUMENT"
	BadAddress              = "BAD_ADDRESS"
	NotFound                = "NOT_FOUND"
	Ambiguous               = "AMBIGUOUS_DEVICE"
	Busy                    = "BUSY"
	DeviceGone              = "DEVICE_GONE"
	Timeout                 = "TIMEOUT"
	Cancelled               = "CANCELLED"
	ConfirmationRequired    = "CONFIRMATION_REQUIRED"
	PermissionDenied        = "PERMISSION_DENIED"
	PathDenied              = "PATH_DENIED"
	ProtectedTarget         = "PROTECTED_TARGET"
	Unsupported             = "UNSUPPORTED"
	UnsupportedByAttachedHW = "UNSUPPORTED_BY_ATTACHED_HW"
	SWDNoACK                = "SWD_NO_ACK"
	SWDWait                 = "SWD_WAIT"
	SWDFault                = "SWD_FAULT"
	SWDProtocol             = "SWD_PROTOCOL"
	TargetNotHalted         = "TARGET_NOT_HALTED"
	VerifyFailed            = "VERIFY_FAILED"
	PreconditionFailed      = "PRECONDITION_FAILED"
	Internal                = "INTERNAL"
)

// Error carries a code next to the human readable message.
type Error struct {
	Code string
	Msg  string
	Err  error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Newf creates a coded error.
func Newf(code, format string, a ...any) error {
	err := fmt.Errorf(format, a...)
	return &Error{Code: code, Msg: err.Error(), Err: errors.Unwrap(err)}
}

// Wrap attaches a code to err without changing its message.
func Wrap(code string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Msg: err.Error(), Err: err}
}

// CodeOf returns the code attached to err, if any.
func CodeOf(err error) (string, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}
