package analysis

import (
	"context"
	"math"
	"testing"

	"github.com/faronov/hwlab/internal/session"
)

// memSource is an in-memory PowerSource of one chunk.
type memSource struct{ c session.PowerChunk }

func (m *memSource) Bounds() (int64, int64) { return m.c.StartNs, m.c.EndNs() }

func (m *memSource) ReadPower(ctx context.Context, from, to int64, fn func(*session.PowerChunk) error) error {
	if m.c.EndNs() <= from || m.c.StartNs >= to {
		return nil
	}
	return fn(&m.c)
}

// profile builds 1 s at 10 µs with `idle` nA and a burst of `burst` nA of
// length burstLen samples every 100 ms starting at 12 ms.
func profile(idle, burst int32, burstLen int) *memSource {
	const n = 100_000
	cur := make([]int32, n)
	for i := range cur {
		cur[i] = idle
		if i >= 1200 && (i-1200)%10_000 < burstLen {
			cur[i] = burst
		}
	}
	return &memSource{session.PowerChunk{StartNs: 0, PeriodNs: 10_000, CurrentNA: cur}}
}

func TestComputeStatsConstantCurrent(t *testing.T) {
	src := &memSource{session.PowerChunk{PeriodNs: 10_000, CurrentNA: make([]int32, 100_000)}}
	for i := range src.c.CurrentNA {
		src.c.CurrentNA[i] = 1_000_000 // 1 mA for 1 s
	}
	st, err := ComputeStats(context.Background(), src, 0, 1_000_000_000, 3.3)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != 100_000 || st.MeanNA != 1_000_000 || st.MaxNA != 1_000_000 {
		t.Errorf("stats = %+v", st)
	}
	if math.Abs(st.ChargeUC-1000) > 1 { // 1 mA * 1 s = 1 mC = 1000 µC
		t.Errorf("charge = %v µC, want 1000", st.ChargeUC)
	}
	if !st.HasEnergy || math.Abs(st.EnergyUJ-3300) > 5 { // 3.3 V * 1 mC = 3.3 mJ
		t.Errorf("energy = %v µJ (has=%v), want 3300", st.EnergyUJ, st.HasEnergy)
	}
	st, _ = ComputeStats(context.Background(), src, 0, 1_000_000_000, 0)
	if st.HasEnergy {
		t.Error("energy needs a voltage: nominalV=0 must disable it")
	}
}

func TestEnvelopePreservesShortSpikes(t *testing.T) {
	src := profile(8_000, 16_000_000, 3) // 30 µs bursts, far below one display bucket
	bs, err := Envelope(context.Background(), src, 0, 1_000_000_000, 500)
	if err != nil {
		t.Fatal(err)
	}
	var peaks int
	for _, b := range bs {
		if b.MaxNA >= 16_000_000 {
			peaks++
		}
		if b.N > 0 && b.MinNA > b.MeanNA || b.MeanNA > b.MaxNA {
			t.Fatalf("bucket invariant min<=mean<=max broken: %+v", b)
		}
	}
	if peaks != 10 {
		t.Errorf("envelope shows %d peaks, want 10: decimation would lose them", peaks)
	}
}

func TestDetectSpikes(t *testing.T) {
	src := profile(8_000, 16_000_000, 226) // 2.26 ms bursts
	sp, err := DetectSpikes(context.Background(), src, 0, 1_000_000_000, 3.3, SpikeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp) != 10 {
		t.Fatalf("spikes = %d, want 10", len(sp))
	}
	if sp[0].StartNs < 12_000_000-100_000 || sp[0].StartNs > 12_000_000+100_000 {
		t.Errorf("first spike at %d ns, want ~12 ms", sp[0].StartNs)
	}
	if d := sp[0].DurationNs(); d < 2_000_000 || d > 2_600_000 {
		t.Errorf("first spike lasts %d ns, want ~2.26 ms", d)
	}
	flat := &memSource{session.PowerChunk{PeriodNs: 10_000, CurrentNA: make([]int32, 10_000)}}
	for i := range flat.c.CurrentNA {
		flat.c.CurrentNA[i] = 5_000
	}
	if sp, _ := DetectSpikes(context.Background(), flat, 0, 100_000_000, 3.3, SpikeOptions{}); len(sp) != 0 {
		t.Errorf("flat current has %d spikes", len(sp))
	}
}

func TestDetectIdle(t *testing.T) {
	src := profile(8_000, 16_000_000, 226)
	regs, err := DetectIdle(context.Background(), src, 0, 1_000_000_000, 0.5, 50_000_000, SpikeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) < 9 {
		t.Errorf("idle regions = %d, want >= 9 (between the bursts)", len(regs))
	}
}

func TestFormatCurrent(t *testing.T) {
	for nA, want := range map[float64]string{12: "12.0 nA", 1500: "1.500 µA", 2.5e6: "2.500 mA", 1.2e9: "1.200 A"} {
		if got := FormatCurrent(nA); got != want {
			t.Errorf("FormatCurrent(%v) = %q, want %q", nA, got, want)
		}
	}
}
