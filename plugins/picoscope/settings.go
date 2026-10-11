package picoscopeplugin

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/faronov/hwlab/plugins/picoscope/backend"
)

// settings are the acquisition settings of one device (or one call).
type settings struct {
	channels []backend.Channel
	bits     int
	rate     float64
	samples  int
	pre      float64
	trig     backend.Trigger
}

func defaultSettings(info backend.Info) settings {
	s := settings{bits: info.ResolutionsBits[0], rate: 1e6, samples: 10000, pre: 0.1}
	for i, name := range info.Channels {
		s.channels = append(s.channels, backend.Channel{Name: name, Enabled: i == 0, RangeV: 5, Coupling: "dc"})
	}
	s.trig = backend.Trigger{Source: info.Channels[0], LevelV: 1, Slope: "rising", TimeoutS: 1}
	return s
}

func (s settings) clone() settings {
	c := s
	c.channels = append([]backend.Channel(nil), s.channels...)
	return c
}

func (s settings) channel(name string) *backend.Channel {
	for i := range s.channels {
		if strings.EqualFold(s.channels[i].Name, name) {
			return &s.channels[i]
		}
	}
	return nil
}

func (s settings) request() backend.Request {
	pre := s.pre
	if !s.trig.Enabled {
		pre = 0
	}
	return backend.Request{Channels: s.channels, ResolutionBits: s.bits, SampleRateHz: s.rate, Samples: s.samples, PreTrigger: pre, Trigger: s.trig}
}

func (s settings) enabledNames() []string {
	var out []string
	for _, c := range s.channels {
		if c.Enabled {
			out = append(out, c.Name)
		}
	}
	return out
}

// Setting keys. The core hands the same configuration map to open and to
// capture, and a method call may carry a subset, so each place accepts the keys
// that make sense for it; a key in no set is a typo.
var (
	channelKeys = []string{"channels", "range_v", "ranges", "offset_v", "coupling"}
	timeKeys    = []string{"resolution_bits", "sample_rate_hz", "samples", "pre_trigger"}
	trigKeys    = []string{"trigger_source", "trigger_level_v", "trigger_slope", "trigger_timeout_s", "trigger_auto"}
	allKeys     = append(append(append([]string(nil), channelKeys...), timeKeys...), trigKeys...)
)

func contains(set []string, k string) bool {
	for _, s := range set {
		if s == k {
			return true
		}
	}
	return false
}

// parseSettings applies cfg on top of base. Only keys in allowed are accepted.
func parseSettings(base settings, cfg map[string]any, allowed []string) (settings, error) {
	s := base.clone()
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !contains(allowed, k) {
			return s, fmt.Errorf("unknown setting %q (accepted here: %s)", k, strings.Join(allowed, ", "))
		}
	}
	num := func(key string) (float64, bool, error) {
		v, ok := cfg[key]
		if !ok || v == nil {
			return 0, false, nil
		}
		f, isNum := v.(float64)
		if !isNum || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false, fmt.Errorf("%s: want a finite number, got %v", key, v)
		}
		return f, true, nil
	}
	str := func(key string) (string, bool, error) {
		v, ok := cfg[key]
		if !ok || v == nil {
			return "", false, nil
		}
		x, isStr := v.(string)
		if !isStr {
			return "", false, fmt.Errorf("%s: want a string, got %v", key, v)
		}
		return x, true, nil
	}

	if v, ok := cfg["channels"]; ok {
		list, isList := v.([]any)
		if !isList {
			list = []any{v}
		}
		for i := range s.channels {
			s.channels[i].Enabled = false
		}
		for _, e := range list {
			name, isStr := e.(string)
			if !isStr {
				return s, fmt.Errorf("channels: %v is not a channel name", e)
			}
			c := s.channel(name)
			if c == nil {
				return s, fmt.Errorf("channels: no channel %q (have %s)", name, channelList(s))
			}
			c.Enabled = true
		}
		if len(s.enabledNames()) == 0 {
			return s, fmt.Errorf("channels is empty")
		}
	}
	if f, ok, err := num("range_v"); err != nil {
		return s, err
	} else if ok {
		for i := range s.channels {
			s.channels[i].RangeV = f
		}
	}
	if v, ok := cfg["ranges"]; ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			return s, fmt.Errorf("ranges: want an object like {A: 5, B: 0.5}, got %v", v)
		}
		for name, rv := range m {
			c := s.channel(name)
			f, isNum := rv.(float64)
			if c == nil || !isNum {
				return s, fmt.Errorf("ranges: %q: %v is not a channel with a numeric range", name, rv)
			}
			c.RangeV = f
		}
	}
	if f, ok, err := num("offset_v"); err != nil {
		return s, err
	} else if ok {
		for i := range s.channels {
			s.channels[i].OffsetV = f
		}
	}
	if c, ok, err := str("coupling"); err != nil {
		return s, err
	} else if ok {
		for i := range s.channels {
			s.channels[i].Coupling = strings.ToLower(c)
		}
	}
	if f, ok, err := num("resolution_bits"); err != nil {
		return s, err
	} else if ok {
		if f != math.Trunc(f) {
			return s, fmt.Errorf("resolution_bits %v: want an integer", f)
		}
		s.bits = int(f)
	}
	if f, ok, err := num("sample_rate_hz"); err != nil {
		return s, err
	} else if ok {
		s.rate = f
	}
	if f, ok, err := num("samples"); err != nil {
		return s, err
	} else if ok {
		if f != math.Trunc(f) || f < 1 || f > 1e9 {
			return s, fmt.Errorf("samples %v: want a positive integer", f)
		}
		s.samples = int(f)
	}
	if f, ok, err := num("pre_trigger"); err != nil {
		return s, err
	} else if ok {
		if f < 0 || f > 1 {
			return s, fmt.Errorf("pre_trigger %v: want a fraction 0..1", f)
		}
		s.pre = f
	}
	if src, ok, err := str("trigger_source"); err != nil {
		return s, err
	} else if ok {
		if strings.EqualFold(src, "none") || src == "" {
			s.trig.Enabled = false
		} else {
			c := s.channel(src)
			if c == nil {
				return s, fmt.Errorf("trigger_source: no channel %q (have %s, or none)", src, channelList(s))
			}
			s.trig.Enabled, s.trig.Source = true, c.Name
		}
	}
	if f, ok, err := num("trigger_level_v"); err != nil {
		return s, err
	} else if ok {
		s.trig.LevelV = f
	}
	if sl, ok, err := str("trigger_slope"); err != nil {
		return s, err
	} else if ok {
		sl = strings.ToLower(sl)
		if sl != "rising" && sl != "falling" && sl != "either" {
			return s, fmt.Errorf("trigger_slope %q: want rising, falling or either", sl)
		}
		s.trig.Slope = sl
	}
	if f, ok, err := num("trigger_timeout_s"); err != nil {
		return s, err
	} else if ok {
		if f <= 0 || f > 3600 {
			return s, fmt.Errorf("trigger_timeout_s %v: want 0..3600", f)
		}
		s.trig.TimeoutS = f
	}
	if v, ok := cfg["trigger_auto"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			return s, fmt.Errorf("trigger_auto: want true or false, got %v", v)
		}
		s.trig.Auto = b
	}
	return s, nil
}

func channelList(s settings) string {
	names := make([]string, len(s.channels))
	for i, c := range s.channels {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}
