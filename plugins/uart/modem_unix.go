//go:build linux || darwin

package uart

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// SetModemLines sets DTR and RTS together. On a real port it is one
// TIOCMSET ioctl, so the two lines change at the same instant: boards that
// drive RESET and a boot pin through both lines (TI CC26xx dongles, ESP
// auto-program circuits) must not pass through an intermediate state.
// Other Port implementations get two calls; atomic reports which happened.
func SetModemLines(p Port, dtr, rts bool) (atomic bool, err error) {
	rp, ok := p.(*realPort)
	if !ok {
		return false, setLinesSeq(p, dtr, rts)
	}
	fd, err := fdOf(rp.Port)
	if err != nil {
		return false, setLinesSeq(p, dtr, rts)
	}
	st, err := unix.IoctlGetInt(fd, unix.TIOCMGET)
	if err != nil {
		return false, fmt.Errorf("read modem lines: %w", err)
	}
	st &^= unix.TIOCM_DTR | unix.TIOCM_RTS
	if dtr {
		st |= unix.TIOCM_DTR
	}
	if rts {
		st |= unix.TIOCM_RTS
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCMSET, st); err != nil {
		return false, fmt.Errorf("set modem lines: %w", err)
	}
	return true, nil
}
