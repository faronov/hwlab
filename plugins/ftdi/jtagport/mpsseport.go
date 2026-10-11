// Package jtagport adapts transports to the generic internal/jtag Port.
package jtagport

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/plugins/ftdi/mpsse"
)

// MPSSEPort is a jtag.Port over an MPSSE engine. It uses only the opcodes
// 0x19/0x39 (bytes), 0x1B/0x3B (bits) and 0x4B/0x6B (TMS), which are the
// ones every MPSSE implementation, including partial emulators, supports.
type MPSSEPort struct {
	eng *mpsse.Engine
}

// NewMPSSEPort wraps an engine.
func NewMPSSEPort(eng *mpsse.Engine) *MPSSEPort { return &MPSSEPort{eng: eng} }

// TMS implements Port.
func (p *MPSSEPort) TMS(ctx context.Context, tms []bool, tdi bool) error {
	b := mpsse.NewBuilder()
	for len(tms) > 0 {
		n := min(len(tms), 7)
		var v byte
		for i := 0; i < n; i++ {
			if tms[i] {
				v |= 1 << i
			}
		}
		b.TMS(v, n, tdi, false)
		tms = tms[n:]
	}
	_, err := p.eng.Exec(ctx, b)
	return err
}

// Shift implements Port.
func (p *MPSSEPort) Shift(ctx context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("jtag: shift of zero bits")
	}
	need := (nbits + 7) / 8
	if tdi == nil {
		tdi = make([]byte, need)
	}
	if len(tdi) < need {
		return nil, fmt.Errorf("jtag: %d bit(s) need %d byte(s) of data, have %d", nbits, need, len(tdi))
	}
	body := nbits
	if exit {
		body--
	}
	full, rem := body/8, body%8
	b := mpsse.NewBuilder()
	var byteRs []mpsse.Resp
	var bitR, lastR mpsse.Resp
	if full > 0 {
		byteRs = b.ShiftBytes(tdi[:full], capture)
	}
	if rem > 0 {
		bitR = b.ShiftBits(tdi[full], rem, capture)
	}
	if exit {
		last := nbits - 1
		lastR = b.TMS(1, 1, tdi[last/8]>>(last%8)&1 == 1, capture)
	}
	reply, err := p.eng.Exec(ctx, b)
	if err != nil {
		return nil, err
	}
	if !capture {
		return nil, nil
	}
	out := make([]byte, need)
	copy(out, mpsse.ReadAll(reply, byteRs))
	if rem > 0 {
		v := bitR.Slice(reply)
		if len(v) != 1 {
			return nil, errors.New("jtag: missing bit-mode reply")
		}
		// Bit-mode reads shift in from bit 7, so the first sample is at 8-rem.
		out[full] |= v[0] >> (8 - rem)
	}
	if exit {
		v := lastR.Slice(reply)
		if len(v) != 1 {
			return nil, errors.New("jtag: missing TMS reply")
		}
		if v[0]&0x80 != 0 {
			last := nbits - 1
			out[last/8] |= 1 << (last % 8)
		}
	}
	return out, nil
}
