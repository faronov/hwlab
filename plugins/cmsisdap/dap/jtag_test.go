package dap

import (
	"context"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
)

// jtagTAP is a one-TAP JTAG chain behind the DAP_JTAG_Sequence command.
type jtagTAP struct {
	idcode uint32
	state  jtag.State
	ir     uint32
	shift  uint32
}

func (j *jtagTAP) Exchange(req []byte, _ time.Duration) ([]byte, error) {
	switch req[0] {
	case cmdInfo:
		switch req[1] {
		case InfoPacketSize:
			return []byte{cmdInfo, 2, 64, 0}, nil
		case InfoCaps:
			return []byte{cmdInfo, 1, CapSWD | CapJTAG}, nil
		}
		return []byte{cmdInfo, 0}, nil
	case cmdJTAGSequence:
		resp := []byte{cmdJTAGSequence, 0}
		n := int(req[1])
		off := 2
		for i := 0; i < n; i++ {
			info := req[off]
			bits := int(info & 0x3F)
			if bits == 0 {
				bits = 64
			}
			tms := info&0x40 != 0
			nb := (bits + 7) / 8
			tdi := req[off+1 : off+1+nb]
			off += 1 + nb
			out := make([]byte, nb)
			for b := 0; b < bits; b++ {
				in := tdi[b/8]>>(b%8)&1 == 1
				if j.clock(tms, in) {
					out[b/8] |= 1 << (b % 8)
				}
			}
			if info&0x80 != 0 {
				resp = append(resp, out...)
			}
		}
		return resp, nil
	}
	return []byte{respInvalid}, nil
}

// clock applies one TCK and returns the TDO level before the edge.
func (j *jtagTAP) clock(tms, tdi bool) bool {
	tdo := j.shift&1 == 1
	switch j.state {
	case jtag.ShiftDR:
		j.shift = j.shift>>1 | b2u(tdi)<<31
	case jtag.ShiftIR:
		j.shift = j.shift>>1 | b2u(tdi)<<3
	}
	next := jtag.Next(j.state, tms)
	if next != j.state || tms {
		switch next {
		case jtag.TestLogicReset:
			j.ir = 1
		case jtag.CaptureDR:
			j.shift = j.idcode
		case jtag.CaptureIR:
			j.shift = 1
		case jtag.UpdateIR:
			j.ir = j.shift & 0xF
		}
	}
	j.state = next
	return tdo
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func (j *jtagTAP) PacketSize() int  { return 64 }
func (j *jtagTAP) Describe() string { return "jtag test tap" }
func (j *jtagTAP) Close() error     { return nil }

func TestScanChainOverDAPJTAGSequence(t *testing.T) {
	tap := &jtagTAP{idcode: 0x4BA00477, state: jtag.PauseIR}
	p, err := NewProbe(tap)
	if err != nil {
		t.Fatal(err)
	}
	d := jtag.NewDriver(JTAGPort{P: p})
	c, err := jtag.ScanChain(context.Background(), d, jtag.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Devices) != 1 || c.Devices[0].IDCode.Raw != 0x4BA00477 {
		t.Fatalf("chain %+v", c)
	}
}
