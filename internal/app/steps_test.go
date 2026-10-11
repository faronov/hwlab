package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/timeline"
)

func stepExperiment(steps ...experiment.Step) *experiment.Experiment {
	return &experiment.Experiment{
		Name:     "steps",
		Duration: experiment.Duration(300 * time.Millisecond),
		Devices:  []experiment.DeviceSpec{{Alias: "ctl", Plugin: "fake", Device: "ctl0", Config: map[string]any{}}},
		Steps:    steps,
	}
}

func invokes(evs []timeline.Event) []timeline.Event {
	var out []timeline.Event
	for _, e := range evs {
		if e.Type == timeline.TypeInvoke {
			out = append(out, e)
		}
	}
	return out
}

// A control-only device (no capture capability) is driven by timed steps and
// every call lands on the timeline, in time order.
func TestStepsOnControlDevice(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	res, err := a.RunExperiment(ctx, stepExperiment(
		experiment.Step{Name: "late", At: experiment.Duration(100 * time.Millisecond), Device: "ctl", Call: "DebugProbe.echo", Args: map[string]any{"n": 2.0}},
		experiment.Step{Name: "early", At: experiment.Duration(20 * time.Millisecond), Device: "ctl", Call: "DebugProbe.echo", Args: map[string]any{"n": 1.0}},
	), false)
	if err != nil || res.Status != "complete" || len(res.Warnings) != 0 {
		t.Fatalf("run: %+v err=%v", res, err)
	}
	evs, err := a.QueryTimeline(app.TimelineQuery{Session: res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	inv := invokes(evs)
	if len(inv) != 2 {
		t.Fatalf("want 2 invoke events, got %d: %+v", len(inv), evs)
	}
	if inv[0].Data["step"] != "early" || inv[1].Data["step"] != "late" || inv[0].TimestampNs >= inv[1].TimestampNs {
		t.Errorf("steps out of order: %+v", inv)
	}
	if inv[0].Data["ok"] != true || inv[0].SourceID != "ctl" {
		t.Errorf("unexpected invoke event: %+v", inv[0])
	}
	if got := inv[0].Data["result"].(map[string]any)["n"]; got != 1.0 {
		t.Errorf("result not recorded: %v", inv[0].Data)
	}
}

func TestDangerousStepNeedsConfirmation(t *testing.T) {
	a := newApp(t)
	e := stepExperiment(experiment.Step{Name: "v", At: experiment.Duration(10 * time.Millisecond), Device: "ctl", Call: "PowerSource.set_voltage", Args: map[string]any{"volts": 3.0}})
	_, err := a.RunExperiment(context.Background(), e, false)
	if !policy.IsConfirmationRequired(err) {
		t.Fatalf("want confirmation-required, got %v", err)
	}
	if ss, _ := a.ListSessions(); len(ss) != 0 {
		t.Errorf("a refused run must not create a session, found %d", len(ss))
	}
	if res, err := a.RunExperiment(context.Background(), e, true); err != nil || res.Status != "complete" {
		t.Fatalf("confirmed run: %+v %v", res, err)
	}
}

func TestStepErrorsAreRecordedNotFatal(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	res, err := a.RunExperiment(ctx, stepExperiment(
		experiment.Step{Name: "bad", At: experiment.Duration(10 * time.Millisecond), Device: "ctl", Call: "PowerSource.set_voltage", Args: map[string]any{"volts": 9.0}},
		experiment.Step{Name: "ok", At: experiment.Duration(20 * time.Millisecond), Device: "ctl", Call: "DebugProbe.echo"},
	), true)
	if err != nil || res.Status != "complete" {
		t.Fatalf("a failing step must not abort the run: %+v %v", res, err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "step(s) failed") {
		t.Fatalf("want a failed-step warning: %v", res.Warnings)
	}
	evs, _ := a.QueryTimeline(app.TimelineQuery{Session: res.SessionID})
	inv := invokes(evs)
	if len(inv) != 2 || inv[0].Data["ok"] != false || inv[1].Data["ok"] != true || inv[0].Data["error"] == "" {
		t.Fatalf("failed step not recorded: %+v", inv)
	}
	// An undeclared method is refused before anything runs.
	_, err = a.RunExperiment(ctx, stepExperiment(experiment.Step{Name: "x", Device: "ctl", Call: "DebugProbe.nope"}), false)
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("undeclared method: %v", err)
	}
}

func TestControlOnlyRunWithoutStepsIsRefused(t *testing.T) {
	_, err := newApp(t).RunExperiment(context.Background(), stepExperiment(), false)
	if err == nil || !strings.Contains(err.Error(), "nothing to run") {
		t.Fatalf("got %v", err)
	}
}

// Invoke events replay without the plugin, like any other timeline event.
func TestStepsReplayWithoutPlugin(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	res, err := a.RunExperiment(ctx, stepExperiment(experiment.Step{Name: "e", At: experiment.Duration(10 * time.Millisecond), Device: "ctl", Call: "DebugProbe.echo", Args: map[string]any{"k": "v"}}), false)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := a.ListSessions()
	if len(ms) != 1 || len(ms[0].Devices) != 1 || len(ms[0].Captures) != 0 {
		t.Fatalf("control device must be listed without a capture entry: %+v", ms)
	}

	b := newApp(t)
	b.Cfg.SessionsDir = a.Cfg.SessionsDir
	if err := b.UseReplay(res.SessionID); err != nil {
		t.Fatal(err)
	}
	rr, err := b.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "ctl", Plugin: replay.PluginID, Device: "ctl", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v %v", rr, err)
	}
	evs, err := b.QueryTimeline(app.TimelineQuery{Session: rr.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if inv := invokes(evs); len(inv) != 1 || inv[0].Data["method"] != "echo" {
		t.Fatalf("replayed invoke events: %+v", inv)
	}
}
