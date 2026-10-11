package espusbjtag

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// opsRig is a rig whose device was opened with the given permissions, backed
// by the synthetic SPI flash.
type opsRig struct {
	*rig
	fl *esp.SimFlash
}

func newOps(t *testing.T, cfg map[string]any) *opsRig {
	t.Helper()
	sc, fl := SimConfigFlash()
	r := newRigCfg(t, sc)
	if err := r.OpenDevice(context.Background(), wantID, cfg); err != nil {
		t.Fatal(err)
	}
	r.target.SetCSR(0, 0x7B1, 0x40800000)
	return &opsRig{rig: r, fl: fl}
}

var allow = map[string]any{"allow_memory_write": true, "allow_reset": true, "allow_flash": true, "allow_erase": true}

func (r *opsRig) fail(t *testing.T, want codes.Code, sub, capName, method string, args map[string]any) {
	t.Helper()
	_, err := r.call(capName, method, args)
	if err == nil || code(err) != want || !strings.Contains(err.Error(), sub) {
		t.Fatalf("%s.%s %v: want %v containing %q, got %v", capName, method, args, want, sub, err)
	}
}

func TestMemoryWriteNeedsPermission(t *testing.T) {
	r := newOps(t, nil)
	r.fail(t, codes.PermissionDenied, "allow_memory_write", "MemoryAccess", "write", map[string]any{"address": "0x40801000", "data": "00"})
}

func TestMemoryWriteSRAMOnly(t *testing.T) {
	r := newOps(t, allow)
	for _, a := range []string{"0x40001000", "0x42000000", "0x600B0000", "0x50004000", "0x40000000", "0x4087FFFF"} {
		data := "00112233"
		if a == "0x4087FFFF" {
			data = "0011" // runs off the end of SRAM
		}
		r.fail(t, codes.InvalidArgument, "SRAM", "MemoryAccess", "write", map[string]any{"address": a, "data": data})
	}
	for _, d := range []string{"", "zz", "123"} {
		if _, err := r.call("MemoryAccess", "write", map[string]any{"address": "0x40801000", "data": d}); code(err) != codes.InvalidArgument {
			t.Fatalf("%q: %v", d, err)
		}
	}
	r.fail(t, codes.InvalidArgument, "limit", "MemoryAccess", "write", map[string]any{"address": "0x40801000", "data": strings.Repeat("ab", 4097)})
	if r.fl.Erases+r.fl.Programs != 0 {
		t.Fatal("a refused write touched flash")
	}
}

func TestMemoryWriteWidthsAndRestore(t *testing.T) {
	r := newOps(t, allow)
	const base = 0x40802000
	orig, err := r.call("MemoryAccess", "read", map[string]any{"address": "0x40802000", "length": 64.0})
	if err != nil {
		t.Fatal(err)
	}
	// 1, 2, 3, 4, 5, 7, 8 bytes at every alignment inside a word.
	pat := []byte{0xA5, 0x5A, 0xC3, 0x3C, 0x96, 0x69, 0xF0, 0x0F}
	for off := 0; off < 8; off++ {
		for n := 1; n <= 8; n++ {
			if off+n > 16 {
				continue
			}
			addr := base + off
			m := r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(addr), "data": hex.EncodeToString(pat[:n])})
			if m["verified"] != true || m["length"] != float64(n) {
				t.Fatalf("off %d n %d: %v", off, n, m)
			}
			got := r.must(t, "MemoryAccess", "read", map[string]any{"address": float64(addr), "length": float64(n)})
			if got["hex"] != hex.EncodeToString(pat[:n]) {
				t.Fatalf("off %d n %d: %v", off, n, got)
			}
		}
	}
	// A neighbour byte of a sub-word write must survive (read-modify-write).
	r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(base + 32), "data": "11223344"})
	r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(base + 33), "data": "ee"})
	if g := r.must(t, "MemoryAccess", "read", map[string]any{"address": float64(base + 32), "length": 4.0}); g["hex"] != "11ee3344" {
		t.Fatalf("%v", g)
	}
	// previous_hex lets the caller undo the write exactly.
	prev := r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(base + 40), "data": "deadbeef"})["previous_hex"].(string)
	r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(base + 40), "data": prev})
	if g := r.must(t, "MemoryAccess", "read", map[string]any{"address": float64(base + 40), "length": 4.0}); g["hex"] != prev {
		t.Fatalf("%v vs %s", g, prev)
	}
	// Restore the scratch area and compare to the original bytes.
	r.must(t, "MemoryAccess", "write", map[string]any{"address": float64(base), "data": orig["hex"]})
	if g := r.must(t, "MemoryAccess", "read", map[string]any{"address": float64(base), "length": 64.0}); g["hex"] != orig["hex"] {
		t.Fatalf("not restored: %v vs %v", g["hex"], orig["hex"])
	}
}

func TestStepRequiresHaltedHart(t *testing.T) {
	r := newOps(t, nil)
	r.fail(t, codes.FailedPrecondition, "halt it first", "DebugProbe", "step", nil)
	r.must(t, "DebugProbe", "halt", nil)
	m := r.must(t, "DebugProbe", "step", map[string]any{"count": 5.0})
	if m["count"] != float64(5) || m["cause_name"] != "step" || m["halted"] != true {
		t.Fatalf("%v", m)
	}
	for _, c := range []float64{0, -1, 1001, 1.5} {
		if _, err := r.call("DebugProbe", "step", map[string]any{"count": c}); code(err) != codes.InvalidArgument {
			t.Fatalf("count %v: %v", c, err)
		}
	}
	// The step bit must not stay set: a normal resume runs freely.
	if dcsr := r.target.CSR(0, 0x7B0); dcsr&(1<<2) != 0 {
		t.Fatalf("dcsr.step left set: %#x", dcsr)
	}
	r.must(t, "DebugProbe", "resume", nil)
}

func TestBreakpointLifecycle(t *testing.T) {
	r := newOps(t, nil)
	r.must(t, "DebugProbe", "halt", nil)
	before := [4][2]uint32{}
	for i := range before {
		before[i][0], before[i][1] = r.target.Trigger(0, i)
	}
	m := r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800100"})
	if m["installed"] != float64(1) {
		t.Fatalf("%v", m)
	}
	r.fail(t, codes.AlreadyExists, "already set", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800100"})
	r.fail(t, codes.InvalidArgument, "aligned", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800101"})
	r.fail(t, codes.InvalidArgument, "not executable", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x30000000"})
	l := r.must(t, "DebugProbe", "list_breakpoints", nil)
	if bs, _ := l["installed_by_session"].([]any); len(bs) != 1 {
		t.Fatalf("%v", l)
	}
	r.fail(t, codes.NotFound, "no breakpoint", "DebugProbe", "clear_breakpoint", map[string]any{"address": "0x40800200"})
	r.must(t, "DebugProbe", "clear_breakpoint", map[string]any{"address": "0x40800100"})
	for i := range before {
		if a, b := r.target.Trigger(0, i); a != before[i][0] || b != before[i][1] {
			t.Fatalf("trigger %d not restored: %#x %#x vs %#x %#x", i, a, b, before[i][0], before[i][1])
		}
	}
}

func TestBreakpointHitAndSlotExhaustion(t *testing.T) {
	r := newOps(t, nil)
	r.must(t, "DebugProbe", "halt", nil)
	r.target.SetCSR(0, 0x7B1, 0x40800000)
	r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800040"})
	r.must(t, "DebugProbe", "resume", nil)
	m := r.must(t, "DebugProbe", "wait_halt", map[string]any{"wait_ms": 1000.0})
	if m["halted"] != true || m["cause_name"] != "trigger" || m["pc"] != "0x40800040" {
		t.Fatalf("%v", m)
	}
	r.must(t, "DebugProbe", "clear_breakpoint", map[string]any{"all": true})
	for i := 0; i < 4; i++ {
		r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": float64(0x40800100 + 2*i)})
	}
	r.fail(t, codes.ResourceExhausted, "", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800200"})
	r.must(t, "DebugProbe", "clear_breakpoint", map[string]any{"all": true})
	// Closing the device restores triggers the session still owned.
	r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40800300"})
	if err := r.CloseDevice(context.Background(), wantID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if a, _ := r.target.Trigger(0, i); a>>28 == 2 && a&0x44 == 0x44 {
			t.Fatalf("slot %d still armed after close", i)
		}
	}
}

func TestHartsAreReportedNotAssumed(t *testing.T) {
	r := newOps(t, nil)
	m := r.must(t, "DebugProbe", "harts", nil)
	hs := m["harts"].([]any)
	if len(hs) != 2 || m["count"] != float64(2) {
		t.Fatalf("%v", m)
	}
	if s := hs[1].(map[string]any); s["State"] != "unavailable" && s["state"] != "unavailable" {
		t.Fatalf("hart 1: %v", s)
	}
	for _, h := range hs {
		if strings.Contains(strings.ToLower(hex.EncodeToString([]byte(strings.Join(keys(h.(map[string]any)), ",")))), "lp") {
			t.Fatalf("hart role guessed: %v", h)
		}
	}
	if r.target.Halts != 0 || r.target.Resumes != 0 {
		t.Fatal("harts() halted or resumed something")
	}
}

func keys(m map[string]any) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestHartSelectionDoesNotDisturbOtherHart(t *testing.T) {
	r := newOps(t, nil)
	r.must(t, "DebugProbe", "halt", map[string]any{"hart": 0.0})
	// Hart 1 is unavailable in this target: halting it must fail cleanly and
	// hart 0 must stay halted.
	if _, err := r.call("DebugProbe", "halt", map[string]any{"hart": 1.0}); err == nil {
		t.Fatal("halting an unavailable hart reported success")
	}
	if !r.target.Halted(0) {
		t.Fatal("hart 0 was disturbed")
	}
	for i := 0; i < 5; i++ {
		m := r.must(t, "DebugProbe", "read_registers", map[string]any{"hart": 0.0, "keep_halted": true})
		if m["halted"] != true {
			t.Fatalf("%v", m)
		}
		if _, err := r.call("DebugProbe", "read_registers", map[string]any{"hart": 1.0}); err == nil {
			t.Fatal("registers of an unavailable hart")
		}
	}
	if _, err := r.call("DebugProbe", "halt", map[string]any{"hart": 9.0}); err == nil {
		t.Fatalf("%v", err)
	}
	r.must(t, "DebugProbe", "resume", map[string]any{"hart": 0.0})
}

func TestResetNeedsPermissionAndObservesReset(t *testing.T) {
	r := newOps(t, nil)
	r.fail(t, codes.PermissionDenied, "allow_reset", "DebugProbe", "reset", map[string]any{"kind": "system"})
	r = newOps(t, allow)
	r.fail(t, codes.InvalidArgument, "kind", "DebugProbe", "reset", map[string]any{"kind": "bogus"})
	for _, k := range []string{"hart", "system"} {
		m := r.must(t, "DebugProbe", "reset", map[string]any{"kind": k, "halt": true})
		if m["reset_observed"] != true || m["usb_dropped"] != false || m["halted"] != true {
			t.Fatalf("%s: %v", k, m)
		}
	}
	m := r.must(t, "DebugProbe", "reset", map[string]any{"kind": "system"})
	if m["halted"] == true {
		t.Fatalf("%v", m)
	}
}

func TestSoCResetNeedsExplicitToken(t *testing.T) {
	r := newOps(t, allow)
	r.fail(t, codes.PermissionDenied, "reset-soc", "DebugProbe", "reset", map[string]any{"kind": "soc"})
	r = newOps(t, nil)
	r.fail(t, codes.PermissionDenied, "allow_reset", "DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc"})
}

func TestReconnectRebuildsSession(t *testing.T) {
	r := newOps(t, nil)
	r.must(t, "DebugProbe", "chip_info", nil)
	n := len(r.devs)
	m := r.must(t, "DebugProbe", "reconnect", nil)
	if m["reconnected"] != true || m["identity_verified"] != true {
		t.Fatalf("%v", m)
	}
	if len(r.devs) != n+1 || !r.devs[n-1].Closed() {
		t.Fatalf("old port not released / new one not opened: %d devices", len(r.devs))
	}
	if m2 := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 4.0}); m2["hex"] != "00010203" {
		t.Fatalf("%v", m2)
	}
}

func TestUnplugIsDeviceGoneThenAutoRecovers(t *testing.T) {
	r := newOps(t, nil)
	r.must(t, "DebugProbe", "chip_info", nil)
	r.devs[len(r.devs)-1].Disconnect()
	_, err := r.call("MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 4.0})
	if err == nil || !strings.Contains(err.Error(), DeviceGone) || !IsDeviceGone(err) {
		t.Fatalf("want DEVICE_GONE, got %v", err)
	}
	if code(err) != codes.Unavailable {
		t.Fatalf("%v", err)
	}
	// The same USB serial reappears (the rig still lists it): the next call
	// rebuilds the session and the target identity is checked.
	if m := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 4.0}); m["hex"] != "00010203" {
		t.Fatalf("%v", m)
	}
}

func TestDeviceGoneWhenSerialDoesNotReturn(t *testing.T) {
	r := newOps(t, map[string]any{"reconnect_timeout_ms": 300.0})
	r.must(t, "DebugProbe", "chip_info", nil)
	r.devs[len(r.devs)-1].Disconnect()
	if _, err := r.call("DebugProbe", "chip_info", nil); !IsDeviceGone(err) {
		t.Fatalf("%v", err)
	}
	r.mu.Lock()
	r.Plugin.list = func() []Entry { return nil }
	r.mu.Unlock()
	_, err := r.call("DebugProbe", "chip_info", nil)
	if err == nil || !IsDeviceGone(err) || !strings.Contains(err.Error(), "did not") {
		t.Fatalf("want a bounded DEVICE_GONE, got %v", err)
	}
}

// ---- flash ----

type flashFix struct {
	*opsRig
	dir    string
	backup string
}

func newFlash(t *testing.T) *flashFix {
	t.Helper()
	r := newOps(t, allow)
	// bootloader header, a partition table and an app image
	parts := []esp.Partition{
		{Label: "nvs", Type: 1, Subtype: 2, Offset: 0x9000, Size: 0x6000},
		{Label: "otadata", Type: 1, Subtype: 0, Offset: 0xF000, Size: 0x2000},
		{Label: "app0", Type: 0, Subtype: 0x10, Offset: 0x20000, Size: 0x10000},
		{Label: "app1", Type: 0, Subtype: 0x11, Offset: 0x30000, Size: 0x10000},
	}
	boot := esp.BuildImage(esp.ChipIDESP32C6, 0x40800000, map[uint32][]byte{0x40800000: make([]byte, 64)}, []uint32{0x40800000})
	copy(r.fl.Mem[0:], boot)
	copy(r.fl.Mem[0x8000:], esp.BuildPartitionTable(parts))
	for i := 0x9000; i < 0xF000; i++ {
		r.fl.Mem[i] = byte(i * 7)
	}
	for i := 0x20000; i < 0x20800; i++ {
		r.fl.Mem[i] = byte(i * 3)
	}
	for i := 0x30000; i < 0x30400; i++ {
		r.fl.Mem[i] = byte(i * 5)
	}
	f := &flashFix{opsRig: r, dir: t.TempDir()}
	m := r.must(t, "FlashProgramming", "backup", map[string]any{"dir": f.dir})
	if m["reads_identical"] != true {
		t.Fatalf("%v", m)
	}
	f.backup = m["backup_path"].(string)
	return f
}

func (f *flashFix) image(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appImage(fill byte) []byte {
	seg := make([]byte, 256)
	for i := range seg {
		seg[i] = fill ^ byte(i)
	}
	return esp.BuildImage(esp.ChipIDESP32C6, 0x40800000, map[uint32][]byte{0x40800000: seg}, []uint32{0x40800000})
}

func TestFlashInfoAndRead(t *testing.T) {
	f := newFlash(t)
	m := f.must(t, "FlashProgramming", "info", nil)
	fl := m["flash"].(map[string]any)
	if fl["size_bytes"] != float64(256<<10) || fl["jedec_id"] == nil {
		t.Fatalf("%v", fl)
	}
	if ps := m["partitions"].([]any); len(ps) != 4 {
		t.Fatalf("%v", m["partitions"])
	}
	for _, c := range []struct {
		a, n float64
	}{{0, 4096}, {0x9001, 777}, {0x20000, 2048}} {
		g := f.must(t, "FlashProgramming", "read", map[string]any{"address": c.a, "length": c.n})
		a := int(c.a)
		if g["hex"] != hex.EncodeToString(f.fl.Mem[a:a+int(c.n)]) {
			t.Fatalf("%v", c)
		}
	}
	for _, bad := range []map[string]any{
		{"address": float64(1 << 20), "length": 1.0}, {"address": float64(256<<10 - 1), "length": 2.0}, {"address": 0.0, "length": 0.0}, {"address": 0.0, "length": 5000.0},
	} {
		if _, err := f.call("FlashProgramming", "read", bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if f.target.Halted(0) {
		t.Fatal("a read-only flash call left the hart halted")
	}
}

func TestFlashReadToFileIsArtifact(t *testing.T) {
	f := newFlash(t)
	p := filepath.Join(f.dir, "out.bin")
	m := f.must(t, "FlashProgramming", "read", map[string]any{"address": 0.0, "length": 16384.0, "path": p})
	b, err := os.ReadFile(p)
	if err != nil || len(b) != 16384 || m["sha256"] != sum(b) || m["hex"] != nil {
		t.Fatalf("%v %v", err, m)
	}
	f.fail(t, codes.InvalidArgument, "", "FlashProgramming", "read", map[string]any{"address": 0.0, "length": 16384.0, "path": p})
}

func TestBackupIsVerifiedAndMatchesFlash(t *testing.T) {
	f := newFlash(t)
	b, err := os.ReadFile(f.backup)
	if err != nil || len(b) != 256<<10 || sum(b) != sum(f.fl.Mem) {
		t.Fatalf("%v", err)
	}
	st, _ := os.Stat(f.backup)
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("backup mode %v", st.Mode())
	}
	if _, err := os.Stat(f.backup + ".json"); err != nil {
		t.Fatal(err)
	}
}

func TestEraseGates(t *testing.T) {
	f := newFlash(t)
	reg := func(extra map[string]any) map[string]any {
		a := map[string]any{"mode": "region", "address": float64(0x20000), "length": 4096.0, "confirm": "erase-flash-region", "backup": f.backup}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	f.fail(t, codes.InvalidArgument, "no implicit erase", "FlashProgramming", "erase", map[string]any{})
	f.fail(t, codes.PermissionDenied, "confirm", "FlashProgramming", "erase", reg(map[string]any{"confirm": ""}))
	f.fail(t, codes.PermissionDenied, "backup", "FlashProgramming", "erase", reg(map[string]any{"backup": ""}))
	f.fail(t, codes.PermissionDenied, "allow_chip_erase", "FlashProgramming", "erase", map[string]any{"mode": "chip", "confirm": "erase-entire-chip", "backup": f.backup})
	f.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "erase", reg(map[string]any{"address": 0.0}))
	f.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "erase", reg(map[string]any{"address": float64(0x8000)}))
	f.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "erase", reg(map[string]any{"address": float64(0x9000)}))
	f.fail(t, codes.InvalidArgument, "multiples", "FlashProgramming", "erase", reg(map[string]any{"address": float64(0x20001)}))
	f.fail(t, codes.OutOfRange, "", "FlashProgramming", "erase", reg(map[string]any{"address": float64(1 << 20)}))
	f.fail(t, codes.NotFound, "partition", "FlashProgramming", "erase", map[string]any{"mode": "app", "partition": "nope", "confirm": "erase-app-partition", "backup": f.backup})
	f.fail(t, codes.InvalidArgument, "not an app", "FlashProgramming", "erase", map[string]any{"mode": "app", "partition": "nvs", "confirm": "erase-app-partition", "backup": f.backup})
	if f.fl.Erases != 0 {
		t.Fatalf("a refused erase erased %d sectors", f.fl.Erases)
	}
	// A wrong token is refused even with every other protection satisfied.
	f.fail(t, codes.PermissionDenied, "confirm", "FlashProgramming", "erase", reg(map[string]any{"confirm": "erase-entire-chip"}))
	// otadata is the one data partition that is not protected.
	m := f.must(t, "FlashProgramming", "erase", reg(map[string]any{"address": float64(0xF000)}))
	if m["verified_erased"] != true || !allFF(f.fl.Mem[0xF000:0x10000]) {
		t.Fatalf("%v", m)
	}
	if f.fl.Mem[0x9000] == 0xFF {
		t.Fatal("unrelated region touched")
	}
	m = f.must(t, "FlashProgramming", "erase", map[string]any{"mode": "app", "partition": "app1", "confirm": "erase-app-partition", "backup": f.backup})
	if !allFF(f.fl.Mem[0x30000:0x40000]) || m["partition"] != "app1" {
		t.Fatalf("%v", m)
	}
}

func allFF(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return false
		}
	}
	return true
}

func TestChipEraseNeedsStrongerPermission(t *testing.T) {
	f := newFlash(t)
	if err := f.OpenDevice(context.Background(), wantID, map[string]any{"allow_flash": true, "allow_erase": true, "allow_reset": true}); err != nil {
		t.Fatal(err)
	}
	f.fail(t, codes.PermissionDenied, "allow_chip_erase", "FlashProgramming", "erase", map[string]any{"mode": "chip", "confirm": "erase-entire-chip", "backup": f.backup, "confirm_protected": "modify-protected-region"})
	if f.fl.Erases != 0 {
		t.Fatal("chip erase ran")
	}
}

func TestEraseBackupMustMatchFlash(t *testing.T) {
	f := newFlash(t)
	f.fl.Mem[0x20010] ^= 0xFF // the flash changed after the backup
	f.fail(t, codes.FailedPrecondition, "no longer matches", "FlashProgramming", "erase",
		map[string]any{"mode": "region", "address": float64(0x20000), "length": 4096.0, "confirm": "erase-flash-region", "backup": f.backup})
	// A "backup" without manifest or from another flash is refused too.
	raw := f.image(t, "raw.bin", append([]byte(nil), f.fl.Mem...))
	f.fail(t, codes.FailedPrecondition, "manifest", "FlashProgramming", "erase",
		map[string]any{"mode": "region", "address": float64(0x20000), "length": 4096.0, "confirm": "erase-flash-region", "backup": raw})
	if f.fl.Erases != 0 {
		t.Fatal("erased")
	}
}

// Regression: raw zero bytes written across the bootloader, the partition
// table and a partition destroyed the bootloader on the first real run, once
// the write path really worked. Even with every token supplied, multi-region
// and unflagged raw writes must be refused before anything is erased.
func TestProgramRefusesRawAndMultiRegion(t *testing.T) {
	f := newFlash(t)
	raw := f.image(t, "zeros.bin", make([]byte, 0x9000))
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"path": raw, "address": float64(0x1000), "confirm": "program-flash", "confirm_protected": "modify-protected-region", "backup": f.backup, "reset": "none"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	f.fail(t, codes.InvalidArgument, "raw=true", "FlashProgramming", "program", args(nil))
	f.fail(t, codes.InvalidArgument, "spans", "FlashProgramming", "program", args(map[string]any{"raw": true}))
	if f.fl.Erases+f.fl.Programs != 0 {
		t.Fatal("refused programs touched flash")
	}
}

func TestProgramVerifyCycle(t *testing.T) {
	f := newFlash(t)
	img := appImage(0x11)
	p := f.image(t, "app.bin", img)
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	f.fail(t, codes.PermissionDenied, "confirm", "FlashProgramming", "program", args(map[string]any{"confirm": "nope"}))
	f.fail(t, codes.PermissionDenied, "backup", "FlashProgramming", "program", args(map[string]any{"backup": ""}))
	f.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "program", args(map[string]any{"address": 0.0}))
	f.fail(t, codes.InvalidArgument, "spans", "FlashProgramming", "program", args(map[string]any{"address": float64(0x2FF00)}))
	f.fail(t, codes.OutOfRange, "", "FlashProgramming", "program", args(map[string]any{"address": float64(256<<10 - 8)}))
	if f.fl.Erases+f.fl.Programs != 0 {
		t.Fatal("refused programs touched flash")
	}
	otherApp := append([]byte(nil), f.fl.Mem[0x30000:0x30400]...)
	m := f.must(t, "FlashProgramming", "program", args(nil))
	if m["verified"] != true || m["image_validated"] != true {
		t.Fatalf("%v", m)
	}
	if string(f.fl.Mem[0x20000:0x20000+len(img)]) != string(img) {
		t.Fatal("image not in flash")
	}
	if string(f.fl.Mem[0x30000:0x30400]) != string(otherApp) {
		t.Fatal("an unrelated partition changed")
	}
	f.must(t, "FlashProgramming", "verify", map[string]any{"path": p, "address": float64(0x20000)})
	// intentionally corrupted: verify must fail, never succeed
	f.fl.Mem[0x20000+10] ^= 1
	f.fail(t, codes.DataLoss, "differs", "FlashProgramming", "verify", map[string]any{"path": p, "address": float64(0x20000)})
}

func TestProgramRefusesBadImages(t *testing.T) {
	f := newFlash(t)
	good := appImage(0x22)
	cases := map[string][]byte{
		"truncated.bin": good[:len(good)-20],
		"corrupt.bin":   func() []byte { b := append([]byte(nil), good...); b[40] ^= 0xFF; return b }(),
		"wrongchip.bin": esp.BuildImage(0x0005, 0x40800000, map[uint32][]byte{0x40800000: make([]byte, 64)}, []uint32{0x40800000}),
	}
	for name, data := range cases {
		p := f.image(t, name, data)
		f.fail(t, codes.InvalidArgument, "", "FlashProgramming", "program",
			map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	}
	f.fail(t, codes.InvalidArgument, "", "FlashProgramming", "program", map[string]any{"path": filepath.Join(f.dir, "missing.bin"), "address": 131072.0, "confirm": "program-flash", "backup": f.backup})
	empty := f.image(t, "empty.bin", nil)
	f.fail(t, codes.InvalidArgument, "", "FlashProgramming", "program", map[string]any{"path": empty, "address": 131072.0, "confirm": "program-flash", "backup": f.backup})
	if f.fl.Erases+f.fl.Programs != 0 {
		t.Fatal("a refused image touched flash")
	}
}

func TestProgramDetectsHardwareCorruption(t *testing.T) {
	f := newFlash(t)
	f.fl.CorruptProgram = true
	p := f.image(t, "app.bin", appImage(0x33))
	_, err := f.call("FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	if err == nil {
		t.Fatal("a corrupting flash reported success")
	}
}

func TestProgramFlashWriteProtected(t *testing.T) {
	f := newFlash(t)
	f.fl.Protected = true
	p := f.image(t, "app.bin", appImage(0x44))
	if _, err := f.call("FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"}); err == nil {
		t.Fatal("write-protected flash reported success")
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	f := newFlash(t)
	orig := sum(f.fl.Mem)
	p := f.image(t, "app.bin", appImage(0x55))
	f.must(t, "FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	if sum(f.fl.Mem) == orig {
		t.Fatal("program changed nothing")
	}
	f.fail(t, codes.PermissionDenied, "confirm", "FlashProgramming", "restore", map[string]any{"path": f.backup, "reset": "none"})
	m := f.must(t, "FlashProgramming", "restore", map[string]any{"path": f.backup, "confirm": "restore-flash", "confirm_protected": "modify-protected-region", "reset": "none"})
	if m["verified"] != true || sum(f.fl.Mem) != orig || m["flash_sha256_after"] != orig {
		t.Fatalf("%v", m)
	}
}

func TestRestoreRefusesProtectedWithoutToken(t *testing.T) {
	f := newFlash(t)
	f.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "restore", map[string]any{"path": f.backup, "confirm": "restore-flash", "reset": "none"})
}

func TestFlashNeedsPermissions(t *testing.T) {
	sc, fl := SimConfigFlash()
	_ = fl
	r := newRigCfg(t, sc)
	if err := r.OpenDevice(context.Background(), wantID, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		m, sub string
		a      map[string]any
	}{
		{"erase", "allow_flash", map[string]any{"mode": "region", "address": 131072.0, "length": 4096.0, "confirm": "erase-flash-region"}},
		{"program", "allow_flash", map[string]any{"path": "/nonexistent", "address": 0.0}},
	} {
		_, err := r.call("FlashProgramming", c.m, c.a)
		if err == nil {
			t.Fatalf("%s allowed", c.m)
		}
		if code(err) == codes.Unimplemented {
			t.Fatalf("%s not declared: %v", c.m, err)
		}
	}
	if fl.Erases+fl.Programs != 0 {
		t.Fatal("touched")
	}
}

func TestZeroLengthAndOverlapErrors(t *testing.T) {
	f := newFlash(t)
	f.fail(t, codes.InvalidArgument, "length", "FlashProgramming", "erase", map[string]any{"mode": "region", "address": 131072.0, "length": 0.0, "confirm": "erase-flash-region", "backup": f.backup})
	f.fail(t, codes.InvalidArgument, "", "FlashProgramming", "read", map[string]any{"address": 0.0})
}
