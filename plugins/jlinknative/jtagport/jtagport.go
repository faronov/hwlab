// Package jtagport is the native J-Link wire transport for internal/jtag: it
// implements jtag.Port on the probe's bit-sequence command. The TAP state
// machine, IR/DR shifting and chain enumeration stay in internal/jtag.
//
// STATUS: IMPLEMENTED_UNTESTED. The mapping of the sequence vectors in JTAG
// mode (first = TMS, second = TDI, reply = sampled TDO) follows what the
// probe does in SWD mode and public interoperability notes; no downstream
// JTAG target was available to confirm it on real hardware. See
// docs/notes/phase-native-jlink-results.md.
package jtagport

import (
	"context"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
)

// Runner executes one bit sequence; *protocol.Client implements it.
type Runner interface {
	Run(ctx context.Context, s protocol.Sequence) (protocol.SequenceResult, error)
}

// Port implements jtag.Port. It does not select the interface or the clock:
// the caller does that on the probe first.
type Port struct {
	r   Runner
	max int
}

var _ jtag.Port = (*Port)(nil)

// New returns a Port on r.
func New(r Runner) *Port { return &Port{r: r, max: protocol.MaxSequenceBits} }

func (p *Port) run(ctx context.Context, bits int, tms, tdi []byte) ([]byte, error) {
	res, err := p.r.Run(ctx, protocol.Sequence{Bits: bits, A: tms, B: tdi})
	if err != nil {
		return nil, err
	}
	if len(res.In) != (bits+7)/8 {
		return nil, jlerr.New(jlerr.ProtocolError, "jtag sequence", fmt.Errorf("short reply: %d bytes for %d bits", len(res.In), bits))
	}
	return res.In, nil
}

// TMS clocks one TCK per element with that TMS value and TDI held at tdi.
func (p *Port) TMS(ctx context.Context, tms []bool, tdi bool) error {
	for off := 0; off < len(tms); off += p.max {
		n := min(p.max, len(tms)-off)
		nb := (n + 7) / 8
		a, b := make([]byte, nb), make([]byte, nb)
		for i := 0; i < n; i++ {
			jtag.SetBit(a, i, tms[off+i])
			jtag.SetBit(b, i, tdi)
		}
		if _, err := p.run(ctx, n, a, b); err != nil {
			return err
		}
	}
	return nil
}

// Shift clocks nbits of tdi with TMS low (high on the last bit with exit) and
// returns the sampled TDO bits when capture is set.
func (p *Port) Shift(ctx context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits <= 0 {
		return nil, jlerr.New(jlerr.ProtocolError, "jtag shift", fmt.Errorf("%d bits", nbits))
	}
	if tdi != nil && len(tdi) < (nbits+7)/8 {
		return nil, jlerr.New(jlerr.ProtocolError, "jtag shift", fmt.Errorf("%d data bytes for %d bits", len(tdi), nbits))
	}
	var out []byte
	if capture {
		out = make([]byte, (nbits+7)/8)
	}
	for off := 0; off < nbits; off += p.max {
		n := min(p.max, nbits-off)
		nb := (n + 7) / 8
		a, b := make([]byte, nb), make([]byte, nb)
		for i := 0; i < n && tdi != nil; i++ {
			jtag.SetBit(b, i, jtag.GetBit(tdi, off+i))
		}
		if exit && off+n == nbits {
			jtag.SetBit(a, n-1, true)
		}
		in, err := p.run(ctx, n, a, b)
		if err != nil {
			return nil, err
		}
		if capture {
			for i := 0; i < n; i++ {
				jtag.SetBit(out, off+i, jtag.GetBit(in, i))
			}
		}
	}
	return out, nil
}
