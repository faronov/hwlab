package jtagport

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
)

// tapChain models a chain of TAPs with IDCODE after reset and a 4-bit IR, as
// a SIMULATED stand-in for a downstream JTAG target. It validates the vector
// mapping and chunking of the port, not the probe's real JTAG behaviour.
type tapChain struct {
	ids   []uint32
	state jtag.State
	dr    []bool // concatenated DR, nearest TDO first
	ir    []bool
	cmds  int
}

func newChain(ids ...uint32) *tapChain { return &tapChain{ids: ids, state: jtag.TestLogicReset} }

func (c *tapChain) load() {
	c.dr = c.dr[:0]
	for _, id := range c.ids {
		for i := 0; i < 32; i++ {
			c.dr = append(c.dr, id>>i&1 == 1)
		}
	}
}

func (c *tapChain) Run(_ context.Context, s protocol.Sequence) (protocol.SequenceResult, error) {
	c.cmds++
	in := make([]byte, (s.Bits+7)/8)
	for i := 0; i < s.Bits; i++ {
		tms, tdi := jtag.GetBit(s.A, i), jtag.GetBit(s.B, i)
		switch c.state {
		case jtag.CaptureDR:
			c.load()
		case jtag.CaptureIR:
			c.ir = make([]bool, 4*len(c.ids))
			for k := 0; k < len(c.ir); k += 4 {
				c.ir[k] = true // 0b0001 capture pattern per device
			}
		}
		switch c.state {
		case jtag.ShiftDR:
			jtag.SetBit(in, i, c.dr[0])
			c.dr = append(c.dr[1:], tdi)
		case jtag.ShiftIR:
			jtag.SetBit(in, i, c.ir[0])
			c.ir = append(c.ir[1:], tdi)
		}
		c.state = jtag.Next(c.state, tms)
		if c.state == jtag.TestLogicReset {
			c.load()
		}
	}
	return protocol.SequenceResult{In: in}, nil
}

func TestScanChainOverPort(t *testing.T) {
	ids := []uint32{0x4BA00477, 0x0BC11477}
	c := newChain(ids...)
	c.load()
	d := jtag.NewDriver(New(c))
	chain, err := jtag.ScanChain(context.Background(), d, jtag.ScanOptions{MaxDevices: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Devices) != 2 || chain.Devices[0].IDCode == nil || chain.Devices[0].IDCode.Raw != ids[0] || chain.Devices[1].IDCode == nil || chain.Devices[1].IDCode.Raw != ids[1] {
		t.Fatalf("chain %+v", chain)
	}
}

func TestShiftChunksLongVectors(t *testing.T) {
	c := newChain(0x4BA00477)
	c.load()
	p := New(c)
	p.max = 40 // force several commands for one 96-bit DR scan
	d := jtag.NewDriver(p)
	if err := d.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.cmds = 0
	out, err := d.ShiftDR(context.Background(), jtag.Ones(96), 96, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := uint32(jtag.Field(out, 0, 32)); got != 0x4BA00477 {
		t.Errorf("idcode %#x", got)
	}
	if c.cmds < 3 {
		t.Errorf("only %d commands for a 96-bit shift at 40 bits/command", c.cmds)
	}
	if d.State() != jtag.Exit1DR && d.State() != jtag.RunTestIdle {
		t.Logf("state %v", d.State())
	}
}

func TestShortReplyIsAnError(t *testing.T) {
	p := New(shortRunner{})
	if err := p.TMS(context.Background(), []bool{true, true}, true); err == nil {
		t.Fatal("short reply accepted")
	}
}

type shortRunner struct{}

func (shortRunner) Run(context.Context, protocol.Sequence) (protocol.SequenceResult, error) {
	return protocol.SequenceResult{In: nil}, nil
}
