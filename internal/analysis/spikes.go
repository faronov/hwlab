package analysis

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

// SpikeOptions configures spike detection. Zero values pick defaults.
type SpikeOptions struct {
	// BucketNs is the analysis resolution (default 50 µs, widened so that at
	// most MaxBuckets buckets are used).
	BucketNs   int64
	MaxBuckets int
	// Factor: a bucket is "high" when its max exceeds baseline*Factor ...
	Factor float64
	// MinDeltaNA: ... and baseline+MinDeltaNA (default 50 µA).
	MinDeltaNA float64
	// MergeGapNs merges high regions closer than this (default 3 buckets).
	MergeGapNs int64
}

func (o *SpikeOptions) defaults() {
	if o.BucketNs <= 0 {
		o.BucketNs = 50_000
	}
	if o.MaxBuckets <= 0 {
		o.MaxBuckets = 200_000
	}
	if o.Factor <= 0 {
		o.Factor = 3
	}
	if o.MinDeltaNA <= 0 {
		o.MinDeltaNA = 50_000
	}
}

// Spike is a contiguous high-current region.
type Spike struct {
	StartNs    int64   `json:"start_ns"`
	EndNs      int64   `json:"end_ns"`
	PeakNA     float64 `json:"peak_na"`
	PeakAtNs   int64   `json:"peak_at_ns"`
	MeanNA     float64 `json:"mean_na"`
	BaselineNA float64 `json:"baseline_na"`
	ChargeUC   float64 `json:"charge_uc"`
	EnergyUJ   float64 `json:"energy_uj,omitempty"`
	HasEnergy  bool    `json:"has_energy"`
}

func (s Spike) DurationNs() int64 { return s.EndNs - s.StartNs }

// Baseline estimates the idle current as the median bucket mean.
func Baseline(buckets []Bucket) float64 {
	var vals []float64
	for _, b := range buckets {
		if b.N > 0 {
			vals = append(vals, b.MeanNA)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	return vals[len(vals)/2]
}

func prepare(ctx context.Context, src PowerSource, from, to int64, o *SpikeOptions) ([]Bucket, float64, error) {
	o.defaults()
	if to <= from {
		from, to = src.Bounds()
	}
	n := int((to - from) / o.BucketNs)
	if n < 1 {
		n = 1
	}
	if n > o.MaxBuckets {
		n = o.MaxBuckets
	}
	bs, err := Envelope(ctx, src, from, to, n)
	if err != nil {
		return nil, 0, err
	}
	return bs, Baseline(bs), nil
}

// DetectSpikes finds high-current regions in [from,to) (to<=from: whole
// stream) and refines their edges at sample resolution.
func DetectSpikes(ctx context.Context, src PowerSource, from, to int64, nominalV float64, o SpikeOptions) ([]Spike, error) {
	bs, base, err := prepare(ctx, src, from, to, &o)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	thr := math.Max(base*o.Factor, base+o.MinDeltaNA)
	gap := o.MergeGapNs
	if gap <= 0 {
		gap = 3 * (bs[0].EndNs - bs[0].StartNs)
	}
	type region struct{ s, e int64 }
	var regs []region
	for _, b := range bs {
		if b.N == 0 || b.MaxNA <= thr {
			continue
		}
		if n := len(regs); n > 0 && b.StartNs-regs[n-1].e <= gap {
			regs[n-1].e = b.EndNs
			continue
		}
		regs = append(regs, region{b.StartNs, b.EndNs})
	}
	var out []Spike
	for _, r := range regs {
		sp := Spike{BaselineNA: base, StartNs: -1}
		var sum float64
		var cnt int64
		err := src.ReadPower(ctx, r.s, r.e, func(c *session.PowerChunk) error {
			for i, v := range c.CurrentNA {
				t := c.StartNs + int64(i)*c.PeriodNs
				if t < r.s || t >= r.e || float64(v) <= thr {
					continue
				}
				if sp.StartNs < 0 {
					sp.StartNs = t
				}
				sp.EndNs = t + c.PeriodNs
				if f := float64(v); f > sp.PeakNA {
					sp.PeakNA, sp.PeakAtNs = f, t
				}
				sum += float64(v)
				cnt++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if cnt == 0 {
			continue
		}
		sp.MeanNA = sum / float64(cnt)
		st, err := ComputeStats(ctx, src, sp.StartNs, sp.EndNs, nominalV)
		if err != nil {
			return nil, err
		}
		sp.ChargeUC, sp.EnergyUJ, sp.HasEnergy = st.ChargeUC, st.EnergyUJ, st.HasEnergy
		out = append(out, sp)
	}
	return out, nil
}

// Region is an idle (low-current) period.
type Region struct {
	StartNs int64   `json:"start_ns"`
	EndNs   int64   `json:"end_ns"`
	MeanNA  float64 `json:"mean_na"`
}

// DetectIdle finds periods where the bucket mean stays within
// baseline*tolerance (default 1.5) for at least minDurNs.
func DetectIdle(ctx context.Context, src PowerSource, from, to int64, tolerance float64, minDurNs int64, o SpikeOptions) ([]Region, error) {
	bs, base, err := prepare(ctx, src, from, to, &o)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	if tolerance <= 0 {
		tolerance = 1.5
	}
	limit := math.Max(base*tolerance, base+1000)
	var out []Region
	var cur *Region
	var sum float64
	var cnt int
	flush := func() {
		if cur != nil && cnt > 0 && cur.EndNs-cur.StartNs >= minDurNs {
			cur.MeanNA = sum / float64(cnt)
			out = append(out, *cur)
		}
		cur, sum, cnt = nil, 0, 0
	}
	for _, b := range bs {
		if b.N > 0 && b.MaxNA <= limit {
			if cur == nil {
				cur = &Region{StartNs: b.StartNs}
			}
			cur.EndNs = b.EndNs
			sum += b.MeanNA
			cnt++
			continue
		}
		flush()
	}
	flush()
	return out, nil
}

// AnnotatePower runs spike detection on every power stream of a writable
// session and appends analysis events ("current rises ...", "current returns
// ...") to the timeline. It returns the number of spikes found.
func AnnotatePower(ctx context.Context, sess *session.Session, nominalV float64, o SpikeOptions) (int, error) {
	total := 0
	for _, sm := range sess.StreamsByKind(session.KindPower, "") {
		rd, err := sess.OpenStream(sm)
		if err != nil {
			return total, err
		}
		spikes, err := DetectSpikes(ctx, rd, 0, 0, nominalV, o)
		rd.Close()
		if err != nil {
			return total, fmt.Errorf("analyze %s: %w", sm.ID, err)
		}
		for _, sp := range spikes {
			data := map[string]any{"start_ns": sp.StartNs, "end_ns": sp.EndNs, "duration_ns": sp.DurationNs(),
				"peak_na": sp.PeakNA, "mean_na": sp.MeanNA, "baseline_na": sp.BaselineNA, "charge_uc": sp.ChargeUC}
			if sp.HasEnergy {
				data["energy_uj"] = sp.EnergyUJ
			}
			base := timeline.Event{SourceID: sm.Alias, StreamID: sm.ID, DurationNs: sp.DurationNs(), Data: data}
			rise, fall := base, base
			rise.TimestampNs, rise.Type = sp.StartNs, timeline.TypePowerRise
			rise.Summary = fmt.Sprintf("current rises %s -> %s", FormatCurrent(sp.BaselineNA), FormatCurrent(sp.PeakNA))
			fall.TimestampNs, fall.Type, fall.DurationNs = sp.EndNs, timeline.TypePowerFall, 0
			fall.Summary = fmt.Sprintf("current returns to %s", FormatCurrent(sp.BaselineNA))
			for _, e := range []timeline.Event{rise, fall} {
				if _, err := sess.AppendEvent(e); err != nil {
					return total, err
				}
			}
			total++
		}
	}
	return total, sess.Flush()
}
