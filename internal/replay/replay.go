// Package replay is an in-process plugin backend that re-emits a recorded
// session. Devices are the original experiment aliases; captures replay the
// stored sample streams and timeline events with their original timestamps,
// so every analysis, plot and correlation can run without hardware.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// PluginID is the id replay registers under.
const PluginID = "replay"

const eventBatchSize = 256

type capture struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Backend replays one session. It implements plugin.Backend.
type Backend struct {
	sess *session.Session

	mu       sync.Mutex
	captures map[string]*capture
	done     chan struct{}
}

// New opens the session at dir read-only.
func New(dir string) (*Backend, error) {
	s, err := session.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Backend{sess: s, captures: map[string]*capture{}, done: make(chan struct{})}, nil
}

// Session returns the source session.
func (b *Backend) Session() *session.Session { return b.sess }

func (b *Backend) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: PluginID, Name: "hwlab session replay", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: []*pluginv1.Capability{{Id: pluginv1.CapOfflineCapture, Version: 1}, {Id: pluginv1.CapTimedCapture, Version: 1}},
	}
}

func (b *Backend) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, d := range b.sess.Meta().Devices {
		dev := &pluginv1.Device{Id: d.Alias, Name: fmt.Sprintf("replay of %s (%s/%s)", d.Alias, d.PluginID, d.DeviceID),
			Transport: "session", Properties: map[string]string{"session": b.sess.ID(), "origin_plugin": d.PluginID}}
		for _, c := range d.Capabilities {
			dev.Capabilities = append(dev.Capabilities, &pluginv1.Capability{Id: c, Version: 1})
		}
		dev.Capabilities = append(dev.Capabilities, &pluginv1.Capability{Id: pluginv1.CapOfflineCapture, Version: 1})
		out = append(out, dev)
	}
	return out, nil
}

func (b *Backend) hasDevice(id string) bool {
	for _, d := range b.sess.Meta().Devices {
		if d.Alias == id {
			return true
		}
	}
	return false
}

func (b *Backend) OpenDevice(_ context.Context, id string, _ map[string]any) error {
	if !b.hasDevice(id) {
		return fmt.Errorf("replay: session %s has no device %q", b.sess.ID(), id)
	}
	return nil
}

func (b *Backend) CloseDevice(context.Context, string) error { return nil }

func (b *Backend) StartCapture(_ context.Context, req *pluginv1.StartCaptureRequest) (*pluginv1.StartCaptureResponse, error) {
	if !b.hasDevice(req.GetDeviceId()) {
		return nil, fmt.Errorf("replay: unknown device %q", req.GetDeviceId())
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	b.captures[req.GetCaptureId()] = &capture{ctx: ctx, cancel: cancel}
	b.mu.Unlock()
	resp := &pluginv1.StartCaptureResponse{StartedSessionNs: req.GetSessionTimeNs()}
	return resp, nil
}

func (b *Backend) StopCapture(_ context.Context, id string) (*pluginv1.StopCaptureResponse, error) {
	b.mu.Lock()
	c := b.captures[id]
	delete(b.captures, id)
	b.mu.Unlock()
	if c != nil {
		c.cancel()
	}
	res := &pluginv1.StopCaptureResponse{}
	for _, cm := range b.sess.Meta().Captures {
		if cm.Alias == id && cm.Clock != nil {
			k := cm.Clock
			note := k.Note
			if k.OffsetAppliedNs != 0 {
				note += fmt.Sprintf(" (a clock offset of %d ns was applied when this was recorded; the bounds include it)", k.OffsetAppliedNs)
			}
			res.Clock = &pluginv1.ClockInfo{Source: k.Source, ResolutionNs: k.ResolutionNs,
				ErrorMinNs: k.ErrorMinNs, ErrorMinBounded: k.ErrorMinBounded, ErrorMaxNs: k.ErrorMaxNs, ErrorMaxBounded: k.ErrorMaxBounded,
				JitterNs: k.JitterNs, DriftPpm: k.DriftPPM, Note: note}
		}
	}
	for _, a := range b.sess.Artifacts.List() {
		if a.SourceID != id {
			continue
		}
		res.Artifacts = append(res.Artifacts, &pluginv1.ArtifactRef{Name: a.Name, Kind: a.Kind, MimeType: a.MimeType,
			Path: b.sess.Artifacts.Abs(a), Metadata: a.Metadata})
	}
	return res, nil
}

// StreamEvents replays the device recorded under captureID (the alias).
func (b *Backend) StreamEvents(ctx context.Context, alias string, fn func(*pluginv1.EventBatch) error) error {
	b.mu.Lock()
	c := b.captures[alias]
	b.mu.Unlock()
	if c == nil {
		return fmt.Errorf("replay: capture %q is not running", alias)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-c.ctx.Done():
			stop()
		case <-ctx.Done():
		}
	}()

	streams := map[string]bool{}
	for _, sm := range b.sess.Meta().Captures {
		if sm.Alias != alias {
			continue
		}
		for _, st := range sm.Streams {
			rd, err := b.sess.OpenStream(st)
			if err != nil {
				return err
			}
			err = replayStream(ctx, st, rd, fn)
			rd.Close()
			if err != nil {
				return err
			}
			if st.Kind == session.KindDigital || st.Kind == session.KindPower {
				streams[st.ID] = true
			}
		}
	}

	evs := b.sess.Timeline().All()
	batch := &pluginv1.EventBatch{}
	flush := func() error {
		if len(batch.Events) == 0 {
			return nil
		}
		err := fn(batch)
		batch = &pluginv1.EventBatch{}
		return err
	}
	for _, e := range evs {
		if e.SourceID != alias || ctx.Err() != nil {
			continue
		}
		// Edges derived from stored samples and analysis results are
		// regenerated by the core; replaying them would duplicate them.
		if e.Type == timeline.TypeDigitalEdge && streams[e.StreamID] {
			continue
		}
		if e.Type == timeline.TypePowerRise || e.Type == timeline.TypePowerFall {
			continue
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		batch.Events = append(batch.Events, &pluginv1.Event{TimestampNs: e.TimestampNs, StreamId: e.StreamID, Type: e.Type,
			Payload: &pluginv1.Event_Generic{Generic: &pluginv1.GenericPayload{ContentType: pluginv1.ContentTypeTimelineEvent, Data: data}}})
		if len(batch.Events) >= eventBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func replayStream(ctx context.Context, st session.StreamMeta, rd session.StreamReader, fn func(*pluginv1.EventBatch) error) error {
	emit := func(ts int64, p *pluginv1.Event) error {
		p.TimestampNs, p.StreamId = ts, st.ID
		return fn(&pluginv1.EventBatch{Events: []*pluginv1.Event{p}})
	}
	switch st.Kind {
	case session.KindPower:
		return rd.ReadPower(ctx, math.MinInt64, math.MaxInt64, func(c *session.PowerChunk) error {
			pb := &pluginv1.PowerBatch{SamplePeriodNs: c.PeriodNs, CurrentNa: c.CurrentNA, VoltageUv: c.VoltageUV,
				Digital: c.Digital, DigitalNames: st.DigitalNames}
			return emit(c.StartNs, &pluginv1.Event{Payload: &pluginv1.Event_Power{Power: pb}})
		})
	case session.KindAnalog:
		return rd.ReadAnalog(ctx, math.MinInt64, math.MaxInt64, func(c *session.AnalogChunk) error {
			pb := &pluginv1.AnalogBatch{SamplePeriodNs: c.PeriodNs, Channels: st.Channels, Unit: st.Unit, Samples: c.Samples}
			return emit(c.StartNs, &pluginv1.Event{Payload: &pluginv1.Event_Analog{Analog: pb}})
		})
	case session.KindDigital:
		return rd.ReadDigital(ctx, math.MinInt64, math.MaxInt64, func(c *session.DigitalChunk) error {
			pb := &pluginv1.DigitalBatch{Channels: st.Channels, Encoding: &pluginv1.DigitalBatch_Sampled{
				Sampled: &pluginv1.DigitalSampled{SamplePeriodNs: c.PeriodNs, Words: c.Words}}}
			return emit(c.StartNs, &pluginv1.Event{Payload: &pluginv1.Event_Digital{Digital: pb}})
		})
	}
	return nil
}

func (b *Backend) Invoke(context.Context, *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	return nil, errors.New("replay: devices are read-only; Invoke is not supported")
}

func (b *Backend) Done() <-chan struct{} { return b.done }

func (b *Backend) Close(context.Context) error { return nil }
