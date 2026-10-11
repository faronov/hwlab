// Package experiment parses declarative experiment files.
package experiment

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML duration such as "10s" or "250ms".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", n.Line, s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ParseSignedDuration accepts a Go duration string ("-1.5ms") or an integer
// number of nanoseconds. Unlike Duration it allows negative values.
func ParseSignedDuration(v any) (int64, error) {
	switch x := v.(type) {
	case string:
		d, err := time.ParseDuration(strings.ReplaceAll(strings.TrimSpace(x), "µ", "u"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use e.g. -1.5ms)", x)
		}
		return int64(d), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case uint64:
		return int64(x), nil
	case float64:
		return int64(x), nil
	}
	return 0, fmt.Errorf("expected a duration string or integer nanoseconds, got %T", v)
}

// DeviceSpec is one entry of `devices:`. Reserved keys are plugin, device,
// requires, control and clock_offset; every other key is passed to the plugin as
// configuration.
type DeviceSpec struct {
	Alias    string
	Plugin   string
	Device   string
	Requires []string
	Config   map[string]any
	// Control drives the device only through steps and never starts its
	// capture stream, even if it advertises one (a debug probe that can also
	// measure power).
	Control bool
	// ClockOffsetNs is added to every timestamp of this device on ingest. Use
	// it for an offset measured with `hwlab calibrate`; it is stored with the
	// session and shifts the reported clock bounds.
	ClockOffsetNs int64
}

// PlotSpec is one entry of `plots:`.
type PlotSpec struct {
	Name     string      `yaml:"name"`
	Title    string      `yaml:"title"`
	Start    Duration    `yaml:"start"`
	End      Duration    `yaml:"end"`
	Format   string      `yaml:"format"`
	LogPower bool        `yaml:"log_power"`
	Width    int         `yaml:"width"`
	Tracks   []TrackSpec `yaml:"tracks"`
}

// TrackSpec mirrors plot.TrackSpec for YAML.
type TrackSpec struct {
	Kind   string   `yaml:"kind"`
	Source string   `yaml:"source"`
	Stream string   `yaml:"stream"`
	Types  []string `yaml:"types"`
	Title  string   `yaml:"title"`
}

// Analysis configures post-capture analysis.
type Analysis struct {
	// PowerSpikes annotates the timeline with current rises/returns (default true).
	PowerSpikes *bool `yaml:"power_spikes"`
	// NominalVoltage is used for energy when the meter has no voltage samples.
	NominalVoltage float64 `yaml:"nominal_voltage"`
}

// Step is a scheduled method call on a device, executed by the core at a
// time offset from the start of the run and recorded on the timeline.
type Step struct {
	Name   string         `yaml:"name"`
	At     Duration       `yaml:"at"`
	Device string         `yaml:"device"`
	Call   string         `yaml:"call"`
	Args   map[string]any `yaml:"args"`
}

// Capability and Method split Call ("DebugProbe.info").
func (s Step) Capability() string { c, _, _ := strings.Cut(s.Call, "."); return c }
func (s Step) Method() string     { _, m, _ := strings.Cut(s.Call, "."); return m }

// Experiment is a parsed experiment file.
type Experiment struct {
	Name     string       `yaml:"name"`
	Duration Duration     `yaml:"duration"`
	Devices  []DeviceSpec `yaml:"-"`
	Steps    []Step       `yaml:"steps"`
	Plots    []PlotSpec   `yaml:"plots"`
	Analysis Analysis     `yaml:"analysis"`
	// AllowDangerous pre-approves dangerous configuration for unattended
	// runs. Never honoured for MCP-originated runs.
	AllowDangerous bool `yaml:"allow_dangerous"`

	// Raw is the original document, stored in the session for reproducibility.
	Raw string `yaml:"-"`
}

type rawExperiment struct {
	Experiment `yaml:",inline"`
	Devices    yaml.Node `yaml:"devices"`
}

// Parse decodes and validates an experiment document.
func Parse(data []byte) (*Experiment, error) {
	var raw rawExperiment
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse experiment: %w", err)
	}
	e := raw.Experiment
	e.Raw = string(data)
	if raw.Devices.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("experiment: `devices` must be a mapping of alias -> device settings")
	}
	for i := 0; i+1 < len(raw.Devices.Content); i += 2 {
		alias := raw.Devices.Content[i].Value
		var m map[string]any
		if err := raw.Devices.Content[i+1].Decode(&m); err != nil {
			return nil, fmt.Errorf("device %q: %w", alias, err)
		}
		ds := DeviceSpec{Alias: alias, Config: map[string]any{}}
		for k, v := range m {
			switch k {
			case "plugin":
				ds.Plugin, _ = v.(string)
			case "device":
				ds.Device = fmt.Sprint(v)
			case "requires":
				list, ok := v.([]any)
				if !ok {
					return nil, fmt.Errorf("device %q: `requires` must be a list", alias)
				}
				for _, x := range list {
					ds.Requires = append(ds.Requires, fmt.Sprint(x))
				}
			case "control":
				b, ok := v.(bool)
				if !ok {
					return nil, fmt.Errorf("device %q: `control` must be true or false", alias)
				}
				ds.Control = b
			case "clock_offset":
				ns, err := ParseSignedDuration(v)
				if err != nil {
					return nil, fmt.Errorf("device %q: clock_offset: %w", alias, err)
				}
				ds.ClockOffsetNs = ns
			default:
				ds.Config[k] = Normalize(v)
			}
		}
		e.Devices = append(e.Devices, ds)
	}
	return &e, e.Validate()
}

// Load reads and parses a file.
func Load(path string) (*Experiment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	e, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// Validate checks structural constraints (device/capability resolution
// happens later, against real plugins).
func (e *Experiment) Validate() error {
	if e.Name == "" {
		return fmt.Errorf("experiment: `name` is required")
	}
	if len(e.Devices) == 0 {
		return fmt.Errorf("experiment %q: at least one device is required", e.Name)
	}
	if e.Duration <= 0 {
		return fmt.Errorf("experiment %q: `duration` must be > 0", e.Name)
	}
	seen := map[string]bool{}
	for _, d := range e.Devices {
		if d.Plugin == "" {
			return fmt.Errorf("experiment %q: device %q has no `plugin`", e.Name, d.Alias)
		}
		if seen[d.Alias] {
			return fmt.Errorf("experiment %q: duplicate device alias %q", e.Name, d.Alias)
		}
		seen[d.Alias] = true
	}
	for i, s := range e.Steps {
		where := fmt.Sprintf("experiment %q: step %d", e.Name, i+1)
		if !seen[s.Device] {
			return fmt.Errorf("%s: unknown device %q (steps refer to aliases under `devices`)", where, s.Device)
		}
		if s.Capability() == "" || s.Method() == "" || strings.Count(s.Call, ".") != 1 {
			return fmt.Errorf("%s: `call` must look like Capability.method, got %q", where, s.Call)
		}
		if s.At.D() < 0 || s.At.D() > e.Duration.D() {
			return fmt.Errorf("%s: `at` %s is outside the experiment duration %s", where, s.At.D(), e.Duration.D())
		}
		for k, v := range s.Args {
			s.Args[k] = Normalize(v)
		}
	}
	return nil
}

// Normalize converts YAML-decoded values into types protobuf Struct accepts
// (maps with string keys, int -> float64).
func Normalize(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case map[string]any:
		for k, vv := range x {
			x[k] = Normalize(vv)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[fmt.Sprint(k)] = Normalize(vv)
		}
		return m
	case []any:
		for i := range x {
			x[i] = Normalize(x[i])
		}
		return x
	}
	return v
}
