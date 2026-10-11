package cmsisdapplugin

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
)

// Message prefixes of the two probe-level conditions callers must be able to
// tell apart from an ordinary failure.
const (
	// DeviceGone marks a call that failed because the probe vanished
	// (unplugged, or its USB port reset and not yet back).
	DeviceGone = "DEVICE_GONE"
	// Busy marks a probe whose debug interface is already claimed by another
	// process or driver; hwlab never detaches a driver to get at it.
	Busy = "BUSY"
	// UnsupportedByHW marks a call that cannot work because the attached
	// core does not implement the resource (no watchpoint comparators, no
	// VECTRESET). It is a fact about the target, not a gap in hwlab.
	UnsupportedByHW = "UNSUPPORTED_BY_ATTACHED_HW"
)

func goneErr(msg string) error { return status.Error(codes.Unavailable, DeviceGone+": "+msg) }

func busyErr(msg string) error { return status.Error(codes.FailedPrecondition, Busy+": "+msg) }

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

// classify turns a transport, ADIv5 or Cortex-M error into a gRPC status.
func classify(err error) error {
	if primary := cortexm.PrimaryError(err); primary != err {
		st := status.Convert(classify(primary))
		return status.Error(st.Code(), st.Message()+strings.TrimPrefix(err.Error(), primary.Error()))
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, usb.ErrDisconnected):
		return goneErr(err.Error())
	case errors.Is(err, usb.ErrBusy):
		return busyErr(err.Error())
	case errors.Is(err, cortexm.ErrAbsent):
		return status.Error(codes.FailedPrecondition, UnsupportedByHW+": "+err.Error())
	case errors.Is(err, usb.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.Is(err, usb.ErrTimeout), errors.Is(err, cortexm.ErrTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}
