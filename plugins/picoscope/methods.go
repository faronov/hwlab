package picoscopeplugin

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/waveform"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/picoscope/backend"
)

const (
	maxEnvelope     = 1000
	defaultEnvelope = 200
	// maxBlockSamples bounds one block (all channels together): the samples
	// become session events and a CSV artifact, and are held in memory.
	maxBlockSamples = 4_000_000
)

func m(danger pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapOscilloscope, Name: name, Description: desc, Danger: danger, ArgsSchemaJson: schema}
}

const (
	chanSchema = `"channel":{"type":"string","description":"channel name, e.g. A"}`
	setNote    = "host-side setting, applied at the next acquisition; nothing is driven on the instrument or the target"
)

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe := pluginv1.Danger_DANGER_SAFE
	return &pluginv1.PluginInfo{
		Id: "picoscope", Name: "PicoScope oscilloscopes (block mode)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(),
		Methods: []*pluginv1.MethodSpec{
			m(safe, "info", "instrument identity, channels, input ranges, resolutions and limits; validated is false until a real driver has run on hardware", `{"type":"object"}`),
			m(safe, "channels", "enabled state, range, offset and coupling of every channel", `{"type":"object"}`),
			m(safe, "configure_channel", "set enabled, range_v, offset_v and/or coupling (dc|ac) of one channel; "+setNote,
				`{"type":"object","required":["channel"],"properties":{`+chanSchema+`,"enabled":{"type":"boolean"},"range_v":{"type":"number"},"offset_v":{"type":"number"},"coupling":{"enum":["dc","ac"]}}}`),
			m(safe, "timebase", "sample rate, sample count, resolution and pre-trigger fraction, with the rate the instrument would really use", `{"type":"object"}`),
			m(safe, "set_timebase", "set sample_rate_hz, samples, resolution_bits and/or pre_trigger; the instrument rounds the rate to its grid and the result says what it will use; "+setNote,
				`{"type":"object","properties":{"sample_rate_hz":{"type":"number"},"samples":{"type":"number"},"resolution_bits":{"type":"number"},"pre_trigger":{"type":"number","minimum":0,"maximum":1}}}`),
			m(safe, "trigger", "edge trigger source, level, slope and timeout", `{"type":"object"}`),
			m(safe, "set_trigger", "set trigger_source (a channel or none), trigger_level_v, trigger_slope (rising|falling|either), trigger_timeout_s and/or trigger_auto; "+setNote,
				`{"type":"object","properties":{"trigger_source":{"type":"string"},"trigger_level_v":{"type":"number"},"trigger_slope":{"enum":["rising","falling","either"]},"trigger_timeout_s":{"type":"number"},"trigger_auto":{"type":"boolean"}}}`),
			m(safe, "acquire", "run ONE block acquisition (optionally with per-call overrides of any setting) and return per-channel measurements; the block stays in memory for measure and waveform; a capture stores samples in the session",
				`{"type":"object"}`),
			m(safe, "measure", "measurements of the last block for one channel, optionally in a window: min, max, mean, rms, vpp, frequency, period, duty, pulse widths, rise and fall time",
				`{"type":"object","properties":{`+chanSchema+`,"from_s":{"type":"number"},"to_s":{"type":"number"}}}`),
			m(safe, "waveform", "statistics plus a min/max envelope of at most `points` buckets (default 200, max 1000) of the last block for one or more channels and an optional window; full data belongs in a capture artifact",
				`{"type":"object","properties":{`+chanSchema+`,"channels":{"type":"array","items":{"type":"string"}},"from_s":{"type":"number"},"to_s":{"type":"number"},"points":{"type":"number","maximum":1000}}}`),
		},
	}
}

// acquisition is the last block with the settings it was taken with.
type acquisition struct {
	blk    *backend.Block
	set    settings
	res    backend.Resolved
	traces map[string]waveform.Trace
}

func newAcquisition(blk *backend.Block, s settings, res backend.Resolved) *acquisition {
	a := &acquisition{blk: blk, set: s, res: res, traces: map[string]waveform.Trace{}}
	t0 := 0.0
	if blk.Triggered {
		t0 = -float64(blk.TriggerIndex) * blk.SampleIntervalS
	}
	for _, c := range blk.Channels {
		a.traces[c.Name] = waveform.Trace{T0: t0, DT: blk.SampleIntervalS, V: c.Volts()}
	}
	return a
}

func (a *acquisition) channelNames() []string {
	out := make([]string, len(a.blk.Channels))
	for i, c := range a.blk.Channels {
		out[i] = c.Name
	}
	return out
}

func (a *acquisition) data(name string) *backend.ChannelData {
	for i := range a.blk.Channels {
		if strings.EqualFold(a.blk.Channels[i].Name, name) {
			return &a.blk.Channels[i]
		}
	}
	return nil
}

// toMap turns a JSON-tagged struct into a structpb-compatible map.
func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if c := req.GetCapability(); c != pluginv1.CapOscilloscope {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", c)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, transient, release, err := p.lease(req.GetDeviceId(), nil)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	// reads of settings and of the last block stay available during a capture;
	// anything that would touch the instrument or change its setup does not
	if d.busy {
		switch req.GetMethod() {
		case "acquire", "configure_channel", "set_timebase", "set_trigger":
			return nil, errf(codes.FailedPrecondition, "%s is running a capture", d.id)
		}
	}

	var res map[string]any
	switch req.GetMethod() {
	case "info":
		res = d.infoResult()
	case "channels":
		res = d.channelsResult()
	case "configure_channel":
		res, err = d.configureChannel(transient, args)
	case "timebase":
		res, err = d.timebaseResult(d.set)
	case "set_timebase":
		res, err = d.setSettings(transient, args, timeKeys, d.timebaseResult)
	case "trigger":
		res = d.triggerResult(d.set)
	case "set_trigger":
		res, err = d.setSettings(transient, args, trigKeys, func(s settings) (map[string]any, error) { return d.triggerResult(s), nil })
	case "acquire":
		res, err = d.acquire(ctx, args)
	case "measure":
		res, err = d.measure(args)
	case "waveform":
		res, err = d.waveform(args)
	default:
		return nil, status.Errorf(codes.Unimplemented, "method %s.%s", req.GetCapability(), req.GetMethod())
	}
	if err != nil {
		if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func floats(v []float64) []any {
	out := make([]any, len(v))
	for i, f := range v {
		out[i] = f
	}
	return out
}

func strs(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func ints(v []int) []any {
	out := make([]any, len(v))
	for i, n := range v {
		out[i] = float64(n)
	}
	return out
}

func (d *device) infoResult() map[string]any {
	i := d.info
	lim := d.b.Limits(max(1, len(d.set.enabledNames())), d.set.bits)
	res := map[string]any{
		"vendor": i.Vendor, "model": i.Model, "serial": i.Serial, "firmware": i.Firmware, "driver": i.Driver,
		"channels": strs(i.Channels), "resolutions_bits": ints(i.ResolutionsBits), "ranges_v": floats(i.Ranges),
		"max_memory_samples": float64(i.MaxMemory), "max_block_samples": float64(maxBlockSamples),
		"max_sample_rate_hz": lim.MaxSampleRateHz, "max_samples_per_channel": float64(lim.MaxSamples),
		"mode": "block", "streaming": "not-implemented", "validated": false,
		"limits_note": "the rate and memory limits depend on the enabled channels and the resolution; these are for the current setup",
	}
	if i.Synthetic {
		res["synthetic"] = true
	}
	return res
}

func channelMap(c backend.Channel) map[string]any {
	return map[string]any{"channel": c.Name, "enabled": c.Enabled, "range_v": c.RangeV, "offset_v": c.OffsetV, "coupling": c.Coupling}
}

func (d *device) channelsResult() map[string]any {
	out := make([]any, len(d.set.channels))
	for i, c := range d.set.channels {
		out[i] = channelMap(c)
	}
	return map[string]any{"channels": out}
}

// plan validates s and describes what the instrument would do.
func (d *device) plan(s settings) (backend.Resolved, map[string]any, error) {
	r := s.request()
	res, err := d.b.Plan(r)
	if err != nil {
		return res, nil, argErr(err)
	}
	lim := d.b.Limits(res.Enabled, s.bits)
	out := map[string]any{
		"requested_sample_rate_hz": s.rate, "sample_rate_hz": res.SampleRateHz, "sample_interval_s": res.SampleIntervalS,
		"samples": float64(res.Samples), "duration_s": float64(res.Samples) * res.SampleIntervalS, "resolution_bits": float64(s.bits),
		"pre_trigger": r.PreTrigger, "enabled_channels": strs(s.enabledNames()),
		"max_sample_rate_hz": lim.MaxSampleRateHz, "max_samples_per_channel": float64(lim.MaxSamples),
		"rate_adjusted": math.Abs(res.SampleRateHz-s.rate) > 1e-9*s.rate,
	}
	if res.Samples*res.Enabled > maxBlockSamples {
		return res, nil, errf(codes.InvalidArgument, "%d samples on %d channel(s) exceed the %d samples a block may hold in total", res.Samples, res.Enabled, maxBlockSamples)
	}
	if s.rate > lim.MaxSampleRateHz {
		out["note"] = "the requested rate exceeds the instrument's maximum for this setup and was limited"
	}
	return res, out, nil
}

func (d *device) timebaseResult(s settings) (map[string]any, error) {
	_, out, err := d.plan(s)
	return out, err
}

func (d *device) triggerResult(s settings) map[string]any {
	t := s.trig
	res := map[string]any{"enabled": t.Enabled, "source": t.Source, "level_v": t.LevelV, "slope": t.Slope, "timeout_s": t.TimeoutS, "auto": t.Auto}
	return res
}

func (d *device) refuseTransient(transient bool) error {
	if transient {
		return errf(codes.FailedPrecondition, "settings live in an opened device and would be lost after this call: open the scope (an experiment device, or hwlab lab) first, or pass the settings to Oscilloscope.acquire directly")
	}
	return nil
}

// commit validates the new settings with the backend and keeps them.
func (d *device) commit(s settings) error {
	if _, _, err := d.plan(s); err != nil {
		return err
	}
	d.set = s
	return nil
}

func (d *device) configureChannel(transient bool, a map[string]any) (map[string]any, error) {
	if err := d.refuseTransient(transient); err != nil {
		return nil, err
	}
	name, _ := a["channel"].(string)
	if name == "" {
		return nil, errf(codes.InvalidArgument, "channel is required")
	}
	s := d.set.clone()
	c := s.channel(name)
	if c == nil {
		return nil, errf(codes.InvalidArgument, "no channel %q (have %s)", name, channelList(s))
	}
	for k := range a {
		if k != "channel" && k != "enabled" && k != "range_v" && k != "offset_v" && k != "coupling" {
			return nil, errf(codes.InvalidArgument, "unknown argument %q", k)
		}
	}
	if v, ok := a["enabled"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, errf(codes.InvalidArgument, "enabled: want true or false, got %v", v)
		}
		c.Enabled = b
	}
	for _, f := range []struct {
		key string
		dst *float64
	}{{"range_v", &c.RangeV}, {"offset_v", &c.OffsetV}} {
		if v, ok := a[f.key]; ok {
			x, isNum := v.(float64)
			if !isNum || math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, errf(codes.InvalidArgument, "%s: want a finite number, got %v", f.key, v)
			}
			*f.dst = x
		}
	}
	if v, ok := a["coupling"]; ok {
		cs, isStr := v.(string)
		if !isStr {
			return nil, errf(codes.InvalidArgument, "coupling: want dc or ac, got %v", v)
		}
		c.Coupling = strings.ToLower(cs)
	}
	if err := d.commit(s); err != nil {
		return nil, err
	}
	return channelMap(*d.set.channel(name)), nil
}

func (d *device) setSettings(transient bool, a map[string]any, allowed []string, report func(settings) (map[string]any, error)) (map[string]any, error) {
	if err := d.refuseTransient(transient); err != nil {
		return nil, err
	}
	s, err := parseSettings(d.set, a, allowed)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.commit(s); err != nil {
		return nil, err
	}
	return report(d.set)
}

// triggerWait bounds an acquisition: the trigger timeout plus the block itself.
func triggerWait(s settings) time.Duration {
	w := 5 * time.Second
	if s.trig.Enabled {
		w += time.Duration(s.trig.TimeoutS * float64(time.Second))
	}
	return w
}

func (d *device) acquire(ctx context.Context, a map[string]any) (map[string]any, error) {
	s, err := parseSettings(d.set, a, allKeys)
	if err != nil {
		return nil, argErr(err)
	}
	res, plan, err := d.plan(s)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithTimeout(ctx, triggerWait(s))
	defer cancel()
	blk, err := d.b.Acquire(actx, s.request())
	if err != nil {
		if err == backend.ErrNoTrigger {
			return nil, errf(codes.DeadlineExceeded, "%v (source %s, level %g V, %s): check the signal, the level and the slope, or set trigger_auto", err, s.trig.Source, s.trig.LevelV, s.trig.Slope)
		}
		return nil, err
	}
	acq := newAcquisition(blk, s, res)
	d.last = acq
	out := map[string]any{"plan": plan, "triggered": blk.Triggered, "samples": float64(blk.Samples)}
	if blk.Triggered {
		out["trigger_index"] = float64(blk.TriggerIndex)
	}
	if s.trig.Enabled && !blk.Triggered {
		out["note"] = "the trigger timed out and the block is untriggered (trigger_auto)"
	}
	if d.info.Synthetic {
		out["synthetic"] = true
	}
	chs := make([]any, 0, len(blk.Channels))
	for _, c := range blk.Channels {
		ms, err := waveform.Measure(acq.traces[c.Name])
		if err != nil {
			return nil, err
		}
		mm, err := toMap(ms)
		if err != nil {
			return nil, err
		}
		chs = append(chs, map[string]any{"channel": c.Name, "range_v": c.RangeV, "overflow": c.Overflow, "measurements": mm})
	}
	out["channels"] = chs
	return out, nil
}

func (d *device) lastFor(a map[string]any) (*acquisition, error) {
	if d.last == nil {
		return nil, errf(codes.FailedPrecondition, "no block yet: call Oscilloscope.acquire (or run a capture) first")
	}
	return d.last, nil
}

func windowArgs(a map[string]any) (from, to float64, err error) {
	for _, f := range []struct {
		key string
		dst *float64
	}{{"from_s", &from}, {"to_s", &to}} {
		if v, ok := a[f.key]; ok && v != nil {
			x, isNum := v.(float64)
			if !isNum || math.IsNaN(x) || math.IsInf(x, 0) {
				return 0, 0, errf(codes.InvalidArgument, "%s: want a finite number of seconds, got %v", f.key, v)
			}
			*f.dst = x
		}
	}
	return from, to, nil
}

// pickChannel resolves the single channel argument (default: the first).
func (acq *acquisition) pickChannel(a map[string]any) (string, error) {
	name, _ := a["channel"].(string)
	if name == "" {
		return acq.channelNames()[0], nil
	}
	if c := acq.data(name); c != nil {
		return c.Name, nil
	}
	return "", errf(codes.InvalidArgument, "channel %q is not in the last block (have %s)", name, strings.Join(acq.channelNames(), ", "))
}

func (d *device) measure(a map[string]any) (map[string]any, error) {
	acq, err := d.lastFor(a)
	if err != nil {
		return nil, err
	}
	name, err := acq.pickChannel(a)
	if err != nil {
		return nil, err
	}
	from, to, err := windowArgs(a)
	if err != nil {
		return nil, err
	}
	w := acq.traces[name].Window(from, to)
	ms, err := waveform.Measure(w)
	if err != nil {
		return nil, errf(codes.InvalidArgument, "channel %s window [%g, %g) s: %v", name, from, to, err)
	}
	mm, err := toMap(ms)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"channel": name, "measurements": mm, "overflow": acq.data(name).Overflow}
	if d.info.Synthetic {
		out["synthetic"] = true
	}
	return out, nil
}

func (d *device) waveform(a map[string]any) (map[string]any, error) {
	acq, err := d.lastFor(a)
	if err != nil {
		return nil, err
	}
	points := defaultEnvelope
	if v, ok := a["points"]; ok && v != nil {
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || f < 1 || f > maxEnvelope {
			return nil, errf(codes.InvalidArgument, "points %v: want an integer 1..%d", v, maxEnvelope)
		}
		points = int(f)
	}
	var names []string
	switch {
	case a["channels"] != nil:
		list, ok := a["channels"].([]any)
		if !ok {
			return nil, errf(codes.InvalidArgument, "channels: want a list of channel names")
		}
		for _, e := range list {
			s, _ := e.(string)
			n, err := acq.pickChannel(map[string]any{"channel": s})
			if err != nil || s == "" {
				return nil, errf(codes.InvalidArgument, "channels: %v is not in the last block (have %s)", e, strings.Join(acq.channelNames(), ", "))
			}
			names = append(names, n)
		}
	default:
		n, err := acq.pickChannel(a)
		if err != nil {
			return nil, err
		}
		names = []string{n}
	}
	from, to, err := windowArgs(a)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(names))
	for _, n := range names {
		w := acq.traces[n].Window(from, to)
		if len(w.V) == 0 {
			return nil, errf(codes.InvalidArgument, "channel %s window [%g, %g) s holds no samples", n, from, to)
		}
		ms, err := waveform.Measure(w)
		if err != nil {
			return nil, err
		}
		env := waveform.MakeEnvelope(w, points)
		em, err := toMap(env)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"channel": n, "samples": float64(len(w.V)), "t0_s": w.T0, "dt_s": w.DT,
			"min": ms.Min, "max": ms.Max, "mean": ms.Mean, "rms": ms.RMS, "vpp": ms.Vpp, "envelope": em})
	}
	res := map[string]any{"channels": out, "unit": "V"}
	if d.info.Synthetic {
		res["synthetic"] = true
	}
	return res, nil
}
