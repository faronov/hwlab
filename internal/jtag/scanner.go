package jtag

import "context"

// ScanOp is one whole instruction or data register scan for adapters whose
// engine works between stable TAP states instead of raw TMS bits (the TI
// XDS110 and its IEEE 1149.7 two-pin formats). The adapter walks from its
// current stable state to Shift-IR or Shift-DR, shifts Bits of Out (LSB
// first, nil shifts zeros) and stops in End. A ScanOp with Bits == 0 is a
// run-test: Idle clocks in Run-Test/Idle, then End.
type ScanOp struct {
	IR   bool
	Bits int
	Out  []byte
	// End is the stable state the scan finishes in and must be RunTestIdle,
	// PauseDR or PauseIR. Test-Logic-Reset is deliberately not offered: it
	// would drop an IEEE 1149.7 target out of its two-pin scan format.
	End State
	// Idle is the number of Run-Test/Idle clocks of a run-test op.
	Idle int
}

// Scanner runs scan-level operations. The result holds, for every op with
// Bits > 0 in order, the captured TDO bits packed LSB first.
type Scanner interface {
	Scans(ctx context.Context, ops []ScanOp) ([][]byte, error)
}
