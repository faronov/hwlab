package app_test

import (
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/app"
)

// REAL: a BL702-DAPLink probe (d6e7:3507) with NO target attached. The probe
// side answered; every target operation failed explicitly and must stay failed
// in replay. The USB serial is the firmware's placeholder value.
func TestRealCMSISDAPBL702SessionReplaysWithoutHardware(t *testing.T) {
	aliasReplay(t, "real-cmsisdap-bl702", "dbg", func(t *testing.T, a *app.App, ref string) {
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		methods := []string{"info", "pins", "clock_probe", "jtag_scan", "connect", "read", "status", "info"}
		if err != nil || len(evs) != len(methods) {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		for i, e := range evs {
			if e.Data["method"] != methods[i] {
				t.Fatalf("event %d: %v, want %s", i, e.Data["method"], methods[i])
			}
			targetOp := i >= 4 && i <= 6
			if ok, _ := e.Data["ok"].(bool); ok == targetOp {
				t.Fatalf("event %d (%s): ok=%v; target operations must fail and probe operations succeed: %+v", i, methods[i], ok, e.Data)
			}
			if targetOp {
				msg, _ := e.Data["error"].(string)
				if !strings.Contains(msg, "no target answered on SWD") {
					t.Errorf("event %d: %q", i, msg)
				}
			}
		}
		res := func(i int) map[string]any { return evs[i].Data["result"].(map[string]any) }
		for _, i := range []int{0, 7} {
			r := res(i)
			caps := r["capabilities"].(map[string]any)
			if r["synthetic"] != nil || r["dap_protocol_version"] != "2.1.0" || caps["raw"] != "33" || caps["swd"] != true || caps["jtag"] != true || caps["swo_uart"] != false {
				t.Errorf("info %d: %v", i, r)
			}
		}
		clocks := res(2)["clocks"].([]any)
		if len(clocks) != 5 {
			t.Fatalf("clocks: %v", clocks)
		}
		for _, c := range clocks {
			if c.(map[string]any)["accepted"] != true {
				t.Errorf("clock not accepted: %v", c)
			}
		}
		if j := res(3); j["status"] != "no-device" || j["devices"] != nil {
			t.Errorf("empty JTAG chain must read as no-device: %v", j)
		}
	})
}
