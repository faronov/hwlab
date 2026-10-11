package cc23xx

import (
	"errors"
	"fmt"
)

// Error codes of the CC23xx target layer. PROBE_GONE, TARGET_POWER_OFF and
// SWD_LINK_LOST are produced by the probe plugins; the rest here.
const (
	CodeSACIActive         = "SACI_ACTIVE"
	CodeAHBAPUnavailable   = "AHB_AP_UNAVAILABLE"
	CodeTargetSecured      = "TARGET_SECURED"
	CodePermissionDenied   = "FLASH_PERMISSION_DENIED"
	CodeCCFGInvalid        = "CCFG_INVALID"
	CodeSACITimeout        = "SACI_TIMEOUT"
	CodeNotCC23xx          = "NOT_CC23XX"
	CodeResetNotObserved   = "RESET_NOT_OBSERVED"
	CodeBootFailure        = "BOOT_FAILURE"
	CodeVerifyFailed       = "VERIFY_FAILED"
	CodeProtected          = "SECTOR_PROTECTED"
	CodeUnsafeRequest      = "UNSAFE_REQUEST"
	CodeSwdLost            = "SWD_LINK_LOST"
	CodeNotSupportedByPart = "UNSUPPORTED_BY_ATTACHED_HW"
	CodeFlashFailed        = "FLASH_COMMAND_FAILED"
)

// Error is a CC23xx failure with a stable machine-readable code.
type Error struct {
	Code string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

func errf(code string, cause error, format string, a ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...), Err: cause}
}

// CodeOf returns the error code of err, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
