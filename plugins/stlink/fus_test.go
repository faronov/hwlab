package stlinkplugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

func wbRig(t *testing.T, cfg map[string]any) (*rig, string, *stm32.SimWB) {
	t.Helper()
	fusTiming = func(f *stm32.FUS) {
		f.Poll, f.BusyPoll = 0, time.Millisecond
		f.ReadyTimeout, f.CmdTimeout, f.OpTimeout = time.Second, time.Second, 5*time.Second
	}
	t.Cleanup(func() { fusTiming = nil })
	w := stm32.NewSimWB()
	copy(w.Flash, stm32.ParkStub)
	r := newRig(t, map[string]*cortexm.Sim{"SIMW": w.Sim})
	id := idOf("SIMW")
	if err := r.OpenDevice(context.Background(), id, cfg); err != nil {
		t.Fatal(err)
	}
	return r, id, w
}

func coproFile(t *testing.T, name string, n int, kind uint32) (string, string) {
	t.Helper()
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + 1)
	}
	binary.LittleEndian.PutUint32(b[n-8:], kind)
	binary.LittleEndian.PutUint32(b[n-4:], stm32.FUSImageMagic)
	return writeFile(t, name, b), sha(b)
}

func TestFUSInfoIsReadOnly(t *testing.T) {
	r, id, w := wbRig(t, nil)
	m := r.must(t, id, flash, "fus_info", nil)
	sec := m["security"].(map[string]any)
	if sec["secure_flash_start"] != "0x080cb000" || m["user_flash"] != "0x08000000..0x080cb000" {
		t.Fatalf("%v", m)
	}
	if w.Erases+w.Programs+w.CPU2Resets != 0 || len(w.Commands) != 0 {
		t.Fatal("fus_info touched the chip")
	}
	r.fail(t, codes.PermissionDenied, "allow_reset", id, flash, "fus_start", nil)
}

func TestFUSMethodsGates(t *testing.T) {
	r, id, w := wbRig(t, allowAll)
	fusImg, fusSHA := coproFile(t, "fus.bin", 24492, 0x200000FF)
	base := map[string]any{"path": fusImg, "image_sha256": fusSHA, "address": "0x080c3000",
		"expect_fus_version": "1.1.1", "expect_new_fus_version": "1.2.0", "confirm": tokFUSUpgrade}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	r.fail(t, codes.PermissionDenied, "confirm", id, flash, "fus_upgrade", with("confirm", "yes"))
	r.fail(t, codes.PermissionDenied, "verified full backup", id, flash, "fus_upgrade", base)
	r.fail(t, codes.FailedPrecondition, "SHA-256", id, flash, "fus_upgrade", with("image_sha256", "00"))
	r.fail(t, codes.InvalidArgument, "address", id, flash, "fus_upgrade", with("address", nil))
	r.fail(t, codes.OutOfRange, "secure start", id, flash, "fus_upgrade", with("address", "0x080C8000"))
	r.fail(t, codes.OutOfRange, "secure start", id, flash, "fus_upgrade", with("address", "0x080C3100"))
	r.fail(t, codes.PermissionDenied, "1.2.0 first", id, flash, "fus_upgrade", with("expect_new_fus_version", "2.2.0"))
	r.fail(t, codes.InvalidArgument, "not newer", id, flash, "fus_upgrade", with("expect_new_fus_version", "1.1.0"))
	r.fail(t, codes.PermissionDenied, "confirm", id, flash, "fus_delete_stack", map[string]any{"expect_stack_version": "1.10.0"})
	if w.Erases+w.Programs+w.CPU2Resets != 0 || len(w.Commands) != 0 {
		t.Fatal("a refused call touched the chip")
	}
	// A wrong expected version is caught on the chip before anything changes.
	base["backup"] = r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})["path"]
	r.fail(t, codes.FailedPrecondition, "expect_fus_version", id, flash, "fus_upgrade", with("expect_fus_version", "1.1.0"))
	r.fail(t, codes.FailedPrecondition, "expect_stack_version", id, flash, "fus_delete_stack", map[string]any{"expect_stack_version": "1.9.0", "confirm": tokStackDelete})
	if w.Erases+w.Programs != 0 || w.StackVersion == 0 {
		t.Fatal("a failed pre-check modified the chip")
	}
}

func TestFUSParkCPU1(t *testing.T) {
	r, id, w := wbRig(t, allowAll)
	copy(w.Flash, []byte{0x00, 0x10, 0x00, 0x20, 0x41, 0x01, 0x00, 0x08}) // an application
	r.fail(t, codes.FailedPrecondition, "park", id, flash, "fus_start", nil)
	r.fail(t, codes.PermissionDenied, "confirm", id, flash, "fus_park_cpu1", map[string]any{"backup": "x"})
	r.fail(t, codes.PermissionDenied, "verified full backup", id, flash, "fus_park_cpu1", map[string]any{"confirm": tokParkCPU1})
	if w.Erases+w.Programs != 0 || len(w.Commands) != 0 {
		t.Fatal("a refused call touched the chip")
	}
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})["path"]
	m := r.must(t, id, flash, "fus_park_cpu1", map[string]any{"confirm": tokParkCPU1, "backup": bk})
	if m["changed"] != true || w.Erases != 1 || !bytes.Equal(w.Flash[:len(stm32.ParkStub)], stm32.ParkStub) {
		t.Fatalf("%v", m)
	}
	if m = r.must(t, id, flash, "fus_park_cpu1", map[string]any{"confirm": tokParkCPU1, "backup": bk}); m["changed"] != false || w.Erases != 1 {
		t.Fatalf("%v", m)
	}
	r.must(t, id, flash, "fus_start", nil)
}

func TestFUSMethodsFullSequence(t *testing.T) {
	r, id, w := wbRig(t, allowAll)
	dir := t.TempDir()
	fusImg, fusSHA := coproFile(t, "fus.bin", 24492, 0x200000FF)
	macImg, macSHA := coproFile(t, "mac.bin", 74100, 0x22110045)

	r.must(t, id, flash, "fus_start", nil)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": dir})["path"]
	m := r.must(t, id, flash, "fus_upgrade", map[string]any{"path": fusImg, "image_sha256": fusSHA, "address": "0x080c3000",
		"expect_fus_version": "1.1.1", "expect_new_fus_version": "1.2.0", "confirm": tokFUSUpgrade, "backup": bk})
	if m["after"].(map[string]any)["fus_version"] != "1.2.0" || m["staged_at"] != "0x080c3000" || w.LastStagedImage != 0x080C3000+24492-4 {
		t.Fatalf("%v", m)
	}
	m = r.must(t, id, flash, "fus_delete_stack", map[string]any{"expect_stack_version": "1.10.0", "confirm": tokStackDelete})
	if m["security_after"].(map[string]any)["secure_flash_start"] != "0x080f4000" || m["after"].(map[string]any)["stack_version"] != "none" {
		t.Fatalf("%v", m)
	}
	// The old backup no longer describes the layout.
	r.fail(t, codes.FailedPrecondition, "secure", id, flash, "fus_install_stack", map[string]any{"path": macImg, "image_sha256": macSHA, "address": "0x080E1000",
		"expect_new_stack_version": "1.24.0", "confirm": tokStackInstall, "backup": bk})
	bk = r.must(t, id, flash, "backup", map[string]any{"dir": dir})["path"]
	m = r.must(t, id, flash, "fus_install_stack", map[string]any{"path": macImg, "image_sha256": macSHA, "address": "0x080E1000",
		"expect_new_stack_version": "1.24.0", "confirm": tokStackInstall, "backup": bk})
	if m["after"].(map[string]any)["stack_version"] != "1.24.0" || w.LastStagedImage != 0x080E1000+74100-4 {
		t.Fatalf("%v", m)
	}
	r.fail(t, codes.PermissionDenied, "confirm", id, flash, "fus_start_stack", nil)
	m = r.must(t, id, flash, "fus_start_stack", map[string]any{"confirm": tokStackStart, "expect_stack_version": "1.24.0"})
	if m["after"].(map[string]any)["layout"] != "wireless_stack" {
		t.Fatalf("%v", m)
	}
	if w.FlashWhileC2 != 0 {
		t.Fatal("flash was modified while CPU2 ran")
	}
	if inf := r.must(t, id, flash, "info", nil); inf["supported"] != true {
		t.Fatalf("%v", inf)
	}
}
