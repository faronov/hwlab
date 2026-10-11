package sim

import (
	"encoding/binary"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
)

// tapModel is a SYNTHETIC chain of 4-bit-IR TAPs that present their IDCODE
// after Test-Logic-Reset. It only checks that the host's JTAG vectors are
// consistent (TMS/TDI in, sampled TDO out); it does not describe how any real
// probe samples TDO.
type tapModel struct {
	ids   []uint32
	state jtag.State
	dr    []bool
	ir    []bool
	init  bool
}

// SetJTAGChain installs the IDCODEs (nearest TDO first) seen in JTAG mode.
func (s *Sim) SetJTAGChain(ids ...uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tap = tapModel{ids: ids}
}

func (t *tapModel) loadDR() {
	t.dr = t.dr[:0]
	for _, id := range t.ids {
		for i := 0; i < 32; i++ {
			t.dr = append(t.dr, id>>i&1 == 1)
		}
	}
}

func (s *Sim) jtagSequence(cmd []byte) ([]byte, error) {
	n := int(binary.LittleEndian.Uint16(cmd[2:]))
	nb := (n + 7) / 8
	if len(cmd) != 4+2*nb {
		s.wedged = true
		return nil, jlerr.New(jlerr.ProtocolError, "sim", fmt.Errorf("sequence length %d for %d bits", len(cmd), n))
	}
	t := &s.tap
	if !t.init {
		t.init, t.state = true, jtag.TestLogicReset
		t.loadDR()
	}
	tmsV, tdiV := cmd[4:4+nb], cmd[4+nb:]
	reply := make([]byte, nb+1)
	for i := 0; i < n; i++ {
		tms, tdi := getBit(tmsV, i) == 1, getBit(tdiV, i) == 1
		switch t.state {
		case jtag.CaptureDR:
			t.loadDR()
		case jtag.CaptureIR:
			t.ir = make([]bool, 4*len(t.ids))
			for k := 0; k < len(t.ir); k += 4 {
				t.ir[k] = true
			}
		}
		switch {
		case t.state == jtag.ShiftDR && len(t.dr) > 0:
			if t.dr[0] {
				reply[i/8] |= 1 << (i % 8)
			}
			t.dr = append(t.dr[1:], tdi)
		case t.state == jtag.ShiftIR && len(t.ir) > 0:
			if t.ir[0] {
				reply[i/8] |= 1 << (i % 8)
			}
			t.ir = append(t.ir[1:], tdi)
		case t.state == jtag.ShiftDR || t.state == jtag.ShiftIR:
			// an empty chain: TDO floats high
			reply[i/8] |= 1 << (i % 8)
		}
		t.state = jtag.Next(t.state, tms)
		if t.state == jtag.TestLogicReset {
			t.loadDR()
		}
	}
	return reply, nil
}
