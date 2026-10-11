package jtag

import (
	"context"
	"errors"
	"fmt"
)

// Driver tracks the TAP state over a Port and offers the standard scan
// operations. It is not safe for concurrent use.
type Driver struct {
	port  Port
	state State
}

// NewDriver returns a Driver whose TAP state is unknown until Reset.
func NewDriver(p Port) *Driver { return &Driver{port: p, state: StateUnknown} }

// State returns the believed TAP state.
func (d *Driver) State() State { return d.state }

// Reset forces Test-Logic-Reset with five TMS-high clocks, which works from
// any state without TRST.
func (d *Driver) Reset(ctx context.Context) error {
	if err := d.port.TMS(ctx, []bool{true, true, true, true, true}, true); err != nil {
		d.state = StateUnknown
		return err
	}
	d.state = TestLogicReset
	return nil
}

// GoTo moves to the target state by the shortest TMS path. From an unknown
// state it resets first.
func (d *Driver) GoTo(ctx context.Context, to State) error {
	if d.state == StateUnknown {
		if err := d.Reset(ctx); err != nil {
			return err
		}
	}
	path, err := Path(d.state, to)
	if err != nil {
		return err
	}
	if len(path) == 0 {
		return nil
	}
	if err := d.port.TMS(ctx, path, true); err != nil {
		d.state = StateUnknown
		return err
	}
	d.state = to
	return nil
}

// RunTest idles in Run-Test/Idle for the given number of clocks.
func (d *Driver) RunTest(ctx context.Context, cycles int) error {
	if err := d.GoTo(ctx, RunTestIdle); err != nil {
		return err
	}
	if cycles <= 0 {
		return nil
	}
	if err := d.port.TMS(ctx, make([]bool, cycles), false); err != nil {
		d.state = StateUnknown
		return err
	}
	return nil
}

func (d *Driver) scan(ctx context.Context, ir bool, data []byte, nbits int, capture bool, end State) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("jtag: scan of zero bits")
	}
	if end != RunTestIdle && end != PauseDR && end != PauseIR && end != TestLogicReset && end != UpdateDR && end != UpdateIR {
		return nil, fmt.Errorf("jtag: unsupported end state %v", end)
	}
	shift, exit := ShiftDR, Exit1DR
	if ir {
		shift, exit = ShiftIR, Exit1IR
	}
	if err := d.GoTo(ctx, shift); err != nil {
		return nil, err
	}
	out, err := d.port.Shift(ctx, data, nbits, true, capture)
	if err != nil {
		d.state = StateUnknown
		return nil, err
	}
	d.state = exit
	if err := d.GoTo(ctx, end); err != nil {
		return nil, err
	}
	return out, nil
}

// ShiftIR scans nbits through the instruction register chain and returns to
// Run-Test/Idle. data is LSB first; with capture the bits shifted out
// (the Capture-IR pattern first) are returned.
func (d *Driver) ShiftIR(ctx context.Context, data []byte, nbits int, capture bool) ([]byte, error) {
	return d.scan(ctx, true, data, nbits, capture, RunTestIdle)
}

// ShiftDR scans nbits through the selected data registers and returns to
// Run-Test/Idle.
func (d *Driver) ShiftDR(ctx context.Context, data []byte, nbits int, capture bool) ([]byte, error) {
	return d.scan(ctx, false, data, nbits, capture, RunTestIdle)
}

// ShiftIRTo and ShiftDRTo are like ShiftIR/ShiftDR but finish in a chosen
// state (for example a Pause state).
func (d *Driver) ShiftIRTo(ctx context.Context, data []byte, nbits int, capture bool, end State) ([]byte, error) {
	return d.scan(ctx, true, data, nbits, capture, end)
}

// ShiftDRTo see ShiftIRTo.
func (d *Driver) ShiftDRTo(ctx context.Context, data []byte, nbits int, capture bool, end State) ([]byte, error) {
	return d.scan(ctx, false, data, nbits, capture, end)
}

// Bit helpers. Bit streams are LSB first within each byte.

// GetBit returns bit i of b.
func GetBit(b []byte, i int) bool { return b[i/8]>>(i%8)&1 == 1 }

// SetBit sets bit i of b.
func SetBit(b []byte, i int, v bool) {
	if v {
		b[i/8] |= 1 << (i % 8)
	} else {
		b[i/8] &^= 1 << (i % 8)
	}
}

// Ones returns n bits, all set (the unused bits of the last byte are zero).
func Ones(n int) []byte {
	b := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		SetBit(b, i, true)
	}
	return b
}

// Field extracts width (<=64) bits starting at bit off as an integer, LSB
// first.
func Field(b []byte, off, width int) uint64 {
	var v uint64
	for i := 0; i < width; i++ {
		if GetBit(b, off+i) {
			v |= 1 << i
		}
	}
	return v
}
