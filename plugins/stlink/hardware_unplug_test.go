//go:build hardware

package stlinkplugin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHWUnplugReplug needs a person: it waits for the USB cable to be
// physically removed and re-inserted, twice. It never simulates the unplug; if
// nobody touches the cable it times out and FAILS.
//
//	HWLAB_STLINK_HW=1 HWLAB_STLINK_UNPLUG=1 go test -tags hardware -run HWUnplugReplug -v -timeout 40m ./plugins/stlink/
//
// Progress markers are written to the file named by HWLAB_STLINK_UNPLUG_LOG
// (default a new file in t.TempDir), one line each, for the operator to follow.
func TestHWUnplugReplug(t *testing.T) {
	if os.Getenv("HWLAB_STLINK_UNPLUG") == "" {
		t.Skip("set HWLAB_STLINK_UNPLUG=1 and be ready to unplug the probe")
	}
	logPath := os.Getenv("HWLAB_STLINK_UNPLUG_LOG")
	if logPath == "" {
		logPath = filepath.Join(t.TempDir(), "stlink-unplug.log")
	}
	t.Logf("progress log: %s", logPath)
	mark := func(format string, a ...any) {
		line := fmt.Sprintf("%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
		t.Log(line[:len(line)-1])
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
	h := hwOpen(t, map[string]any{"reconnect_timeout_ms": 200.0})
	id0 := h.call(t, "TargetIdentify", "identify", nil)
	serial := fmt.Sprint(id0["probe_serial"])
	word := h.read(t, 0x20000000, 16)
	vec := h.read(t, 0, 8)

	probeOnBus := func() bool {
		m, _ := filepath.Glob("/sys/bus/usb/devices/*/idProduct")
		for _, f := range m {
			p, _ := os.ReadFile(f)
			v, _ := os.ReadFile(filepath.Join(filepath.Dir(f), "idVendor"))
			if strings.TrimSpace(string(p)) == "3748" && strings.TrimSpace(string(v)) == "0483" {
				return true
			}
		}
		return false
	}

	var slowest time.Duration
	timed := func(f func()) {
		s := time.Now()
		f()
		if d := time.Since(s); d > slowest {
			slowest = d
		}
	}

	for round := 1; round <= 2; round++ {
		mark("ROUND %d: connection active and verified; WAITING FOR PHYSICAL UNPLUG (unplug ST-Link now)", round)
		var gone time.Time
		var firstErr error
		deadline := time.Now().Add(15 * time.Minute)
		for time.Now().Before(deadline) {
			var err error
			timed(func() {
				_, err = h.try("MemoryAccess", "read", map[string]any{"address": "0x20000000", "length": 16.0})
			})
			if err != nil {
				gone, firstErr = time.Now(), err
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if firstErr == nil {
			t.Fatalf("round %d: the cable was never unplugged within 15 minutes (the test cannot pass without a real unplug)", round)
		}
		if !IsDeviceGone(firstErr) {
			t.Fatalf("round %d: unplug classified as %v, want DEVICE_GONE", round, firstErr)
		}
		mark("ROUND %d: UNPLUG DETECTED: %v", round, firstErr)

		// No stale success: every kind of call must now fail cleanly and quickly.
		for _, c := range []struct{ cap, m string }{{"MemoryAccess", "read"}, {"TargetIdentify", "identify"}, {"DebugProbe", "halt"}, {"DebugProbe", "read_registers"}, {"DebugProbe", "chip_info"}} {
			var err error
			timed(func() {
				args := map[string]any(nil)
				if c.m == "read" {
					args = map[string]any{"address": "0x20000000", "length": 16.0}
				}
				_, err = h.try(c.cap, c.m, args)
			})
			if err == nil {
				// A success is only legitimate if the probe is really back on the bus
				// (the session was rebuilt with the identity verified).
				if !probeOnBus() {
					t.Fatalf("round %d: %s.%s succeeded with the probe unplugged (stale success)", round, c.cap, c.m)
				}
				mark("ROUND %d: %s.%s succeeded because the probe is already back on the bus (identity-verified rebuild)", round, c.cap, c.m)
				continue
			}
			if !IsDeviceGone(err) {
				t.Fatalf("round %d: %s.%s: %v, want DEVICE_GONE", round, c.cap, c.m, err)
			}
		}
		mark("ROUND %d: all calls fail with DEVICE_GONE, none hung (slowest call so far %v); WAITING FOR REPLUG (plug the probe back in)", round, slowest.Round(time.Millisecond))

		var rec map[string]any
		deadline = time.Now().Add(15 * time.Minute)
		for time.Now().Before(deadline) {
			var err error
			timed(func() { rec, err = h.try("DebugProbe", "reconnect", nil) })
			if err == nil {
				break
			}
			if !IsDeviceGone(err) {
				t.Fatalf("round %d: reconnect while unplugged: %v, want DEVICE_GONE", round, err)
			}
			rec = nil
			time.Sleep(300 * time.Millisecond)
		}
		if rec == nil {
			t.Fatalf("round %d: the probe never came back within 15 minutes", round)
		}
		mark("ROUND %d: REPLUG DETECTED after %v away: %v", round, time.Since(gone).Round(time.Millisecond), rec)
		if rec["reconnected"] != true || rec["identity_verified"] != true {
			t.Fatalf("round %d: reconnect without identity verification: %v", round, rec)
		}
		id := h.call(t, "TargetIdentify", "identify", nil)
		if id["probe_serial"] != serial {
			t.Fatalf("round %d: re-found a different probe: %v vs %v", round, id["probe_serial"], serial)
		}
		if fmt.Sprint(id["core"]) != fmt.Sprint(id0["core"]) || fmt.Sprint(id["debug_port"].(map[string]any)["dp_idcode"]) != fmt.Sprint(id0["debug_port"].(map[string]any)["dp_idcode"]) {
			t.Fatalf("round %d: identity changed after replug: %v vs %v", round, id["core"], id0["core"])
		}
		if !bytes.Equal(h.read(t, 0, 8), vec) {
			t.Fatalf("round %d: flash vector words differ after replug", round)
		}
		_ = word // SRAM is live firmware state; only readability is asserted
		if len(h.read(t, 0x20000000, 16)) != 16 {
			t.Fatalf("round %d: SRAM read failed after replug", round)
		}
		h.halt(t)
		h.resume(t)
		mark("ROUND %d: PASS: serial match, identity unchanged, memory read OK, halt/resume OK", round)
	}
	mark("DONE: 2 physical unplug/replug cycles; slowest single call %v", slowest.Round(time.Millisecond))
}
