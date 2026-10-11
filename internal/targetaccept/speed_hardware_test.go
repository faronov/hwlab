//go:build hardware && linux

package targetaccept_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/targetaccept"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
	xds110plugin "github.com/faronov/hwlab/plugins/xds110"
)

// Non-destructive SWD speed acceptance: the same checks at 100 kHz ... 10 MHz
// on both probes, with the device opened at each clock so every call (not only
// connect) runs at it. Opt-in with HWLAB_CC2340_SPEED=1; it does not write
// flash. HWLAB_CC2340_SPEED_RATES=100000,400000,... overrides the ladder.
//
// Neither probe reports the clock it really generates, so "accepted" means the
// probe took the setting and every check passed at it.

type speedPoint struct {
	RequestedHz  float64            `json:"requested_hz"`
	Stable       bool               `json:"stable"`
	Pass         int                `json:"pass"`
	Fail         int                `json:"fail"`
	Failed       []string           `json:"failed,omitempty"`
	FlashMatches bool               `json:"flash_read_matches_reference"`
	PerfMS       map[string]float64 `json:"perf_ms,omitempty"`
	Error        string             `json:"error,omitempty"`
}

func TestCC2340SpeedLadder(t *testing.T) {
	if os.Getenv("HWLAB_CC2340_SPEED") != "1" {
		t.Skip("set HWLAB_CC2340_SPEED=1 to run the SWD speed ladder against the attached target")
	}
	ctx := context.Background()
	x, c := xds110plugin.New(), cmsisdapplugin.New()
	id := probeID(t, x)
	rates := []float64{100e3, 400e3, 1e6, 2e6, 4e6, 8e6, 10e6}

	var ref []byte
	out := map[string][]speedPoint{}
	for _, b := range []string{"xds110", "cmsisdap"} {
		for _, hz := range rates {
			p := speedPoint{RequestedHz: hz}
			var inv targetaccept.Invoker
			var closeFn func()
			if b == "xds110" {
				cfg := map[string]any{"clock_hz": hz}
				if err := x.OpenDevice(ctx, id, cfg); err != nil {
					t.Fatal(err)
				}
				inv, closeFn = invoker(t, x, id), func() { _ = x.CloseDevice(ctx, id) }
			} else {
				if err := c.OpenDevice(ctx, id, map[string]any{"clock_hz": hz}); err != nil {
					t.Fatal(err)
				}
				inv, closeFn = invoker(t, c, id), func() { _ = c.CloseDevice(ctx, id) }
			}
			rep := targetaccept.Run(inv, targetaccept.Options{
				Backend: b, Log: func(string, ...any) {}, ClockHz: hz, Steps: 20, BreakpointRounds: 2, PerfRepeats: 5,
				Reset: true, SkipROMTable: true,
				Phases: []string{"registers", "stepping", "breakpoints", "perf", "reset"},
			})
			p.Pass, p.Fail, p.PerfMS = rep.Count(targetaccept.Pass), rep.Count(targetaccept.Fail), rep.PerfMS
			for _, f := range rep.Failed() {
				p.Failed = append(p.Failed, f.Phase+"/"+f.Name)
			}
			// 4 KiB of flash must read back identically at every clock.
			if r, err := inv("MemoryAccess", "read", map[string]any{"address": "0x0", "length": 4096.0}); err != nil {
				p.Error = err.Error()
			} else {
				h, _ := r["hex"].(string)
				got := []byte(h)
				if ref == nil {
					ref = got
				}
				p.FlashMatches = bytes.Equal(got, ref)
			}
			p.Stable = p.Fail == 0 && p.Error == "" && p.FlashMatches && p.Pass > 0
			out[b] = append(out[b], p)
			t.Logf("%s @ %.0f kHz: stable=%v pass=%d fail=%d failed=%v flash_ok=%v err=%q perf=%v", b, hz/1000, p.Stable, p.Pass, p.Fail, p.Failed, p.FlashMatches, p.Error, p.PerfMS)
			_, _ = inv("DebugProbe", "disconnect", nil)
			closeFn()
			time.Sleep(200 * time.Millisecond)
		}
		best := 0.0
		for _, p := range out[b] {
			if p.Stable && p.RequestedHz > best {
				best = p.RequestedHz
			}
		}
		t.Logf("%s: highest stable requested clock %.0f kHz", b, best/1000)
		if best == 0 {
			t.Errorf("%s: no clock was stable", b)
		}
	}
	if dir := os.Getenv("HWLAB_CC2340_REPORT_DIR"); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		j, _ := json.MarshalIndent(out, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "speed.json"), append(j, '\n'), 0o644)
	}
}
