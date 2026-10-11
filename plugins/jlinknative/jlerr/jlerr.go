// Package jlerr holds the error classes of the native J-Link backend. Every
// layer (USB transport, probe protocol, SWD/JTAG wire, plugin) reports a
// failure with exactly one class so that a USB timeout, a protocol violation
// and a target that does not answer are never collapsed into one "timeout".
package jlerr

import "errors"

// Class names one failure mode.
type Class string

// The classes.
const (
	ProbeGone               Class = "PROBE_GONE"
	ProbeBusy               Class = "PROBE_BUSY"
	USBTimeout              Class = "USB_TIMEOUT"
	USBStall                Class = "USB_STALL"
	USBPermission           Class = "USB_PERMISSION"
	ProtocolError           Class = "PROTOCOL_ERROR"
	TargetPowerOff          Class = "TARGET_POWER_OFF"
	SWDNoACK                Class = "SWD_NO_ACK"
	SWDWait                 Class = "SWD_WAIT"
	SWDFault                Class = "SWD_FAULT"
	TargetUnavailable       Class = "TARGET_UNAVAILABLE"
	UnsupportedProbeCommand Class = "UNSUPPORTED_PROBE_COMMAND"
	WrongProbe              Class = "WRONG_PROBE"
	ProbeBootloader         Class = "PROBE_IN_BOOTLOADER"
)

// Error is a classified failure.
type Error struct {
	Class Class
	Op    string // what was being done
	Err   error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Class) + ": " + e.Op
	}
	return string(e.Class) + ": " + e.Op + ": " + e.Err.Error()
}

// Unwrap exposes the cause (an internal/usb sentinel, an adiv5 sentinel, ...).
func (e *Error) Unwrap() error { return e.Err }

// New classifies err.
func New(c Class, op string, err error) *Error { return &Error{Class: c, Op: op, Err: err} }

// ClassOf returns the class of the first *Error in err's chain.
func ClassOf(err error) (Class, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return "", false
}

// Is reports whether err carries class c.
func Is(err error, c Class) bool {
	got, ok := ClassOf(err)
	return ok && got == c
}
