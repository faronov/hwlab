package scpi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// MaxChannels is the largest channel count a Scope accepts.
const MaxChannels = 16

// Scope is the oscilloscope semantic layer over an Instrument and a vendor
// Profile. Settings go out as plain SCPI commands; reads return what the
// instrument said, parsed where it is a number.
type Scope struct {
	I        *Instrument
	P        *Profile
	Channels int
}

// NewScope binds a profile; channels <= 0 means 4.
func NewScope(i *Instrument, p *Profile, channels int) *Scope {
	if channels <= 0 {
		channels = 4
	}
	return &Scope{I: i, P: p, Channels: channels}
}

func (s *Scope) ready() error {
	if s.P.Scopeless() {
		return fmt.Errorf("no oscilloscope profile for this instrument (profile %q): use identify/command/query, or set profile: rigol|siglent in the device config", s.P.ID)
	}
	return nil
}

func (s *Scope) chanOK(ch int) error {
	if ch < 1 || ch > s.Channels {
		return fmt.Errorf("channel %d out of range 1..%d (set channels: N in the device config for other models)", ch, s.Channels)
	}
	return nil
}

// reply is one parsed instrument reply.
func reply(raw string) map[string]any {
	m := map[string]any{"raw": raw}
	if f, err := parseFloat(raw); err == nil {
		m["value"] = f
	}
	return m
}

func (s *Scope) ask(q string) (map[string]any, error) {
	raw, err := s.I.Query(q)
	if err != nil {
		return nil, err
	}
	return reply(raw), nil
}

// ChannelState reads one channel's settings. A setting the instrument does not
// answer is reported under "errors" instead of failing the call.
func (s *Scope) ChannelState(ch int) (map[string]any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.chanOK(ch); err != nil {
		return nil, err
	}
	out := map[string]any{"channel": float64(ch), "source": s.P.Source(ch)}
	errs := map[string]any{}
	for name, node := range map[string]string{"scale_v_per_div": s.P.ChanScale, "offset_v": s.P.ChanOffset,
		"coupling": s.P.ChanCoupling, "enabled": s.P.ChanEnable, "probe_ratio": s.P.ChanProbe} {
		r, err := s.ask(s.P.Chan(ch) + node + "?")
		if err != nil {
			errs[name] = err.Error()
			continue
		}
		if name == "enabled" {
			up := strings.ToUpper(r["raw"].(string))
			out[name] = up == "1" || up == "ON"
			continue
		}
		if v, ok := r["value"]; ok && name != "coupling" {
			out[name] = v
		} else {
			out[name] = r["raw"]
		}
	}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out, nil
}

// ChannelSettings are the values ConfigureChannel may change; nil leaves a
// setting alone. Impedance is deliberately absent.
type ChannelSettings struct {
	Scale, Offset, Probe *float64
	Coupling             string // dc, ac, gnd
	Enabled              *bool
}

// ConfigureChannel applies settings and returns the commands it sent.
func (s *Scope) ConfigureChannel(ch int, cs ChannelSettings) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.chanOK(ch); err != nil {
		return nil, err
	}
	var cmds []string
	base := s.P.Chan(ch)
	if cs.Scale != nil {
		if err := finite("scale", *cs.Scale); err != nil {
			return nil, err
		}
		if *cs.Scale <= 0 {
			return nil, errors.New("scale must be > 0 volts per division")
		}
		cmds = append(cmds, base+s.P.ChanScale+" "+num(*cs.Scale))
	}
	if cs.Offset != nil {
		if err := finite("offset", *cs.Offset); err != nil {
			return nil, err
		}
		cmds = append(cmds, base+s.P.ChanOffset+" "+num(*cs.Offset))
	}
	if cs.Probe != nil {
		if err := finite("probe", *cs.Probe); err != nil {
			return nil, err
		}
		if *cs.Probe <= 0 {
			return nil, errors.New("probe ratio must be > 0")
		}
		cmds = append(cmds, base+s.P.ChanProbe+" "+num(*cs.Probe))
	}
	if cs.Coupling != "" {
		c := strings.ToUpper(cs.Coupling)
		if c != "DC" && c != "AC" && c != "GND" {
			return nil, fmt.Errorf("coupling %q: want dc, ac or gnd", cs.Coupling)
		}
		cmds = append(cmds, base+s.P.ChanCoupling+" "+c)
	}
	if cs.Enabled != nil {
		v := "OFF"
		if *cs.Enabled {
			v = "ON"
		}
		cmds = append(cmds, base+s.P.ChanEnable+" "+v)
	}
	if len(cmds) == 0 {
		return nil, errors.New("nothing to configure: give scale, offset, probe, coupling or enabled")
	}
	return cmds, s.send(cmds)
}

func (s *Scope) send(cmds []string) error {
	for _, c := range cmds {
		if err := s.I.Command(c); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	return nil
}

// Timebase reads the horizontal settings.
func (s *Scope) Timebase() (map[string]any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for name, node := range map[string]string{"scale_s_per_div": s.P.TimebaseScale, "offset_s": s.P.TimebaseOffset} {
		r, err := s.ask(node + "?")
		if err != nil {
			return nil, err
		}
		if v, ok := r["value"]; ok {
			out[name] = v
		} else {
			out[name] = r["raw"]
		}
	}
	return out, nil
}

// SetTimebase sets the horizontal scale and/or offset.
func (s *Scope) SetTimebase(scale, offset *float64) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var cmds []string
	if scale != nil {
		if err := finite("scale", *scale); err != nil {
			return nil, err
		}
		if *scale <= 0 {
			return nil, errors.New("timebase scale must be > 0 seconds per division")
		}
		cmds = append(cmds, s.P.TimebaseScale+" "+num(*scale))
	}
	if offset != nil {
		if err := finite("offset", *offset); err != nil {
			return nil, err
		}
		cmds = append(cmds, s.P.TimebaseOffset+" "+num(*offset))
	}
	if len(cmds) == 0 {
		return nil, errors.New("nothing to set: give scale and/or offset")
	}
	return cmds, s.send(cmds)
}

// shortForm is the upper-case letters of a SCPI mnemonic: "POSitive" -> "POS".
func shortForm(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalise maps a vendor reply back to the semantic name using the short or
// long form of each vendor value.
func normalise(reply string, vendor map[string]string) string {
	up := strings.ToUpper(strings.TrimSpace(reply))
	for sem, v := range vendor {
		if up == strings.ToUpper(v) || up == shortForm(v) {
			return sem
		}
	}
	return strings.ToLower(reply)
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

// Trigger reads the edge-trigger settings.
func (s *Scope) Trigger() (map[string]any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	out := map[string]any{}
	src, err := s.I.Query(s.P.TrigSource + "?")
	if err != nil {
		return nil, err
	}
	out["source"] = src
	slope, err := s.I.Query(s.P.TrigSlope + "?")
	if err != nil {
		return nil, err
	}
	out["slope"] = normalise(slope, s.P.Slopes)
	lvl, err := s.ask(s.P.TrigLevel + "?")
	if err != nil {
		return nil, err
	}
	if v, ok := lvl["value"]; ok {
		out["level_v"] = v
	} else {
		out["level_v"] = lvl["raw"]
	}
	sweep, err := s.I.Query(s.P.TrigSweep + "?")
	if err != nil {
		return nil, err
	}
	out["sweep"] = normalise(sweep, s.P.Sweeps)
	return out, nil
}

// TriggerSettings are the edge-trigger values SetTrigger may change.
type TriggerSettings struct {
	Channel int // 0 leaves the source alone
	Slope   string
	Level   *float64
	Sweep   string
}

// SetTrigger applies edge-trigger settings.
func (s *Scope) SetTrigger(ts TriggerSettings) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var cmds []string
	if ts.Channel != 0 {
		if err := s.chanOK(ts.Channel); err != nil {
			return nil, err
		}
		cmds = append(cmds, s.P.TrigSource+" "+s.P.Source(ts.Channel))
	}
	if ts.Slope != "" {
		v, ok := s.P.Slopes[strings.ToLower(ts.Slope)]
		if !ok {
			return nil, fmt.Errorf("slope %q: want one of %s", ts.Slope, strings.Join(sortedKeys(s.P.Slopes), ", "))
		}
		cmds = append(cmds, s.P.TrigSlope+" "+v)
	}
	if ts.Level != nil {
		if err := finite("level", *ts.Level); err != nil {
			return nil, err
		}
		cmds = append(cmds, s.P.TrigLevel+" "+num(*ts.Level))
	}
	if ts.Sweep != "" {
		v, ok := s.P.Sweeps[strings.ToLower(ts.Sweep)]
		if !ok {
			return nil, fmt.Errorf("sweep %q: want one of %s", ts.Sweep, strings.Join(sortedKeys(s.P.Sweeps), ", "))
		}
		cmds = append(cmds, s.P.TrigSweep+" "+v)
	}
	if len(cmds) == 0 {
		return nil, errors.New("nothing to set: give channel, slope, level or sweep")
	}
	return cmds, s.send(cmds)
}

// Acquire runs one acquisition control: run, stop, single or force.
func (s *Scope) Acquire(action string) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	var cmd string
	switch strings.ToLower(action) {
	case "run":
		cmd = s.P.Run
	case "stop":
		cmd = s.P.Stop
	case "single":
		cmd = s.P.Single
	case "force":
		cmd = s.P.Force
	default:
		return "", fmt.Errorf("action %q: want run, stop, single or force", action)
	}
	return cmd, s.I.Command(cmd)
}

// TriggerState reads the acquisition state, normalised to triggered, waiting,
// running, auto or stopped ("unknown" for a reply the profile does not know).
func (s *Scope) TriggerState() (state, raw string, err error) {
	if err := s.ready(); err != nil {
		return "", "", err
	}
	raw, err = s.I.Query(s.P.TrigStatus)
	if err != nil {
		return "", "", err
	}
	if st, ok := s.P.TrigStates[strings.ToUpper(raw)]; ok {
		return st, raw, nil
	}
	return "unknown", raw, nil
}

// WaitDone polls until the acquisition has triggered or stopped.
func (s *Scope) WaitDone(ctx context.Context, timeout, poll time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	last := "unknown"
	for {
		st, _, err := s.TriggerState()
		if err != nil {
			return last, err
		}
		last = st
		if st == "triggered" || st == "stopped" {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("no trigger within %s (state %s): check the trigger source, level and sweep", timeout, st)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// MeasureNames lists the normalised measurement names of the profile.
func (s *Scope) MeasureNames() []string { return sortedKeys(s.P.Measures) }

// Measure reads one built-in measurement of a channel.
func (s *Scope) Measure(item string, ch int) (map[string]any, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.chanOK(ch); err != nil {
		return nil, err
	}
	v, ok := s.P.Measures[strings.ToLower(item)]
	if !ok {
		return nil, fmt.Errorf("measurement %q: want one of %s", item, strings.Join(s.MeasureNames(), ", "))
	}
	r, err := s.ask(s.P.MeasureQuery(v, s.P.Source(ch)))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"item": strings.ToLower(item), "channel": float64(ch), "raw": r["raw"]}
	if f, ok := r["value"].(float64); ok {
		// Instruments answer 9.9E37 for "no valid measurement".
		if math.Abs(f) >= 9e37 {
			out["valid"] = false
		} else {
			out["valid"] = true
			out["value"] = f
		}
	} else {
		out["valid"] = false
	}
	return out, nil
}

// Waveform transfers one channel.
func (s *Scope) Waveform(ch int) (*Waveform, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.chanOK(ch); err != nil {
		return nil, err
	}
	return s.P.Waveform(s.I, s.P, ch)
}

// Screenshot transfers the screen image.
func (s *Scope) Screenshot() ([]byte, string, error) {
	if err := s.ready(); err != nil {
		return nil, "", err
	}
	return s.P.Screenshot(s.I)
}
