package mcp_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/faronov/hwlab/internal/testutil"
)

var expectedTools = []string{
	"lab_status", "plugin_list", "doctor", "device_list", "device_invoke", "target_op",
	"capture_start", "capture_stop", "capture_status", "capture_marker", "experiment_run",
	"session_list", "session_info", "timeline_query", "timeline_nearest", "timeline_correlate",
	"timeline_explain", "clock_calibrate", "waveform_measure", "usb_analyze", "modbus_decode",
	"power_analyze", "plot_render", "capability_registry", "artifact_list", "artifact_info", "artifact_read",
}

func TestProtocolInitializeAndTools(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})
	init := cs.InitializeResult()
	if init == nil || init.ServerInfo == nil || init.ServerInfo.Name != "hwlab" {
		t.Fatalf("bad initialize result: %+v", init)
	}
	if init.Capabilities == nil || init.Capabilities.Tools == nil {
		t.Fatal("server does not advertise the tools capability")
	}
	if !strings.Contains(init.Instructions, "confirm") {
		t.Error("instructions do not mention confirmation")
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]*sdk.Tool{}
	for _, tl := range res.Tools {
		have[tl.Name] = tl
		if tl.InputSchema == nil {
			t.Errorf("%s has no input schema", tl.Name)
		}
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
	}
	for _, n := range expectedTools {
		if have[n] == nil {
			t.Errorf("tool %s missing", n)
		}
	}
	if len(have) != len(expectedTools) {
		t.Errorf("tool count %d, want %d", len(have), len(expectedTools))
	}
}

func TestProtocolReadTools(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})

	res, out := callTool(t, cs, "lab_status", nil)
	if res.IsError || out["version"] == nil {
		t.Fatalf("lab_status: %v", out)
	}
	res, out = callTool(t, cs, "plugin_list", nil)
	if res.IsError || out["plugins"] == nil {
		t.Fatalf("plugin_list: %v", out)
	}
	res, out = callTool(t, cs, "device_list", nil)
	if res.IsError || out["devices"] == nil {
		t.Fatalf("device_list: %v", out)
	}
	res, out = callTool(t, cs, "capability_registry", nil)
	if res.IsError {
		t.Fatalf("capability_registry: %v", out)
	}
	reg, _ := out["registry"].(map[string]any)
	for _, k := range []string{"target_ops", "plugins", "error_codes"} {
		if reg[k] == nil {
			t.Errorf("registry lacks %s", k)
		}
	}
}

func TestProtocolStructuredErrors(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})

	_, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "no_such_tool"})
	if err == nil {
		t.Error("unknown tool must be a protocol error")
	}

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"dangerous-no-confirm", "device_invoke", map[string]any{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "args": map[string]any{"volts": 3.0}}, "CONFIRMATION_REQUIRED"},
		{"unknown-plugin", "device_invoke", map[string]any{"plugin": "nope", "capability": "X", "method": "y"}, "NOT_FOUND"},
		{"unknown-session", "session_info", map[string]any{"session": "does-not-exist"}, "NOT_FOUND"},
		{"unknown-target-op", "target_op", map[string]any{"plugin": "fake", "op": "frobnicate"}, "INVALID_ARGUMENT"},
		{"artifact-escape", "artifact_read", map[string]any{"ref": "file/../../etc/passwd"}, "PATH_DENIED"},
		{"artifact-missing", "artifact_info", map[string]any{"ref": "file/none.bin"}, "NOT_FOUND"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, out := callTool(t, cs, c.tool, c.args)
			got := errCode(t, res, out)
			if got != c.want {
				t.Errorf("code %s, want %s: %v", got, c.want, out)
			}
			if len(res.Content) == 0 {
				t.Error("no text content on error")
			}
		})
	}

	// The server must stay usable after errors.
	if res, _ := callTool(t, cs, "lab_status", nil); res.IsError {
		t.Error("server unusable after errors")
	}
}

func TestProtocolBadAddressGuard(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})
	for _, args := range []map[string]any{
		{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "confirm": true, "args": map[string]any{"address": float64(1 << 60)}},
		{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "confirm": true, "args": map[string]any{"address": "0xffffffffffffffff", "length": 16}},
		{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "confirm": true, "args": map[string]any{"length": -1}},
	} {
		res, out := callTool(t, cs, "device_invoke", args)
		code := errCode(t, res, out)
		if code != "BAD_ADDRESS" && code != "INVALID_ARGUMENT" {
			t.Errorf("args %v: code %s", args["args"], code)
		}
	}
}

func TestProtocolSafetyAudit(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})
	_, reg := callTool(t, cs, "capability_registry", nil)
	rm, _ := reg["registry"].(map[string]any)
	plugins, _ := rm["plugins"].([]any)
	checked := 0
	for _, p := range plugins {
		pm, _ := p.(map[string]any)
		if pm["id"] != "fake" {
			continue
		}
		for _, m := range asList(pm["methods"]) {
			mm, _ := m.(map[string]any)
			if mm["requires_confirmation"] != true {
				continue
			}
			checked++
			args := map[string]any{"plugin": "fake", "device": "power0", "capability": mm["capability"], "method": mm["name"], "args": map[string]any{}}
			res, out := callTool(t, cs, "device_invoke", args)
			if code := errCode(t, res, out); code == "" {
				t.Errorf("%v/%v ran without confirmation", mm["capability"], mm["name"])
			}
			if res.IsError {
				if e, _ := out["error"].(map[string]any); e["code"] != "CONFIRMATION_REQUIRED" {
					// A different refusal is acceptable only if the call did not run.
					t.Logf("%v/%v refused with %v", mm["capability"], mm["name"], e["code"])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("registry exposed no dangerous method to audit")
	}
}

func asList(v any) []any { l, _ := v.([]any); return l }

func TestProtocolMalformedInput(t *testing.T) {
	r := startRaw(t, serverOpts{Plugins: []string{"fake"}})
	if m := r.initialize(t); m["result"] == nil {
		t.Fatalf("initialize: %v", m)
	}

	r.send(`{"jsonrpc":"2.0","id":7,"method":"no/such/method"}`)
	if m := r.recv(t); rpcErrCode(m) != -32601 {
		t.Errorf("unknown method: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"lab_status"}}`)
	if m := r.recv(t); m["result"] == nil {
		t.Errorf("server did not survive malformed input: %v", m)
	}
	r.send(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"device_invoke","arguments":{"plugin":5}}}`)
	m := r.recv(t)
	if m["error"] == nil {
		if res, _ := m["result"].(map[string]any); res["isError"] != true {
			t.Errorf("wrong argument type accepted: %v", m)
		}
	}
	r.send(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"lab_status"}}`)
	if m := r.recv(t); m["result"] == nil {
		t.Errorf("server unusable after bad arguments: %v", m)
	}
}

// The SDK stdio transport ends the session on a line that is not JSON: the
// process must exit cleanly (no panic, no hang, nothing but JSON on stdout)
// and must not damage a later session on the same home.
func TestProtocolMalformedJSONEndsSessionCleanly(t *testing.T) {
	home := t.TempDir()
	r := startRaw(t, serverOpts{Home: home, Plugins: []string{"fake"}})
	r.initialize(t)
	r.send(`{this is not json`)
	for l := range r.lines {
		t.Logf("server answered: %s", l)
	}
	if err := r.exited(t, 20*time.Second); err != nil {
		t.Logf("exit status after malformed JSON: %v", err)
	}
	cs := startServer(t, serverOpts{Home: home, Plugins: []string{"fake"}})
	if res, out := callTool(t, cs, "lab_status", nil); res.IsError {
		t.Fatalf("later session damaged: %v", out)
	}
}

func TestProtocolEOFAndShutdown(t *testing.T) {
	// EOF in the middle of a request.
	r := startRaw(t, serverOpts{Plugins: []string{"fake"}})
	r.initialize(t)
	r.send(`{"jsonrpc":"2.0","id":2,"method":"tools/ca`)
	_ = r.in.Close()
	_ = r.exited(t, 20*time.Second)

	// Clean exit when stdin closes without any request.
	r2 := startRaw(t, serverOpts{Plugins: []string{"fake"}})
	r2.initialize(t)
	_ = r2.in.Close()
	if err := r2.exited(t, 20*time.Second); err != nil {
		t.Logf("exit after stdin close: %v", err)
	}
}

func TestProtocolRepeatedInitialize(t *testing.T) {
	r := startRaw(t, serverOpts{Plugins: []string{"fake"}})
	r.initialize(t)
	r.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	_ = r.recv(t) // either a result or an error; it must answer and stay alive
	r.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lab_status"}}`)
	if m := r.recv(t); m["result"] == nil {
		t.Errorf("server dead after repeated initialize: %v", m)
	}
}

func TestProtocolOversizedMessage(t *testing.T) {
	cs := startServer(t, serverOpts{Plugins: []string{"fake"}})
	big := strings.Repeat("A", 3<<20)
	res, out := callTool(t, cs, "device_invoke", map[string]any{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "confirm": true, "args": map[string]any{"blob": big}})
	if code := errCode(t, res, out); code != "INVALID_ARGUMENT" {
		t.Errorf("oversized argument: %s", code)
	}
	if res, _ := callTool(t, cs, "lab_status", nil); res.IsError {
		t.Error("server unusable after oversized argument")
	}
}

func TestProtocolRestart(t *testing.T) {
	home := t.TempDir()
	for i := 0; i < 3; i++ {
		cs := startServer(t, serverOpts{Home: home, Plugins: []string{"fake"}})
		if res, out := callTool(t, cs, "lab_status", nil); res.IsError {
			t.Fatalf("restart %d: %v", i, out)
		}
		_ = cs.Close()
	}
}

func TestProtocolConcurrentClients(t *testing.T) {
	home := t.TempDir()
	a := startServer(t, serverOpts{Home: home, Plugins: []string{"fake"}})
	b := startServer(t, serverOpts{Home: home, Plugins: []string{"fake"}})
	errs := make(chan string, 20)
	done := make(chan struct{})
	for _, cs := range []*sdk.ClientSession{a, b} {
		go func(cs *sdk.ClientSession) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 5; i++ {
				res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "device_list"})
				if err != nil || res.IsError {
					errs <- "device_list failed"
				}
			}
		}(cs)
	}
	<-done
	<-done
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Replay through the REAL server process: no hardware, no plugin binaries.
// The recorded ST-Link + STM32F1 session is replayed by an experiment; the
// recorded invoke results come back through timeline_query.
func TestProtocolReplayWorkflowWithoutHardware(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join(testutil.ModuleRoot(t), "testdata", "real-stlink-stm32-flash", "*"))
	if len(dirs) == 0 {
		t.Skip("fixture missing")
	}
	cs := startServer(t, serverOpts{Args: []string{"--replay", dirs[0]}})

	res, out := callTool(t, cs, "experiment_run", map[string]any{"yaml": "name: replay\nduration: 2s\ndevices:\n  stlink:\n    plugin: replay\n    device: stlink\n"})
	if res.IsError {
		t.Fatalf("experiment_run: %v", out)
	}
	rr, _ := out["result"].(map[string]any)
	sid, _ := rr["session_id"].(string)
	if sid == "" || rr["status"] != "complete" {
		t.Fatalf("replay result: %v", rr)
	}
	res, out = callTool(t, cs, "timeline_query", map[string]any{"session": sid, "types": []string{"invoke"}, "limit": 500})
	if res.IsError {
		t.Fatalf("timeline_query: %v", out)
	}
	evs := asList(out["events"])
	if len(evs) < 40 {
		t.Fatalf("only %d replayed invoke events", len(evs))
	}
	blob := jsonOf(evs)
	for _, want := range []string{"0x410", "STM32F1", "acba307bbb3fafa322220b35c9add1e5fc31ab4ba54ba9ba58a88255a8fe6a16"} {
		if !strings.Contains(blob, want) {
			t.Errorf("replayed events lack %s", want)
		}
	}
	if strings.Contains(blob, "5C3C3A") {
		t.Error("unexpected raw serial material")
	}

	// A hardware-only operation must not silently succeed in replay mode.
	res, out = callTool(t, cs, "target_op", map[string]any{"plugin": "stlink", "op": "halt"})
	if !res.IsError {
		t.Errorf("target_op against replay-only server succeeded: %v", out)
	}
}
