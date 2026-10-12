package jlinknativeplugin

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
)

// Message prefixes callers can match. A vanished probe carries both the
// generic hwlab marker (DEVICE_GONE, matched by the application error layer)
// and the native class.
const (
	DeviceGone      = "DEVICE_GONE"
	Busy            = "BUSY"
	UnsupportedByHW = "UNSUPPORTED_BY_ATTACHED_HW"
)

func hasPrefix(err error, p string, c codes.Code) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == c && strings.HasPrefix(st.Message(), p)
}

// IsDeviceGone classifies an error returned by the plugin.
func IsDeviceGone(err error) bool { return hasPrefix(err, DeviceGone, codes.Unavailable) }

// IsBusy classifies an error returned by the plugin.
func IsBusy(err error) bool { return hasPrefix(err, Busy, codes.FailedPrecondition) }

// IsUnsupportedByHW classifies an error returned by the plugin.
func IsUnsupportedByHW(err error) bool {
	return hasPrefix(err, UnsupportedByHW, codes.FailedPrecondition)
}

// classify turns a transport, protocol, SWD, ADIv5 or Cortex-M error into a
// gRPC status whose message starts with the failure class.
func classify(err error) error {
	if primary, cleanup := cortexm.PrimaryError(err); cleanup {
		st := status.Convert(classify(primary))
		return status.Error(st.Code(), st.Message()+strings.TrimPrefix(err.Error(), primary.Error()))
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if c, ok := jlerr.ClassOf(err); ok {
		switch c {
		case jlerr.ProbeGone:
			return status.Error(codes.Unavailable, DeviceGone+": "+err.Error())
		case jlerr.ProbeBusy:
			return status.Error(codes.FailedPrecondition, Busy+": "+err.Error())
		case jlerr.USBTimeout:
			return status.Error(codes.DeadlineExceeded, err.Error())
		case jlerr.USBPermission:
			return status.Error(codes.PermissionDenied, err.Error())
		case jlerr.UnsupportedProbeCommand:
			return status.Error(codes.Unimplemented, err.Error())
		case jlerr.TargetPowerOff, jlerr.SWDNoACK, jlerr.TargetUnavailable:
			return status.Error(codes.Unavailable, err.Error())
		default:
			return status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	switch {
	case errors.Is(err, usb.ErrDisconnected):
		return status.Error(codes.Unavailable, DeviceGone+": "+string(jlerr.ProbeGone)+": "+err.Error())
	case errors.Is(err, usb.ErrBusy):
		return status.Error(codes.FailedPrecondition, Busy+": "+string(jlerr.ProbeBusy)+": "+err.Error())
	case errors.Is(err, cortexm.ErrAbsent):
		return status.Error(codes.FailedPrecondition, UnsupportedByHW+": "+err.Error())
	case errors.Is(err, usb.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.Is(err, usb.ErrTimeout), errors.Is(err, cortexm.ErrTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}
