package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/capture"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/device"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/session"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// stopGrace is how long after the requested duration a run may take to end
// on its own before it is stopped forcibly.
const stopGrace = 10 * time.Second

// RunSpec describes a multi-device capture.
type RunSpec struct {
	Name     string
	Duration time.Duration // 0: until stopped
	Devices  []experiment.DeviceSpec
	// Confirm pre-approves dangerous configuration (CLI --yes / MCP confirm).
	Confirm bool
	// Experiment, when set, supplies plots/analysis settings and is stored
	// in the session.
	Experiment *experiment.Experiment
	// Analysis applies to runs without an Experiment (ad-hoc captures).
	Analysis experiment.Analysis
	// Steps override the Experiment's steps (ad-hoc and MCP runs).
	Steps []experiment.Step
}

// Run is a capture bound to a session, finalised exactly once.
type Run struct {
	app     *App
	spec    RunSpec
	cap     *capture.Run
	Session *session.Session

	started time.Time
	once    sync.Once
	result  *RunResult
	err     error
	done    chan struct{}
}

// RunResult summarises a finished run.
type RunResult struct {
	SessionID  string           `json:"session_id"`
	Dir        string           `json:"dir"`
	Status     string           `json:"status"`
	DurationNs int64            `json:"duration_ns"`
	Events     map[string]int   `json:"events_by_type"`
	Samples    uint64           `json:"samples"`
	Spikes     int              `json:"power_spikes"`
	Artifacts  []string         `json:"artifacts,omitempty"`
	Plots      []string         `json:"plots,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
	Error      string           `json:"error,omitempty"`
	Devices    []string         `json:"devices"`
	Captures   []map[string]any `json:"captures,omitempty"`
}

// RunStatus is a snapshot of a run in progress.
type RunStatus struct {
	SessionID string        `json:"session_id"`
	Name      string        `json:"name"`
	State     string        `json:"state"` // "running" | "finished"
	Elapsed   time.Duration `json:"elapsed_ns"`
	Events    uint64        `json:"events"`
	Samples   uint64        `json:"samples"`
}

// Finished is closed once the run has been finalised.
func (r *Run) Finished() <-chan struct{} { return r.done }

// Result returns the final result (valid after Finished is closed).
func (r *Run) Result() (*RunResult, error) { <-r.done; return r.result, r.err }

// Status reports progress.
func (r *Run) Status() RunStatus {
	st := RunStatus{SessionID: r.Session.ID(), Name: r.spec.Name, State: "running", Elapsed: time.Since(r.started)}
	st.Events, st.Samples = r.cap.Counters()
	select {
	case <-r.done:
		st.State = "finished"
	default:
	}
	return st
}

// StartCapture resolves devices, checks policy, creates a session and starts
// capturing. The run is detached from ctx so it survives the request (MCP);
// it finalises itself when the duration elapses or the plugins end.
func (a *App) StartCapture(ctx context.Context, spec RunSpec) (*Run, error) {
	targets, err := a.resolveTargets(ctx, spec)
	if err != nil {
		return nil, err
	}
	steps, err := a.buildSteps(spec, targets)
	if err != nil {
		return nil, err
	}
	sess, err := session.Create(a.Cfg.SessionsDir, spec.Name, config.Version)
	if err != nil {
		return nil, err
	}
	if spec.Experiment != nil && spec.Experiment.Raw != "" {
		sess.Update(func(m *session.Meta) { m.Experiment = spec.Experiment.Raw })
		if err := os.WriteFile(filepath.Join(sess.Dir, "experiment.yaml"), []byte(spec.Experiment.Raw), 0o644); err != nil {
			a.Log.Warn("could not store experiment copy", "err", err)
		}
	}
	cr, err := capture.Start(a.baseCtx, sess, targets, capture.Options{Duration: spec.Duration, Logger: a.Log, Steps: steps})
	if err != nil {
		sess.Update(func(m *session.Meta) { m.Status, m.Error = session.StatusFailed, err.Error() })
		_ = sess.Close()
		return nil, err
	}
	r := &Run{app: a, spec: spec, cap: cr, Session: sess, started: time.Now(), done: make(chan struct{})}
	a.mu.Lock()
	a.runs[sess.ID()] = r
	a.mu.Unlock()
	go r.watch()
	return r, nil
}

func (r *Run) watch() {
	var timeout <-chan time.Time
	if r.spec.Duration > 0 {
		timeout = time.After(r.spec.Duration + stopGrace)
	}
	select {
	case <-r.cap.Done():
	case <-timeout:
		r.app.Log.Warn("capture exceeded its duration; stopping", "session", r.Session.ID())
	case <-r.done:
		return
	}
	_, _ = r.Finalize(context.Background())
}

// AddMarker inserts a user marker on the live timeline.
func (r *Run) AddMarker(name, description string) { r.cap.AddMarker(name, description) }

// Finalize stops the capture, runs analysis, renders plots and closes the
// session. It is idempotent and safe to call from several goroutines.
func (r *Run) Finalize(ctx context.Context) (*RunResult, error) {
	r.once.Do(func() {
		defer close(r.done)
		r.result, r.err = r.finalize(ctx)
	})
	<-r.done
	return r.result, r.err
}

func (r *Run) finalize(ctx context.Context) (*RunResult, error) {
	a := r.app
	sess := r.Session
	res := &RunResult{SessionID: sess.ID(), Dir: sess.Dir}
	var errs []error

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.cap.Stop(sctx); err != nil {
		errs = append(errs, err)
	}

	an := r.spec.Analysis
	if r.spec.Experiment != nil {
		an = r.spec.Experiment.Analysis
	}
	if an.PowerSpikes == nil || *an.PowerSpikes {
		n, err := analysis.AnnotatePower(sctx, sess, an.NominalVoltage, analysis.SpikeOptions{})
		if err != nil {
			errs = append(errs, err)
		}
		res.Spikes = n
	}

	plotSpecs := r.plotSpecs()
	for _, ps := range plotSpecs {
		p, err := a.writePlot(sctx, sess, ps.name, ps.spec)
		if errors.Is(err, plot.ErrNothingToPlot) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("plot %s skipped: %v", ps.name, err))
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("plot %s: %w", ps.name, err))
			continue
		}
		res.Plots = append(res.Plots, p)
	}

	err := errors.Join(errs...)
	sess.Update(func(m *session.Meta) {
		m.Status = session.StatusComplete
		if err != nil {
			m.Status, m.Error = session.StatusFailed, err.Error()
		}
	})
	if cerr := sess.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}

	meta := sess.Meta()
	res.Status, res.DurationNs, res.Error = meta.Status, meta.DurationNs, meta.Error
	res.Events = sess.Timeline().Count()
	for _, d := range meta.Devices {
		res.Devices = append(res.Devices, d.Alias)
	}
	for _, c := range meta.Captures {
		res.Samples += c.Samples
		res.Captures = append(res.Captures, map[string]any{"alias": c.Alias, "samples": c.Samples, "events": c.Events,
			"dropped_batches": c.Dropped, "streams": len(c.Streams)})
		if c.Dropped > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s dropped %d batches (buffer overrun)", c.Alias, c.Dropped))
		}
	}
	for _, art := range sess.Artifacts.List() {
		res.Artifacts = append(res.Artifacts, art.Path)
	}
	sort.Strings(res.Artifacts)
	if n := r.cap.StepFailures(); n > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d step(s) failed; see the invoke events on the timeline", n))
	}
	return res, err
}

type namedPlot struct {
	name string
	spec plot.Spec
}

// plotSpecs returns the plots to render: the experiment's, or an SVG and a
// PNG overview by default.
func (r *Run) plotSpecs() []namedPlot {
	var out []namedPlot
	if e := r.spec.Experiment; e != nil && len(e.Plots) > 0 {
		for i, p := range e.Plots {
			name := p.Name
			if name == "" {
				name = fmt.Sprintf("plot%d", i+1)
			}
			ps := plot.Spec{Title: p.Title, StartNs: int64(p.Start), EndNs: int64(p.End), Format: p.Format, LogPower: p.LogPower, Width: p.Width}
			for _, t := range p.Tracks {
				ps.Tracks = append(ps.Tracks, plot.TrackSpec{Kind: t.Kind, Source: t.Source, Stream: t.Stream, Types: t.Types, Title: t.Title})
			}
			out = append(out, namedPlot{name, ps})
		}
		return out
	}
	title := r.spec.Name
	return []namedPlot{
		{"overview", plot.Spec{Title: title, Format: "svg"}},
		{"overview", plot.Spec{Title: title, Format: "png"}},
	}
}

// resolveTargets maps the spec onto concrete devices and enforces policy.
func (a *App) resolveTargets(ctx context.Context, spec RunSpec) ([]capture.Target, error) {
	if len(spec.Devices) == 0 {
		return nil, errors.New("no devices selected")
	}
	cache := map[string][]device.Device{}
	var targets []capture.Target
	for _, ds := range spec.Devices {
		b, err := a.Plugins.Get(ctx, ds.Plugin)
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", ds.Alias, err)
		}
		devs, ok := cache[ds.Plugin]
		if !ok {
			if devs, err = a.pluginDevices(ctx, ds.Plugin); err != nil {
				return nil, fmt.Errorf("device %q: list devices of %s: %w", ds.Alias, ds.Plugin, err)
			}
			cache[ds.Plugin] = devs
		}
		d, err := device.Select(devs, ds.Plugin, ds.Device)
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", ds.Alias, err)
		}
		control := ds.Control
		if !control && !d.Has(pluginv1.CapTimedCapture) && !d.Has(pluginv1.CapLiveRawStream) && !d.Has(pluginv1.CapOfflineCapture) {
			if !invocable(b.Info(), d) {
				return nil, fmt.Errorf("device %q (%s) cannot capture and offers no methods: it has none of %s, %s, %s", ds.Alias, d.ID,
					pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream, pluginv1.CapOfflineCapture)
			}
			// A command/response device: opened for the run and driven by steps.
			control = true
		}
		for _, req := range ds.Requires {
			if !d.Has(req) {
				return nil, fmt.Errorf("device %q (%s) lacks required capability %s (has: %v)", ds.Alias, d.ID, req, d.Capabilities)
			}
		}
		var danger []string
		for _, k := range b.Info().GetDangerousConfigKeys() {
			if _, ok := ds.Config[k]; ok {
				danger = append(danger, k)
			}
		}
		if len(danger) > 0 {
			sort.Strings(danger)
			act := policy.Action{Kind: policy.KindConfig, Plugin: ds.Plugin, Device: d.LocalID, ConfigKeys: danger}
			if err := a.Policy.Authorize(act, spec.Confirm); err != nil {
				return nil, fmt.Errorf("device %q: %w", ds.Alias, err)
			}
		}
		targets = append(targets, capture.Target{Alias: ds.Alias, PluginID: ds.Plugin, DeviceID: d.LocalID, DeviceName: d.Name,
			Capabilities: d.Capabilities, Backend: b, Config: ds.Config, ClockOffsetNs: ds.ClockOffsetNs, Control: control})
	}
	return targets, nil
}

// invocable reports whether the plugin declares a method on one of the
// device's capabilities.
func invocable(info *pluginv1.PluginInfo, d device.Device) bool {
	for _, m := range info.GetMethods() {
		if d.Has(m.GetCapability()) {
			return true
		}
	}
	return false
}

// buildSteps resolves experiment steps against the opened devices, refusing
// undeclared methods and unconfirmed dangerous ones before anything runs.
func (a *App) buildSteps(spec RunSpec, targets []capture.Target) ([]capture.Step, error) {
	steps := spec.Steps
	if steps == nil && spec.Experiment != nil {
		steps = spec.Experiment.Steps
	}
	var out []capture.Step
	streaming := false
	for _, t := range targets {
		streaming = streaming || !t.Control
	}
	for i, s := range steps {
		var tg *capture.Target
		for j := range targets {
			if targets[j].Alias == s.Device {
				tg = &targets[j]
			}
		}
		if tg == nil {
			return nil, fmt.Errorf("step %d: unknown device %q", i+1, s.Device)
		}
		var ms *pluginv1.MethodSpec
		for _, m := range tg.Backend.Info().GetMethods() {
			if m.GetCapability() == s.Capability() && m.GetName() == s.Method() {
				ms = m
			}
		}
		if ms == nil {
			return nil, fmt.Errorf("step %d: plugin %q does not declare method %s", i+1, tg.PluginID, s.Call)
		}
		if ms.GetDanger() != pluginv1.Danger_DANGER_SAFE {
			act := policy.Action{Kind: policy.KindInvoke, Plugin: tg.PluginID, Device: tg.DeviceID, Capability: s.Capability(), Method: s.Method()}
			if err := a.Policy.Authorize(act, spec.Confirm); err != nil {
				return nil, fmt.Errorf("step %d (%s): %w", i+1, s.Call, err)
			}
		}
		out = append(out, capture.Step{Name: s.Name, At: s.At.D(), Alias: s.Device, Capability: s.Capability(), Method: s.Method(), Args: s.Args})
	}
	if !streaming && len(out) == 0 {
		return nil, errors.New("nothing to run: every device is control-only (no capture capability) and the experiment has no steps")
	}
	return out, nil
}

// RunExperiment runs an experiment to completion. Cancelling ctx stops the
// capture early; the partial session is still finalised and returned.
func (a *App) RunExperiment(ctx context.Context, e *experiment.Experiment, confirm bool) (*RunResult, error) {
	r, err := a.StartCapture(ctx, RunSpec{Name: e.Name, Duration: e.Duration.D(), Devices: e.Devices, Confirm: confirm, Experiment: e})
	if err != nil {
		return nil, err
	}
	select {
	case <-r.Finished():
	case <-ctx.Done():
	}
	return r.Finalize(context.WithoutCancel(ctx))
}

// StopCapture finalises a running capture by session id.
func (a *App) StopCapture(ctx context.Context, sessionID string) (*RunResult, error) {
	r, err := a.findRun(sessionID)
	if err != nil {
		return nil, err
	}
	return r.Finalize(ctx)
}

func (a *App) findRun(id string) (*Run, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == "" && len(a.runs) == 1 {
		for _, r := range a.runs {
			return r, nil
		}
	}
	if r, ok := a.runs[id]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("no running capture %q (running: %d)", id, len(a.runs))
}

// AddMarker puts a marker on a running capture.
func (a *App) AddMarker(sessionID, name, description string) error {
	r, err := a.findRun(sessionID)
	if err != nil {
		return err
	}
	r.AddMarker(name, description)
	return nil
}

// Status lists captures in progress.
func (a *App) Status() []RunStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]RunStatus, 0, len(a.runs))
	for _, r := range a.runs {
		out = append(out, r.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}
