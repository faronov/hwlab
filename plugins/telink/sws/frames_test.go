package sws

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// scriptLink replays recorded frames: each Write must equal the expected
// request, after which the recorded answer chunks become readable.
type scriptLink struct {
	t       *testing.T
	steps   []scriptStep
	pending [][]byte
}

type scriptStep struct{ tx, rx []string }

func (s *scriptLink) Write(b []byte) (int, error) {
	s.t.Helper()
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected request %x", b)
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	if got := hex.EncodeToString(b); got != st.tx[0] {
		s.t.Fatalf("request %s, want %s", got, st.tx[0])
	}
	s.pending = nil
	for _, r := range st.rx {
		c, err := hex.DecodeString(r)
		if err != nil {
			s.t.Fatal(err)
		}
		s.pending = append(s.pending, c)
	}
	return len(b), nil
}

func (s *scriptLink) Read(b []byte) (int, error) {
	if len(s.pending) == 0 {
		return 0, nil
	}
	n := copy(b, s.pending[0])
	s.pending[0] = s.pending[0][n:]
	if len(s.pending[0]) == 0 {
		s.pending = s.pending[1:]
	}
	return n, nil
}

func (s *scriptLink) SetReadTimeout(time.Duration) error { return nil }
func (s *scriptLink) ResetInput() error                  { s.pending = nil; return nil }

func scripted(t *testing.T, steps ...scriptStep) *Programmer {
	p := New(&scriptLink{t: t, steps: steps})
	p.SetTimeout(20 * time.Millisecond)
	return p
}

// The frames below were captured from a real CH340 + TB-0x programmer (firmware
// 0x0008) and a real TLSR825x target (see
// docs/notes/phase14-telink-real-hardware-results.md). They pin the framing,
// the CRC and the answer layout to what the hardware really sends.
var (
	realVersion = scriptStep{tx: []string{"000000000024"}, rx: []string{"00000d00", "080062550105035a0006020005d7cb"}}
	realChipID  = scriptStep{tx: []string{"077d000003006d56"}, rx: []string{"07000300", "026255615f"}}
	realJEDEC   = scriptStep{tx: []string{"0500000000e8"}, rx: []string{"05000300", "c86013e233"}}
)

func TestRealCapturedVersion(t *testing.T) {
	v, err := scripted(t, realVersion).Version()
	if err != nil {
		t.Fatal(err)
	}
	if v.Firmware != 0x0008 || v.ChipID != 0x5562 || v.Chip != "TLSR825x" || v.SWDiv != 5 || v.AddrLen != 3 || !v.Power {
		t.Fatalf("%+v", v)
	}
}

func TestRealCapturedChipIDAndJEDEC(t *testing.T) {
	p := scripted(t, realChipID, realJEDEC)
	rev, id, err := p.ReadChipID()
	if err != nil || rev != 0x02 || id != 0x5562 {
		t.Fatalf("rev 0x%02x id 0x%04x %v", rev, id, err)
	}
	j, err := p.JEDEC()
	if err != nil || j != 0xc86013 {
		t.Fatalf("0x%06x %v", j, err)
	}
}

func TestRealCapturedFramesDamaged(t *testing.T) {
	flipped := realChipID
	flipped.rx = []string{"07000300", "026255615e"} // last CRC byte altered
	if _, _, err := scripted(t, flipped).ReadChipID(); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("bad CRC: %v", err)
	}
	truncated := realChipID
	truncated.rx = []string{"07000300", "0262"}
	if _, _, err := scripted(t, truncated).ReadChipID(); err == nil {
		t.Fatal("a truncated answer must fail")
	}
	silent := realChipID
	silent.rx = nil
	if _, _, err := scripted(t, silent).ReadChipID(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("no answer: %v", err)
	}
	hdrOnly := realChipID
	hdrOnly.rx = []string{"0700"}
	if _, _, err := scripted(t, hdrOnly).ReadChipID(); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("short header: %v", err)
	}
	wrongCmd := realChipID
	wrongCmd.rx = []string{"05000300", "026255615f"}
	if _, _, err := scripted(t, wrongCmd).ReadChipID(); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("answer to a different command: %v", err)
	}
}
