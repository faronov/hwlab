package protocol

import (
	"bytes"
	"context"
	"math"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		kind LineKind
		data string
		rssi int
		lqi  int
		us   int64
	}{
		{"ack", "received: 0200641000 power: -62 lqi: 64 time: 100123456", FrameLine, "020064", -62, 64, 100123456},
		{"ansi prefix", "\x1b[J\x1b[1;32mreceived: 0200641000 power: -62 lqi: 64 time: 5\x1b[m", FrameLine, "020064", -62, 64, 5},
		{"prompt before", "uart:~$ received: 020064aabb power: -90 lqi: 3 time: 7", FrameLine, "020064", -90, 3, 7},
		{"uppercase hex", "received: 02006410AA power: -1 lqi: 0 time: 0", FrameLine, "020064", -1, 0, 0},
		{"trailing prompt", "received: 0200641000 power: -62 lqi: 64 time: 12 uart:~$", FrameLine, "020064", -62, 64, 12},
		{"echo noise", "shell echo off", Noise, "", 0, 0, 0},
		{"empty", "", Noise, "", 0, 0, 0},
		{"garbled power", "received: 0200 power: ds6 lqi: 5 time: 9", Malformed, "", 0, 0, 0},
		{"truncated", "received: 02006410", Malformed, "", 0, 0, 0},
		{"odd hex", "received: 020064100 power: -1 lqi: 1 time: 1", Malformed, "", 0, 0, 0},
		{"too short", "received: 0200 power: -1 lqi: 1 time: 1", Malformed, "", 0, 0, 0},
		{"too long", "received: " + strings.Repeat("00", 130) + " power: -1 lqi: 1 time: 1", Malformed, "", 0, 0, 0},
		{"glued digits", "received: 0200641000 power: -62 lqi: 64 time: 12uart", Malformed, "", 0, 0, 0},
		{"bad lqi", "received: 0200641000 power: -62 lqi: 999 time: 1", Malformed, "", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, k := ParseLine(c.line)
			if k != c.kind {
				t.Fatalf("kind = %v, want %v", k, c.kind)
			}
			if k != FrameLine {
				return
			}
			want := make([]byte, 0)
			for i := 0; i < len(c.data); i += 2 {
				var b byte
				for _, ch := range c.data[i : i+2] {
					b = b<<4 | byte(strings.IndexRune("0123456789abcdef", ch))
				}
				want = append(want, b)
			}
			if !bytes.Equal(f.Data, want) || f.RSSI != c.rssi || f.LQI != c.lqi || f.DevUs != c.us {
				t.Fatalf("frame = %+v", f)
			}
		})
	}
}

func TestLineBuffer(t *testing.T) {
	var lb LineBuffer
	var got []string
	emit := func(s string) { got = append(got, s) }
	lb.Feed([]byte("abc\r\nde"), emit)
	lb.Feed([]byte("f\n\n\r"), emit)
	lb.Feed([]byte(strings.Repeat("x", 3000)+"\nok\n"), emit)
	if len(got) != 3 || got[0] != "abc" || got[1] != "def" || got[2] != "ok" {
		t.Fatalf("lines = %q", got)
	}
}

func TestClockMapJitterAndMonotonic(t *testing.T) {
	c := NewClockMap()
	const trueOffset = 7_000_000_000 // device timer is 7 s behind session time
	var prev int64
	for i := 0; i < 5000; i++ {
		devUs := int64(1_000_000 + i*10_000)    // a frame every 10 ms
		jitter := int64((i * 7919) % 2_000_000) // up to 2 ms of host-side delay
		host := devUs*1000 + trueOffset + 300_000 + jitter
		ts := c.Map(devUs, host)
		if ts < prev {
			t.Fatalf("non-monotonic at %d: %d < %d", i, ts, prev)
		}
		prev = ts
	}
	s := c.Stats()
	if s.LatencyMinNs != 0 && s.LatencyMinNs > 1000 {
		t.Errorf("min latency %d", s.LatencyMinNs)
	}
	if s.LatencyMaxNs > 2_000_000 || s.LatencyMaxNs < 1_000_000 {
		t.Errorf("max latency = %d, want ~2ms", s.LatencyMaxNs)
	}
	// The mapping must land within the minimum transit time (0.3 ms) of the truth.
	if bias := s.OffsetNs - trueOffset; bias < 0 || bias > 300_000+10_000 {
		t.Errorf("offset bias = %d ns", bias)
	}
}

func TestClockMapTracksDrift(t *testing.T) {
	c := NewClockMap()
	const ppm = 40.0 // device timer runs 40 ppm slow
	for i := 0; i < 60*100; i++ {
		hostNs := int64(i) * 10_000_000
		devUs := int64(float64(hostNs)/(1+ppm*1e-6)) / 1000
		jitter := int64((i * 104729) % 500_000)
		c.Map(devUs, hostNs+jitter)
	}
	s := c.Stats()
	if math.Abs(s.DriftPPM-ppm) > 8 {
		t.Errorf("drift = %.1f ppm, want ~%.0f", s.DriftPPM, ppm)
	}
	if s.Windows < 55 {
		t.Errorf("windows = %d", s.Windows)
	}
}

func TestClockMapDetectsReboot(t *testing.T) {
	c := NewClockMap()
	c.Map(5_000_000, 10_000_000_000)
	c.Map(5_010_000, 10_010_000_000)
	ts := c.Map(100, 20_000_000_000) // device rebooted
	if c.Stats().Resets != 1 {
		t.Fatalf("resets = %d", c.Stats().Resets)
	}
	if ts < 10_010_000_000 {
		t.Errorf("timestamp went backwards after reboot: %d", ts)
	}
}

func frames() []SimFrame {
	return []SimFrame{
		{Data: []byte{0x02, 0x00, 0x64}, RSSI: -60, LQI: 200},
		{Data: []byte{0x61, 0x88, 0x01, 0xe9, 0x2f, 0x00, 0x00, 0x01, 0x00}, RSSI: -75, LQI: 120},
	}
}

func goroutines() int { return runtime.NumGoroutine() }

func TestDeviceStartAndRun(t *testing.T) {
	sim := NewSim(frames()...)
	sim.Garble = 3
	d, err := Open(sim.Opener(), "sim")
	if err != nil {
		t.Fatal(err)
	}
	d.Quiet = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx, 15); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx, func(f Frame, _ time.Time) {
			if n.Add(1) == 20 {
				cancel()
			}
		}, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if sim.Channel() != 15 {
		t.Errorf("channel = %d", sim.Channel())
	}
	if sim.Receiving() {
		t.Error("radio still receiving after Close; sleep was not sent")
	}
	cmds := sim.Commands()
	want := []string{"", "sleep", "shell echo off", "channel 15", "receive"}
	for i, w := range want {
		if i >= len(cmds) || cmds[i] != w {
			t.Fatalf("commands = %q, want prefix %q", cmds, want)
		}
	}
	if cmds[len(cmds)-1] != "sleep" {
		t.Errorf("last command = %q, want sleep", cmds[len(cmds)-1])
	}
	if d.Malformed == 0 {
		t.Error("garbled lines should be counted as malformed")
	}
}

func TestDeviceRejectsBadChannel(t *testing.T) {
	sim := NewSim()
	d, _ := Open(sim.Opener(), "sim")
	defer d.Close()
	if err := d.Start(context.Background(), 10); err == nil {
		t.Fatal("expected error for channel 10")
	}
}

func TestDeviceReportsPortFailure(t *testing.T) {
	sim := NewSim(frames()...)
	sim.FailAfter = 5
	d, _ := Open(sim.Opener(), "sim")
	d.Quiet = 20 * time.Millisecond
	defer d.Close()
	if err := d.Start(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	err := d.Run(context.Background(), func(Frame, time.Time) {}, nil)
	if err == nil {
		t.Fatal("Run should report the unplugged device")
	}
}

func TestNoGoroutineLeakAcrossStartStop(t *testing.T) {
	before := goroutines()
	for i := 0; i < 8; i++ {
		sim := NewSim(frames()...)
		d, _ := Open(sim.Opener(), "sim")
		d.Quiet = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		if err := d.Start(ctx, 11+i%16); err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		if err := d.Run(ctx, func(Frame, time.Time) {}, nil); err != nil {
			t.Fatal(err)
		}
		_ = d.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for goroutines() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := goroutines(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}
