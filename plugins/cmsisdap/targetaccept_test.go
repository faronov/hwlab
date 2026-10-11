package cmsisdapplugin

import (
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/targetaccept"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

// simInvoker adapts the plugin on the simulated probe to the acceptance run.
func simInvoker(t *testing.T, p *Plugin) targetaccept.Invoker {
	return func(c, m string, a map[string]any) (map[string]any, error) { return invoke(t, p, SimID, c, m, a) }
}

func runSim(t *testing.T, sim *dap.Sim, resetPhase bool) *targetaccept.Report {
	t.Helper()
	p := NewWith(func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }, func(e Entry) (dap.Transport, error) {
		if e.Sim != nil {
			return e.Sim, nil
		}
		return nil, errors.New("no such device")
	})
	openSim(t, p)
	rep := targetaccept.Run(simInvoker(t, p), targetaccept.Options{
		Backend: "cmsisdap-sim", Log: t.Logf, Reset: resetPhase, ResetVectorBreakpoint: true, PerfRepeats: 3, SkipROMTable: true,
	})
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	return rep
}

// The acceptance run on a simulated Cortex-M0+ without a DWT: the plugin must
// report the watchpoint unit as absent in the hardware, not as unimplemented.
func TestAcceptanceRunOnSimulatedCortexM0Plus(t *testing.T) {
	rep := runSim(t, dap.NewSimM0Plus(), false)
	if rep.Facts["wp.present"] != "false" || rep.Facts["bp.present"] != "true" || rep.Facts["bp.slots"] != "4" {
		t.Fatalf("facts %v", rep.Facts)
	}
	if rep.Count(targetaccept.UnsupportedHW) < 1 || rep.Find("set_watchpoint reports UNSUPPORTED_BY_ATTACHED_HW").Status != targetaccept.Pass {
		t.Fatalf("watchpoint verdict: %+v", rep.Checks)
	}
	if rep.Facts["bp.rounds_hit"] != "5/5" {
		t.Fatalf("breakpoint rounds %v", rep.Facts["bp.rounds_hit"])
	}
}

func TestAcceptanceRunOnSimulatedCortexM4(t *testing.T) {
	rep := runSim(t, dap.NewSim(), false)
	if rep.Facts["wp.present"] != "true" || rep.Facts["bp.slots"] != "6" {
		t.Fatalf("facts %v", rep.Facts)
	}
}
