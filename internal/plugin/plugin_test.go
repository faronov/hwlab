package plugin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"

	"github.com/faronov/hwlab/internal/testutil"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(Options{Paths: []string{testutil.BuildFakePlugin(t)}, CoreVersion: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m.Discover()
	t.Cleanup(func() { m.Close(context.Background()) })
	return m
}

func TestLaunchHandshakeAndList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := newManager(t)
	if ids := m.IDs(); len(ids) != 1 || ids[0] != "fake" {
		t.Fatalf("ids = %v", ids)
	}
	b, err := m.Get(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if b.Info().GetId() != "fake" || b.Info().GetProtocolVersion() != pluginv1.ProtocolVersion {
		t.Errorf("info = %v", b.Info())
	}
	devs, err := b.ListDevices(ctx)
	if err != nil || len(devs) == 0 {
		t.Fatalf("devices = %v, err = %v", devs, err)
	}
	if b2, _ := m.Get(ctx, "fake"); b2 != b {
		t.Error("a healthy plugin must be reused, not relaunched")
	}
	if _, err := m.Get(ctx, "nope"); err == nil {
		t.Error("unknown plugin must fail")
	}
}

func TestCrashIsReportedAndPluginRelaunched(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := newManager(t)
	b, err := m.Get(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	p := b.(*process)
	p.kill()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() not closed after the process died")
	}
	if _, err := b.ListDevices(ctx); !errors.Is(err, ErrCrashed) {
		t.Errorf("call on dead plugin = %v, want ErrCrashed", err)
	}
	b2, err := m.Get(ctx, "fake")
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if b2 == b {
		t.Fatal("dead plugin must be replaced")
	}
	if devs, err := b2.ListDevices(ctx); err != nil || len(devs) == 0 {
		t.Errorf("relaunched plugin unusable: %v %v", devs, err)
	}
}

func TestBuiltinBackend(t *testing.T) {
	m := NewManager(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	called := 0
	m.RegisterBuiltin("b", func() (Backend, error) { called++; return nil, errors.New("boom") })
	if _, err := m.Get(context.Background(), "b"); err == nil || called != 1 {
		t.Errorf("builtin factory error must surface: %v (called %d)", err, called)
	}
}
