package capture

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

func structFromMap(m map[string]any) (*structpb.Struct, error) {
	if m == nil {
		return nil, nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return s, nil
}

// ingest routes one plugin batch into the sample store / timeline.
func (r *Run) ingest(t *runTarget, b *pluginv1.EventBatch) error {
	for _, e := range b.GetEvents() {
		ts := e.GetTimestampNs() + t.ClockOffsetNs
		base := timeline.Event{TimestampNs: ts, SourceID: t.Alias, DeviceID: t.DeviceID, StreamID: e.GetStreamId()}
		switch p := e.GetPayload().(type) {
		case *pluginv1.Event_Power:
			pb := p.Power
			chunk := session.PowerChunk{StartNs: ts, PeriodNs: pb.GetSamplePeriodNs(), CurrentNA: pb.GetCurrentNa(),
				VoltageUV: pb.GetVoltageUv(), Digital: pb.GetDigital()}
			if len(chunk.VoltageUV) != len(chunk.CurrentNA) {
				chunk.VoltageUV = nil
			}
			if len(chunk.Digital) != len(chunk.CurrentNA) {
				chunk.Digital = nil
			}
			ss, err := r.stream(t, e.GetStreamId(), session.KindPower, func(m *session.StreamMeta) { m.DigitalNames = pb.GetDigitalNames() })
			if err != nil {
				return err
			}
			if err := ss.w.WritePower(&chunk); err != nil {
				return fmt.Errorf("write power chunk: %w", err)
			}
			t.addSamples(r, uint64(chunk.N()))
			if chunk.Digital != nil {
				r.deriveEdges(t, base, ss, pb.GetDigitalNames(), chunk.Digital, chunk.PeriodNs)
			}
		case *pluginv1.Event_Analog:
			ab := p.Analog
			ch := len(ab.GetChannels())
			if ch == 0 {
				ch = 1
			}
			chunk := session.AnalogChunk{StartNs: ts, PeriodNs: ab.GetSamplePeriodNs(), Channels: ch, Samples: ab.GetSamples()}
			ss, err := r.stream(t, e.GetStreamId(), session.KindAnalog, func(m *session.StreamMeta) {
				m.Channels, m.Unit = ab.GetChannels(), ab.GetUnit()
			})
			if err != nil {
				return err
			}
			if err := ss.w.WriteAnalog(&chunk); err != nil {
				return fmt.Errorf("write analog chunk: %w", err)
			}
			t.addSamples(r, uint64(chunk.N()))
		case *pluginv1.Event_Digital:
			if err := r.ingestDigital(t, base, e, p.Digital); err != nil {
				return err
			}
		default:
			if ev, ok := convertEvent(base, e); ok {
				r.addEvent(ev)
			}
		}
	}
	return nil
}

// stream returns (creating on first use) the sample writer of a stream.
func (r *Run) stream(t *runTarget, id, kind string, fill func(*session.StreamMeta)) (*streamState, error) {
	if id == "" {
		id = kind
	}
	if ss, ok := t.writers[id]; ok {
		return ss, nil
	}
	meta := session.StreamMeta{ID: id, Kind: kind, Alias: t.Alias, CaptureID: t.Alias}
	if d := t.declared[id]; d != nil {
		meta.Attributes = d.GetAttributes()
	}
	fill(&meta)
	w, meta, err := r.Session.CreateStream(meta)
	if err != nil {
		return nil, fmt.Errorf("create stream %s/%s: %w", t.Alias, id, err)
	}
	ss := &streamState{meta: meta, w: w, names: meta.DigitalNames}
	t.writers[id] = ss
	return ss, nil
}

func (r *Run) ingestDigital(t *runTarget, base timeline.Event, e *pluginv1.Event, d *pluginv1.DigitalBatch) error {
	names := d.GetChannels()
	switch enc := d.GetEncoding().(type) {
	case *pluginv1.DigitalBatch_Sampled:
		ss, err := r.stream(t, e.GetStreamId(), session.KindDigital, func(m *session.StreamMeta) { m.Channels = names })
		if err != nil {
			return err
		}
		chunk := session.DigitalChunk{StartNs: base.TimestampNs, PeriodNs: enc.Sampled.GetSamplePeriodNs(), Words: enc.Sampled.GetWords()}
		if err := ss.w.WriteDigital(&chunk); err != nil {
			return fmt.Errorf("write digital chunk: %w", err)
		}
		t.addSamples(r, uint64(chunk.N()))
		r.deriveEdges(t, base, ss, names, chunk.Words, chunk.PeriodNs)
	case *pluginv1.DigitalBatch_Edges:
		// Edges are already sparse: keep them as timeline events only.
		ss := t.edgeState(e.GetStreamId(), names)
		if !ss.havePrev {
			ss.prevWord, ss.havePrev = enc.Edges.GetInitialState(), true
		}
		prev := ss.prevWord
		for i, off := range enc.Edges.GetOffsetNs() {
			if i >= len(enc.Edges.GetState()) {
				break
			}
			cur := enc.Edges.GetState()[i]
			r.emitEdges(t, base, names, prev, cur, base.TimestampNs+off)
			prev = cur
		}
		ss.prevWord = prev
	}
	return nil
}

// edgeState returns state for streams that only produce edge events.
func (t *runTarget) edgeState(id string, names []string) *streamState {
	if ss, ok := t.edgeOnly[id]; ok {
		return ss
	}
	ss := &streamState{names: names}
	t.edgeOnly[id] = ss
	return ss
}

func (r *Run) deriveEdges(t *runTarget, base timeline.Event, ss *streamState, names []string, words []uint32, period int64) {
	for i, w := range words {
		if !ss.havePrev {
			ss.prevWord, ss.havePrev = w, true
			continue
		}
		if w != ss.prevWord {
			r.emitEdges(t, base, names, ss.prevWord, w, base.TimestampNs+int64(i)*period)
			ss.prevWord = w
		}
	}
}

func (r *Run) emitEdges(t *runTarget, base timeline.Event, names []string, prev, cur uint32, ts int64) {
	diff := prev ^ cur
	for diff != 0 {
		bit := bits.TrailingZeros32(diff)
		diff &= diff - 1
		if r.edges.Add(1) > r.opts.MaxEdgeEvents {
			if r.edges.Load() == r.opts.MaxEdgeEvents+1 {
				r.addEvent(timeline.Event{TimestampNs: ts, SourceID: t.Alias, DeviceID: t.DeviceID, Type: "warning",
					Summary: "digital edge event limit reached; further edges are only in raw samples"})
			}
			return
		}
		name := fmt.Sprintf("D%d", bit)
		if bit < len(names) && names[bit] != "" {
			name = names[bit]
		}
		level := cur>>bit&1 == 1
		dir := "falling"
		if level {
			dir = "rising"
		}
		ev := base
		ev.TimestampNs = ts
		ev.Type = timeline.TypeDigitalEdge
		ev.Summary = name + " " + dir
		ev.Data = map[string]any{"channel": name, "bit": bit, "level": level}
		r.addEvent(ev)
	}
}

// convertEvent maps low-rate payloads onto timeline events.
func convertEvent(base timeline.Event, e *pluginv1.Event) (timeline.Event, bool) {
	ev := base
	switch p := e.GetPayload().(type) {
	case *pluginv1.Event_Packet:
		pk := p.Packet
		ev.Type = timeline.TypePacket
		ev.Summary = pk.GetSummary()
		if ev.Summary == "" {
			ev.Summary = fmt.Sprintf("%s len=%d", pk.GetProtocol(), len(pk.GetData()))
		}
		ev.Data = map[string]any{"protocol": pk.GetProtocol(), "link_type": pk.GetLinkType(), "len": len(pk.GetData()),
			"hex": hex.EncodeToString(pk.GetData()), "rssi_dbm": pk.GetRssiDbm(), "lqi": pk.GetLqi(), "channel": pk.GetChannel()}
		for k, v := range pk.GetAttributes() {
			ev.Data["attr."+k] = v
		}
	case *pluginv1.Event_Log:
		l := p.Log
		ev.Type = timeline.TypeLog
		ev.Summary = l.GetMessage()
		ev.Data = map[string]any{"level": l.GetLevel(), "log_source": l.GetSource()}
		for k, v := range l.GetAttributes() {
			ev.Data["attr."+k] = v
		}
	case *pluginv1.Event_Marker:
		m := p.Marker
		ev.Type = timeline.TypeMarker
		ev.Summary = m.GetName()
		ev.Data = map[string]any{"name": m.GetName(), "description": m.GetDescription()}
	case *pluginv1.Event_Decoded:
		d := p.Decoded
		ev.Type = timeline.TypeDecoded
		ev.DurationNs = d.GetDurationNs()
		ev.Summary = d.GetSummary()
		ev.Data = map[string]any{"protocol": d.GetProtocol()}
		if d.GetFields() != nil {
			ev.Data["fields"] = d.GetFields().AsMap()
		}
	case *pluginv1.Event_Trigger:
		tr := p.Trigger
		ev.Type = timeline.TypeTrigger
		edge := "falling"
		if tr.GetRising() {
			edge = "rising"
		}
		ev.Summary = tr.GetName() + " " + edge
		ev.Data = map[string]any{"name": tr.GetName(), "rising": tr.GetRising(), "description": tr.GetDescription()}
	case *pluginv1.Event_Debug:
		d := p.Debug
		ev.Type = timeline.TypeDebug
		ev.Summary = d.GetKind()
		if d.GetDescription() != "" {
			ev.Summary += ": " + d.GetDescription()
		}
		ev.Data = map[string]any{"kind": d.GetKind(), "pc": d.GetPc()}
	case *pluginv1.Event_Generic:
		g := p.Generic
		if g.GetContentType() == pluginv1.ContentTypeTimelineEvent {
			var re timeline.Event
			if err := json.Unmarshal(g.GetData(), &re); err != nil {
				return ev, false
			}
			re.TimestampNs, re.SourceID, re.DeviceID, re.Seq = base.TimestampNs, base.SourceID, base.DeviceID, 0
			return re, true
		}
		ev.Type = pluginv1.EventType(e)
		ev.Summary = fmt.Sprintf("%s (%d bytes)", g.GetContentType(), len(g.GetData()))
		ev.Data = map[string]any{"content_type": g.GetContentType(), "size": len(g.GetData())}
	default:
		return ev, false
	}
	if e.GetType() != "" {
		ev.Type = e.GetType()
	}
	return ev, true
}

func (t *runTarget) addSamples(r *Run, n uint64) {
	r.samples.Add(n)
	t.nsamples.Add(n)
}
