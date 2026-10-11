package jtag

import (
	"context"
	"fmt"
)

// Op is one step of a pipelined JTAG sequence: either a run of TMS clocks
// (NBits == 0) or a data shift.
type Op struct {
	TMS   []bool // TMS-only: one TCK per element with TDI held at TDI
	TDI   bool
	Data  []byte // shift data, LSB first (nil shifts zeros)
	NBits int    // 0 for a TMS-only op
	Exit  bool   // clock the last shifted bit with TMS high
	// Capture is how many of the leading shifted bits are captured from TDO.
	// Bits beyond it are clocked but not returned, which saves return-path
	// bandwidth on adapters with small IN buffers.
	Capture int
}

// Batcher is implemented by ports that can execute a whole sequence of ops
// with far fewer host round-trips than one call per op. It returns, for every
// op with Capture > 0 in order, the captured bits packed LSB first.
type Batcher interface {
	Batch(ctx context.Context, ops []Op) ([][]byte, error)
}

// RunOps executes ops on p using Batch when the port has it and plain Port
// calls otherwise. The result has one entry per op with Capture > 0.
func RunOps(ctx context.Context, p Port, ops []Op) ([][]byte, error) {
	if b, ok := p.(Batcher); ok {
		return b.Batch(ctx, ops)
	}
	var res [][]byte
	for _, op := range ops {
		if op.NBits == 0 {
			if err := p.TMS(ctx, op.TMS, op.TDI); err != nil {
				return nil, err
			}
			continue
		}
		out, err := p.Shift(ctx, op.Data, op.NBits, op.Exit, op.Capture > 0)
		if err != nil {
			return nil, err
		}
		if op.Capture > 0 {
			res = append(res, truncateBits(out, op.Capture))
		}
	}
	return res, nil
}

func truncateBits(b []byte, n int) []byte {
	need := (n + 7) / 8
	out := make([]byte, need)
	copy(out, b)
	if n%8 != 0 && need > 0 {
		out[need-1] &= byte(1<<(n%8)) - 1
	}
	return out
}

// Batch collects TAP operations, tracking the TAP state virtually, and runs
// them in one go. Build it with Driver.NewBatch. A Batch is single use.
type Batch struct {
	d     *Driver
	ops   []Op
	state State
	ncap  int
	err   error
}

// NewBatch starts a batch from the driver's believed TAP state. From an unknown
// state the batch begins with a TAP reset.
func (d *Driver) NewBatch() *Batch {
	b := &Batch{d: d, state: d.state}
	if b.state == StateUnknown {
		b.ops = append(b.ops, Op{TMS: []bool{true, true, true, true, true}, TDI: true})
		b.state = TestLogicReset
	}
	return b
}

// Len is the number of queued ops.
func (b *Batch) Len() int { return len(b.ops) }

// GoTo queues the shortest TMS path to the state.
func (b *Batch) GoTo(to State) {
	if b.err != nil {
		return
	}
	path, err := Path(b.state, to)
	if err != nil {
		b.err = err
		return
	}
	if len(path) > 0 {
		b.ops = append(b.ops, Op{TMS: path, TDI: true})
	}
	b.state = to
}

// RunTest queues cycles of Run-Test/Idle.
func (b *Batch) RunTest(cycles int) {
	b.GoTo(RunTestIdle)
	if cycles > 0 && b.err == nil {
		b.ops = append(b.ops, Op{TMS: make([]bool, cycles)})
	}
}

func (b *Batch) scan(ir bool, data []byte, nbits, capture int, end State) int {
	if b.err != nil {
		return -1
	}
	if nbits < 1 || capture < 0 || capture > nbits {
		b.err = fmt.Errorf("jtag: invalid batch scan (%d bits, capture %d)", nbits, capture)
		return -1
	}
	shift, exit := ShiftDR, Exit1DR
	if ir {
		shift, exit = ShiftIR, Exit1IR
	}
	b.GoTo(shift)
	b.ops = append(b.ops, Op{Data: data, NBits: nbits, Exit: true, Capture: capture})
	b.state = exit
	b.GoTo(end)
	if capture == 0 {
		return -1
	}
	b.ncap++
	return b.ncap - 1
}

// ShiftDR queues a DR scan that ends in Run-Test/Idle. It returns the index
// of its result in Run's output, or -1 if capture is 0.
func (b *Batch) ShiftDR(data []byte, nbits, capture int) int {
	return b.scan(false, data, nbits, capture, RunTestIdle)
}

// ShiftIR queues an IR scan that ends in Run-Test/Idle.
func (b *Batch) ShiftIR(data []byte, nbits, capture int) int {
	return b.scan(true, data, nbits, capture, RunTestIdle)
}

// Run executes the batch. The driver's TAP state follows the batch on success
// and becomes unknown on failure.
func (b *Batch) Run(ctx context.Context) ([][]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	res, err := RunOps(ctx, b.d.port, b.ops)
	if err != nil {
		b.d.state = StateUnknown
		return nil, err
	}
	b.d.state = b.state
	if len(res) != b.ncap {
		b.d.state = StateUnknown
		return nil, fmt.Errorf("jtag: batch returned %d capture(s), expected %d", len(res), b.ncap)
	}
	return res, nil
}
