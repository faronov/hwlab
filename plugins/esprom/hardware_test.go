//go:build hardware

package espromplugin

import (
	"context"
	"os"
	"testing"
)

// TestHWIdentify enters the ROM of the board on HWLAB_ESPROM_DEVICE (a
// discovered id, e.g. cp210x-<serial>), identifies it and resets it back
// into the application. With HWLAB_ESPROM_BACKUP_DIR it also takes a
// verified full backup. Read-only: no erase or write is sent.
//
//	HWLAB_ESPROM_DEVICE=cp210x-... go test -tags hardware -count=1 -v ./plugins/esprom/ -run TestHW
func TestHWIdentify(t *testing.T) {
	id := os.Getenv("HWLAB_ESPROM_DEVICE")
	if id == "" {
		t.Skip("HWLAB_ESPROM_DEVICE not set")
	}
	p := New()
	if err := p.OpenDevice(context.Background(), id, map[string]any{"allow_reset": true}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.CloseDevice(context.Background(), id) }()
	r := must(t, p, id, "TargetIdentify.identify", nil)
	t.Logf("identify: %v", r)
	if dir := os.Getenv("HWLAB_ESPROM_BACKUP_DIR"); dir != "" {
		b := must(t, p, id, "FlashProgramming.backup", map[string]any{"dir": dir})
		t.Logf("backup: %v", b)
	}
	t.Logf("reset: %v", must(t, p, id, "FlashProgramming.reset", map[string]any{"console_ms": float64(3000)}))
}
