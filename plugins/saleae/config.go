package saleae

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/faronov/hwlab/plugins/saleae/internal/saleaepb"
)

// analyzerSpec is one protocol analyzer to add after the capture.
type analyzerSpec struct {
	Kind     string // spi, i2c, uart or "" for a raw Logic 2 analyzer name
	Name     string // exact Logic 2 analyzer name
	Label    string
	Settings map[string]*saleaepb.AnalyzerSettingValue
}

type glitchFilter struct {
	Channel uint32
	Seconds float64
}

type config struct {
	Digital          []uint32
	Analog           []uint32
	DigitalRate      uint32
	AnalogRate       uint32
	Threshold        float64
	Glitch           []glitchFilter
	BufferMB         uint32
	Names            map[uint32]string
	AnalogDownsample uint64
	SaveNative       bool
	Analyzers        []analyzerSpec
}

// Logic 2 setting names used by the three initial analyzers. They are the
// labels shown in the Logic 2 analyzer dialog; unverified on real hardware.
var analyzerKinds = map[string]struct {
	name     string
	channels map[string]string
}{
	"spi":  {"SPI", map[string]string{"clk": "Clock", "mosi": "MOSI", "miso": "MISO", "cs": "Enable"}},
	"i2c":  {"I2C", map[string]string{"sda": "SDA", "scl": "SCL"}},
	"uart": {"Async Serial", map[string]string{"rx": "Input Channel"}},
}

func asInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

func intList(key string, v any) ([]uint32, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of channel numbers", key)
	}
	seen := map[uint32]bool{}
	var out []uint32
	for _, e := range l {
		n, ok := asInt(e)
		if !ok || n < 0 || n > 31 {
			return nil, fmt.Errorf("%s: %v is not a channel number (0-31)", key, e)
		}
		if !seen[uint32(n)] {
			seen[uint32(n)] = true
			out = append(out, uint32(n))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func rate(key string, m map[string]any) (uint32, error) {
	v, ok := m[key]
	if !ok {
		return 0, nil
	}
	n, ok := asInt(v)
	if !ok || n <= 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("%s must be a positive integer (samples/s)", key)
	}
	return uint32(n), nil
}

// parseConfig validates the device config. Sample rates are required: legal
// rates depend on the device model and the combination of enabled channels,
// so hwlab does not guess them.
func parseConfig(m map[string]any) (*config, error) {
	c := &config{BufferMB: 512, AnalogDownsample: 1, SaveNative: true, Names: map[uint32]string{}}
	var err error
	if v, ok := m["digital_channels"]; ok {
		if c.Digital, err = intList("digital_channels", v); err != nil {
			return nil, err
		}
	}
	if v, ok := m["analog_channels"]; ok {
		if c.Analog, err = intList("analog_channels", v); err != nil {
			return nil, err
		}
	}
	if len(c.Digital) == 0 && len(c.Analog) == 0 {
		return nil, fmt.Errorf("config: enable at least one of digital_channels / analog_channels")
	}
	if c.DigitalRate, err = rate("digital_sample_rate", m); err != nil {
		return nil, err
	}
	if c.AnalogRate, err = rate("analog_sample_rate", m); err != nil {
		return nil, err
	}
	if len(c.Digital) > 0 && c.DigitalRate == 0 {
		return nil, fmt.Errorf("config: digital_sample_rate is required with digital_channels (legal rates depend on the device)")
	}
	if len(c.Analog) > 0 && c.AnalogRate == 0 {
		return nil, fmt.Errorf("config: analog_sample_rate is required with analog_channels (legal rates depend on the device)")
	}
	if v, ok := m["digital_threshold_volts"]; ok {
		f, ok := v.(float64)
		if !ok || f <= 0 {
			return nil, fmt.Errorf("digital_threshold_volts must be a positive number")
		}
		c.Threshold = f
	}
	if v, ok := m["buffer_mb"]; ok {
		n, ok := asInt(v)
		if !ok || n < 1 || n > 1<<20 {
			return nil, fmt.Errorf("buffer_mb must be a positive integer")
		}
		c.BufferMB = uint32(n)
	}
	if v, ok := m["analog_downsample"]; ok {
		n, ok := asInt(v)
		if !ok || n < 1 || n > 1_000_000 {
			return nil, fmt.Errorf("analog_downsample must be 1..1000000")
		}
		c.AnalogDownsample = uint64(n)
	}
	if v, ok := m["save_native"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("save_native must be a boolean")
		}
		c.SaveNative = b
	}
	if v, ok := m["glitch_filters"]; ok {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("glitch_filters must map channel -> seconds")
		}
		for k, s := range mm {
			ch, err := strconv.ParseUint(k, 10, 32)
			f, ok := s.(float64)
			if err != nil || !ok || f <= 0 {
				return nil, fmt.Errorf("glitch_filters: bad entry %q: %v", k, s)
			}
			c.Glitch = append(c.Glitch, glitchFilter{uint32(ch), f})
		}
		sort.Slice(c.Glitch, func(i, j int) bool { return c.Glitch[i].Channel < c.Glitch[j].Channel })
	}
	if v, ok := m["channel_names"]; ok {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("channel_names must map channel -> name")
		}
		for k, s := range mm {
			ch, err := strconv.ParseUint(k, 10, 32)
			name, ok := s.(string)
			if err != nil || !ok || name == "" {
				return nil, fmt.Errorf("channel_names: bad entry %q: %v", k, s)
			}
			c.Names[uint32(ch)] = name
		}
	}
	if v, ok := m["analyzers"]; ok {
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("analyzers must be a list")
		}
		for i, e := range l {
			a, err := parseAnalyzer(e)
			if err != nil {
				return nil, fmt.Errorf("analyzers[%d]: %w", i, err)
			}
			c.Analyzers = append(c.Analyzers, a)
		}
	}
	return c, nil
}

func settingValue(v any) (*saleaepb.AnalyzerSettingValue, error) {
	switch x := v.(type) {
	case string:
		return &saleaepb.AnalyzerSettingValue{Value: &saleaepb.AnalyzerSettingValue_StringValue{StringValue: x}}, nil
	case bool:
		return &saleaepb.AnalyzerSettingValue{Value: &saleaepb.AnalyzerSettingValue_BoolValue{BoolValue: x}}, nil
	case float64:
		if n, ok := asInt(x); ok {
			return &saleaepb.AnalyzerSettingValue{Value: &saleaepb.AnalyzerSettingValue_Int64Value{Int64Value: n}}, nil
		}
		return &saleaepb.AnalyzerSettingValue{Value: &saleaepb.AnalyzerSettingValue_DoubleValue{DoubleValue: x}}, nil
	}
	return nil, fmt.Errorf("unsupported setting value %v (%T)", v, v)
}

// parseAnalyzer reads {type: spi|i2c|uart, label, channels: {...}, settings: {...}}
// or {name: "<exact Logic 2 analyzer name>", label, settings}.
func parseAnalyzer(e any) (analyzerSpec, error) {
	m, ok := e.(map[string]any)
	if !ok {
		return analyzerSpec{}, fmt.Errorf("must be a mapping")
	}
	a := analyzerSpec{Settings: map[string]*saleaepb.AnalyzerSettingValue{}}
	a.Kind, _ = m["type"].(string)
	a.Kind = strings.ToLower(a.Kind)
	a.Name, _ = m["name"].(string)
	a.Label, _ = m["label"].(string)
	var chanKeys map[string]string
	if a.Kind != "" {
		k, ok := analyzerKinds[a.Kind]
		if !ok {
			return a, fmt.Errorf("unknown analyzer type %q (spi, i2c, uart, or use name:)", a.Kind)
		}
		if a.Name == "" {
			a.Name = k.name
		}
		chanKeys = k.channels
	}
	if a.Name == "" {
		return a, fmt.Errorf("needs type: (spi, i2c, uart) or name:")
	}
	if a.Label == "" {
		if a.Kind != "" {
			a.Label = a.Kind
		} else {
			a.Label = a.Name
		}
	}
	if ch, ok := m["channels"].(map[string]any); ok {
		for k, v := range ch {
			setting, ok := chanKeys[strings.ToLower(k)]
			if !ok {
				return a, fmt.Errorf("unknown channel role %q for %s", k, a.Kind)
			}
			n, ok := asInt(v)
			if !ok || n < 0 {
				return a, fmt.Errorf("channel %q: %v is not a channel number", k, v)
			}
			a.Settings[setting], _ = settingValue(float64(n))
		}
	}
	if st, ok := m["settings"].(map[string]any); ok {
		for k, v := range st {
			sv, err := settingValue(v)
			if err != nil {
				return a, fmt.Errorf("setting %q: %w", k, err)
			}
			a.Settings[k] = sv
		}
	}
	return a, nil
}

// channelName is the label of a digital channel on the timeline.
func (c *config) channelName(ch uint32) string {
	if n := c.Names[ch]; n != "" {
		return n
	}
	return fmt.Sprintf("CH%d", ch)
}
