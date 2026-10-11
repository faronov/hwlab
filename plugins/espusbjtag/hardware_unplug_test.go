//go:build hardware

package espusbjtag

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHWUnplugReplug needs a person: it waits for the USB cable to be
// physically removed and re-inserted, twice. It never simulates the unplug; if
// nobody touches the cable it times out and FAILS.
//
//	HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_UNPLUG=1 go test -tags hardware -run HWUnplugReplug -v -timeout 20m ./plugins/espusbjtag/
//
// Progress markers are written to the file named by HWLAB_ESPJTAG_UNPLUG_LOG
// (default a new file in t.TempDir), one line each, for the operator to follow.
func TestHWUnplugReplug(t *testing.T) {
	if os.Getenv("HWLAB_ESPJTAG_UNPLUG") == "" {
		t.Skip("set HWLAB_ESPJTAG_UNPLUG=1 and be ready to unplug the board")
	}
	logPath := os.Getenv("HWLAB_ESPJTAG_UNPLUG_LOG")
	if logPath == "" {
		logPath = filepath.Join(t.TempDir(), "esp-unplug.log")
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
	h := hwOpen(t, nil)
	rom := []byte{0x6f, 0x80, 0xe0, 0x2c}
	id0 := h.call(t, "TargetIdentify", "identify", nil)
	serial := fmt.Sprint(h.call(t, "JTAGTransport", "info", nil)["usb_serial"])
	if !bytes.Equal(h.read(t, 0x40001000, 4), rom) {
		t.Fatal("baseline ROM word differs")
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
		mark("ROUND %d: connection active and verified; WAITING FOR PHYSICAL UNPLUG (unplug ESP32-C6 now)", round)
		var gone time.Time
		var firstErr error
		deadline := time.Now().Add(15 * time.Minute)
		for time.Now().Before(deadline) {
			var err error
			timed(func() { _, err = h.try("MemoryAccess", "read", map[string]any{"address": "0x40001000", "length": 4}) })
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
		for _, c := range []struct{ cap, m string }{{"MemoryAccess", "read"}, {"TargetIdentify", "identify"}, {"JTAGScan", "scan"}, {"DebugProbe", "chip_info"}} {
			var err error
			timed(func() {
				args := map[string]any(nil)
				if c.m == "read" {
					args = map[string]any{"address": "0x40001000", "length": 4}
				}
				_, err = h.try(c.cap, c.m, args)
			})
			if err == nil {
				t.Fatalf("round %d: %s.%s succeeded with the device unplugged (stale success)", round, c.cap, c.m)
			}
			if !IsDeviceGone(err) {
				t.Fatalf("round %d: %s.%s: %v, want DEVICE_GONE", round, c.cap, c.m, err)
			}
		}
		mark("ROUND %d: all calls fail with DEVICE_GONE, none hung (slowest call so far %v); WAITING FOR REPLUG (plug the cable back in)", round, slowest.Round(time.Millisecond))

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
			t.Fatalf("round %d: the device never came back within 15 minutes", round)
		}
		mark("ROUND %d: REPLUG DETECTED after %v away: %v", round, time.Since(gone).Round(time.Millisecond), rec)
		if rec["reconnected"] != true || rec["identity_verified"] != true {
			t.Fatalf("round %d: reconnect without identity verification: %v", round, rec)
		}
		if got := fmt.Sprint(h.call(t, "JTAGTransport", "info", nil)["usb_serial"]); got != serial {
			t.Fatalf("round %d: re-found a different device: %s vs %s", round, got, serial)
		}
		scan := h.call(t, "JTAGScan", "scan", nil)
		id := h.call(t, "TargetIdentify", "identify", nil)
		if fmt.Sprint(id["targets"]) != fmt.Sprint(id0["targets"]) {
			t.Fatalf("round %d: identity changed after replug: %v vs %v", round, id["targets"], id0["targets"])
		}
		if !bytes.Equal(h.read(t, 0x40001000, 4), rom) {
			t.Fatalf("round %d: ROM word wrong after replug", round)
		}
		mark("ROUND %d: PASS: serial match, scan=%v, identity unchanged, ROM read OK", round, scan["chain"])
	}
	mark("DONE: 2 physical unplug/replug cycles; slowest single call %v", slowest.Round(time.Millisecond))
}
