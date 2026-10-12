package protocol

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestCmdSetRegulator(t *testing.T) {
	cases := []struct {
		mv     int
		b1, b2 byte
	}{
		{800, 3, 32},
		{3300, 12, 228},
		{5000, 3 + (4200+32)/256, byte((4200 + 32) % 256)},
		{100, 3, 32}, // clamped low
		{9000, 3 + (4200+32)/256, byte((4200 + 32) % 256)}, // clamped high
	}
	for _, c := range cases {
		got := CmdSetRegulator(c.mv)
		want := []byte{CmdRegulatorSet, c.b1, c.b2}
		if !bytes.Equal(got, want) {
			t.Errorf("CmdSetRegulator(%d) = %v, want %v", c.mv, got, want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"ampere": ModeAmpere, "SOURCE_MODE": ModeSource, "ampere_meter": ModeAmpere} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Error("expected error")
	}
}

func TestParseMetadata(t *testing.T) {
	if _, err := ParseMetadata("Calibrated: 0\nR0: 1000\n"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	md, err := ParseMetadata("Calibrated: 1\nR0: 1000.5\nR1: 0\nO2: 12\nUG4: 0.5\nHW: 1234\nEND\n")
	if err != nil {
		t.Fatal(err)
	}
	c := md.Calibration
	if c.R[0] != 1000.5 {
		t.Errorf("R0=%v", c.R[0])
	}
	if c.R[1] != DefaultCalibration().R[1] {
		t.Errorf("zero R1 must keep default, got %v", c.R[1])
	}
	if c.O[2] != 12 || c.UG[4] != 0.5 {
		t.Errorf("O2=%v UG4=%v", c.O[2], c.UG[4])
	}
	if md.Raw["HW"] != "1234" {
		t.Errorf("raw HW=%q", md.Raw["HW"])
	}
	if _, err := ParseMetadata("R0: abc\nEND"); err == nil {
		t.Error("expected parse error")
	}
}

func rawSample(adc, rng, logic uint32) []byte {
	v := adc&adcMask | rng<<rangeShift | logic<<logicShift
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func TestDecode(t *testing.T) {
	d := &Decoder{Cal: DefaultCalibration(), VddMV: 3300}
	var stream []byte
	stream = append(stream, rawSample(1000, 0, 0b101)...)
	stream = append(stream, rawSample(2000, 3, 0b010)...)
	stream = append(stream, rawSample(0, 7, 0xff)...) // range clamps to 4

	// Feed in awkward fragments to exercise the remainder logic.
	var cur []int32
	var logic []uint32
	for i := 0; i < len(stream); i += 5 {
		cur, logic = d.Decode(stream[i:min(i+5, len(stream))], cur, logic)
	}
	if len(cur) != 3 || len(logic) != 3 {
		t.Fatalf("decoded %d/%d samples", len(cur), len(logic))
	}
	want0 := math.Round(float64(1000*4) * adcMult / 1031.64 * 1e9)
	if math.Abs(float64(cur[0])-want0) > 1 {
		t.Errorf("sample0 = %d nA, want %v", cur[0], want0)
	}
	if logic[0] != 0b101 || logic[1] != 0b010 || logic[2] != 0xff {
		t.Errorf("logic = %v", logic)
	}

	// Whole-buffer decode must equal the fragmented decode.
	d2 := &Decoder{Cal: DefaultCalibration(), VddMV: 3300}
	cur2, logic2 := d2.Decode(stream, nil, nil)
	for i := range cur {
		if cur[i] != cur2[i] || logic[i] != logic2[i] {
			t.Errorf("sample %d differs between fragmented and whole decode", i)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7})
	f.Fuzz(func(t *testing.T, b []byte) {
		d := &Decoder{Cal: DefaultCalibration(), VddMV: 3300}
		cur, logic := d.Decode(b, nil, nil)
		if len(cur) != len(b)/SampleSize || len(logic) != len(cur) {
			t.Fatalf("got %d samples from %d bytes", len(cur), len(b))
		}
	})
}
