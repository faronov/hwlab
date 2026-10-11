//go:build hardware

package ccdebugger

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Opt-in: needs a CC-Debugger with a supported target and cc-tool. Only
// non-destructive operations run (info, MAC, flash read, info page).
func hwPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	p := New()
	if _, ok := p.tooling(); !ok {
		t.Skip("cc-tool not installed")
	}
	devs, _ := p.ListDevices(context.Background())
	if len(devs) == 0 {
		t.Skip("no CC-Debugger attached")
	}
	return p, devs[0].Id
}

func TestHardwareInfo(t *testing.T) {
	p, id := hwPlugin(t)
	res, err := invoke(t, p, pluginv1.CapDebugProbe, "info", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("programmer=%v target=%v chip=%v", res["programmer"], res["target"], res["chip"])
	if res["programmer"] == "" || res["target"] == "" {
		t.Fatalf("%v", res)
	}
}

func TestHardwareMAC(t *testing.T) {
	p, id := hwPlugin(t)
	res, err := invoke(t, p, pluginv1.CapDebugProbe, "read_mac", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("primary=%v", res["primary"])
	if res["primary"] == "" {
		t.Fatal("no primary MAC")
	}
}

func TestHardwareReadInfoPageAndFlash(t *testing.T) {
	p, id := hwPlugin(t)
	dir := t.TempDir()
	res, err := invoke(t, p, pluginv1.CapFlashProgramming, "read_info_page", id, map[string]any{"path": filepath.Join(dir, "info.bin")})
	if err != nil || res["bytes"].(float64) != 2048 {
		t.Fatalf("%v %v", res, err)
	}
	if os.Getenv("HWLAB_CC_READ_FLASH") == "" {
		t.Skip("set HWLAB_CC_READ_FLASH=1 to also read the whole flash (slow, read-only)")
	}
	res, err = invoke(t, p, pluginv1.CapFlashProgramming, "read", id, map[string]any{"path": filepath.Join(dir, "flash.bin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("flash bytes=%v sha256=%v", res["bytes"], res["sha256"])
}
