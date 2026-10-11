package app

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/usb"
)

// ErrorInfo is the machine-readable form of a failure. Frontends render it;
// they must not parse Message.
type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint,omitempty"`
}

// ErrorCodes lists every code Classify can return, with the usual remedy.
var ErrorCodes = map[string]string{
	apperr.InvalidArgument:         "fix the arguments; nothing was sent to the hardware",
	apperr.BadAddress:              "address or range is outside what the target/flash accepts",
	apperr.NotFound:                "plugin, device, session or artifact does not exist; list them first",
	apperr.Ambiguous:               "several devices match; select one by USB serial",
	apperr.Busy:                    "another process or session owns the probe; close it and retry",
	apperr.DeviceGone:              "the probe disappeared; reconnect it, list devices, reopen",
	apperr.Timeout:                 "the operation did not finish in time; retry or raise the timeout",
	apperr.Cancelled:               "the request was cancelled by the client",
	apperr.ConfirmationRequired:    "ask the user, then repeat with confirm=true",
	apperr.PermissionDenied:        "a plugin permission, token or backup requirement was not met, or the OS denied access",
	apperr.PathDenied:              "file paths over MCP must stay inside the hwlab artifacts directory",
	apperr.ProtectedTarget:         "the target is read-protected or write-protected; hwlab never changes protection",
	apperr.Unsupported:             "the capability is not implemented by this plugin or target",
	apperr.UnsupportedByAttachedHW: "the attached hardware cannot do this",
	apperr.SWDNoACK:                "no answer on SWD: check wiring, target power and reset",
	apperr.SWDWait:                 "target answered WAIT repeatedly; lower the clock or retry",
	apperr.SWDFault:                "target answered FAULT; a sticky error was set",
	apperr.SWDProtocol:             "SWD parity/framing error; lower the clock, check wiring",
	apperr.TargetNotHalted:         "halt the core first",
	apperr.VerifyFailed:            "read-back differs from what was written",
	apperr.PreconditionFailed:      "a required precondition (such as a readable verified backup or a halted core) is not met",
	apperr.Internal:                "unexpected failure; see the message",
}

// Classify maps any service error to an ErrorInfo. Plugin failures arrive as
// gRPC statuses with message prefixes, so it checks, in order: explicit
// codes, in-process sentinels, gRPC codes, then the sentinel texts the
// shared ARM/USB layers produce.
func Classify(err error) ErrorInfo {
	if err == nil {
		return ErrorInfo{}
	}
	code := classifyCode(err)
	return ErrorInfo{Code: code, Message: cleanMessage(err), Retryable: retryable(code), Hint: ErrorCodes[code]}
}

func retryable(code string) bool {
	switch code {
	case apperr.Busy, apperr.Timeout, apperr.SWDWait, apperr.SWDProtocol, apperr.DeviceGone:
		return true
	}
	return false
}

func classifyCode(err error) string {
	if c, ok := apperr.CodeOf(err); ok {
		return c
	}
	switch {
	case policy.IsConfirmationRequired(err):
		return apperr.ConfirmationRequired
	case errors.Is(err, context.Canceled):
		return apperr.Cancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, usb.ErrTimeout), errors.Is(err, cortexm.ErrTimeout):
		return apperr.Timeout
	case errors.Is(err, usb.ErrDisconnected):
		return apperr.DeviceGone
	case errors.Is(err, usb.ErrBusy):
		return apperr.Busy
	case errors.Is(err, usb.ErrPermission):
		return apperr.PermissionDenied
	case errors.Is(err, adiv5.ErrNoACK):
		return apperr.SWDNoACK
	case errors.Is(err, adiv5.ErrWait):
		return apperr.SWDWait
	case errors.Is(err, adiv5.ErrFault):
		return apperr.SWDFault
	case errors.Is(err, adiv5.ErrProtocol):
		return apperr.SWDProtocol
	case errors.Is(err, cortexm.ErrNotHalted):
		return apperr.TargetNotHalted
	case errors.Is(err, cortexm.ErrVerify):
		return apperr.VerifyFailed
	case errors.Is(err, errNoSuchSession):
		return apperr.NotFound
	}
	msg := rawMessage(err)
	if st, ok := status.FromError(err); ok {
		m := st.Message()
		switch {
		case st.Code() == codes.Unavailable && strings.Contains(m, "DEVICE_GONE"):
			return apperr.DeviceGone
		case st.Code() == codes.FailedPrecondition && strings.Contains(m, "BUSY:"),
			strings.Contains(m, usb.ErrBusy.Error()):
			return apperr.Busy
		case strings.Contains(m, usb.ErrDisconnected.Error()):
			return apperr.DeviceGone
		case strings.Contains(m, "UNSUPPORTED_BY_ATTACHED_HW"):
			return apperr.UnsupportedByAttachedHW
		case st.Code() == codes.PermissionDenied && strings.Contains(m, "protected"):
			return apperr.ProtectedTarget
		}
		switch st.Code() {
		case codes.InvalidArgument:
			return apperr.InvalidArgument
		case codes.OutOfRange:
			return apperr.BadAddress
		case codes.NotFound:
			return apperr.NotFound
		case codes.PermissionDenied:
			return apperr.PermissionDenied
		case codes.DeadlineExceeded:
			return apperr.Timeout
		case codes.Canceled:
			return apperr.Cancelled
		case codes.Unimplemented:
			return apperr.Unsupported
		case codes.DataLoss:
			return apperr.VerifyFailed
		case codes.FailedPrecondition:
			return apperr.PreconditionFailed
		case codes.Unavailable:
			return apperr.DeviceGone
		}
	}
	// Errors that crossed a process boundary as text only.
	switch {
	case strings.Contains(msg, adiv5.ErrNoACK.Error()):
		return apperr.SWDNoACK
	case strings.Contains(msg, adiv5.ErrWait.Error()):
		return apperr.SWDWait
	case strings.Contains(msg, adiv5.ErrFault.Error()):
		return apperr.SWDFault
	case strings.Contains(msg, adiv5.ErrProtocol.Error()):
		return apperr.SWDProtocol
	case strings.Contains(msg, cortexm.ErrNotHalted.Error()):
		return apperr.TargetNotHalted
	case strings.Contains(msg, usb.ErrDisconnected.Error()):
		return apperr.DeviceGone
	case strings.Contains(msg, usb.ErrBusy.Error()):
		return apperr.Busy
	}
	return apperr.Internal
}

func rawMessage(err error) string {
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return st.Message()
	}
	return err.Error()
}

// cleanMessage drops transport framing and bounds the length; it never
// includes stack traces.
func cleanMessage(err error) string {
	m := rawMessage(err)
	m = rpcPrefix.ReplaceAllString(m, "")
	if i := strings.Index(m, "\ngoroutine "); i >= 0 {
		m = m[:i]
	}
	if len(m) > 2000 {
		m = m[:2000] + "..."
	}
	return strings.TrimSpace(m)
}

var rpcPrefix = regexp.MustCompile(`rpc error: code = [A-Za-z]+ desc = `)
