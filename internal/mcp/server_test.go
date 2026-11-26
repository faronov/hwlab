package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	hmcp "github.com/faronov/hwlab/internal/mcp"
	"github.com/faronov/hwlab/internal/testutil"
)

func connect(t *testing.T) *sdk.ClientSession {
	t.Helper()
	cs, _ := connectWith(t, nil)
	return cs
}

// connectWith connects a client with opts and returns the server's hwlab home.
func connectWith(t *testing.T, opts *sdk.ClientOptions) (*sdk.ClientSession, string) {
	t.Helper()
	cfg := config.Config{Home: t.TempDir(), PluginPaths: []string{testutil.BuildFakePlugin(t)}}
	cfg.SessionsDir = filepath.Join(cfg.Home, "sessions")
	a := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { a.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := hmcp.NewServer(a).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, cfg.Home
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("%s: encode structured result: %v", name, err)
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: decode structured result: %v", name, err)
		}
	}
	return res, out
}

func fakeDevices() []any {
	mk := func(alias, dev string, extra map[string]any) map[string]any {
		cfg := map[string]any{"realtime": false}
		for k, v := range extra {
			cfg[k] = v
		}
		return map[string]any{"alias": alias, "plugin": "fake", "device": dev, "config": cfg}
	}
	return []any{
		mk("power", "power0", map[string]any{"first_tx_ms": 12.34, "period_ms": 100.0}),
		mk("radio", "radio0", nil),
	}
}

func TestToolsListed(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range res.Tools {
		have[tl.Name] = true
	}
	for _, want := range []string{"device_list", "capture_start", "capture_stop", "session_info", "timeline_query",
		"timeline_nearest", "timeline_correlate", "plot_render", "device_invoke", "power_analyze", "modbus_decode", "waveform_measure", "usb_analyze"} {
		if !have[want] {
			t.Errorf("tool %s not registered", want)
		}
	}
}

func TestCaptureWorkflow(t *testing.T) {
	cs := connect(t)

	_, devs := call(t, cs, "device_list", nil)
	if l, _ := devs["devices"].([]any); len(l) == 0 {
		t.Fatalf("device_list returned no devices: %v", devs)
	}

	res, start := call(t, cs, "capture_start", map[string]any{"name": "mcp-test", "duration": "1s", "devices": fakeDevices(), "nominal_voltage": 3.3})
	if res.IsError {
		t.Fatalf("capture_start failed: %+v", res.Content)
	}
	sid, _ := start["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session id: %v", start)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		res, packets := call(t, cs, "timeline_query", map[string]any{"session": sid, "types": []string{"packet"}, "limit": 1})
		if res.IsError {
			t.Fatalf("live timeline_query failed: %+v", res.Content)
		}
		res, progress := call(t, cs, "capture_status", nil)
		if res.IsError {
			t.Fatalf("capture_status failed: %+v", res.Content)
		}
		haveSamples := false
		for _, v := range progress["captures"].([]any) {
			c := v.(map[string]any)
			if c["session_id"] == sid && c["samples"].(float64) > 0 {
				haveSamples = true
			}
		}
		if evs, _ := packets["events"].([]any); len(evs) > 0 && haveSamples {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("capture produced no packet/power data: packets=%v progress=%v", packets, progress)
		case <-poll.C:
		}
	}

	res, _ = call(t, cs, "capture_stop", map[string]any{"session": sid})
	if res.IsError {
		t.Fatalf("capture_stop failed: %+v", res.Content)
	}

	_, info := call(t, cs, "session_info", map[string]any{"session": sid})
	if b, _ := json.Marshal(info); !strings.Contains(string(b), "radio") {
		t.Errorf("session_info lacks the radio source: %s", b)
	}

	_, q := call(t, cs, "timeline_query", map[string]any{"session": sid, "types": []string{"packet"}, "limit": 5})
	if evs, _ := q["events"].([]any); len(evs) == 0 || len(evs) > 5 {
		t.Errorf("timeline_query events = %d (want 1..5): %v", len(evs), q)
	}

	_, n := call(t, cs, "timeline_nearest", map[string]any{"session": sid, "at": "12.34ms", "types": []string{"packet"}})
	if b, _ := json.Marshal(n); !strings.Contains(string(b), "delta_ns") {
		t.Errorf("timeline_nearest = %s", b)
	}

	res, _ = call(t, cs, "plot_render", map[string]any{"session": sid, "format": "png"})
	if res.IsError {
		t.Fatalf("plot_render failed: %+v", res.Content)
	}
	var gotImage bool
	for _, c := range res.Content {
		if img, ok := c.(*sdk.ImageContent); ok && len(img.Data) > 100 && img.MIMEType == "image/png" {
			gotImage = true
		}
	}
	if !gotImage {
		t.Error("plot_render must return the PNG inline")
	}
}

func TestDangerousOperationNeedsConfirm(t *testing.T) {
	cs := connect(t)
	args := map[string]any{"plugin": "fake", "device": "power0", "capability": "PowerSource", "method": "set_voltage", "args": map[string]any{"volts": 3.0}}
	res, _ := call(t, cs, "device_invoke", args)
	if !res.IsError {
		t.Fatal("dangerous invoke without confirm must be rejected")
	}
	if len(res.Content) == 0 {
		t.Fatal("error carries no message")
	}
	if tc, ok := res.Content[0].(*sdk.TextContent); !ok || !strings.Contains(tc.Text, "confirm") {
		t.Errorf("error should tell the model to ask for confirmation: %+v", res.Content[0])
	}
}

// A long experiment does not hold the request open: experiment_run returns
// state=running after `wait`, the run goes on, and capture_status with the
// session collects the result.
func TestExperimentRunReturnsEarlyAndStatusCollectsTheResult(t *testing.T) {
	cs := connect(t)
	yaml := "name: long\nduration: 1500ms\ndevices:\n  power:\n    plugin: fake\n    device: power0\n"
	res, out := call(t, cs, "experiment_run", map[string]any{"yaml": yaml, "wait": "0s"})
	if res.IsError || out["state"] != "running" {
		t.Fatalf("experiment_run: %+v %v", res.Content, out)
	}
	sid, _ := out["session_id"].(string)
	if sid == "" || !strings.Contains(out["next"].(string), "capture_status") {
		t.Fatalf("no session id or hint: %v", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, st := call(t, cs, "capture_status", map[string]any{"session": sid})
		if res.IsError {
			t.Fatalf("capture_status: %+v", res.Content)
		}
		c := st["capture"].(map[string]any)
		if c["state"] == "finished" {
			r, _ := c["result"].(map[string]any)
			if r == nil || r["session_id"] != sid || r["status"] != "complete" || r["samples"].(float64) == 0 {
				t.Fatalf("finished without a result: %v", c)
			}
			break
		}
		if c["duration_ns"].(float64) != 1.5e9 {
			t.Fatalf("status %v", c)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never finished: %v", c)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if res, _ := call(t, cs, "capture_status", map[string]any{"session": "nope"}); !res.IsError {
		t.Error("an unknown session must be an error")
	}
	// A short run within the default wait still answers with its result.
	res, out = call(t, cs, "experiment_run", map[string]any{"yaml": "name: short\nduration: 200ms\ndevices:\n  power:\n    plugin: fake\n    device: power0\n"})
	if res.IsError || out["state"] != "finished" || out["result"] == nil {
		t.Fatalf("short run: %v", out)
	}
	if res, _ := call(t, cs, "experiment_run", map[string]any{"yaml": yaml, "wait": "2h"}); !res.IsError {
		t.Error("a wait above the maximum must be refused")
	}
}

func TestExperimentRunSendsProgress(t *testing.T) {
	var mu sync.Mutex
	var notes []*sdk.ProgressNotificationParams
	cs, _ := connectWith(t, &sdk.ClientOptions{ProgressNotificationHandler: func(_ context.Context, r *sdk.ProgressNotificationClientRequest) {
		mu.Lock()
		notes = append(notes, r.Params)
		mu.Unlock()
	}})
	p := &sdk.CallToolParams{Name: "experiment_run", Arguments: map[string]any{"wait": "30s",
		"yaml": "name: progress\nduration: 6s\ndevices:\n  power:\n    plugin: fake\n    device: power0\n"}}
	p.SetProgressToken("tok")
	res, err := cs.CallTool(context.Background(), p)
	if err != nil || res.IsError {
		t.Fatalf("experiment_run: %v %+v", err, res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) == 0 || notes[0].ProgressToken != "tok" || notes[0].Total != 6 || notes[0].Progress <= 0 {
		t.Fatalf("progress notifications: %+v", notes)
	}
}

// Step file arguments of an MCP experiment are confined to the artifacts
// directory like device_invoke's, and their parent directory is created.
func TestExperimentRunConfinesStepPaths(t *testing.T) {
	cs, home := connectWith(t, nil)
	yaml := "name: paths\nduration: 200ms\ndevices:\n  ctl:\n    plugin: fake\n    device: ctl0\n" +
		"steps:\n  - {name: save, at: 10ms, device: ctl, call: DebugProbe.echo, args: {path: dumps/run1/flash.bin}}\n"
	res, out := call(t, cs, "experiment_run", map[string]any{"yaml": yaml})
	if res.IsError || out["state"] != "finished" {
		t.Fatalf("experiment_run: %+v %v", res.Content, out)
	}
	want := filepath.Join(home, "artifacts", "mcp", "dumps", "run1")
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("parent directory %s not created: %v", want, err)
	}
	_, q := call(t, cs, "timeline_query", map[string]any{"session": out["session_id"], "types": []string{"invoke"}})
	if b, _ := json.Marshal(q); !strings.Contains(string(b), filepath.Join(want, "flash.bin")) {
		t.Errorf("the step did not get the confined path: %s", b)
	}
	esc := strings.Replace(yaml, "dumps/run1/flash.bin", "../../escape.bin", 1)
	if res, _ := call(t, cs, "experiment_run", map[string]any{"yaml": esc}); !res.IsError {
		t.Error("a step path escaping the artifacts directory must be refused")
	}
}
