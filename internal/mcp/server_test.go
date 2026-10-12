package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
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
