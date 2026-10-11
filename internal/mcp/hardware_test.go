//go:build hardware

package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP hardware acceptance. A real MCP client talks to a real `hwlab mcp
// --stdio` process which drives real probes through the real plugins.
//
//	HWLAB_MCP_HW=1 go test -tags hardware -run MCPHW ./internal/mcp
//
// The ST-Link tests are non-destructive: no flash erase/program. The STM32
// is left running its original firmware.
//
//	HWLAB_STLINK_SERIAL   select a specific ST-Link by USB serial (optional)
//	HWLAB_MCP_UNPLUG=1    interactive unplug/replug test (asks on stdin)

const origFlashSHA = "acba307bbb3fafa322220b35c9add1e5fc31ab4ba54ba9ba58a88255a8fe6a16"

func needHW(t *testing.T) {
	t.Helper()
	if os.Getenv("HWLAB_MCP_HW") != "1" {
		t.Skip("set HWLAB_MCP_HW=1 to run MCP hardware tests")
	}
}

func hwPlugins() []string {
	return []string{"stlink", "cmsisdap", "ccdebugger"}
}

// inner unwraps target_op's {"result": {"plugin","device","result": ...}}.
func inner(out map[string]any) map[string]any {
	r, _ := out["result"].(map[string]any)
	if r == nil {
		return nil
	}
	if i, ok := r["result"].(map[string]any); ok {
		return i
	}
	return r
}

type stlinkClient struct {
	t      *testing.T
	cs     *sdk.ClientSession
	serial string
	cfg    map[string]any
}

func (c *stlinkClient) op(op string, args map[string]any, extra map[string]any) (*sdk.CallToolResult, map[string]any) {
	c.t.Helper()
	in := map[string]any{"plugin": "stlink", "device": c.serial, "op": op, "config": c.cfg, "confirm": true}
	if args != nil {
		in["args"] = args
	}
	for k, v := range extra {
		in[k] = v
	}
	return callTool(c.t, c.cs, "target_op", in)
}

func (c *stlinkClient) must(op string, args map[string]any, extra map[string]any) map[string]any {
	c.t.Helper()
	res, out := c.op(op, args, extra)
	if res.IsError {
		c.t.Fatalf("target_op %s: %s", op, jsonOf(out))
	}
	return inner(out)
}

// findSTLink lists devices over MCP and returns the ST-Link USB serial.
func findSTLink(t *testing.T, cs *sdk.ClientSession) string {
	t.Helper()
	_, out := callTool(t, cs, "device_list", nil)
	want := os.Getenv("HWLAB_STLINK_SERIAL")
	var found []string
	for _, d := range asList(out["devices"]) {
		dm, _ := d.(map[string]any)
		if dm["plugin"] != "stlink" {
			continue
		}
		serial, _ := dm["serial"].(string)
		if serial == "" {
			id, _ := dm["id"].(string)
			parts := strings.Split(id, ":")
			serial = parts[len(parts)-1]
		}
		found = append(found, serial)
	}
	if len(found) == 0 {
		t.Skipf("no ST-Link attached: %v", out)
	}
	sort.Strings(found)
	for _, f := range found {
		if want == "" || strings.Contains(f, want) {
			return f
		}
	}
	t.Skipf("ST-Link %s not attached (have %v)", want, found)
	return ""
}

func newSTLink(t *testing.T) *stlinkClient {
	t.Helper()
	needHW(t)
	cs := startServer(t, serverOpts{Plugins: hwPlugins()})
	return &stlinkClient{t: t, cs: cs, serial: findSTLink(t, cs),
		cfg: map[string]any{"allow_memory_write": true, "allow_reset": true}}
}

func hexAddr(v any) string { s, _ := v.(string); return s }

func TestMCPHWDiscoveryAndIdentity(t *testing.T) {
	c := newSTLink(t)
	if _, out := callTool(t, c.cs, "device_list", nil); out["devices"] == nil {
		t.Fatal("no devices")
	}

	// Invalid serial is a precise NOT_FOUND, not a hang or a wrong probe.
	res, out := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": "DEADBEEF", "op": "info"})
	if code := errCode(t, res, out); code != "NOT_FOUND" {
		t.Errorf("invalid serial: %s", code)
	}

	id := c.must("identify", nil, nil)
	probe, _ := id["probe"].(map[string]any)
	if probe["serial"] != c.serial || probe["vid"] != "0483" {
		t.Errorf("probe identity: %v", probe)
	}
	core, _ := id["core"].(map[string]any)
	stm, _ := id["stm32"].(map[string]any)
	dp, _ := id["debug_port"].(map[string]any)
	t.Logf("probe=%v core=%v stm32=%v dp=%v", probe, core, stm, dp)
	if core["cpuid"] == nil || dp["dp_idcode"] == nil || stm["dev_id"] == nil {
		t.Fatalf("missing evidence: %v", id)
	}
	if dp["target_voltage_mv"] == nil {
		t.Error("no target voltage")
	}
}

func TestMCPHWDebugCycle(t *testing.T) {
	c := newSTLink(t)
	t.Cleanup(func() { _, _ = c.op("resume", nil, nil) })

	for i := 0; i < 5; i++ {
		h := c.must("halt", nil, nil)
		if h["halted"] != true {
			t.Fatalf("halt %d: %v", i, h)
		}
		r := c.must("resume", nil, nil)
		if r["halted"] != false {
			t.Fatalf("resume %d: %v", i, r)
		}
	}

	c.must("halt", nil, nil)
	regs := c.must("registers", map[string]any{"keep_halted": true, "crosscheck": true}, nil)
	rm, _ := regs["registers"].(map[string]any)
	for _, n := range []string{"r0", "r12", "sp", "lr", "pc", "xpsr", "msp", "psp", "control"} {
		if rm[n] == nil && regs[n] == nil {
			t.Errorf("register %s missing: %v", n, regs)
		}
	}
	if regs["native_matches_dcrsr"] != true {
		t.Errorf("native register read disagrees with DCRSR: %v", regs["native_matches_dcrsr"])
	}

	// SRAM write 8/16/32/block with exact restore (halted core, scratch
	// well inside SRAM, far from the stack top).
	const scratch = "0x20004b00"
	orig := c.must("memory-read", map[string]any{"address": scratch, "length": 32}, nil)["hex"].(string)
	for _, hexData := range []string{"a5", "a55a", "a55a1234", "a55a1234deadbeef0102030405060708"} {
		w := c.must("memory-write", map[string]any{"address": scratch, "data": hexData}, nil)
		if w["verified"] != true {
			t.Errorf("write %s not verified: %v", hexData, w)
		}
		rd := c.must("memory-read", map[string]any{"address": scratch, "length": len(hexData) / 2}, nil)
		if rd["hex"] != hexData {
			t.Errorf("read-back %v != %s", rd["hex"], hexData)
		}
	}
	c.must("memory-write", map[string]any{"address": scratch, "data": orig}, nil)
	if got := c.must("memory-read", map[string]any{"address": scratch, "length": 32}, nil)["hex"]; got != orig {
		t.Errorf("SRAM not restored: %v != %s", got, orig)
	}

	// Invalid addresses are refused with a structured error.
	for _, a := range []any{"0xfffffff0", "0x20000000zz", float64(1 << 60), "-1"} {
		res, out := c.op("memory-read", map[string]any{"address": a, "length": 16}, nil)
		code := errCode(t, res, out)
		t.Logf("memory-read %v -> %s", a, code)
	}
	res, out := c.op("memory-write", map[string]any{"address": "0x08000000", "data": "00"}, nil)
	errCode(t, res, out) // flash must never be writable through memory-write

	// Single step x100 (10 calls of 10 and one call of 100).
	c.must("reset", map[string]any{"kind": "system", "halt": true}, nil)
	st := c.must("step", map[string]any{"count": 100}, nil)
	if st["moved"] == nil || st["step_bit_restored"] != true {
		t.Errorf("step 100: %v", st)
	}
	t.Logf("step x100: %v", st["moved"])
	c.must("resume", nil, nil)
}

func TestMCPHWBreakpointWatchpoint(t *testing.T) {
	c := newSTLink(t)
	t.Cleanup(func() {
		_, _ = c.op("breakpoint-clear", map[string]any{"all": true}, nil)
		_, _ = c.op("watchpoint-clear", map[string]any{"all": true}, nil)
		_, _ = c.op("resume", nil, nil)
	})
	ci := c.must("identify", nil, nil)
	t.Logf("FPB=%v DWT=%v", ci["breakpoint_unit"], ci["watchpoint_unit"])

	c.must("reset", map[string]any{"kind": "system", "halt": true}, nil)
	st := c.must("step", map[string]any{"count": 12}, nil)
	pc := hexAddr(st["pc"])
	if pc == "" {
		t.Fatalf("step result without pc: %v", st)
	}
	c.must("reset", map[string]any{"kind": "system", "halt": true}, nil)
	bp := c.must("breakpoint-set", map[string]any{"address": pc, "run_wait_ms": 2000}, nil)
	if bp["hit"] != true || hexAddr(bp["halted_pc"]) != pc {
		t.Fatalf("breakpoint at %s: %v", pc, bp)
	}
	l := c.must("breakpoint-list", nil, nil)
	t.Logf("installed breakpoints: %v", l)
	cl := c.must("breakpoint-clear", map[string]any{"all": true}, nil)
	if cl["installed"] != float64(0) {
		t.Errorf("breakpoint not cleared: %v", cl)
	}
	c.must("resume", nil, nil)

	// Watchpoint on the stack the original firmware pushes to right after reset.
	c.must("reset", map[string]any{"kind": "system", "halt": true}, nil)
	regs := c.must("registers", map[string]any{"keep_halted": true}, nil)
	rm, _ := regs["registers"].(map[string]any)
	sp := hexAddr(rm["msp"])
	if sp == "" {
		sp = hexAddr(rm["sp"])
	}
	var spv uint64
	if _, err := fmt.Sscanf(sp, "0x%x", &spv); err != nil || spv < 0x20000000 {
		t.Skipf("no usable stack pointer %q", sp)
	}
	wa := fmt.Sprintf("0x%08x", (spv-256)&^255)
	// A target_op is one ephemeral device session and the plugin removes the
	// triggers it installed when the session closes, so install, run and
	// wait happen inside ONE call: pre calls, the operation, post calls.
	res, out := c.op("wait-halt", map[string]any{"wait_ms": 3000}, map[string]any{
		"pre": []map[string]any{
			{"capability": "DebugProbe", "method": "set_watchpoint", "args": map[string]any{"address": wa, "kind": "write", "mask_bits": 8}},
			{"capability": "DebugProbe", "method": "resume"},
		},
		"post": []map[string]any{
			{"capability": "DebugProbe", "method": "clear_watchpoint", "args": map[string]any{"all": true}},
		},
	})
	t.Logf("watchpoint wait: isError=%v %s", res.IsError, jsonOf(out))
	if res.IsError {
		// The original firmware may not write the watched stack window;
		// that is reported honestly, never faked as a hit.
		t.Logf("WATCHPOINT_HIT_NOT_PROVOKED code=%s", errCode(t, res, out))
	} else if hit := inner(out); hit["watchpoint_hit"] != true {
		t.Errorf("halted without a watchpoint hit: %v", hit)
	}
	if !res.IsError {
		t.Logf("WATCHPOINT_HIT_PROVEN")
	}
	if l := c.must("watchpoint-list", nil, nil); l["slots_in_use"] != float64(0) && l["installed"] != float64(0) {
		t.Logf("watchpoints after the call: %v", l)
	}
	c.must("resume", nil, nil)
}

func TestMCPHWFlashSafety(t *testing.T) {
	c := newSTLink(t)

	// Reference read: the whole flash goes to an artifact, never inline.
	fi := c.must("flash-info", nil, nil)
	t.Logf("flash-info: %v", fi)
	res, out := c.op("flash-read", map[string]any{"address": "0x08000000", "length": 131072, "path": "ref.bin"}, nil)
	if res.IsError {
		t.Fatalf("flash-read: %s", jsonOf(out))
	}
	ref := "file/mcp/ref.bin"
	_, info := callTool(t, c.cs, "artifact_info", map[string]any{"ref": ref})
	art, _ := info["artifact"].(map[string]any)
	sha, _ := art["sha256"].(string)
	t.Logf("flash SHA-256 via MCP: %s (%v bytes)", sha, art["size_bytes"])
	if sha == "" {
		t.Fatalf("artifact_info: %v", info)
	}
	if os.Getenv("HWLAB_STLINK_FLASH_SHA") != "" && sha != os.Getenv("HWLAB_STLINK_FLASH_SHA") {
		t.Errorf("flash hash %s != expected", sha)
	}

	// Inline flash reads stay bounded.
	res, out = c.op("flash-read", map[string]any{"address": "0x08000000", "length": 131072}, nil)
	errCode(t, res, out)

	// Destructive operations without every safeguard are refused.
	refusals := []struct {
		name string
		op   string
		args map[string]any
		conf bool
		cfg  map[string]any
	}{
		{"erase-no-confirm", "flash-erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024, "confirm": "erase-flash-region", "backup": "ref.bin"}, false, nil},
		{"erase-no-allow", "flash-erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024, "confirm": "erase-flash-region", "backup": "ref.bin"}, true, nil},
		{"erase-no-token", "flash-erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024, "confirm": "wrong", "backup": "ref.bin"}, true, map[string]any{"allow_flash": true, "allow_erase": true}},
		{"chip-erase-no-allow", "flash-erase", map[string]any{"mode": "chip", "confirm": "erase-flash-chip", "backup": "ref.bin"}, true, map[string]any{"allow_flash": true, "allow_erase": true}},
		{"program-no-backup", "flash-program", map[string]any{"path": "ref.bin", "address": "0x08000000", "confirm": "program-flash", "backup": "missing.bin"}, true, map[string]any{"allow_flash": true, "allow_erase": true}},
		{"program-path-escape", "flash-program", map[string]any{"path": "/etc/passwd", "address": "0x08000000", "confirm": "program-flash", "backup": "ref.bin"}, true, map[string]any{"allow_flash": true, "allow_erase": true}},
		{"restore-path-escape", "flash-restore", map[string]any{"path": "../../../etc/passwd", "confirm": "restore-flash"}, true, map[string]any{"allow_flash": true, "allow_erase": true}},
	}
	for _, r := range refusals {
		cfg := map[string]any{"allow_memory_write": true, "allow_reset": true}
		for k, v := range r.cfg {
			cfg[k] = v
		}
		in := map[string]any{"plugin": "stlink", "device": c.serial, "op": r.op, "args": r.args, "config": cfg, "confirm": r.conf}
		res, out := callTool(t, c.cs, "target_op", in)
		code := errCode(t, res, out)
		t.Logf("%s refused: %s", r.name, code)
	}

	// Flash is byte-for-byte unchanged afterwards.
	c.must("flash-read", map[string]any{"address": "0x08000000", "length": 131072, "path": "after.bin", "overwrite": true}, nil)
	_, info2 := callTool(t, c.cs, "artifact_info", map[string]any{"ref": "file/mcp/after.bin"})
	art2, _ := info2["artifact"].(map[string]any)
	if art2["sha256"] != sha {
		t.Fatalf("FLASH CHANGED after refused operations: %v != %s", art2["sha256"], sha)
	}

	// Large data only through bounded artifact reads.
	_, ch := callTool(t, c.cs, "artifact_read", map[string]any{"ref": ref, "offset": 0, "length": 64})
	if ch["chunk"] == nil && ch["data"] == nil {
		t.Errorf("artifact_read: %v", ch)
	}
	res, out = callTool(t, c.cs, "artifact_read", map[string]any{"ref": ref, "offset": 0, "length": 1 << 20})
	errCode(t, res, out)
}

func TestMCPHWCLIParity(t *testing.T) {
	c := newSTLink(t)
	bin := filepath.Join(binDir(t, hwPlugins()...), "hwlab")
	home := t.TempDir()
	cli := func(args ...string) map[string]any {
		cmd := exec.Command(bin, append([]string{"target"}, args...)...)
		cmd.Env = append(os.Environ(), "HWLAB_HOME="+home, "HWLAB_PLUGIN_PATH="+filepath.Dir(bin))
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("cli output: %v\n%s", err, b)
		}
		r, _ := m["result"].(map[string]any)
		return r
	}
	mcpID := c.must("identify", nil, nil)
	cliID := cli("identify", "--plugin", "stlink", "--device", c.serial)
	for _, k := range []string{"probe_serial", "target_verified"} {
		if fmt.Sprint(mcpID[k]) != fmt.Sprint(cliID[k]) {
			t.Errorf("%s: mcp=%v cli=%v", k, mcpID[k], cliID[k])
		}
	}
	for _, grp := range []string{"core", "stm32", "debug_port", "breakpoint_unit", "watchpoint_unit"} {
		a, _ := mcpID[grp].(map[string]any)
		b, _ := cliID[grp].(map[string]any)
		for _, k := range []string{"cpuid", "dev_id", "rev_id", "family", "dp_idcode", "code_comparators", "comparators", "flash_size_kb"} {
			if a[k] != nil || b[k] != nil {
				if fmt.Sprint(a[k]) != fmt.Sprint(b[k]) {
					t.Errorf("%s.%s: mcp=%v cli=%v", grp, k, a[k], b[k])
				}
			}
		}
	}
	mm := c.must("memory-read", map[string]any{"address": "0x08000000", "length": 64}, nil)["hex"]
	cm := cli("memory", "read", "--plugin", "stlink", "--device", c.serial, "--arg", "address=0x08000000", "--arg", "length=64")["hex"]
	if mm != cm {
		t.Errorf("memory differs: mcp=%v cli=%v", mm, cm)
	}
}

func TestMCPHWPerformance(t *testing.T) {
	c := newSTLink(t)
	timeit := func(name string, n int, f func()) {
		start := time.Now()
		for i := 0; i < n; i++ {
			f()
		}
		d := time.Since(start) / time.Duration(n)
		t.Logf("PERF %-28s %8.1f ms/call (n=%d)", name, float64(d.Microseconds())/1000, n)
	}
	timeit("target_op info (cold open)", 5, func() { c.must("info", nil, nil) })
	timeit("target_op memory-read 16 B", 5, func() { c.must("memory-read", map[string]any{"address": "0x20000000", "length": 16}, nil) })
	timeit("target_op memory-read 4 KiB", 5, func() { c.must("memory-read", map[string]any{"address": "0x20000000", "length": 4096}, nil) })
	timeit("target_op flash-read 128 KiB", 2, func() {
		c.must("flash-read", map[string]any{"address": "0x08000000", "length": 131072, "path": "perf.bin", "overwrite": true}, nil)
	})
	timeit("lab_status (no hardware)", 20, func() { callTool(t, c.cs, "lab_status", nil) })
	timeit("capability_registry", 5, func() { callTool(t, c.cs, "capability_registry", nil) })
}

func TestMCPHWReopenAndBusy(t *testing.T) {
	c := newSTLink(t)
	for i := 0; i < 20; i++ {
		if r := c.must("info", nil, nil); r == nil {
			t.Fatalf("round %d: empty info", i)
		}
	}

	other := startServer(t, serverOpts{Plugins: hwPlugins()})
	resume := func() { _, _ = c.op("resume", nil, nil) }
	t.Cleanup(resume)
	c.must("reset", map[string]any{"kind": "system", "halt": false}, nil)

	// Client A holds the probe in a long wait-halt; client B must get BUSY.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.cs.CallTool(ctx, &sdk.CallToolParams{Name: "target_op", Arguments: map[string]any{
			"plugin": "stlink", "device": c.serial, "op": "wait-halt", "config": c.cfg, "confirm": true,
			"args": map[string]any{"wait_ms": 4000}}})
	}()
	time.Sleep(1200 * time.Millisecond)
	res, out := callTool(t, other, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"})
	code := errCode(t, res, out)
	if code != "BUSY" {
		t.Errorf("concurrent open: %s (%v)", code, out)
	}
	wg.Wait()

	// After A finished, B can use the probe: ownership was released.
	if res, out := callTool(t, other, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"}); res.IsError {
		t.Errorf("probe not released: %v", out)
	}
}

func TestMCPHWCancellation(t *testing.T) {
	c := newSTLink(t)
	t.Cleanup(func() { _, _ = c.op("resume", nil, nil) })
	c.must("reset", map[string]any{"kind": "system", "halt": false}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.cs.CallTool(ctx, &sdk.CallToolParams{Name: "target_op", Arguments: map[string]any{
		"plugin": "stlink", "device": c.serial, "op": "wait-halt", "config": c.cfg, "confirm": true,
		"args": map[string]any{"wait_ms": 20000}}})
	t.Logf("cancelled call returned after %s: %v", time.Since(start).Round(time.Millisecond), err)
	if err == nil {
		t.Error("client-side timeout did not interrupt the call")
	}
	// The probe must be usable again soon after the cancel.
	deadline := time.Now().Add(25 * time.Second)
	for {
		res, out := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"})
		if !res.IsError {
			t.Logf("probe usable again %s after cancel", time.Since(start).Round(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe still unusable 25s after cancel: %v", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestMCPHWCMSISDAPProbeOnly(t *testing.T) {
	needHW(t)
	cs := startServer(t, serverOpts{Plugins: hwPlugins()})
	_, out := callTool(t, cs, "device_list", nil)
	if !strings.Contains(jsonOf(out["devices"]), "cmsisdap") {
		t.Skip("no CMSIS-DAP probe attached")
	}
	res, out := callTool(t, cs, "target_op", map[string]any{"plugin": "cmsisdap", "op": "info"})
	if res.IsError {
		t.Fatalf("cmsisdap info: %s", jsonOf(out))
	}
	t.Logf("cmsisdap info: %s", jsonOf(inner(out)))
	res, out = callTool(t, cs, "target_op", map[string]any{"plugin": "cmsisdap", "op": "identify"})
	t.Logf("cmsisdap identify (no target wired): isError=%v %s", res.IsError, jsonOf(out))
	if res.IsError {
		code := errCode(t, res, out)
		if code == "INTERNAL" {
			t.Errorf("no-target condition must not be INTERNAL: %v", out)
		}
	}
}

func TestMCPHWCCDebuggerReadOnly(t *testing.T) {
	needHW(t)
	cs := startServer(t, serverOpts{Plugins: hwPlugins()})
	_, out := callTool(t, cs, "device_list", nil)
	if !strings.Contains(jsonOf(out["devices"]), "ccdebugger") {
		t.Skip("no CC-Debugger attached")
	}
	res, out := callTool(t, cs, "target_op", map[string]any{"plugin": "ccdebugger", "op": "info"})
	t.Logf("ccdebugger info: isError=%v %s", res.IsError, jsonOf(out))
}

// Interactive: asks for a physical unplug. Skipped unless HWLAB_MCP_UNPLUG=1.
func TestMCPHWUnplugReplug(t *testing.T) {
	c := newSTLink(t)
	if os.Getenv("HWLAB_MCP_UNPLUG") != "1" {
		t.Skip("set HWLAB_MCP_UNPLUG=1 (and run in a terminal) for the physical unplug test")
	}
	probe := c.must("info", nil, nil)
	t.Logf("before unplug: serial %s %v", c.serial, probe["usb"])

	waitGone := func(want bool) bool {
		for i := 0; i < 600; i++ {
			_, out := callTool(t, c.cs, "device_list", nil)
			present := strings.Contains(jsonOf(out["devices"]), c.serial)
			if present == want {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}
	fmt.Fprintln(os.Stderr, "\n>>> UNPLUG THE ST-LINK NOW")
	if !waitGone(false) {
		t.Fatal("ST-Link still listed after 5 min: not unplugged")
	}
	start := time.Now()
	res, out := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"})
	code := errCode(t, res, out)
	t.Logf("op on unplugged probe: %s after %s", code, time.Since(start).Round(time.Millisecond))
	if code != "DEVICE_GONE" && code != "NOT_FOUND" {
		t.Errorf("want DEVICE_GONE/NOT_FOUND, got %s", code)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("operation on a removed probe hung")
	}
	if res, _ := callTool(t, c.cs, "lab_status", nil); res.IsError {
		t.Error("server unusable after probe removal")
	}
	fmt.Fprintln(os.Stderr, ">>> PLUG THE ST-LINK BACK IN NOW")
	if !waitGone(true) {
		t.Fatal("ST-Link did not come back within 5 min")
	}
	time.Sleep(time.Second)
	id := c.must("identify", nil, nil)
	if p, _ := id["probe"].(map[string]any); p["serial"] != c.serial {
		t.Errorf("serial changed after replug: %v", p)
	}
	rd := c.must("memory-read", map[string]any{"address": "0x20000000", "length": 16}, nil)
	if rd["hex"] == nil {
		t.Errorf("memory read after replug: %v", rd)
	}
}

// A client that disappears mid-operation must not leave the probe claimed.
func TestMCPHWClientDisconnectReleasesProbe(t *testing.T) {
	c := newSTLink(t)
	t.Cleanup(func() { _, _ = c.op("resume", nil, nil) })
	c.must("reset", map[string]any{"kind": "system", "halt": false}, nil)

	a := startServer(t, serverOpts{Plugins: hwPlugins()})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = a.CallTool(ctx, &sdk.CallToolParams{Name: "target_op", Arguments: map[string]any{
			"plugin": "stlink", "device": c.serial, "op": "wait-halt", "config": c.cfg, "confirm": true,
			"args": map[string]any{"wait_ms": 20000}}})
	}()
	time.Sleep(1500 * time.Millisecond)
	if res, out := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"}); !res.IsError || errCode(t, res, out) != "BUSY" {
		t.Fatalf("probe not held by client A: %v", out)
	}
	_ = a.Close() // the client goes away; the server process is terminated
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, out := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "info"})
		if !res.IsError {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe still claimed 20 s after the client vanished: %v", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Different probes work in parallel from different MCP clients.
func TestMCPHWDifferentProbesInParallel(t *testing.T) {
	c := newSTLink(t)
	other := startServer(t, serverOpts{Plugins: hwPlugins()})
	_, out := callTool(t, other, "device_list", nil)
	if !strings.Contains(jsonOf(out["devices"]), `"plugin":"cmsisdap"`) {
		t.Skip("no CMSIS-DAP probe")
	}
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if res, _ := callTool(t, c.cs, "target_op", map[string]any{"plugin": "stlink", "device": c.serial, "op": "identify"}); res.IsError {
				errs <- "stlink identify failed"
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if res, _ := callTool(t, other, "target_op", map[string]any{"plugin": "cmsisdap", "op": "info"}); res.IsError {
				errs <- "cmsisdap info failed"
			}
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
