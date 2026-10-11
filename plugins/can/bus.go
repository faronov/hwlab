// Package canplugin is the hwlab plugin for CAN and CAN-FD buses.
//
// It is a transport: frames are can.Frame values, independent of the backend
// (SocketCAN, the in-process virtual bus, later USB-CAN adapters). Protocol
// analyzers (ISO-TP, UDS, CANopen, J1939) consume the frames and never touch a
// backend.
package canplugin

import (
	"errors"
	"time"

	"github.com/faronov/hwlab/internal/can"
)

// ErrTimeout is returned by Bus.Recv when no frame arrived in time.
var ErrTimeout = errors.New("no CAN frame before the timeout")

// ErrClosed is returned by a Bus after Close.
var ErrClosed = errors.New("CAN interface closed")

var errFDDisabled = errors.New("this interface was opened without CAN FD (set fd: true on an FD-capable interface)")

// Options are the receive-side settings of an open interface.
type Options struct {
	// FD enables CAN-FD frames (needs an FD-capable interface).
	FD bool
	// Own delivers frames this handle transmitted back to its own receiver, so
	// a capture sees what the host sent (the SocketCAN RECV_OWN_MSGS option).
	Own bool
	// ErrorFrames delivers controller/bus error frames.
	ErrorFrames bool
	// Filters are the receive filters; none means everything.
	Filters []can.Filter
}

// Rx is one received frame with its best available timestamp.
type Rx struct {
	Frame can.Frame
	// At is when the frame reached the host, on the monotonic clock of
	// time.Now. For a kernel timestamp it is the receive time converted onto
	// the monotonic clock.
	At time.Time
	// Kernel is true when At came from the kernel receive timestamp rather than
	// from the moment the read returned.
	Kernel bool
}

// Bus is one open CAN interface. It carries frames only: bit rate, interface
// state and termination are configured outside hwlab (ip link) and never by a
// plugin call. A backend is a USB-CAN adapter, SocketCAN, the in-process
// virtual bus or any later source; analyzers see only can.Frame.
type Bus interface {
	// Recv waits up to timeout for the next frame. It returns ErrTimeout when
	// none arrived and ErrClosed after Close.
	Recv(timeout time.Duration) (Rx, error)
	// Send transmits one frame.
	Send(f can.Frame) error
	// Close releases the interface; it is safe to call more than once.
	Close() error
}

// IfaceInfo describes an interface found during discovery.
type IfaceInfo struct {
	Name string
	// FD is true when the interface can carry CAN-FD frames.
	FD bool
	// Up reports administrative state; a down interface cannot be opened.
	Up bool
	// Virtual marks vcan/vxcan and the in-process virtual bus.
	Virtual bool
	MTU     int
	// Backend names the implementation, e.g. "socketcan" or "virtual".
	Backend string
}

// Lister enumerates available interfaces.
type Lister func() ([]IfaceInfo, error)

// Opener opens the named interface.
type Opener func(name string, o Options) (Bus, error)
