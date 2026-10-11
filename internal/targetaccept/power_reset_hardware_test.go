//go:build hardware && linux

package targetaccept_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/targetaccept"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
	xds110plugin "github.com/faronov/hwlab/plugins/xds110"
)

// Reset suite: a long reset/reconnect loop without any power cycle on both
// probes. Writes nothing to flash. Opt-in with HWLAB_CC2340_POWER_RESET=1;
// HWLAB_CC2340_RESET_LOOPS overrides the 50 iterations per probe.
func TestCC2340ResetLoop(t *testing.T) {
	if os.Getenv("HWLAB_CC2340_POWER_RESET") != "1" {
		t.Skip("set HWLAB_CC2340_POWER_RESET=1 to run the reset loop and the power-cycle test")
	}
	ctx := context.Background()
	x, c := xds110plugin.New(), cmsisdapplugin.New()
	id := probeID(t, x)
	if err := x.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	defer x.CloseDevice(ctx, id)
	xinv := invoker(t, x, id)
	if err := c.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	defer c.CloseDevice(ctx, id)
	cinv := invoker(t, c, id)

	loops := 50
	if v := os.Getenv("HWLAB_CC2340_RESET_LOOPS"); v != "" {
		fmt.Sscan(v, &loops)
	}
	type identity struct{ dpidr, cpuid, vec string }
	read := func(inv targetaccept.Invoker) (identity, error) {
		cn, err := inv("DebugProbe", "connect", map[string]any{"clock_hz": 400000.0})
		if err != nil {
			return identity{}, err
		}
		m, err := inv("MemoryAccess", "read", map[string]any{"address": "0x0", "length": 16.0})
		if err != nil {
			return identity{}, err
		}
		if _, err := inv("DebugProbe", "halt", nil); err != nil {
			return identity{}, err
		}
		rg, err := inv("DebugProbe", "read_registers", nil)
		if err != nil || rg["registers"] == nil {
			return identity{}, fmt.Errorf("registers: %v", err)
		}
		if _, err := inv("DebugProbe", "resume", nil); err != nil {
			return identity{}, err
		}
		return identity{fmt.Sprint(cn["dpidr"]), fmt.Sprint(cn["cpuid"]), fmt.Sprint(m["hex"])}, nil
	}
	kinds := []map[string]any{
		{"kind": "hardware"}, {"kind": "sysreset"}, {"kind": "sysreset", "halt": true}, {"kind": "hardware", "halt": true},
	}
	var want identity
	for _, b := range []struct {
		name string
		inv  targetaccept.Invoker
	}{{"xds110", xinv}, {"cmsisdap", cinv}} {
		if b.name == "cmsisdap" {
			_, _ = xinv("DebugProbe", "disconnect", nil)
		}
		t0 := time.Now()
		fails := 0
		for i := 0; i < loops; i++ {
			k := kinds[i%len(kinds)]
			if _, err := b.inv("DebugProbe", "connect", map[string]any{"clock_hz": 400000.0}); err != nil {
				t.Errorf("%s loop %d connect: %v", b.name, i, err)
				fails++
				continue
			}
			if _, err := b.inv("DebugProbe", "reset", k); err != nil {
				t.Errorf("%s loop %d reset %v: %v", b.name, i, k, err)
				fails++
				continue
			}
			got, err := read(b.inv)
			if err != nil {
				t.Errorf("%s loop %d after %v: %v", b.name, i, k, err)
				fails++
				continue
			}
			if want == (identity{}) {
				want = got
			}
			if got != want {
				t.Errorf("%s loop %d after %v: identity %v, want %v", b.name, i, k, got, want)
				fails++
			}
		}
		t.Logf("%s: %d reset/reconnect iterations (hardware, sysreset, sysreset+halt, hardware+halt), %d failures, %.1f s, no power cycle", b.name, loops, fails, time.Since(t0).Seconds())
	}
	_, _ = cinv("DebugProbe", "disconnect", nil)

	// A hard power cycle is not tested here: the LP-XDS110ET cannot switch its
	// 3V3 pin (docs/notes/phase27-lp-xds110et-target-power.md). It needs an
	// external PowerSwitch provider (docs/adr/0033-power-switch.md).
	t.Logf("no power cycle: the probe has no software-switchable target supply; power-cycle recovery is NOT exercised")
}
