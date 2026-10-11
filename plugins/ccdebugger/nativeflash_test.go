package ccdebugger

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

func flashCfg() map[string]any {
	m := map[string]any{}
	for k, v := range sessionCfg {
		m[k] = v
	}
	m["allow_flash_erase"], m["allow_flash_program"], m["allow_chip_erase"], m["allow_config_write"] = true, true, true, true
	return m
}

func attached(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p, _ := nativePlugin(t, &fakeTool{err: os.ErrNotExist})
	openWith(t, p, cfg)
	call(t, p, pluginv1.CapDebugProbe, "attach", nil)
	if cfg["allow_dma_enable"] == true {
		call(t, p, pluginv1.CapDebugProbe, "enable_dma", nil)
	}
	return p
}

func TestNativeChipInfoLockAndIEEE(t *testing.T) {
	p := attached(t, flashCfg())
	ci := call(t, p, pluginv1.CapDebugProbe, "chip_info", nil)
	if ci["chip"] != "CC2530" || ci["chip_id"] != "0xA5" || ci["flash_size"] != float64(262144) || ci["page_size"] != float64(2048) ||
		ci["ram_size"] != float64(8192) || ci["programmable"] != true {
		t.Fatalf("chip_info %v", ci)
	}
	if ci["lock"].(map[string]any)["debug_locked"] != false {
		t.Fatalf("lock %v", ci["lock"])
	}
	lk := call(t, p, pluginv1.CapDebugProbe, "read_lock", nil)
	if lk["raw"] != strings.Repeat("ff", 16) {
		t.Fatalf("lock %v", lk)
	}
	ie := call(t, p, pluginv1.CapDebugProbe, "read_ieee", nil)
	if ie["secondary"] != "88:77:66:55:44:33:22:11" || ie["secondary_stored"] != "1122334455667788" || ie["primary_present"] != true {
		t.Fatalf("ieee %v", ie)
	}
	info := call(t, p, pluginv1.CapDebugProbe, "info", nil)
	if info["chip"].(map[string]any)["chip"] != "CC2530" {
		t.Fatalf("info %v", info)
	}
	if hw := info["implemented_hardware_unverified"].([]any); len(hw) == 0 {
		t.Fatalf("destructive operations must be listed as hardware-unverified: %v", info)
	}
}

func TestNativeFlashReadToFile(t *testing.T) {
	p := attached(t, flashCfg())
	dir := t.TempDir()
	out := filepath.Join(dir, "flash.bin")
	r := call(t, p, pluginv1.CapFlashProgramming, "read", map[string]any{"path": out, "address": "0x17FFC", "length": 600.0})
	b, err := os.ReadFile(out)
	if err != nil || len(b) != 600 || r["bytes"] != float64(600) {
		t.Fatalf("%v %d %v", err, len(b), r)
	}
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "read", "90000003", map[string]any{"path": out})
	wantCode(t, err, codes.AlreadyExists)
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "read", "90000003", map[string]any{"path": filepath.Join(dir, "x.bin"), "address": "0x3FFF0", "length": 32.0, "overwrite": true})
	wantCode(t, err, codes.InvalidArgument)
	whole := filepath.Join(dir, "whole.bin")
	call(t, p, pluginv1.CapFlashProgramming, "read", map[string]any{"path": whole})
	if fi, _ := os.Stat(whole); fi.Size() != 262144 {
		t.Fatalf("whole flash read is %d bytes", fi.Size())
	}
	ip := filepath.Join(dir, "info.bin")
	call(t, p, pluginv1.CapFlashProgramming, "read_info_page", map[string]any{"path": ip})
	if fi, _ := os.Stat(ip); fi.Size() != 2048 {
		t.Fatalf("info page %d", fi.Size())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 3 {
		t.Fatalf("temporary files leaked: %v", ents)
	}
}

func TestNativeProgramVerifyEndToEnd(t *testing.T) {
	p := attached(t, flashCfg())
	dir := t.TempDir()
	img := bytes.Repeat([]byte{0xA5, 0x5A, 0x01, 0x02}, 600)
	path := filepath.Join(dir, "fw.bin")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": path, "base": "0x1000"}
	_, err := invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", args)
	wantCode(t, err, codes.PermissionDenied) // no confirmation token
	args["confirm"] = "program-flash"
	r := call(t, p, pluginv1.CapFlashProgramming, "program", args)
	if r["verify"] != "readback" || r["verified"] != true || r["mode"] != "preserve-device-config" || r["reset_required"] != true {
		t.Fatalf("program %v", r)
	}
	got := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "flash", "address": "0x1000", "length": 64.0})
	if got["data"] != hex.EncodeToString(img[:64]) {
		t.Fatalf("flash after program: %v", got["data"])
	}
	// The target must be reset before it runs again.
	_, err = invoke(t, p, pluginv1.CapDebugProbe, "resume", "90000003", nil)
	if err == nil {
		t.Fatal("resume must be refused until the target is reset")
	}
	call(t, p, pluginv1.CapFlashProgramming, "verify", map[string]any{"path": path, "base": "0x1000", "mode": "both"})
	// Writing over the lock page needs the stronger mode, not the default.
	over := filepath.Join(dir, "over.bin")
	if err := os.WriteFile(over, bytes.Repeat([]byte{0x11}, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": over, "base": "0x3FFF0", "confirm": "program-flash"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": path, "base": "0x3F000", "mode": "bogus", "confirm": "program-flash"})
	wantCode(t, err, codes.InvalidArgument)
	img[100] ^= 0xFF
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "verify", "90000003", map[string]any{"path": path, "base": "0x1000"})
	wantCode(t, err, codes.DataLoss)
}

func TestNativeProgramNeedsPermission(t *testing.T) {
	p := attached(t, sessionCfg)
	path := filepath.Join(t.TempDir(), "fw.bin")
	os.WriteFile(path, []byte{1, 2, 3, 4}, 0o644)
	_, err := invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": path, "confirm": "program-flash"})
	wantCode(t, err, codes.PermissionDenied)
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "erase", "90000003", map[string]any{"confirm": "erase-entire-chip"})
	wantCode(t, err, codes.PermissionDenied)
}

func TestNativeErasePagesAndChipErase(t *testing.T) {
	p := attached(t, flashCfg())
	_, err := invoke(t, p, pluginv1.CapFlashProgramming, "erase_pages", "90000003", map[string]any{"first_page": 4.0, "count": 2.0})
	wantCode(t, err, codes.PermissionDenied)
	r := call(t, p, pluginv1.CapFlashProgramming, "erase_pages", map[string]any{"first_page": 4.0, "count": 2.0, "confirm": "erase-flash-pages"})
	if r["blank_checked"] != true || len(r["pages"].([]any)) != 2 {
		t.Fatalf("%v", r)
	}
	got := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "flash", "address": "0x2000", "length": 8.0})
	if got["data"] != "ffffffffffffffff" {
		t.Fatalf("erased data %v", got["data"])
	}
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "erase_pages", "90000003", map[string]any{"pages": []any{127.0}, "confirm": "erase-flash-pages"})
	wantCode(t, err, codes.InvalidArgument) // the lock-bit page is refused
	call(t, p, pluginv1.CapDebugProbe, "reset_halt", nil)
	call(t, p, pluginv1.CapDebugProbe, "enable_dma", nil)
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "erase", "90000003", nil)
	wantCode(t, err, codes.PermissionDenied)
	res := call(t, p, pluginv1.CapFlashProgramming, "erase", map[string]any{"confirm": "erase-entire-chip"})
	if res["reattach_required"] != true {
		t.Fatalf("%v", res)
	}
}

func TestNativeWriteConfigBackupAndConfirmations(t *testing.T) {
	p := attached(t, flashCfg())
	dir := t.TempDir()
	bak := filepath.Join(dir, "lockpage.bak")
	args := map[string]any{"secondary_ieee": "00:11:22:33:44:55:66:77", "backup_path": bak, "confirm": "write-device-config"}
	_, err := invoke(t, p, pluginv1.CapFlashProgramming, "write_config", "90000003", map[string]any{"secondary_ieee": "00:11:22:33:44:55:66:77", "confirm": "write-device-config"})
	wantCode(t, err, codes.InvalidArgument) // no backup path
	r := call(t, p, pluginv1.CapFlashProgramming, "write_config", args)
	if r["verified"] != true || r["backup_path"] != bak {
		t.Fatalf("%v", r)
	}
	if b, err := os.ReadFile(bak); err != nil || len(b) != 2048 {
		t.Fatalf("backup %v %d", err, len(b))
	}
	call(t, p, pluginv1.CapDebugProbe, "reset_halt", nil)
	call(t, p, pluginv1.CapDebugProbe, "enable_dma", nil)
	ie := call(t, p, pluginv1.CapDebugProbe, "read_ieee", nil)
	if ie["secondary"] != "00:11:22:33:44:55:66:77" || ie["secondary_stored"] != "7766554433221100" {
		t.Fatalf("ieee %v", ie)
	}
	// An existing backup is never overwritten, and debug-locking needs its own token.
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "write_config", "90000003", map[string]any{"secondary_ieee": "01:11:22:33:44:55:66:77", "backup_path": bak, "confirm": "write-device-config"})
	if err == nil {
		t.Fatal("an existing backup must never be overwritten")
	}
	lock := "ffffffffffffffffffffffffffffff7f"
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "write_config", "90000003", map[string]any{"lock": lock, "backup_path": filepath.Join(dir, "b2"), "confirm": "write-device-config"})
	wantCode(t, err, codes.PermissionDenied)
}

func TestNativeFlashMethodsDanger(t *testing.T) {
	p, _ := nativePlugin(t, &fakeTool{})
	want := map[string]pluginv1.Danger{
		"erase": pluginv1.Danger_DANGER_CONFIRM, "erase_pages": pluginv1.Danger_DANGER_CONFIRM, "program": pluginv1.Danger_DANGER_CONFIRM,
		"write_config": pluginv1.Danger_DANGER_CONFIRM, "chip_info": pluginv1.Danger_DANGER_SAFE, "read_lock": pluginv1.Danger_DANGER_SAFE,
		"read_ieee": pluginv1.Danger_DANGER_SAFE, "verify": pluginv1.Danger_DANGER_SAFE,
	}
	seen := map[string]bool{}
	for _, s := range p.Info().Methods {
		if d, ok := want[s.Name]; ok {
			seen[s.Name] = true
			if s.Danger != d {
				t.Errorf("%s danger = %v, want %v", s.Name, s.Danger, d)
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("missing methods: %v", seen)
	}
}
