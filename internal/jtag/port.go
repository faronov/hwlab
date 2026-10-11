package jtag

import "context"

// Port moves bits through a JTAG interface. Implementations do not track TAP
// state; the Driver does.
type Port interface {
	// TMS clocks one TCK per element with that TMS value and TDI held at tdi.
	TMS(ctx context.Context, tms []bool, tdi bool) error
	// Shift clocks nbits of tdi (bit 0 of tdi[0] first) with TMS low. With
	// exit, the final bit is clocked with TMS high (leaving Shift-xR for
	// Exit1-xR). With capture, the TDO bits sampled on the rising edge are
	// returned in the same bit order; otherwise the result is nil. A nil tdi
	// shifts zeros.
	Shift(ctx context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error)
}
