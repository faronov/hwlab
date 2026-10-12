package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/correlation"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

// ExplainQuery asks "what happened around this point?". Exactly one anchor is
// used: Spike (1-based, in time order), Packet (1-based, in time order) or AtNs.
type ExplainQuery struct {
	Session        string
	Source         string // power alias; empty = first power stream
	Spike          int
	Packet         int
	AtNs           int64
	HasAt          bool
	WindowNs       int64 // context window (default 5 ms)
	NominalVoltage float64
}

// SpikeInfo describes one detected current spike.
type SpikeInfo struct {
	Index      int     `json:"index"`
	Total      int     `json:"total"`
	StartNs    int64   `json:"start_ns"`
	EndNs      int64   `json:"end_ns"`
	DurationNs int64   `json:"duration_ns"`
	PeakNA     float64 `json:"peak_na"`
	PeakAtNs   int64   `json:"peak_at_ns"`
	MeanNA     float64 `json:"mean_na"`
	BaselineNA float64 `json:"baseline_na"`
	ChargeUC   float64 `json:"charge_uc"`
	EnergyUJ   float64 `json:"energy_uj,omitempty"`
	HasEnergy  bool    `json:"has_energy"`
	// DistanceNs is set when the spike was chosen as the nearest to the anchor.
	DistanceNs int64 `json:"distance_ns,omitempty"`
}

// PacketRef is a packet near the anchor. Payload bytes are never included.
type PacketRef struct {
	Index   int     `json:"index"` // 1-based position among all packets
	TimeNs  int64   `json:"t_ns"`
	DeltaNs int64   `json:"delta_ns"` // packet time - anchor time
	Source  string  `json:"source"`
	Kind    string  `json:"kind"`
	Bytes   int     `json:"bytes"`
	RSSIdBm float64 `json:"rssi_dbm"`
	LQI     uint32  `json:"lqi"`
}

// CurrentContext is the current just before and after the anchor.
type CurrentContext struct {
	WindowNs   int64   `json:"window_ns"`
	BeforeMean float64 `json:"before_mean_na"`
	BeforePeak float64 `json:"before_peak_na"`
	AfterMean  float64 `json:"after_mean_na"`
	AfterPeak  float64 `json:"after_peak_na"`
	HasData    bool    `json:"has_data"`
}

// EnergyContext is the charge and energy around the anchor.
type EnergyContext struct {
	NominalV float64 `json:"nominal_voltage,omitempty"`
	// Burst is the spike itself (zero when the anchor is not a spike).
	BurstUJ     float64 `json:"burst_uj,omitempty"`
	BurstUC     float64 `json:"burst_uc,omitempty"`
	WindowUJ    float64 `json:"window_uj,omitempty"`
	WindowUC    float64 `json:"window_uc"`
	WindowFrom  int64   `json:"window_from_ns"`
	WindowTo    int64   `json:"window_to_ns"`
	GapUJ       float64 `json:"gap_uj,omitempty"` // previous spike end -> this spike start
	GapUC       float64 `json:"gap_uc,omitempty"`
	GapMeanNA   float64 `json:"gap_mean_na,omitempty"`
	GapNs       int64   `json:"gap_ns,omitempty"`
	HasEnergy   bool    `json:"has_energy"`
	Description string  `json:"description"`
}

// Timing carries the cross-device uncertainty of the headline delta.
type Timing struct {
	Reference    string `json:"reference"`    // what the delta is measured to
	DeltaNs      int64  `json:"delta_ns"`     // stamped: packet - anchor
	TrueLowNs    int64  `json:"true_low_ns"`  // valid only when LowBounded
	TrueHighNs   int64  `json:"true_high_ns"` // valid only when HighBounded
	LowBounded   bool   `json:"low_bounded"`
	HighBounded  bool   `json:"high_bounded"`
	Order        string `json:"order"` // after | before | undetermined
	Statement    string `json:"statement"`
	PowerClock   string `json:"power_clock,omitempty"`
	PacketsClock string `json:"packets_clock,omitempty"`
}

// Explanation answers an ExplainQuery.
type Explanation struct {
	Session  string         `json:"session"`
	Anchor   string         `json:"anchor"` // spike | packet | time
	AnchorNs int64          `json:"anchor_ns"`
	Spike    *SpikeInfo     `json:"spike,omitempty"`
	Before   []PacketRef    `json:"packets_before"`
	After    []PacketRef    `json:"packets_after"`
	Nearest  *PacketRef     `json:"nearest_packet,omitempty"`
	InBurst  map[string]int `json:"packets_during_burst,omitempty"`
	FirstTx  *PacketRef     `json:"first_data_after,omitempty"`
	AckAfter *PacketRef     `json:"ack_after_data,omitempty"`
	DataAck  *int64         `json:"data_to_ack_ns,omitempty"`
	Current  CurrentContext `json:"current"`
	Energy   EnergyContext  `json:"energy"`
	Timing   *Timing        `json:"timing,omitempty"`
	Clocks   []ClockSummary `json:"clocks"`
	Notes    []string       `json:"notes,omitempty"`
}

// ClockSummary is the timing quality of one capture.
type ClockSummary struct {
	Capture string             `json:"capture"`
	Clock   *session.ClockMeta `json:"clock,omitempty"`
}

var packetFilter = timeline.Filter{Types: []string{timeline.TypePacket}}

// Explain relates a current spike (or a time, or a packet) to nearby radio
// activity, energy and clock uncertainty.
func (a *App) Explain(ctx context.Context, q ExplainQuery) (_ *Explanation, retErr error) {
	s, err := a.OpenSession(q.Session)
	if err != nil {
		return nil, err
	}
	meta := s.Meta()
	window := q.WindowNs
	if window <= 0 {
		window = 5_000_000
	}
	streams := s.StreamsByKind(session.KindPower, q.Source)
	if len(streams) == 0 {
		return nil, fmt.Errorf("session has no power streams%s", suffix(q.Source))
	}
	sm := streams[0]
	rd, err := s.OpenStream(sm)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rd.Close()) }()

	spikes, err := analysis.DetectSpikes(ctx, rd, 0, 0, q.NominalVoltage, analysis.SpikeOptions{})
	if err != nil {
		return nil, err
	}
	sort.Slice(spikes, func(i, j int) bool { return spikes[i].StartNs < spikes[j].StartNs })

	tl := s.Timeline()
	allPk := tl.Between(math.MinInt64, math.MaxInt64, packetFilter, 0)
	idxOf := func(e timeline.Event) int {
		i := sort.Search(len(allPk), func(i int) bool {
			if allPk[i].TimestampNs != e.TimestampNs {
				return allPk[i].TimestampNs >= e.TimestampNs
			}
			return allPk[i].Seq >= e.Seq
		})
		return i + 1
	}
	ref := func(e timeline.Event, anchor int64) PacketRef {
		rssi, _ := e.Data["rssi_dbm"].(float64)
		lqi, _ := e.Data["lqi"].(float64)
		n, _ := e.Data["len"].(float64)
		return PacketRef{Index: idxOf(e), TimeNs: e.TimestampNs, DeltaNs: e.TimestampNs - anchor, Source: e.SourceID,
			Kind: packetKind(e), Bytes: int(n), RSSIdBm: rssi, LQI: uint32(lqi)}
	}

	ex := &Explanation{Session: meta.ID}
	var spk *analysis.Spike
	switch {
	case q.Spike > 0:
		if q.Spike > len(spikes) {
			return nil, fmt.Errorf("spike #%d requested but only %d spikes were detected", q.Spike, len(spikes))
		}
		spk = &spikes[q.Spike-1]
		ex.Anchor, ex.AnchorNs = "spike", spk.StartNs
		ex.Spike = spikeInfo(spk, q.Spike, len(spikes), 0)
	case q.Packet > 0:
		if q.Packet > len(allPk) {
			return nil, fmt.Errorf("packet #%d requested but the session has %d packets", q.Packet, len(allPk))
		}
		ex.Anchor, ex.AnchorNs = "packet", allPk[q.Packet-1].TimestampNs
	case q.HasAt:
		ex.Anchor, ex.AnchorNs = "time", q.AtNs
	default:
		return nil, fmt.Errorf("choose an anchor: a spike number, a packet number or a time")
	}
	if spk == nil && len(spikes) > 0 {
		best, bd := 0, int64(math.MaxInt64)
		for i, sp := range spikes {
			d := distanceTo(sp.StartNs, sp.EndNs, ex.AnchorNs)
			if d < bd {
				best, bd = i, d
			}
		}
		ex.Spike = spikeInfo(&spikes[best], best+1, len(spikes), bd)
		spk = &spikes[best]
	}
	anchor := ex.AnchorNs

	for _, h := range correlation.Preceding(tl, anchor, window, packetFilter, 5) {
		ex.Before = append(ex.Before, ref(h.Event, anchor))
	}
	following := correlation.Following(tl, anchor, window, packetFilter, 50)
	for i, h := range following {
		if i < 5 {
			ex.After = append(ex.After, ref(h.Event, anchor))
		}
	}
	if h, ok := correlation.Nearest(tl, anchor, packetFilter); ok {
		r := ref(h.Event, anchor)
		ex.Nearest = &r
	}
	for i, h := range following {
		if packetKind(h.Event) != "data" {
			continue
		}
		r := ref(h.Event, anchor)
		ex.FirstTx = &r
		for _, g := range following[i+1:] {
			if packetKind(g.Event) == "ack" {
				ra := ref(g.Event, anchor)
				ex.AckAfter = &ra
				d := g.Event.TimestampNs - h.Event.TimestampNs
				ex.DataAck = &d
				break
			}
		}
		break
	}
	if ex.Anchor == "spike" {
		ex.InBurst = map[string]int{}
		for _, e := range tl.Between(spk.StartNs, spk.EndNs+1, packetFilter, 0) {
			ex.InBurst[packetKind(e)]++
		}
	}

	if err := a.fillCurrentAndEnergy(ctx, rd, ex, spk, spikes, window, q.NominalVoltage); err != nil {
		return nil, err
	}

	ex.Clocks = clocksOf(meta, sm.Alias, allPk)
	ex.Timing = timingFor(meta, sm.Alias, ex)
	if ex.Timing == nil && len(allPk) == 0 {
		ex.Notes = append(ex.Notes, "the session has no packet events")
	}
	if ex.Spike != nil && ex.Anchor != "spike" {
		ex.Notes = append(ex.Notes, fmt.Sprintf("nearest detected spike is #%d, %s away", ex.Spike.Index, timeline.FormatNs(ex.Spike.DistanceNs)))
	}
	if len(spikes) == 0 {
		ex.Notes = append(ex.Notes, "no current spikes were detected: baseline noise only, or no device under test is drawing current")
	}
	return ex, nil
}

func distanceTo(start, end, t int64) int64 {
	switch {
	case t < start:
		return start - t
	case t > end:
		return t - end
	}
	return 0
}

func spikeInfo(sp *analysis.Spike, idx, total int, dist int64) *SpikeInfo {
	return &SpikeInfo{Index: idx, Total: total, StartNs: sp.StartNs, EndNs: sp.EndNs, DurationNs: sp.DurationNs(),
		PeakNA: sp.PeakNA, PeakAtNs: sp.PeakAtNs, MeanNA: sp.MeanNA, BaselineNA: sp.BaselineNA, ChargeUC: sp.ChargeUC,
		EnergyUJ: sp.EnergyUJ, HasEnergy: sp.HasEnergy, DistanceNs: dist}
}

func (a *App) fillCurrentAndEnergy(ctx context.Context, rd analysis.PowerSource, ex *Explanation, spk *analysis.Spike,
	spikes []analysis.Spike, window int64, nomV float64) error {
	t := ex.AnchorNs
	bef, err := analysis.ComputeStats(ctx, rd, t-window, t, nomV)
	if err != nil {
		return err
	}
	aft, err := analysis.ComputeStats(ctx, rd, t, t+window, nomV)
	if err != nil {
		return err
	}
	ex.Current = CurrentContext{WindowNs: window, HasData: bef.Samples > 0 || aft.Samples > 0}
	if bef.Samples > 0 {
		ex.Current.BeforeMean, ex.Current.BeforePeak = bef.MeanNA, bef.MaxNA
	}
	if aft.Samples > 0 {
		ex.Current.AfterMean, ex.Current.AfterPeak = aft.MeanNA, aft.MaxNA
	}

	from, to := t-window, t+window
	if ex.Anchor == "spike" {
		from, to = spk.StartNs-window, spk.EndNs+window
	}
	w, err := analysis.ComputeStats(ctx, rd, from, to, nomV)
	if err != nil {
		return err
	}
	e := EnergyContext{NominalV: nomV, WindowFrom: from, WindowTo: to, WindowUC: w.ChargeUC, WindowUJ: w.EnergyUJ, HasEnergy: w.HasEnergy}
	if ex.Anchor == "spike" {
		e.BurstUC, e.BurstUJ = spk.ChargeUC, spk.EnergyUJ
		if idx := ex.Spike.Index; idx > 1 {
			prev := spikes[idx-2]
			if g, err := analysis.ComputeStats(ctx, rd, prev.EndNs, spk.StartNs, nomV); err == nil && g.Samples > 0 {
				e.GapNs, e.GapMeanNA, e.GapUC, e.GapUJ = spk.StartNs-prev.EndNs, g.MeanNA, g.ChargeUC, g.EnergyUJ
			}
		}
	}
	e.Description = "window = " + timeline.FormatNs(from) + " .. " + timeline.FormatNs(to)
	if !w.HasEnergy {
		e.Description += "; energy needs a voltage: pass --nominal-voltage (charge is exact)"
	}
	ex.Energy = e
	return nil
}

// packetKind classifies a packet event without exposing its payload. Only
// IEEE 802.15.4 frames are classified; others are reported as "frame".
func packetKind(e timeline.Event) string {
	if p, _ := e.Data["protocol"].(string); p != "ieee802154" {
		return "frame"
	}
	h, _ := e.Data["hex"].(string)
	if len(h) < 2 {
		return "frame"
	}
	b, err := hex.DecodeString(h[:2])
	if err != nil {
		return "frame"
	}
	switch b[0] & 0x07 {
	case 0:
		return "beacon"
	case 1:
		return "data"
	case 2:
		return "ack"
	case 3:
		return "command"
	}
	return "frame"
}

func clocksOf(meta session.Meta, powerAlias string, pk []timeline.Event) []ClockSummary {
	seen := map[string]bool{powerAlias: true}
	out := []ClockSummary{{Capture: powerAlias, Clock: captureClock(meta, powerAlias)}}
	for _, e := range pk {
		if !seen[e.SourceID] {
			seen[e.SourceID] = true
			out = append(out, ClockSummary{Capture: e.SourceID, Clock: captureClock(meta, e.SourceID)})
		}
	}
	return out
}

func captureClock(meta session.Meta, alias string) *session.ClockMeta {
	for _, c := range meta.Captures {
		if c.Alias == alias {
			return c.Clock
		}
	}
	return nil
}

func boundOf(c *session.ClockMeta) correlation.Bound {
	if c == nil {
		return correlation.Bound{}
	}
	return correlation.Bound{Known: true, MinNs: c.ErrorMinNs, MinKnown: c.ErrorMinBounded,
		MaxNs: c.ErrorMaxNs, MaxKnown: c.ErrorMaxBounded, Source: c.Source}
}

// timingFor bounds the real delta of the headline packet: the first DATA frame
// after the anchor, else the nearest packet.
func timingFor(meta session.Meta, powerAlias string, ex *Explanation) *Timing {
	head, what := ex.FirstTx, "first DATA after the anchor"
	if head == nil {
		head, what = ex.Nearest, "nearest packet"
	}
	if head == nil {
		return nil
	}
	pc, rc := captureClock(meta, powerAlias), captureClock(meta, head.Source)
	r := correlation.TrueDelta(head.DeltaNs, boundOf(pc), boundOf(rc))
	t := &Timing{Reference: what, DeltaNs: head.DeltaNs, TrueLowNs: r.LowNs, TrueHighNs: r.HighNs,
		LowBounded: r.LowKnown, HighBounded: r.HighKnown, Order: r.Order()}
	if pc != nil {
		t.PowerClock = pc.Source
	}
	if rc != nil {
		t.PacketsClock = rc.Source
	}
	switch {
	case pc == nil || rc == nil:
		t.Statement = "a clock model is missing for one source, so the delta has no stated uncertainty"
	case r.LowKnown && r.HighKnown:
		w, _ := r.Width()
		t.Statement = fmt.Sprintf("true delta lies within %s (width %s); events closer than that cannot be ordered", r, timeline.FormatNs(w))
	default:
		t.Statement = fmt.Sprintf("true delta lies within %s: one clock has no known bound (the sniffer stamps late by an unknown USB latency), "+
			"so the packet may really have happened earlier than shown; order is %s", r, t.Order)
	}
	return t
}
