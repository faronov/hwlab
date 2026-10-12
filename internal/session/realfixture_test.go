package session

import (
	"context"
	"testing"
)

// The fixture is a real 1 s PPK2 capture (ampere mode, nothing connected, so
// it holds the open-circuit noise floor). It guards the on-disk format and the
// reader against drift.
func TestReadRealPPK2Fixture(t *testing.T) {
	r, err := DirSampleStore{Root: "../../testdata/real"}.Open("ppk2-ampere-1s.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	if r.Kind() != KindPower {
		t.Fatalf("kind = %s", r.Kind())
	}
	if n := r.Samples(); n < 99_000 || n > 100_000 {
		t.Fatalf("samples = %d, want ~100000 (1 s at 100 kSa/s)", n)
	}
	var n int64
	var minNA, maxNA int32 = 1 << 30, -1 << 30
	err = r.ReadPower(context.Background(), 0, 1<<62, func(c *PowerChunk) error {
		if c.PeriodNs != 10_000 {
			t.Fatalf("period = %d", c.PeriodNs)
		}
		for _, v := range c.CurrentNA {
			minNA, maxNA = min(minNA, v), max(maxNA, v)
		}
		n += int64(len(c.CurrentNA))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != r.Samples() {
		t.Errorf("read %d samples, index says %d", n, r.Samples())
	}
	if minNA < -50_000 || maxNA > 50_000 {
		t.Errorf("open-circuit current outside ±50 µA: %d..%d nA", minNA, maxNA)
	}
}
