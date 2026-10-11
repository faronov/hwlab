//go:build linux || darwin

package uart

import (
	"errors"
	"fmt"
	"reflect"

	"golang.org/x/sys/unix"
)

// fdOf returns the file descriptor behind a go.bug.st/serial port. The
// library has no flow control and exports no descriptor, and it opens ports
// exclusively (TIOCEXCL), so a second open is refused; reading the unexported
// "handle" field of its unix port type is the only way to reach the termios
// of the open port. The pty test pins this against library upgrades.
func fdOf(port any) (int, error) {
	v := reflect.ValueOf(port)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, errors.New("unsupported serial port type")
	}
	h := v.FieldByName("handle")
	if !h.IsValid() || !h.CanInt() {
		return 0, errors.New("the serial library no longer exposes the port descriptor; flow control is unavailable")
	}
	return int(h.Int()), nil
}

// applyFlow sets the flow control bits of an open serial port.
func applyFlow(port any, flow string) error {
	fd, err := fdOf(port)
	if err != nil {
		return fmt.Errorf("flow control: %w", err)
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return fmt.Errorf("flow control: read termios: %w", err)
	}
	t.Cflag &^= unix.CRTSCTS
	t.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
	switch flow {
	case FlowRTSCTS:
		t.Cflag |= unix.CRTSCTS
	case FlowXonXoff:
		t.Iflag |= unix.IXON | unix.IXOFF
	}
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		return fmt.Errorf("flow control: write termios: %w", err)
	}
	return nil
}

// flowSupported reports whether this build can set flow control.
func flowSupported() bool { return true }
