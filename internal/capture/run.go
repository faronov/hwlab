// Package capture runs synchronized captures on plugin devices and ingests
// their events into a session: high-rate batches go to the sample store,
// low-rate events to the unified timeline.
package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/faronov/hwlab/internal/artifact"
	"github.com/faronov/hwlab/internal/plugin"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Target is one device to capture from.
type Target struct {
	Alias        string // name on the timeline (experiment device key)
	PluginID     string
	DeviceID     string // plugin-local device id
	DeviceName   string
	Capabilities []string
	Backend      plugin.Backend
	Config       map[string]any
	// ClockOffsetNs is added to every plugin timestamp on ingest.
	ClockOffsetNs int64
	// Control marks a device without a capture stream (a debug probe, an
	// instrument): it is opened and closed with the run and may be the target
	// of steps, but is never started or streamed.
	Control bool
}

// Step is a scheduled method call executed during the run. Danger approval
// is the caller's job; the run only executes and records.
type Step struct {
	Name       string
	At         time.Duration
	Alias      string
	Capability string
	Method     string
	Args       map[string]any
}

// Options tunes a run.
type Options struct {
	Duration time.Duration // 0 = until Stop
	Logger   *slog.Logger
	// MaxEdgeEvents bounds derived digital edge events per run (default 1e6).
	MaxEdgeEvents int64
	// Steps are executed at their offsets and recorded as invoke events.
	Steps []Step
}

// maxStepResultBytes bounds the result stored in one invoke event.
const maxStepResultBytes = 64 << 10

// stepTimeout bounds one step call (flash programming can take minutes).
const stepTimeout = 10 * time.Minute

// Run is a running multi-device capture bound to one session.
type Run struct {
	Session  *session.Session
	log      *slog.Logger
	epoch    time.Time
	opts     Options
	targets  []*runTarget
	controls []Target

	cancel context.CancelFunc
	done   chan struct{} // closed when every event stream has ended

	stepsDone chan struct{}
	stepFails atomic.Int64

	stopOnce sync.Once
	stopErr  error
	stopped  chan struct{}

	edges   atomic.Int64
	events  atomic.Uint64
	samples atomic.Uint64
}

type runTarget struct {
	nsamples atomic.Uint64
	Target
	capture   session.CaptureMeta
	writers   map[string]*streamState
	edgeOnly  map[string]*streamState
	mu        sync.Mutex
	streamErr error
	startedNs int64
	opened    bool
	declared  map[string]*pluginv1.StreamInfo
}

type streamState struct {
	meta     session.StreamMeta
	w        session.StreamWriter
	prevWord uint32
	havePrev bool
	names    []string
}

// Start opens devices, starts captures simultaneously and begins ingesting.
// parent bounds the whole run (not individual RPCs); cancelling it aborts.
func Start(parent context.Context, sess *session.Session, targets []Target, opts Options) (*Run, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxEdgeEvents == 0 {
		opts.MaxEdgeEvents = 1_000_000
	}
	ctx, cancel := context.WithCancel(parent)
	r := &Run{Session: sess, log: opts.Logger, epoch: time.Now(), opts: opts, cancel: cancel,
		done: make(chan struct{}), stopped: make(chan struct{}), stepsDone: make(chan struct{})}
	for _, t := range targets {
		if t.Control {
			r.controls = append(r.controls, t)
			continue
		}
		r.targets = append(r.targets, &runTarget{Target: t, writers: map[string]*streamState{}, edgeOnly: map[string]*streamState{}})
	}

	sess.Update(func(m *session.Meta) {
		for _, t := range targets {
			m.Devices = append(m.Devices, session.DeviceMeta{Alias: t.Alias, DeviceID: t.DeviceID, PluginID: t.PluginID,
				Name: t.DeviceName, Capabilities: t.Capabilities, Config: t.Config, ClockOffsetNs: t.ClockOffsetNs})
		}
	})

	// 1. open devices
	g, gctx := errgroup.WithContext(ctx)
	for _, t := range r.targets {
		g.Go(func() error {
			if err := t.Backend.OpenDevice(gctx, t.DeviceID, t.Config); err != nil {
				return fmt.Errorf("open %s: %w", t.Alias, err)
			}
			t.opened = true
			return nil
		})
	}
	opened := make([]atomic.Bool, len(r.controls))
	for i, t := range r.controls {
		g.Go(func() error {
			if err := t.Backend.OpenDevice(gctx, t.DeviceID, t.Config); err != nil {
				return fmt.Errorf("open %s: %w", t.Alias, err)
			}
			opened[i].Store(true)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		r.abort()
		for i, t := range r.controls {
			if opened[i].Load() {
				_ = t.Backend.CloseDevice(context.Background(), t.DeviceID)
			}
		}
		return nil, err
	}

	// 2. start captures as simultaneously as possible: all goroutines are
	// released by one barrier and stamp the request with the session time at
	// send, so each plugin can anchor its monotonic clock to the timeline.
	barrier := make(chan struct{})
	g, gctx = errgroup.WithContext(ctx)
	for _, t := range r.targets {
		g.Go(func() error {
			dir := filepath.Join(sess.Dir, "artifacts", t.Alias)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			select {
			case <-barrier:
			case <-gctx.Done():
				return gctx.Err()
			}
			reqNs := r.now()
			req := &pluginv1.StartCaptureRequest{
				DeviceId: t.DeviceID, CaptureId: t.Alias, SessionTimeNs: reqNs,
				DurationNs: int64(opts.Duration), ArtifactDir: dir,
			}
			if cfg, err := structFromMap(t.Config); err != nil {
				return err
			} else {
				req.Config = cfg
			}
			resp, err := t.Backend.StartCapture(gctx, req)
			if err != nil {
				return fmt.Errorf("start capture %s: %w", t.Alias, err)
			}
			t.startedNs = resp.GetStartedSessionNs() + t.ClockOffsetNs
			t.capture = session.CaptureMeta{ID: t.Alias, Alias: t.Alias, StartedNs: t.startedNs, RequestNs: reqNs}
			// Writers open lazily on the first batch so empty streams leave no files.
			t.declared = map[string]*pluginv1.StreamInfo{}
			for _, si := range resp.GetStreams() {
				t.declared[si.GetStreamId()] = si
			}
			return nil
		})
	}
	close(barrier)
	if err := g.Wait(); err != nil {
		r.abort()
		r.closeControls(context.Background())
		return nil, err
	}

	sess.Update(func(m *session.Meta) {
		for _, t := range r.targets {
			m.Captures = append(m.Captures, session.CaptureMeta{ID: t.Alias, Alias: t.Alias, StartedNs: t.startedNs, RequestNs: t.capture.RequestNs})
		}
	})
	_ = sess.Save()

	// 3. ingest
	var wg sync.WaitGroup
	for _, t := range r.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := t.Backend.StreamEvents(ctx, t.Alias, func(b *pluginv1.EventBatch) error {
				if b.GetDroppedBatches() > 0 {
					t.capture.Dropped += b.GetDroppedBatches()
					r.addEvent(timeline.Event{TimestampNs: r.now(), SourceID: t.Alias, DeviceID: t.DeviceID, Type: "warning",
						Summary: fmt.Sprintf("plugin dropped %d batches (buffer overrun)", b.GetDroppedBatches())})
				}
				return r.ingest(t, b)
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.mu.Lock()
				t.streamErr = err
				t.mu.Unlock()
				r.log.Error("event stream failed", "alias", t.Alias, "err", err)
				r.addEvent(timeline.Event{TimestampNs: r.now(), SourceID: t.Alias, DeviceID: t.DeviceID, Type: "error",
					Summary: "capture stream failed: " + err.Error()})
			}
		}()
	}
	go func() {
		wg.Wait()
		<-r.stepsDone
		close(r.done)
	}()
	go r.runSteps(ctx, opts.Steps)
	return r, nil
}

// StepFailures counts steps whose call returned an error.
func (r *Run) StepFailures() int { return int(r.stepFails.Load()) }

func (r *Run) closeControls(ctx context.Context) {
	for _, t := range r.controls {
		if err := t.Backend.CloseDevice(ctx, t.DeviceID); err != nil && !errors.Is(err, plugin.ErrCrashed) {
			r.log.Warn("close control device", "alias", t.Alias, "err", err)
		}
	}
}

func (r *Run) findDevice(alias string) (Target, bool) {
	for _, t := range r.controls {
		if t.Alias == alias {
			return t, true
		}
	}
	for _, t := range r.targets {
		if t.Alias == alias {
			return t.Target, true
		}
	}
	return Target{}, false
}

// runSteps executes the steps in time order and records each call.
func (r *Run) runSteps(ctx context.Context, steps []Step) {
	defer close(r.stepsDone)
	steps = append([]Step(nil), steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].At < steps[j].At })
	for _, s := range steps {
		timer := time.NewTimer(time.Until(r.epoch.Add(s.At)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		r.runStep(ctx, s)
	}
}

func (r *Run) runStep(ctx context.Context, s Step) {
	t, ok := r.findDevice(s.Alias)
	ev := timeline.Event{TimestampNs: r.now(), SourceID: s.Alias, Type: timeline.TypeInvoke}
	data := map[string]any{"capability": s.Capability, "method": s.Method, "step": s.Name}
	if len(s.Args) > 0 {
		data["args"] = s.Args
	}
	var callErr error
	t0 := time.Now()
	if !ok {
		callErr = fmt.Errorf("device %q is not part of this run", s.Alias)
	} else {
		ev.DeviceID = t.DeviceID
		pb := &pluginv1.InvokeRequest{DeviceId: t.DeviceID, Capability: s.Capability, Method: s.Method}
		if len(s.Args) > 0 {
			pb.Args, callErr = structFromMap(s.Args)
		}
		if callErr == nil {
			ictx, cancel := context.WithTimeout(ctx, stepTimeout)
			var resp *pluginv1.InvokeResponse
			resp, callErr = t.Backend.Invoke(ictx, pb)
			cancel()
			if callErr == nil && resp.GetResult() != nil {
				data["result"] = boundedResult(resp.GetResult().AsMap())
			}
		}
	}
	ev.DurationNs = int64(time.Since(t0))
	call := s.Capability + "." + s.Method
	if callErr != nil {
		r.stepFails.Add(1)
		data["ok"], data["error"] = false, callErr.Error()
		ev.Summary = fmt.Sprintf("%s failed: %v", call, callErr)
	} else {
		data["ok"] = true
		ev.Summary = call + " ok"
	}
	ev.Data = data
	r.addEvent(ev)
}

func boundedResult(m map[string]any) any {
	b, err := json.Marshal(m)
	if err != nil || len(b) > maxStepResultBytes {
		return map[string]any{"truncated": true, "bytes": len(b)}
	}
	return m
}

func (r *Run) now() int64 { return int64(time.Since(r.epoch)) }

// Now returns the current session timeline time.
func (r *Run) Now() int64 { return r.now() }

// Done is closed when all event streams have ended (duration elapsed, plugin
// failure or Stop).
func (r *Run) Done() <-chan struct{} { return r.done }

// Counters returns ingest statistics.
func (r *Run) Counters() (events, samples uint64) { return r.events.Load(), r.samples.Load() }

// AddMarker injects a user marker at the current time.
func (r *Run) AddMarker(name, description string) timeline.Event {
	e, _ := r.Session.AppendEvent(timeline.Event{TimestampNs: r.now(), SourceID: "user", Type: timeline.TypeMarker,
		Summary: name, Data: map[string]any{"name": name, "description": description}})
	return e
}

func (r *Run) addEvent(e timeline.Event) {
	if _, err := r.Session.AppendEvent(e); err != nil {
		r.log.Error("append event", "err", err)
		return
	}
	r.events.Add(1)
}

func (r *Run) abort() {
	r.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, t := range r.targets {
		if t.capture.ID != "" {
			_, _ = t.Backend.StopCapture(ctx, t.Alias)
		}
		if t.opened {
			_ = t.Backend.CloseDevice(ctx, t.DeviceID)
		}
	}
}

// Stop ends all captures, collects artifacts, closes devices and finalises
// the session metadata. It is idempotent.
func (r *Run) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.stopErr = r.stop(ctx)
		close(r.stopped)
	})
	<-r.stopped
	return r.stopErr
}

func (r *Run) stop(ctx context.Context) error {
	var (
		mu   sync.Mutex
		errs []error
	)
	addErr := func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }

	results := make([]*pluginv1.StopCaptureResponse, len(r.targets))
	var wg sync.WaitGroup
	for i, t := range r.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := t.Backend.StopCapture(ctx, t.Alias)
			if err != nil {
				addErr(fmt.Errorf("stop %s: %w", t.Alias, err))
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()

	// The plugin closes its stream after StopCapture; wait for ingestion to
	// drain (bounded) before closing writers.
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.log.Warn("event streams did not drain in time")
		r.cancel()
		<-r.done
	case <-ctx.Done():
		r.cancel()
		<-r.done
	}
	r.cancel()

	stopNs := r.now()
	for i, t := range r.targets {
		t.mu.Lock()
		if t.streamErr != nil {
			addErr(fmt.Errorf("capture %s: %w", t.Alias, t.streamErr))
		}
		t.mu.Unlock()
		for _, ss := range t.writers {
			if err := ss.w.Close(); err != nil {
				addErr(fmt.Errorf("close stream %s/%s: %w", t.Alias, ss.meta.ID, err))
			}
		}
		if res := results[i]; res != nil {
			for _, a := range res.GetArtifacts() {
				if err := r.registerArtifact(t, a); err != nil {
					addErr(err)
				}
			}
			t.capture.Samples = res.GetStats().GetSamples()
		}
		if n := t.nsamples.Load(); n > t.capture.Samples {
			t.capture.Samples = n
		}
		if err := t.Backend.CloseDevice(ctx, t.DeviceID); err != nil && !errors.Is(err, plugin.ErrCrashed) {
			addErr(fmt.Errorf("close %s: %w", t.Alias, err))
		}
	}
	if err := r.Session.Flush(); err != nil {
		addErr(err)
	}
	r.closeControls(ctx)

	// Streams are only registered in the metadata below, so compute the end
	// from the writers directly.
	for _, t := range r.targets {
		for _, ss := range t.writers {
			if rd, err := r.Session.Samples.Open(ss.meta.File); err == nil {
				if _, e := rd.Bounds(); e > stopNs {
					stopNs = e
				}
				rd.Close()
			}
		}
	}
	if _, last, ok := r.Session.Timeline().Bounds(); ok && last > stopNs {
		stopNs = last
	}
	r.Session.Update(func(m *session.Meta) {
		m.DurationNs = stopNs
		for i, t := range r.targets {
			cm := t.capture
			cm.StoppedNs = stopNs
			cm.Streams = nil
			for _, ss := range t.writers {
				cm.Streams = upsertStream(cm.Streams, ss.meta)
			}
			if res := results[i]; res != nil && res.GetStats() != nil {
				cm.Events = res.GetStats().GetEvents()
			}
			if res := results[i]; res != nil && res.GetClock() != nil {
				c := res.GetClock()
				cm.Clock = &session.ClockMeta{
					Source: c.GetSource(), ResolutionNs: c.GetResolutionNs(),
					ErrorMinNs: c.GetErrorMinNs(), ErrorMinBounded: c.GetErrorMinBounded(),
					ErrorMaxNs: c.GetErrorMaxNs(), ErrorMaxBounded: c.GetErrorMaxBounded(),
					JitterNs: c.GetJitterNs(), DriftPPM: c.GetDriftPpm(), Note: c.GetNote(),
				}
			}
			if t.ClockOffsetNs != 0 {
				if cm.Clock == nil {
					cm.Clock = &session.ClockMeta{Source: "not reported by the plugin"}
				}
				// Bounds are stored as effective values, including the offset.
				cm.Clock.ErrorMinNs += t.ClockOffsetNs
				cm.Clock.ErrorMaxNs += t.ClockOffsetNs
				cm.Clock.OffsetAppliedNs = t.ClockOffsetNs
			}
			for j := range m.Captures {
				if m.Captures[j].ID == cm.ID {
					m.Captures[j] = cm
				}
			}
		}
	})
	return errors.Join(errs...)
}

func upsertStream(list []session.StreamMeta, s session.StreamMeta) []session.StreamMeta {
	for i := range list {
		if list[i].ID == s.ID {
			list[i] = s
			return list
		}
	}
	return append(list, s)
}

func (r *Run) registerArtifact(t *runTarget, a *pluginv1.ArtifactRef) error {
	rel, err := filepath.Rel(r.Session.Dir, a.GetPath())
	art := artifact.Artifact{Name: a.GetName(), Kind: a.GetKind(), MimeType: a.GetMimeType(),
		SourceID: t.Alias, CaptureID: t.Alias, Metadata: a.GetMetadata()}
	if err == nil && !strings.HasPrefix(rel, "..") {
		art.Path = filepath.ToSlash(rel)
		_, err = r.Session.Artifacts.Register(art)
		return err
	}
	f, err := os.Open(a.GetPath())
	if err != nil {
		return fmt.Errorf("artifact %s from %s: %w", a.GetName(), t.Alias, err)
	}
	defer f.Close()
	art.Path = "artifacts/" + t.Alias + "/" + filepath.Base(a.GetPath())
	_, err = r.Session.Artifacts.Put(art, f)
	return err
}
