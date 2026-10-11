package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/app"
)

func TestTargetRefusesUnknownAndUndeclaredOperations(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	if _, err := a.Target(ctx, app.TargetRequest{Op: "explode"}); err == nil || !strings.Contains(err.Error(), "unknown target operation") {
		t.Fatalf("unknown op: %v", err)
	}
	// The fake debug probe declares only DebugProbe.echo, so a generic target
	// operation must fail closed instead of guessing a method.
	_, err := a.Target(ctx, app.TargetRequest{Plugin: "fake", Device: "debug0", Op: "halt"})
	if err == nil || !strings.Contains(err.Error(), "does not support DebugProbe.halt") {
		t.Fatalf("undeclared op: %v", err)
	}
	// A pre call is validated before the device is opened.
	_, err = a.Target(ctx, app.TargetRequest{Plugin: "fake", Device: "debug0", Op: "info",
		Pre: []app.TargetCall{{Capability: "DebugProbe", Method: "echo"}}})
	if err == nil {
		t.Fatal("info is not declared by the fake probe")
	}
}

func TestTargetOpTableCoversGenericCapabilities(t *testing.T) {
	for _, op := range []string{"info", "identify", "attach", "halt", "resume", "reset", "registers", "memory-read", "memory-write",
		"flash-read", "flash-program", "flash-verify", "flash-erase"} {
		if _, ok := app.TargetOps[op]; !ok {
			t.Errorf("missing generic operation %s", op)
		}
	}
}
