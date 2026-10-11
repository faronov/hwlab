package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/testutil"
)

// REAL: a standalone TI LP-XDS110ET (0451:bef3) with NO target attached. The
// probe side answered; every target operation failed explicitly and must stay
// failed in replay. The serial in the fixture is sanitized.
func TestRealXDS110SessionReplaysWithoutHardware(t *testing.T) {
	aliasReplay(t, "real-xds110-probe", "dbg", func(t *testing.T, a *app.App, ref string) {
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		methods := []string{"info", "clock_probe", "jtag_scan", "connect", "read", "status", "info"}
		if err != nil || len(evs) != len(methods) {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		for i, e := range evs {
			if e.Data["method"] != methods[i] {
				t.Fatalf("event %d: %v, want %s", i, e.Data["method"], methods[i])
			}
			targetOp := i >= 3 && i <= 5
			if ok, _ := e.Data["ok"].(bool); ok == targetOp {
				t.Fatalf("event %d (%s): ok=%v; target operations must fail and probe operations succeed", i, methods[i], ok)
			}
			if targetOp {
				msg, _ := e.Data["error"].(string)
				if !strings.Contains(msg, "SWD protocol error (-615)") {
					t.Errorf("event %d: %q", i, msg)
				}
			}
		}
		res := func(i int) map[string]any { return evs[i].Data["result"].(map[string]any) }
		for _, i := range []int{0, 6} {
			r := res(i)
			if r["synthetic"] != nil || r["firmware"] != "3.0.0.22" || r["hardware_id"] != "0x23" {
				t.Errorf("info %d: %v", i, r)
			}
			u := r["usb"].(map[string]any)
			if u["claimed_interface"] != 2.0 {
				t.Errorf("claimed interface: %v", u["claimed_interface"])
			}
			for _, x := range u["interfaces"].([]any) {
				it := x.(map[string]any)
				if claimed := it["claimed_by_hwlab"] == true; claimed != (it["number"] == 2.0) {
					t.Errorf("interface %v claimed=%v: only interface 2 may be claimed", it["number"], claimed)
				}
			}
		}
		clocks := res(1)["clocks"].([]any)
		if len(clocks) != 5 {
			t.Fatalf("clocks: %v", clocks)
		}
		for _, c := range clocks {
			if c.(map[string]any)["accepted"] != true {
				t.Errorf("clock not accepted: %v", c)
			}
		}
		if j := res(2); j["status"] != "stuck-low" || j["devices"] != nil {
			t.Errorf("an empty chain reading all zeros must not report devices: %v", j)
		}
	})
}

// The committed evidence must not carry the real USB serial or a home path.
func TestXDS110EvidenceIsSanitized(t *testing.T) {
	root := testutil.ModuleRoot(t)
	for _, d := range []string{"real-xds110-probe", "real-xds110-evidence"} {
		err := filepath.Walk(filepath.Join(root, "testdata", d), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "LS420") || strings.Contains(string(b), "/home/") {
				t.Errorf("%s leaks the real serial or a home path", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
