// Package mcp exposes the hwlab services as Model Context Protocol tools.
//
// Tools are generic and capability-oriented; vendor specifics live in
// plugins. Raw samples are never returned: results are summaries, envelopes
// or rendered plots. Dangerous operations need an explicit confirm=true that
// callers must only set after the user approved the action.
package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/device"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/timeline"
)

const instructions = `hwlab controls a hardware lab (power meters, logic analyzers, sniffers, debug probes)
and keeps everything on one timeline of nanosecond timestamps.
Typical flow: device_list -> capture_start (or experiment_run) -> capture_stop ->
session_info -> timeline_query / timeline_correlate / power_analyze / plot_render.
Times are strings: a duration ("12.5ms", "1s") or integer nanoseconds.
Dangerous operations (supply voltage, raw commands, flashing) return an error
unless confirm=true; set it only after the user explicitly approved that exact action.
Addresses and lengths are strings ("0x08000000") so 64-bit values never lose precision.
Large data never travels inline: file results are artifact references (artifact_list /
artifact_info / artifact_read, bounded to 64 KiB per read); file arguments must stay inside the
hwlab artifacts directory. Failures are tool results with isError=true and a structured
error {code, message, retryable, hint}; capability_registry lists methods, danger levels and codes.`

// NewServer builds an MCP server around the services.
func NewServer(a *app.App) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "hwlab", Version: config.Version}, &sdk.ServerOptions{Instructions: instructions})
	t := &tools{app: a}
	t.register(s)
	return s
}

// ServeStdio runs the server on stdin/stdout until ctx ends or the client disconnects.
func ServeStdio(ctx context.Context, a *app.App) error {
	return NewServer(a).Run(ctx, &sdk.StdioTransport{})
}

type tools struct{ app *app.App }

type obj = map[string]any

func ro(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true}
}

func (t *tools) register(s *sdk.Server) {
	add(s, &sdk.Tool{Name: "lab_status", Description: "Overview: version, plugin/device counts, captures in progress, latest session.", Annotations: ro("Lab status")}, t.labStatus)
	add(s, &sdk.Tool{Name: "plugin_list", Description: "List discovered plugins with capabilities, methods and support status (implementation vs hardware validation).", Annotations: ro("List plugins")}, t.pluginList)
	add(s, &sdk.Tool{Name: "doctor", Description: "Check the environment (storage, external tools, services) and report each plugin's implementation and validation status.", Annotations: ro("Environment doctor")}, t.doctor)
	add(s, &sdk.Tool{Name: "device_list", Description: "List all devices from all plugins with their capabilities.", Annotations: ro("List devices")}, t.deviceList)
	add(s, &sdk.Tool{Name: "device_invoke", Description: "Call a declared capability method on a device. Dangerous methods need confirm=true after user approval."}, t.deviceInvoke)
	add(s, &sdk.Tool{Name: "target_op", Description: "Generic debug-probe/target operation in an ephemeral device session (info, identify, halt, resume, step, harts, reset, registers, memory-read/write, breakpoint-set/clear/list, watchpoint-set/clear/list, set-speed, flash-info/read/backup/erase/program/verify/restore, TI CC23xx cc23xx-info/saci-status/saci-enter/saci-exit/ccfg-info/ccfg-program, ...). The same service and safety policy as `hwlab target`. Destructive operations need confirm=true after user approval, the plugin's own confirmation token in args.confirm and the plugin's allow_* settings in config (and for flash: a verified backup in args.backup). Flash contents are never returned inline beyond 4 KiB: pass args.path to write an artifact."}, t.targetOp)
	add(s, &sdk.Tool{Name: "capture_start", Description: "Start a synchronized capture on one or more devices. Returns immediately with a session id; the capture ends by itself after duration or on capture_stop."}, t.captureStart)
	add(s, &sdk.Tool{Name: "capture_stop", Description: "Stop a capture, run analysis, render overview plots and finalize the session."}, t.captureStop)
	add(s, &sdk.Tool{Name: "capture_status", Description: "Progress of captures in progress (and recently finished ones in this server process).", Annotations: ro("Capture status")}, t.captureStatus)
	add(s, &sdk.Tool{Name: "capture_marker", Description: "Insert a named marker on the live timeline of a running capture."}, t.captureMarker)
	add(s, &sdk.Tool{Name: "experiment_run", Description: "Run an experiment from YAML text or a file path; blocks until it finishes and returns the session summary."}, t.experimentRun)
	add(s, &sdk.Tool{Name: "session_list", Description: "List recorded sessions, newest first.", Annotations: ro("List sessions")}, t.sessionList)
	add(s, &sdk.Tool{Name: "session_info", Description: "Describe a session: devices, streams, event counts, artifacts, plots. session accepts id, prefix, path or 'latest'.", Annotations: ro("Session info")}, t.sessionInfo)
	add(s, &sdk.Tool{Name: "timeline_query", Description: "Events of the unified timeline in a time range, optionally filtered by type/source/text.", Annotations: ro("Query timeline")}, t.timelineQuery)
	add(s, &sdk.Tool{Name: "timeline_nearest", Description: "The event closest in time to a timestamp, optionally filtered (e.g. nearest packet to a power spike).", Annotations: ro("Nearest event")}, t.timelineNearest)
	add(s, &sdk.Tool{Name: "timeline_correlate", Description: "For each anchor event (e.g. type analysis.power_rise) list related events (e.g. type packet) within a time window, plus delay statistics.", Annotations: ro("Correlate events")}, t.timelineCorrelate)
	add(s, &sdk.Tool{Name: "timeline_explain", Description: "Explain what happened around a current spike, a packet or a time: packets before/after with signed deltas, the first DATA and its ACK, energy of the burst and around it, current before/after, and the timing uncertainty of the two clocks (cross-device timing is never ns-accurate). Anchor with exactly one of spike (1-based), packet (1-based) or at. Packet payloads are not returned.", Annotations: ro("Explain event")}, t.timelineExplain)
	add(s, &sdk.Tool{Name: "clock_calibrate", Description: "Measure the clock mapping (offset, rate/drift in ppm, residuals, uncertainty) between two capture devices from digital edges of ONE physical signal that both recorded (the signal needs a non-periodic marker). Read-only: computed from the stored timeline, the session is not modified. Uncertainty excludes common-mode delays and probe-point skew.", Annotations: ro("Calibrate clocks")}, t.clockCalibrate)
	add(s, &sdk.Tool{Name: "waveform_measure", Description: "Measure a recorded analog channel (oscilloscope or analog capture), live or replayed: per contiguous segment min, max, mean, rms, peak-to-peak, base/top level, frequency, period, duty, pulse widths and 10-90% rise/fall time, plus a min/max envelope of at most `points` buckets (default 200, max 1000). Computed from stored samples, never from the instrument; raw samples are never returned. Narrow with from/to.", Annotations: ro("Measure waveform")}, t.waveformMeasure)
	add(s, &sdk.Tool{Name: "usb_analyze", Description: "Analyze the USB packets (usbcap plugin, usbmon URB level) of a session, live or replayed: pairs URB submissions with completions, identifies devices (VID/PID) and interface classes from the enumeration traffic in the capture, labels control requests and HID/CDC/mass-storage/DFU transfers, and reports per-endpoint statistics, error counts and a limited transaction list (filter by device or errors only). Latencies are kernel URB timestamp differences, not bus times; devices enumerated before the capture stay unlabelled. Payload bytes are limited to a short hex preview per transaction.", Annotations: ro("Analyze USB")}, t.usbAnalyze)
	add(s, &sdk.Tool{Name: "modbus_decode", Description: "Decode Modbus RTU transactions from the serial (uart) packets of a session, live or replayed: CRC-checked frames paired into transactions with status (ok, exception, no_response, orphan_response, crc_error...), statistics and a limited transaction list. mode says what the capturing host was: master (default), slave or sniff. Latencies are host-timestamp differences, not wire times. Never returns raw byte dumps beyond each transaction's frames.", Annotations: ro("Decode Modbus")}, t.modbusDecode)
	add(s, &sdk.Tool{Name: "power_analyze", Description: "Current statistics (mean/min/max/charge/energy) and spikes for power streams in a time range. Never returns raw samples.", Annotations: ro("Analyze power")}, t.powerAnalyze)
	add(s, &sdk.Tool{Name: "plot_render", Description: "Render a plot of a time range (power envelope, digital lanes, packets, markers). Saves SVG/PNG in the session and returns PNG inline.", Annotations: ro("Render plot")}, t.plotRender)
	add(s, &sdk.Tool{Name: "capability_registry", Description: "Machine-readable catalogue from the core registry (same source as the CLI): generic target ops and the plugin method each calls, every plugin method with danger level, requires_confirmation and JSON argument schema, per-backend support/validation status, and the error codes this server returns.", Annotations: ro("Capability registry")}, t.capabilityRegistry)
	add(s, &sdk.Tool{Name: "artifact_list", Description: "List artifacts without their bytes: a session's files, or (no session) host artifacts such as flash backups below the hwlab artifacts directory. Each item has a ref, size and kind.", Annotations: ro("List artifacts")}, t.artifactList)
	add(s, &sdk.Tool{Name: "artifact_info", Description: "Describe one artifact by ref: size, SHA-256, path, kind, session. Never returns the bytes.", Annotations: ro("Artifact info")}, t.artifactInfo)
	add(s, &sdk.Tool{Name: "artifact_read", Description: "Bounded read (max 64 KiB per call) of an artifact by ref and offset, as hex or text, with the chunk SHA-256. Only files inside the hwlab artifacts directory and session stores are readable; there is no arbitrary file read.", Annotations: ro("Read artifact")}, t.artifactRead)
}

// ---- status / discovery ----

type noInput struct{}

func (t *tools) labStatus(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, obj, error) {
	plugins := t.app.ListPlugins(ctx)
	devs, derrs := t.app.ListDevices(ctx)
	sessions, _ := t.app.ListSessions()
	out := obj{"version": config.Version, "sessions_dir": t.app.Cfg.SessionsDir, "plugins": len(plugins),
		"devices": len(devs), "device_errors": derrs, "captures": t.app.Status(), "sessions": len(sessions)}
	if len(sessions) > 0 {
		out["latest_session"] = sessions[0].ID
	}
	return nil, out, nil
}

func (t *tools) pluginList(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, obj, error) {
	return nil, obj{"plugins": t.app.ListPlugins(ctx)}, nil
}

func (t *tools) doctor(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, obj, error) {
	r := t.app.Doctor(ctx)
	return nil, obj{"checks": r.Checks, "plugins": r.Plugins, "failed": r.Failed()}, nil
}

func (t *tools) deviceList(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, obj, error) {
	devs, errs := t.app.ListDevices(ctx)
	if devs == nil {
		devs = []device.Device{}
	}
	return nil, obj{"devices": devs, "errors": errs}, nil
}

type invokeIn struct {
	Plugin     string         `json:"plugin" jsonschema:"plugin id, e.g. fake"`
	Device     string         `json:"device,omitempty" jsonschema:"device id/serial; optional when the plugin has one device"`
	Capability string         `json:"capability" jsonschema:"capability id, e.g. PowerSource"`
	Method     string         `json:"method" jsonschema:"method name, e.g. set_voltage"`
	Args       map[string]any `json:"args,omitempty"`
	Confirm    bool           `json:"confirm,omitempty" jsonschema:"set true only after the user explicitly approved this dangerous operation"`
}

func (t *tools) deviceInvoke(ctx context.Context, _ *sdk.CallToolRequest, in invokeIn) (*sdk.CallToolResult, obj, error) {
	res, err := t.app.Invoke(ctx, app.InvokeRequest{Plugin: in.Plugin, Device: in.Device, Capability: in.Capability,
		Method: in.Method, Args: in.Args, Confirm: in.Confirm, ConfineFiles: true})
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"result": res}, nil
}

type targetIn struct {
	Plugin  string         `json:"plugin,omitempty" jsonschema:"plugin id; optional when exactly one device has the needed capability"`
	Device  string         `json:"device,omitempty"`
	Op      string         `json:"op" jsonschema:"info, identify, attach, halt, resume, reset, registers, memory-read, memory-write, flash-read, flash-program, flash-verify, flash-erase, flash-backup, flash-restore, cc23xx-info, saci-status, saci-enter, saci-exit, ccfg-info, ccfg-program, jtag-scan or jtag-identify"`
	Config  map[string]any `json:"config,omitempty" jsonschema:"device session config, e.g. access and allow_* permissions"`
	Args    map[string]any `json:"args,omitempty"`
	Pre     []targetCall   `json:"pre,omitempty" jsonschema:"calls to run before op, e.g. DebugProbe.attach"`
	Post    []targetCall   `json:"post,omitempty" jsonschema:"calls to run after op even on failure, e.g. DebugProbe.resume"`
	Confirm bool           `json:"confirm,omitempty" jsonschema:"set true only after the user explicitly approved this dangerous operation"`
}

type targetCall struct {
	Capability string         `json:"capability"`
	Method     string         `json:"method"`
	Args       map[string]any `json:"args,omitempty"`
}

func (t *tools) targetOp(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, obj, error) {
	conv := func(cs []targetCall) []app.TargetCall {
		var out []app.TargetCall
		for _, c := range cs {
			out = append(out, app.TargetCall{Capability: c.Capability, Method: c.Method, Args: c.Args})
		}
		return out
	}
	res, err := t.app.Target(ctx, app.TargetRequest{Plugin: in.Plugin, Device: in.Device, Config: in.Config, Op: in.Op, Args: in.Args,
		Pre: conv(in.Pre), Post: conv(in.Post), Confirm: in.Confirm, ConfineFiles: true})
	if err != nil {
		if res != nil {
			return nil, obj{"partial": res}, err
		}
		return nil, nil, err
	}
	return nil, obj{"result": res}, nil
}

// ---- capture ----

type deviceIn struct {
	Alias    string         `json:"alias" jsonschema:"name of this device on the timeline"`
	Plugin   string         `json:"plugin"`
	Device   string         `json:"device,omitempty"`
	Requires []string       `json:"requires,omitempty" jsonschema:"capabilities the device must have"`
	Config   map[string]any `json:"config,omitempty" jsonschema:"plugin-specific configuration"`
	// ClockOffset shifts this device's timestamps, e.g. "-2ms"; see hwlab calibrate.
	ClockOffset string `json:"clock_offset,omitempty" jsonschema:"signed offset added to this device's timestamps, e.g. -2ms"`
}

func specs(in []deviceIn) ([]experiment.DeviceSpec, error) {
	var out []experiment.DeviceSpec
	for _, d := range in {
		cfg := d.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		var off int64
		if d.ClockOffset != "" {
			var err error
			if off, err = experiment.ParseSignedDuration(d.ClockOffset); err != nil {
				return nil, apperr.Newf(apperr.InvalidArgument, "device %q: clock_offset: %v", d.Alias, err)
			}
		}
		out = append(out, experiment.DeviceSpec{Alias: d.Alias, Plugin: d.Plugin, Device: d.Device, Requires: d.Requires, Config: cfg, ClockOffsetNs: off})
	}
	return out, nil
}

type captureStartIn struct {
	Name           string     `json:"name,omitempty"`
	Duration       string     `json:"duration" jsonschema:"capture length, e.g. 10s (required so a capture can never run unattended forever)"`
	Devices        []deviceIn `json:"devices"`
	NominalVoltage float64    `json:"nominal_voltage,omitempty" jsonschema:"supply voltage in volts for energy computation when the meter reports no voltage"`
	Confirm        bool       `json:"confirm,omitempty" jsonschema:"set true only after the user approved dangerous configuration such as supply voltage"`
}

func (t *tools) captureStart(ctx context.Context, _ *sdk.CallToolRequest, in captureStartIn) (*sdk.CallToolResult, obj, error) {
	dur, err := app.ParseTime(in.Duration)
	if err != nil || dur <= 0 {
		return nil, nil, apperr.Newf(apperr.InvalidArgument, "duration must be a positive duration such as 10s")
	}
	name := in.Name
	if name == "" {
		name = "mcp-capture"
	}
	devs, err := specs(in.Devices)
	if err != nil {
		return nil, nil, err
	}
	r, err := t.app.StartCapture(ctx, app.RunSpec{Name: name, Duration: time.Duration(dur), Devices: devs, Confirm: in.Confirm,
		Analysis: experiment.Analysis{NominalVoltage: in.NominalVoltage}})
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"session_id": r.Session.ID(), "dir": r.Session.Dir, "state": "running", "duration": in.Duration}, nil
}

type sessionIn struct {
	Session string `json:"session,omitempty" jsonschema:"session id, unique prefix, path or 'latest'"`
}

func (t *tools) captureStop(ctx context.Context, _ *sdk.CallToolRequest, in sessionIn) (*sdk.CallToolResult, obj, error) {
	res, err := t.app.StopCapture(ctx, in.Session)
	if err != nil {
		if res == nil {
			return nil, nil, err
		}
		return nil, obj{"result": res, "error": err.Error()}, nil
	}
	return nil, obj{"result": res}, nil
}

func (t *tools) captureStatus(context.Context, *sdk.CallToolRequest, noInput) (*sdk.CallToolResult, obj, error) {
	return nil, obj{"captures": t.app.Status()}, nil
}

type markerIn struct {
	Session     string `json:"session,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (t *tools) captureMarker(_ context.Context, _ *sdk.CallToolRequest, in markerIn) (*sdk.CallToolResult, obj, error) {
	if in.Name == "" {
		return nil, nil, apperr.Newf(apperr.InvalidArgument, "name is required")
	}
	if err := t.app.AddMarker(in.Session, in.Name, in.Description); err != nil {
		return nil, nil, err
	}
	return nil, obj{"ok": true}, nil
}

type experimentIn struct {
	YAML    string `json:"yaml,omitempty" jsonschema:"experiment document (alternative to path)"`
	Path    string `json:"path,omitempty" jsonschema:"experiment file path on the hwlab host"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set true only after the user approved dangerous configuration; allow_dangerous in the file is ignored over MCP"`
}

func (t *tools) experimentRun(ctx context.Context, _ *sdk.CallToolRequest, in experimentIn) (*sdk.CallToolResult, obj, error) {
	var (
		e   *experiment.Experiment
		err error
	)
	switch {
	case in.YAML != "":
		e, err = experiment.Parse([]byte(in.YAML))
	case in.Path != "":
		var p string
		if p, err = t.app.CheckExperimentPath(in.Path); err != nil {
			return nil, nil, err
		}
		e, err = experiment.Load(p)
	default:
		return nil, nil, apperr.Newf(apperr.InvalidArgument, "provide yaml or path")
	}
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.InvalidArgument, err)
	}
	res, err := t.app.RunExperiment(ctx, e, in.Confirm)
	if err != nil && res == nil {
		return nil, nil, err
	}
	out := obj{"result": res}
	if err != nil {
		out["error"] = err.Error()
		out["error_info"] = app.Classify(err)
	}
	return nil, out, nil
}

// ---- sessions & timeline ----

func (t *tools) sessionList(context.Context, *sdk.CallToolRequest, noInput) (*sdk.CallToolResult, obj, error) {
	ms, err := t.app.ListSessions()
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"sessions": ms}, nil
}

func (t *tools) sessionInfo(_ context.Context, _ *sdk.CallToolRequest, in sessionIn) (*sdk.CallToolResult, obj, error) {
	info, err := t.app.SessionInfo(orLatest(in.Session))
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"session": info}, nil
}

type timelineQueryIn struct {
	Session  string   `json:"session,omitempty"`
	From     string   `json:"from,omitempty" jsonschema:"range start, e.g. 10ms"`
	To       string   `json:"to,omitempty" jsonschema:"range end; empty = end of session"`
	Types    []string `json:"types,omitempty" jsonschema:"event types: packet, marker, log, trigger, decoded, debug, digital.edge, analysis.power_rise, analysis.power_fall"`
	Sources  []string `json:"sources,omitempty" jsonschema:"device aliases"`
	Contains string   `json:"contains,omitempty"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max events (default 100, hard max 1000)"`
}

func (t *tools) timelineQuery(_ context.Context, _ *sdk.CallToolRequest, in timelineQueryIn) (*sdk.CallToolResult, obj, error) {
	from, err := app.ParseTime(in.From)
	if err != nil {
		return nil, nil, err
	}
	to, err := app.ParseTime(in.To)
	if err != nil {
		return nil, nil, err
	}
	limit := clamp(in.Limit, 100, 1000)
	evs, err := t.app.QueryTimeline(app.TimelineQuery{Session: orLatest(in.Session), FromNs: from, ToNs: to, Types: in.Types,
		Sources: in.Sources, Contains: in.Contains, Limit: limit + 1})
	if err != nil {
		return nil, nil, err
	}
	truncated := len(evs) > limit
	if truncated {
		evs = evs[:limit]
	}
	if evs == nil {
		evs = []timeline.Event{}
	}
	return nil, obj{"events": evs, "count": len(evs), "truncated": truncated}, nil
}

type nearestIn struct {
	Session string   `json:"session,omitempty"`
	At      string   `json:"at" jsonschema:"timestamp, e.g. 12.34ms"`
	Types   []string `json:"types,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

func (t *tools) timelineNearest(_ context.Context, _ *sdk.CallToolRequest, in nearestIn) (*sdk.CallToolResult, obj, error) {
	at, err := app.ParseTime(in.At)
	if err != nil {
		return nil, nil, err
	}
	r, err := t.app.Nearest(orLatest(in.Session), at, timeline.Filter{Types: in.Types, Sources: in.Sources})
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"found": r.Found, "event": r.Event, "delta_ns": r.Delta}, nil
}

type clockCalIn struct {
	Session    string `json:"session,omitempty"`
	Reference  string `json:"reference" jsonschema:"alias of the reference capture device"`
	Device     string `json:"device" jsonschema:"alias of the device whose clock is mapped onto the reference"`
	RefChannel string `json:"reference_channel,omitempty" jsonschema:"channel name of the shared signal on the reference device"`
	DevChannel string `json:"device_channel,omitempty" jsonschema:"channel name of the shared signal on the device"`
}

func (t *tools) clockCalibrate(_ context.Context, _ *sdk.CallToolRequest, in clockCalIn) (*sdk.CallToolResult, obj, error) {
	r, err := t.app.CalibrateClocks(app.ClockCalibrationQuery{Session: orLatest(in.Session), Ref: in.Reference, Dev: in.Device,
		RefChan: in.RefChannel, DevChan: in.DevChannel})
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"calibration": r.Calibration, "warnings": r.Warnings, "ref_edges": r.RefEdges, "dev_edges": r.DevEdges}, nil
}

type correlateIn struct {
	Session       string   `json:"session,omitempty"`
	AnchorTypes   []string `json:"anchor_types" jsonschema:"anchor event types, e.g. analysis.power_rise"`
	AnchorSources []string `json:"anchor_sources,omitempty"`
	RelatedTypes  []string `json:"related_types,omitempty" jsonschema:"related event types, e.g. packet"`
	RelatedSource []string `json:"related_sources,omitempty"`
	Window        string   `json:"window,omitempty" jsonschema:"window around each anchor (default 5ms)"`
	Limit         int      `json:"limit,omitempty" jsonschema:"anchors to list (default 20, max 200)"`
}

func (t *tools) timelineCorrelate(_ context.Context, _ *sdk.CallToolRequest, in correlateIn) (*sdk.CallToolResult, obj, error) {
	w, err := app.ParseTime(in.Window)
	if err != nil {
		return nil, nil, err
	}
	res, err := t.app.Correlate(app.CorrelateQuery{Session: orLatest(in.Session), WindowNs: w, Limit: clamp(in.Limit, 20, 200),
		A: timeline.Filter{Types: in.AnchorTypes, Sources: in.AnchorSources},
		B: timeline.Filter{Types: in.RelatedTypes, Sources: in.RelatedSource}})
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"correlation": res}, nil
}

type explainIn struct {
	Session        string  `json:"session,omitempty"`
	Spike          int     `json:"spike,omitempty" jsonschema:"anchor on spike N (1 = first in time)"`
	Packet         int     `json:"packet,omitempty" jsonschema:"anchor on packet N (1 = first in time)"`
	At             string  `json:"at,omitempty" jsonschema:"anchor on a session time such as 4.38s"`
	Window         string  `json:"window,omitempty" jsonschema:"context window (default 5ms)"`
	Source         string  `json:"source,omitempty" jsonschema:"power device alias; default first power stream"`
	NominalVoltage float64 `json:"nominal_voltage,omitempty" jsonschema:"supply voltage for energy when the stream has no voltage samples"`
}

func (t *tools) timelineExplain(ctx context.Context, _ *sdk.CallToolRequest, in explainIn) (*sdk.CallToolResult, obj, error) {
	w, err := app.ParseTime(in.Window)
	if err != nil {
		return nil, nil, err
	}
	q := app.ExplainQuery{Session: orLatest(in.Session), Source: in.Source, Spike: in.Spike, Packet: in.Packet,
		WindowNs: w, NominalVoltage: in.NominalVoltage}
	if in.At != "" {
		if q.AtNs, err = app.ParseTime(in.At); err != nil {
			return nil, nil, err
		}
		q.HasAt = true
	}
	res, err := t.app.Explain(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"explanation": res}, nil
}

type waveformIn struct {
	Session string `json:"session,omitempty"`
	Source  string `json:"source,omitempty" jsonschema:"device alias; default every analog stream"`
	Channel string `json:"channel,omitempty" jsonschema:"channel name; default the first channel of each stream"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Points  int    `json:"points,omitempty" jsonschema:"envelope buckets per segment (default 200, max 1000)"`
}

func (t *tools) waveformMeasure(ctx context.Context, _ *sdk.CallToolRequest, in waveformIn) (*sdk.CallToolResult, obj, error) {
	q := app.WaveformQuery{Session: orLatest(in.Session), Source: in.Source, Channel: in.Channel, Points: in.Points}
	var err error
	for _, f := range []struct {
		s string
		d *int64
	}{{in.From, &q.FromNs}, {in.To, &q.ToNs}} {
		if f.s != "" {
			if *f.d, err = app.ParseTime(f.s); err != nil {
				return nil, nil, err
			}
		}
	}
	rep, err := t.app.MeasureWaveform(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"waveform": rep}, nil
}

type modbusIn struct {
	Session string `json:"session,omitempty"`
	Source  string `json:"source,omitempty" jsonschema:"uart device alias; default every serial source"`
	Mode    string `json:"mode,omitempty" jsonschema:"master (default), slave or sniff"`
	Gap     string `json:"gap,omitempty" jsonschema:"idle gap that ends a frame (default 50ms)"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Limit   int    `json:"limit,omitempty" jsonschema:"transactions listed per source (default 100, max 1000)"`
}

func (t *tools) modbusDecode(_ context.Context, _ *sdk.CallToolRequest, in modbusIn) (*sdk.CallToolResult, obj, error) {
	q := app.ModbusQuery{Session: orLatest(in.Session), Source: in.Source, Mode: in.Mode, Limit: in.Limit}
	var err error
	for _, f := range []struct {
		s string
		d *int64
	}{{in.Gap, &q.GapNs}, {in.From, &q.FromNs}, {in.To, &q.ToNs}} {
		if f.s != "" {
			if *f.d, err = app.ParseTime(f.s); err != nil {
				return nil, nil, err
			}
		}
	}
	rep, err := t.app.DecodeModbus(q)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"modbus": rep}, nil
}

type usbIn struct {
	Session    string `json:"session,omitempty"`
	Source     string `json:"source,omitempty" jsonschema:"usb source alias; default every USB source"`
	Device     string `json:"device,omitempty" jsonschema:"list only this bus:device, e.g. 1:5; statistics stay global"`
	OnlyErrors bool   `json:"only_errors,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Limit      int    `json:"limit,omitempty" jsonschema:"transactions listed per source (default 100, max 1000)"`
}

func (t *tools) usbAnalyze(_ context.Context, _ *sdk.CallToolRequest, in usbIn) (*sdk.CallToolResult, obj, error) {
	q := app.USBQuery{Session: orLatest(in.Session), Source: in.Source, Device: in.Device, OnlyErrors: in.OnlyErrors, Limit: in.Limit}
	var err error
	for _, f := range []struct {
		s string
		d *int64
	}{{in.From, &q.FromNs}, {in.To, &q.ToNs}} {
		if f.s != "" {
			if *f.d, err = app.ParseTime(f.s); err != nil {
				return nil, nil, err
			}
		}
	}
	rep, err := t.app.AnalyzeUSB(q)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"usb": rep}, nil
}

type analyzeIn struct {
	Session        string  `json:"session,omitempty"`
	Source         string  `json:"source,omitempty" jsonschema:"device alias; empty = all power streams"`
	From           string  `json:"from,omitempty"`
	To             string  `json:"to,omitempty"`
	NominalVoltage float64 `json:"nominal_voltage,omitempty"`
	Spikes         bool    `json:"spikes,omitempty" jsonschema:"also detect current spikes"`
}

func (t *tools) powerAnalyze(ctx context.Context, _ *sdk.CallToolRequest, in analyzeIn) (*sdk.CallToolResult, obj, error) {
	from, err := app.ParseTime(in.From)
	if err != nil {
		return nil, nil, err
	}
	to, err := app.ParseTime(in.To)
	if err != nil {
		return nil, nil, err
	}
	res, err := t.app.Analyze(ctx, app.AnalyzeOptions{Session: orLatest(in.Session), Source: in.Source, FromNs: from, ToNs: to,
		NominalVoltage: in.NominalVoltage, Spikes: in.Spikes})
	if err != nil {
		return nil, nil, err
	}
	for i := range res {
		if len(res[i].Spikes) > 100 {
			res[i].Spikes = res[i].Spikes[:100]
		}
	}
	return nil, obj{"power": res}, nil
}

type plotIn struct {
	Session  string           `json:"session,omitempty"`
	From     string           `json:"from,omitempty"`
	To       string           `json:"to,omitempty"`
	Format   string           `json:"format,omitempty" jsonschema:"png (default; returned inline) or svg"`
	Title    string           `json:"title,omitempty"`
	LogPower bool             `json:"log_power,omitempty"`
	Width    int              `json:"width,omitempty"`
	Tracks   []plot.TrackSpec `json:"tracks,omitempty" jsonschema:"explicit tracks; empty = automatic"`
}

const maxInlineImage = 2 << 20

func (t *tools) plotRender(ctx context.Context, _ *sdk.CallToolRequest, in plotIn) (*sdk.CallToolResult, obj, error) {
	from, err := app.ParseTime(in.From)
	if err != nil {
		return nil, nil, err
	}
	to, err := app.ParseTime(in.To)
	if err != nil {
		return nil, nil, err
	}
	format := in.Format
	if format == "" {
		format = "png"
	}
	name := fmt.Sprintf("mcp-%d-%d", from, to)
	pr, err := t.app.RenderPlot(ctx, orLatest(in.Session), name, "", plot.Spec{Title: in.Title, StartNs: from, EndNs: to,
		Format: format, LogPower: in.LogPower, Width: in.Width, Tracks: in.Tracks})
	if err != nil {
		return nil, nil, err
	}
	out := obj{"path": pr.Path, "format": pr.Format, "start_ns": pr.StartNs, "end_ns": pr.EndNs, "tracks": pr.Tracks,
		"width": pr.Width, "height": pr.Height}
	res := &sdk.CallToolResult{}
	if pr.Format == "png" && len(pr.Data) <= maxInlineImage {
		res.Content = []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("Plot saved to %s", pr.Path)}, &sdk.ImageContent{Data: pr.Data, MIMEType: pr.MIME}}
	} else {
		res.Content = []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("Plot saved to %s (%s)", pr.Path, pr.MIME)}}
	}
	return res, out, nil
}

func orLatest(s string) string {
	if s == "" {
		return "latest"
	}
	return s
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
