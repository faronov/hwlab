package experiment

import (
	"strings"
	"testing"
	"time"
)

const valid = `
name: demo
duration: 1500ms
devices:
  power:
    plugin: fake
    device: power0
    requires: [PowerMeasurement]
    period_ms: 100
    nested: {a: 1}
  radio:
    plugin: fake
    device: radio0
analysis:
  nominal_voltage: 3.3
plots:
  - name: o
    format: png
    start: 10ms
`

func TestParse(t *testing.T) {
	e, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "demo" || e.Duration.D() != 1500*time.Millisecond {
		t.Errorf("name/duration: %q %v", e.Name, e.Duration.D())
	}
	if len(e.Devices) != 2 || e.Devices[0].Alias != "power" || e.Devices[1].Alias != "radio" {
		t.Fatalf("devices must keep file order: %+v", e.Devices)
	}
	d := e.Devices[0]
	if d.Plugin != "fake" || d.Device != "power0" || len(d.Requires) != 1 {
		t.Errorf("reserved keys: %+v", d)
	}
	if v, ok := d.Config["period_ms"].(float64); !ok || v != 100 {
		t.Errorf("ints must be normalised to float64 for protobuf: %#v", d.Config["period_ms"])
	}
	if _, ok := d.Config["plugin"]; ok {
		t.Error("reserved keys must not leak into plugin config")
	}
	if e.Plots[0].Start.D() != 10*time.Millisecond {
		t.Errorf("plot start = %v", e.Plots[0].Start.D())
	}
}

func TestClockOffset(t *testing.T) {
	e, err := Parse([]byte("name: n\nduration: 1s\ndevices:\n  a: {plugin: x, clock_offset: -1.5ms}\n  b: {plugin: x, clock_offset: 2500}\n  c: {plugin: x}"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Devices[0].ClockOffsetNs != -1_500_000 || e.Devices[1].ClockOffsetNs != 2500 || e.Devices[2].ClockOffsetNs != 0 {
		t.Errorf("offsets: %d %d %d", e.Devices[0].ClockOffsetNs, e.Devices[1].ClockOffsetNs, e.Devices[2].ClockOffsetNs)
	}
	if _, ok := e.Devices[0].Config["clock_offset"]; ok {
		t.Error("clock_offset must not leak into plugin config")
	}
	if _, err := Parse([]byte("name: n\nduration: 1s\ndevices: {a: {plugin: x, clock_offset: soon}}")); err == nil {
		t.Error("a bad offset must be an error")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no name":         "duration: 1s\ndevices: {a: {plugin: x}}",
		"no duration":     "name: n\ndevices: {a: {plugin: x}}",
		"no devices":      "name: n\nduration: 1s\ndevices: {}",
		"no plugin":       "name: n\nduration: 1s\ndevices: {a: {device: d}}",
		"unknown field":   "name: n\nduration: 1s\nbogus: 1\ndevices: {a: {plugin: x}}",
		"devices as list": "name: n\nduration: 1s\ndevices: [a]",
		"bad duration":    "name: n\nduration: soon\ndevices: {a: {plugin: x}}",
		"bad requires":    "name: n\nduration: 1s\ndevices: {a: {plugin: x, requires: PowerMeasurement}}",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	_, err := Parse([]byte("name: n\nduration: 1s\nbogus: 1\ndevices: {a: {plugin: x}}"))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown-field error should name the field: %v", err)
	}
}

func TestSteps(t *testing.T) {
	const base = "name: s\nduration: 1s\ndevices:\n  p:\n    plugin: fake\n    device: ctl0\n"
	e, err := Parse([]byte(base + "steps:\n  - {name: info, at: 100ms, device: p, call: DebugProbe.info, args: {a: 1}}\n"))
	if err != nil || len(e.Steps) != 1 {
		t.Fatalf("%v %+v", err, e)
	}
	s := e.Steps[0]
	if s.Capability() != "DebugProbe" || s.Method() != "info" || s.At.D() != 100*time.Millisecond {
		t.Errorf("%+v", s)
	}
	for name, steps := range map[string]string{
		"unknown device": "  - {device: nope, call: DebugProbe.info}\n",
		"bad call":       "  - {device: p, call: info}\n",
		"after end":      "  - {device: p, call: DebugProbe.info, at: 2s}\n",
	} {
		if _, err := Parse([]byte(base + "steps:\n" + steps)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
