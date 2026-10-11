package scpiplugin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/scpi"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

const (
	maxEnvelope     = 1000
	defaultEnvelope = 200
	// inlineBinary is the largest binary reply returned inline (base64).
	inlineBinary = 4096
)

func m(capability string, danger pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: capability, Name: name, Description: desc, Danger: danger, ArgsSchemaJson: schema}
}

const chProp = `"channel":{"type":"number","minimum":1}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	S, O := pluginv1.CapSCPI, pluginv1.CapOscilloscope
	return &pluginv1.PluginInfo{
		Id: "scpi", Name: "SCPI instruments (oscilloscopes)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(),
		Methods: []*pluginv1.MethodSpec{
			m(S, safe, "identify", "*IDN? plus the detected profile, its validation state and known gaps", `{"type":"object"}`),
			m(S, safe, "query", "send ONE query (every ';' message must have a header ending in '?') and return the reply text; anything else is refused",
				`{"type":"object","required":["command"],"properties":{"command":{"type":"string"}}}`),
			m(S, safe, "query_binary", "send a query whose reply is an IEEE 488.2 block; returns size, sha256, a hex preview and, up to 4 KiB, the data as base64",
				`{"type":"object","required":["command"],"properties":{"command":{"type":"string"}}}`),
			m(S, confirm, "command", "send a raw SCPI message that is not a query; it can change or actuate the instrument (setting, reset, output enable...); check_error reads :SYSTem:ERRor? afterwards",
				`{"type":"object","required":["command"],"properties":{"command":{"type":"string"},"check_error":{"type":"boolean"}}}`),

			m(O, safe, "channels", "settings of one channel or of all channels (scale, offset, coupling, enabled, probe)", `{"type":"object","properties":{`+chProp+`}}`),
			m(O, confirm, "configure_channel", "set vertical scale (V/div), offset (V), probe ratio, coupling (dc|ac|gnd) and/or enabled of one channel",
				`{"type":"object","required":["channel"],"properties":{`+chProp+`,"scale":{"type":"number"},"offset":{"type":"number"},"probe":{"type":"number"},"coupling":{"enum":["dc","ac","gnd"]},"enabled":{"type":"boolean"}}}`),
			m(O, safe, "timebase", "horizontal scale (s/div) and offset", `{"type":"object"}`),
			m(O, confirm, "set_timebase", "set horizontal scale (s/div) and/or offset (s)", `{"type":"object","properties":{"scale":{"type":"number"},"offset":{"type":"number"}}}`),
			m(O, safe, "trigger", "edge-trigger source, slope, level and sweep", `{"type":"object"}`),
			m(O, confirm, "set_trigger", "set edge-trigger channel, slope (rising|falling|either), level (V) and/or sweep (auto|normal|single)",
				`{"type":"object","properties":{`+chProp+`,"slope":{"enum":["rising","falling","either"]},"level":{"type":"number"},"sweep":{"enum":["auto","normal","single"]}}}`),
			m(O, safe, "trigger_status", "acquisition state: triggered, waiting, running, auto or stopped", `{"type":"object"}`),
			m(O, confirm, "acquire", "run, stop, single or force the acquisition", `{"type":"object","required":["action"],"properties":{"action":{"enum":["run","stop","single","force"]}}}`),
			m(O, safe, "measure", "one built-in measurement of a channel (vpp, vmax, vmin, vavg, vrms, frequency, period, rise_time, fall_time, pulse_width, neg_pulse_width, duty)",
				`{"type":"object","required":["item","channel"],"properties":{"item":{"type":"string"},`+chProp+`}}`),
			m(O, safe, "waveform", "transfer the current waveform of one or more channels and return statistics plus a min/max envelope of at most `points` buckets (default 200, max 1000); it sets the instrument's waveform source; the full data belongs in a capture artifact",
				`{"type":"object","properties":{`+chProp+`,"channels":{"type":"array","items":{"type":"number"}},"points":{"type":"number","maximum":1000}}}`),
		},
	}
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s must be finite", key)
	}
	return f, true, nil
}

func optFloat(a map[string]any, key string) (*float64, error) {
	f, ok, err := optNumber(a, key)
	if err != nil || !ok {
		return nil, err
	}
	return &f, nil
}

func optBool(a map[string]any, key string) (*bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return nil, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return nil, fmt.Errorf("%s: want true or false, got %v", key, v)
	}
	return &b, nil
}

func optString(a map[string]any, key string) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("%s: want a string, got %v", key, v)
	}
	return s, nil
}

func optChannel(a map[string]any, key string) (int, bool, error) {
	f, ok, err := optNumber(a, key)
	if err != nil || !ok {
		return 0, false, err
	}
	if f != math.Trunc(f) || f < 1 {
		return 0, false, fmt.Errorf("%s %v: want a channel number >= 1", key, f)
	}
	return int(f), true, nil
}

func reqChannel(a map[string]any) (int, error) {
	ch, ok, err := optChannel(a, "channel")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("channel is required")
	}
	return ch, nil
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

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if c := req.GetCapability(); c != pluginv1.CapSCPI && c != pluginv1.CapOscilloscope {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", c)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, release, err := p.lease(req.GetDeviceId(), nil)
	if err != nil {
		return nil, err
	}
	defer release()

	var res map[string]any
	switch req.GetCapability() + "." + req.GetMethod() {
	case "SCPI.identify":
		res = d.identify()
	case "SCPI.query":
		res, err = d.query(args)
	case "SCPI.query_binary":
		res, err = d.queryBinary(args)
	case "SCPI.command":
		res, err = d.command(args)
	case "Oscilloscope.channels":
		res, err = d.channels(args)
	case "Oscilloscope.configure_channel":
		res, err = d.configureChannel(args)
	case "Oscilloscope.timebase":
		res, err = d.scope.Timebase()
	case "Oscilloscope.set_timebase":
		res, err = d.setTimebase(args)
	case "Oscilloscope.trigger":
		res, err = d.scope.Trigger()
	case "Oscilloscope.set_trigger":
		res, err = d.setTrigger(args)
	case "Oscilloscope.trigger_status":
		res, err = d.triggerStatus()
	case "Oscilloscope.acquire":
		res, err = d.acquire(args)
	case "Oscilloscope.measure":
		res, err = d.measure(args)
	case "Oscilloscope.waveform":
		res, err = d.waveform(args)
	default:
		return nil, status.Errorf(codes.Unimplemented, "method %s.%s", req.GetCapability(), req.GetMethod())
	}
	if err != nil {
		return nil, wrap(err)
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

// wrap keeps gRPC statuses and maps instrument errors to codes: a timeout is
// Unavailable, validation errors raised by the scope layer are arguments.
func wrap(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	if scpiTimeout(err) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func scpiTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), scpi.ErrTimeout.Error())
}

func (d *device) identify() map[string]any {
	pr := d.scope.P
	return map[string]any{
		"manufacturer": d.idn.Manufacturer, "model": d.idn.Model, "serial": d.idn.Serial, "firmware": d.idn.Firmware, "idn": d.idn.Raw,
		"endpoint": d.inst.Describe(), "profile": pr.ID, "profile_name": pr.Name, "profile_validated": pr.Validated,
		"profile_notes": strs(pr.Notes), "scope_semantics": !pr.Scopeless(), "channels": float64(d.scope.Channels),
		"stale_reply_bytes": float64(d.inst.StaleBytes()),
	}
}

func (d *device) commandArg(a map[string]any) (string, error) {
	c, err := optString(a, "command")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(c) == "" {
		return "", fmt.Errorf("command is required")
	}
	return c, nil
}

func (d *device) query(a map[string]any) (map[string]any, error) {
	c, err := d.commandArg(a)
	if err != nil {
		return nil, argErr(err)
	}
	if !scpi.IsQuery(c) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a query (a query's header ends with '?'): use SCPI.command, which needs confirmation", c)
	}
	r, err := d.inst.Query(c)
	if err != nil {
		return nil, err
	}
	return map[string]any{"command": c, "reply": r}, nil
}

func (d *device) queryBinary(a map[string]any) (map[string]any, error) {
	c, err := d.commandArg(a)
	if err != nil {
		return nil, argErr(err)
	}
	if !scpi.IsQuery(c) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a query (a query's header ends with '?'): use SCPI.command, which needs confirmation", c)
	}
	data, prefix, err := d.inst.QueryBinary(c)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	prev := data
	if len(prev) > 32 {
		prev = prev[:32]
	}
	res := map[string]any{"command": c, "bytes": float64(len(data)), "sha256": hex.EncodeToString(sum[:]), "preview_hex": hex.EncodeToString(prev), "prefix": prefix}
	if len(data) <= inlineBinary {
		res["data_base64"] = base64.StdEncoding.EncodeToString(data)
	} else {
		res["note"] = fmt.Sprintf("%d bytes exceed the %d byte inline limit: use a capture to store large data as an artifact", len(data), inlineBinary)
	}
	return res, nil
}

func (d *device) command(a map[string]any) (map[string]any, error) {
	c, err := d.commandArg(a)
	if err != nil {
		return nil, argErr(err)
	}
	chk, err := optBool(a, "check_error")
	if err != nil {
		return nil, argErr(err)
	}
	if scpi.IsQuery(c) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is a query: use SCPI.query", c)
	}
	if err := d.inst.Command(c); err != nil {
		return nil, err
	}
	res := map[string]any{"command": c, "sent": true}
	if chk != nil && *chk {
		code, msg, qerr := d.inst.ErrorQueue()
		switch {
		case qerr != nil:
			res["error_queue"] = "unavailable: " + qerr.Error()
		default:
			res["error_queue"] = map[string]any{"code": float64(code), "message": msg}
		}
	}
	return res, nil
}

func (d *device) channels(a map[string]any) (map[string]any, error) {
	if ch, ok, err := optChannel(a, "channel"); err != nil {
		return nil, argErr(err)
	} else if ok {
		st, err := d.scope.ChannelState(ch)
		if err != nil {
			return nil, err
		}
		return map[string]any{"channels": []any{st}}, nil
	}
	out := make([]any, 0, d.scope.Channels)
	for ch := 1; ch <= d.scope.Channels; ch++ {
		st, err := d.scope.ChannelState(ch)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return map[string]any{"channels": out}, nil
}

func (d *device) configureChannel(a map[string]any) (map[string]any, error) {
	ch, err := reqChannel(a)
	if err != nil {
		return nil, argErr(err)
	}
	var cs scpi.ChannelSettings
	if cs.Scale, err = optFloat(a, "scale"); err != nil {
		return nil, argErr(err)
	}
	if cs.Offset, err = optFloat(a, "offset"); err != nil {
		return nil, argErr(err)
	}
	if cs.Probe, err = optFloat(a, "probe"); err != nil {
		return nil, argErr(err)
	}
	if cs.Enabled, err = optBool(a, "enabled"); err != nil {
		return nil, argErr(err)
	}
	if cs.Coupling, err = optString(a, "coupling"); err != nil {
		return nil, argErr(err)
	}
	cmds, err := d.scope.ConfigureChannel(ch, cs)
	if err != nil {
		return nil, wrapArg(err)
	}
	return map[string]any{"channel": float64(ch), "sent": strs(cmds)}, nil
}

// wrapArg reports a scope-layer validation error as an argument error unless it
// is already a status or an instrument timeout.
func wrapArg(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	if scpiTimeout(err) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func (d *device) setTimebase(a map[string]any) (map[string]any, error) {
	scale, err := optFloat(a, "scale")
	if err != nil {
		return nil, argErr(err)
	}
	off, err := optFloat(a, "offset")
	if err != nil {
		return nil, argErr(err)
	}
	cmds, err := d.scope.SetTimebase(scale, off)
	if err != nil {
		return nil, wrapArg(err)
	}
	return map[string]any{"sent": strs(cmds)}, nil
}

func (d *device) setTrigger(a map[string]any) (map[string]any, error) {
	var ts scpi.TriggerSettings
	var err error
	if ch, ok, e := optChannel(a, "channel"); e != nil {
		return nil, argErr(e)
	} else if ok {
		ts.Channel = ch
	}
	if ts.Level, err = optFloat(a, "level"); err != nil {
		return nil, argErr(err)
	}
	if ts.Slope, err = optString(a, "slope"); err != nil {
		return nil, argErr(err)
	}
	if ts.Sweep, err = optString(a, "sweep"); err != nil {
		return nil, argErr(err)
	}
	cmds, err := d.scope.SetTrigger(ts)
	if err != nil {
		return nil, wrapArg(err)
	}
	return map[string]any{"sent": strs(cmds)}, nil
}

func (d *device) triggerStatus() (map[string]any, error) {
	st, raw, err := d.scope.TriggerState()
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": st, "raw": raw}, nil
}

func (d *device) acquire(a map[string]any) (map[string]any, error) {
	act, err := optString(a, "action")
	if err != nil {
		return nil, argErr(err)
	}
	cmd, err := d.scope.Acquire(act)
	if err != nil {
		return nil, wrapArg(err)
	}
	return map[string]any{"action": strings.ToLower(act), "sent": cmd}, nil
}

func (d *device) measure(a map[string]any) (map[string]any, error) {
	item, err := optString(a, "item")
	if err != nil {
		return nil, argErr(err)
	}
	ch, err := reqChannel(a)
	if err != nil {
		return nil, argErr(err)
	}
	r, err := d.scope.Measure(item, ch)
	if err != nil {
		return nil, wrapArg(err)
	}
	return r, nil
}

func waveSummary(w *scpi.Waveform, points int) map[string]any {
	s := w.Stats()
	e := w.Envelope(points)
	out := map[string]any{
		"channel": float64(w.Channel), "source": w.Source, "points": float64(s.N), "dt_s": w.DT, "t0_s": w.T0,
		"stats":    map[string]any{"min_v": s.Min, "max_v": s.Max, "mean_v": s.Mean, "rms_v": s.RMS, "vpp_v": s.Vpp},
		"envelope": map[string]any{"buckets": float64(len(e.Min)), "t0_s": e.T0, "bucket_dt_s": e.BucketDT, "min_v": floats(e.Min), "max_v": floats(e.Max)},
	}
	if w.Note != "" {
		out["note"] = w.Note
	}
	return out
}

func (d *device) waveform(a map[string]any) (map[string]any, error) {
	pts := defaultEnvelope
	if f, ok, err := optNumber(a, "points"); err != nil {
		return nil, argErr(err)
	} else if ok {
		if f < 1 || f > maxEnvelope || f != math.Trunc(f) {
			return nil, status.Errorf(codes.InvalidArgument, "points %v: want an integer 1..%d (use a capture for the full data)", f, maxEnvelope)
		}
		pts = int(f)
	}
	var chans []int
	if raw, ok := a["channels"].([]any); ok {
		for _, v := range raw {
			f, isNum := v.(float64)
			if !isNum || f != math.Trunc(f) || f < 1 {
				return nil, status.Errorf(codes.InvalidArgument, "channels: %v is not a channel number", v)
			}
			chans = append(chans, int(f))
		}
	} else if ch, ok, err := optChannel(a, "channel"); err != nil {
		return nil, argErr(err)
	} else if ok {
		chans = []int{ch}
	} else {
		chans = []int{1}
	}
	if len(chans) > d.scope.Channels {
		return nil, status.Errorf(codes.InvalidArgument, "%d channels requested, the instrument has %d", len(chans), d.scope.Channels)
	}
	out := make([]any, 0, len(chans))
	for _, ch := range chans {
		w, err := d.scope.Waveform(ch)
		if err != nil {
			return nil, wrapArg(err)
		}
		out = append(out, waveSummary(w, pts))
	}
	return map[string]any{"waveforms": out}, nil
}
