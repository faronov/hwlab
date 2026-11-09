package jlink

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// efrBanner is what J-Link Commander V9.24a printed while connecting to a
// ZBT-1 (EFR32MG21) through a J-Link V9: none of it is a failure.
const efrBanner = `SEGGER J-Link Commander V9.24a (Compiled Mar  5 2026 10:56:17)
DLL version V9.24a, compiled Mar  5 2026 10:55:17

J-Link Commander will now exit on Error

J-Link Command File read successfully.
Processing script file...
J-Link connection not established yet but required for command.
Connecting to J-Link via USB...O.K.
Firmware: J-Link V9 compiled May  7 2021 16:26:12
Hardware version: V9.70
S/N: 683123456
VTref=3.303V
Device "EFR32MG21AXXXF1024" selected.
Connecting to target via SWD
Found SW-DP with ID 0x6BA02477
Found Cortex-M33 r0p3, Little endian.

**************************
WARNING: Cortex-M (ARMv8-M and later): The connected J-Link (S/N 683123456) uses an old firmware module that does not handle I/D-cache correctly.
**************************

Cortex-M33 identified.
`

// flashSim plays an EFR32MG21 (1 MiB main flash, 1 KiB user data page)
// behind J-Link Commander: halt state, savebin, loadbin and erase.
type flashSim struct {
	mu          sync.Mutex
	main, user  []byte
	startHalted bool
	drift       bool // the second savebin of a session sees a changed byte
	corrupt     bool // loadbin leaves one bit wrong
	failLoad    bool // loadbin reports a failure
	loads       []string
	erases      []string
}

const userBase = 0x0FE00000

func newFlashSim() *flashSim {
	s := &flashSim{main: make([]byte, 1<<20), user: bytes.Repeat([]byte{0xFF}, 0x400)}
	for i := range s.main {
		s.main[i] = byte(i*13 + i>>8)
	}
	for i := len(s.main) - 64<<10; i < len(s.main); i++ {
		s.main[i] = 0xFF
	}
	return s
}

func (s *flashSim) mem(addr, n uint64) []byte {
	switch {
	case addr+n <= uint64(len(s.main)):
		return s.main[addr : addr+n]
	case addr >= userBase && addr+n <= userBase+uint64(len(s.user)):
		return s.user[addr-userBase : addr-userBase+n]
	}
	return nil
}

func (s *flashSim) reply(script []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	halted, saves := s.startHalted, 0
	var out strings.Builder
	out.WriteString(efrBanner)
	num := func(v string) uint64 {
		n, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64)
		return n
	}
	for _, l := range script {
		fmt.Fprintf(&out, "J-Link>%s\n", l)
		f := strings.Fields(l)
		switch f[0] {
		case "IsHalted":
			if halted {
				out.WriteString("CPU is halted (PC = 0x00033948).\n")
			} else {
				out.WriteString("CPU is not halted.\n")
			}
		case "h":
			halted = true
			out.WriteString("PC = 00033948, CycleCnt = 67E45EE9\n")
		case "g":
			halted = false
			out.WriteString("Memory map 'after startup completion point' is active\n")
		case "r":
			halted = true
			out.WriteString("Reset delay: 0 ms\nReset type NORMAL: Resets core & peripherals via SYSRESETREQ & VECTRESET bit.\n")
		case "savebin":
			path, _ := strconv.Unquote(f[1])
			a, n := num(f[2]), num(f[3])
			m := s.mem(a, n)
			if m == nil {
				_ = os.WriteFile(path, nil, 0o600)
				fmt.Fprintf(&out, "Reading %d bytes from addr 0x%08X into file...Could not read memory.\n", n, a)
				return out.String()
			}
			b := append([]byte(nil), m...)
			if saves++; s.drift && saves == 2 {
				b[len(b)/2] ^= 0x40
			}
			_ = os.WriteFile(path, b, 0o600)
			fmt.Fprintf(&out, "Reading %d bytes from addr 0x%08X into file...O.K.\n", n, a)
		case "loadbin":
			path, _ := strconv.Unquote(f[1])
			a := num(f[2])
			b, _ := os.ReadFile(path)
			if s.failLoad {
				out.WriteString("Downloading file...\nError while programming flash: Programming failed.\n")
				return out.String()
			}
			copy(s.mem(a, uint64(len(b))), b)
			if s.corrupt {
				s.mem(a, 1)[0] ^= 1
			}
			s.loads = append(s.loads, fmt.Sprintf("0x%X+0x%X", a, len(b)))
			out.WriteString("Downloading file [" + path + "]...\nO.K.\n")
		case "erase":
			a, e := num(f[1]), num(f[2])
			for i, m := 0, s.mem(a, e-a+1); i < len(m); i++ {
				m[i] = 0xFF
			}
			s.erases = append(s.erases, fmt.Sprintf("0x%X..0x%X", a, e))
			out.WriteString("Erasing selected range...\nO.K.\n")
		}
	}
	return out.String()
}

const efrTarget = "EFR32MG21A020F1024IM32" // the ordering code on the chip

// efrPlugin opens the fake probe with an EFR32MG21 target and permissions.
func efrPlugin(t *testing.T, perms ...string) (*Plugin, *fakeCmd, *flashSim) {
	t.Helper()
	sim := newFlashSim()
	f := &fakeCmd{reply: sim.reply}
	p := NewWith(t.TempDir(), f)
	cfg := map[string]any{"target": efrTarget}
	for _, k := range perms {
		cfg[k] = true
	}
	if err := p.OpenDevice(context.Background(), "683123456", cfg); err != nil {
		t.Fatal(err)
	}
	return p, f, sim
}

func allowFlash(t *testing.T, p *Plugin, perms ...string) {
	t.Helper()
	cfg := map[string]any{}
	for _, k := range perms {
		cfg[k] = true
	}
	if err := p.OpenDevice(context.Background(), "683123456", cfg); err != nil {
		t.Fatal(err)
	}
}

func scripts(f *fakeCmd, from int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s []string
	for _, r := range f.runs[from:] {
		s = append(s, strings.Join(r.script, "|"))
	}
	return strings.Join(s, "\n")
}

func TestDeviceNamesResolveAgainstSeggersList(t *testing.T) {
	db, err := ParseDeviceDB([]byte(testDeviceDB))
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string][2]string{
		"EFR32MG21AXXXF1024":     {"EFR32MG21AxxxF1024", "exact"},
		"EFR32MG21A020F1024IM32": {"EFR32MG21AxxxF1024", "alias"},
		"EFR32MG21A020F1024":     {"EFR32MG21AxxxF1024", "ordering-code pattern"},
		"nrf52840_xxaa":          {"nRF52840_xxAA", "exact"},
		"ARMCM0":                 {"Cortex-M0", "alias"},
	} {
		r, err := db.Resolve(in)
		if err != nil || r.Entry.Name != want[0] || r.How != want[1] {
			t.Errorf("%s: %+v %v, want %v", in, r, err, want)
		}
	}
	for _, in := range []string{"EFR32MG21", "EFR32MG22C224F512", "FOO", ""} {
		if _, err := db.Resolve(in); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "no session was started") {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestUnknownTargetNeverStartsASession(t *testing.T) {
	// J-Link opens a modal device picker for unknown names, even with -NoGui.
	f := &fakeCmd{out: connectOut}
	p := NewWith(t.TempDir(), f)
	for _, m := range [][2]string{{"DebugProbe", "connect"}, {"DebugProbe", "halt"}, {"MemoryAccess", "read"}, {"FlashProgramming", "read"}} {
		_, err := invoke(t, p, m[0], m[1], map[string]any{"target": "EFR32MG21A020F1024X", "address": "0x0", "length": float64(4)})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s.%s: %v", m[0], m[1], err)
		}
	}
	if len(f.runs) != 0 {
		t.Errorf("no commander session may start for an unknown device: %+v", f.runs)
	}
	if f.dbCalls != 1 {
		t.Errorf("the device list is loaded once per process, got %d loads", f.dbCalls)
	}
	f.dbErr, p = fmt.Errorf("no JLinkExe"), NewWith(t.TempDir(), f)
	if _, err := invoke(t, p, "DebugProbe", "connect", map[string]any{"target": "X"}); status.Code(err) != codes.FailedPrecondition || len(f.runs) != 0 {
		t.Errorf("an unreadable device list must fail closed: %v (%d runs)", err, len(f.runs))
	}
}

func TestFlashInfoFromSeggersList(t *testing.T) {
	p, f, _ := efrPlugin(t)
	res, err := invoke(t, p, "FlashProgramming", "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["device"] != "EFR32MG21AxxxF1024" || res["target_requested"] != efrTarget || res["target_resolution"] != "alias" ||
		res["flash_base"] != "0x00000000" || res["flash_bytes"] != float64(1<<20) || res["sector_bytes"] != float64(8192) || res["core"] != "Cortex-M33" {
		t.Errorf("info %+v", res)
	}
	if banks := res["banks"].([]any); len(banks) != 2 {
		t.Errorf("banks %v", banks)
	}
	if len(f.runs) != 0 {
		t.Error("flash info needs no probe session")
	}
	// nRF5340: SEGGER lists the small UICR banks first.
	res, err = invoke(t, p, "FlashProgramming", "info", map[string]any{"target": "nRF5340_xxAA_APP"})
	if err != nil || res["flash_base"] != "0x00000000" || res["flash_bytes"] != float64(1<<20) || res["sector_bytes"] != float64(4096) {
		t.Errorf("nRF5340 %+v %v", res, err)
	}
	res, err = invoke(t, p, "FlashProgramming", "info", map[string]any{"target": "Cortex-M33"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a generic core has no flash: %+v %v", res, err)
	}
	res, err = invoke(t, p, "FlashProgramming", "info", map[string]any{"target": "Cortex-M33", "flash_base": "0x08000000", "flash_size": "0x20000", "flash_sector_bytes": float64(2048)})
	if err != nil || res["flash_base"] != "0x08000000" || res["sector_bytes"] != float64(2048) {
		t.Errorf("configured geometry %+v %v", res, err)
	}
}

func TestFlashReadHaltsAndResumes(t *testing.T) {
	p, f, sim := efrPlugin(t)
	res, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0x100", "length": float64(16)})
	if err != nil {
		t.Fatal(err)
	}
	if res["hex"] != hex.EncodeToString(sim.main[0x100:0x110]) || res["was_halted"] != false || res["resumed"] != true || res["halted_during_read"] != true {
		t.Errorf("read %+v", res)
	}
	got := scripts(f, 0)
	if !strings.HasPrefix(got, "IsHalted\nh|IsHalted|savebin ") || !strings.HasSuffix(got, " 0x100 0x10|g|IsHalted") {
		t.Errorf("scripts:\n%s", got)
	}
	if r := f.last(); r.t.Device != "EFR32MG21AxxxF1024" {
		t.Errorf("the commander must get SEGGER's name, got %q", r.t.Device)
	}

	// A core that was halted stays halted; halt=false never halts.
	sim.startHalted = true
	n := len(f.runs)
	if _, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0x0", "length": float64(4)}); err != nil {
		t.Fatal(err)
	}
	if got := scripts(f, n+1); strings.Contains(got, "h|") || strings.Contains(got, "|g") {
		t.Errorf("halted core: %s", got)
	}
	sim.startHalted = false
	n = len(f.runs)
	res, err = invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0x0", "length": float64(4), "halt": false})
	if err != nil || res["note"] == nil || strings.Contains(scripts(f, n+1), "h|") {
		t.Errorf("halt=false: %+v %v %s", res, err, scripts(f, n))
	}

	// More than 4 KiB needs a file; the file is a new one unless overwrite.
	if _, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0x0", "length": float64(8192)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("large inline read: %v", err)
	}
	out := filepath.Join(t.TempDir(), "dump.bin")
	res, err = invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0xFE00000", "length": float64(0x400), "path": out})
	if err != nil || res["hex"] != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, sim.user) || res["sha256"] != sha(sim.user) {
		t.Error("the user data page was not saved")
	}
	if _, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0xFE00000", "length": float64(0x400), "path": out}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("existing file without overwrite: %v", err)
	}
	if _, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"address": "0xFFF00", "length": float64(0x200)}); status.Code(err) != codes.OutOfRange {
		t.Errorf("read past the flash: %v", err)
	}
}

func TestFlashBackupReadsTwiceAndVerifies(t *testing.T) {
	p, f, sim := efrPlugin(t)
	dir := t.TempDir()
	res, err := invoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	want := sha(sim.main)
	path := res["path"].(string)
	if res["sha256"] != want || res["identical_reads"] != true || res["resumed"] != true ||
		filepath.Base(path) != "jlink-flash-EFR32MG21AxxxF1024-00000000-100000-"+want[:12]+".bin" {
		t.Errorf("backup %+v", res)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("backup file %v %v", st, err)
	}
	var m manifest
	mb, _ := os.ReadFile(path + ".json")
	if err := json.Unmarshal(mb, &m); err != nil || m.SHA256 != want || m.SecondRead != want || m.Device != "EFR32MG21AxxxF1024" || !m.Halted {
		t.Errorf("manifest %+v %v", m, err)
	}
	// One halted session reads the bank twice, then resumes.
	if got := scripts(f, 1); strings.Count(got, "savebin") != 2 || !strings.HasPrefix(got, "h|IsHalted|") || !strings.HasSuffix(got, "|g|IsHalted") {
		t.Errorf("backup session %s", got)
	}
	if _, err := invoke(t, p, "FlashProgramming", "verify", map[string]any{"path": path}); err != nil {
		t.Errorf("the flash must verify against its own backup: %v", err)
	}
	sim.main[0x1234] ^= 0xFF
	if _, err := invoke(t, p, "FlashProgramming", "verify", map[string]any{"path": path}); status.Code(err) != codes.DataLoss || !strings.Contains(err.Error(), "0x00001234") {
		t.Errorf("changed flash: %v", err)
	}
	sim.main[0x1234] ^= 0xFF

	// Two reads that differ never become a backup.
	sim.drift = true
	dir2 := t.TempDir()
	if _, err := invoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": dir2}); status.Code(err) != codes.DataLoss {
		t.Errorf("drifting reads: %v", err)
	}
	if ents, _ := os.ReadDir(dir2); len(ents) != 0 {
		t.Errorf("nothing may be stored after differing reads: %v", ents)
	}
}

// backupOf takes a backup through the plugin and returns its path.
func backupOf(t *testing.T, p *Plugin) string {
	t.Helper()
	res, err := invoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return res["path"].(string)
}

func writes(f *fakeCmd, from int) bool {
	s := scripts(f, from)
	return strings.Contains(s, "loadbin") || strings.Contains(s, "erase ")
}

func TestFlashModificationIsGated(t *testing.T) {
	img := filepath.Join(t.TempDir(), "fw.bin")
	// 0xFF over programmed bytes sets bits, so this image needs an erase.
	if err := os.WriteFile(img, []byte{0xFF, 0xFF, 0xFF, 0xFF}, 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, _ := efrPlugin(t)
	good := backupOf(t, p)
	stale := backupOf(t, p)
	prog := func(kv ...any) map[string]any {
		m := map[string]any{"path": img, "address": "0x2000", "confirm": "program-flash", "backup": good}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for name, tc := range map[string]struct {
		perms  []string
		method string
		args   map[string]any
		code   codes.Code
		stale  bool
	}{
		"program without allow_flash":        {nil, "program", prog(), codes.PermissionDenied, false},
		"allow_flash in call args is unused": {nil, "program", prog("allow_flash", true, "allow_erase", true), codes.PermissionDenied, false},
		"program without token":              {[]string{"allow_flash", "allow_erase"}, "program", prog("confirm", "yes"), codes.PermissionDenied, false},
		"program without backup":             {[]string{"allow_flash", "allow_erase"}, "program", prog("backup", ""), codes.PermissionDenied, false},
		"program with a stale backup":        {[]string{"allow_flash", "allow_erase"}, "program", prog(), codes.FailedPrecondition, true},
		"program needing erase":              {[]string{"allow_flash"}, "program", prog(), codes.PermissionDenied, false},
		"geometry in call args":              {[]string{"allow_flash", "allow_erase"}, "program", prog("flash_sector_bytes", float64(4096)), codes.InvalidArgument, false},
		"program .elf":                       {[]string{"allow_flash", "allow_erase"}, "program", prog("path", strings.TrimSuffix(img, ".bin")+".elf"), codes.InvalidArgument, false},
		"erase without allow_erase":          {[]string{"allow_flash"}, "erase", map[string]any{"mode": "region", "address": "0x2000", "length": float64(8192), "confirm": "erase-flash-region", "backup": good}, codes.PermissionDenied, false},
		"erase unaligned":                    {[]string{"allow_flash", "allow_erase"}, "erase", map[string]any{"mode": "region", "address": "0x2000", "length": float64(4096), "confirm": "erase-flash-region", "backup": good}, codes.InvalidArgument, false},
		"erase without mode":                 {[]string{"allow_flash", "allow_erase"}, "erase", map[string]any{"address": "0x2000", "length": float64(8192), "confirm": "erase-flash-region", "backup": good}, codes.InvalidArgument, false},
		"chip erase without allow":           {[]string{"allow_flash", "allow_erase"}, "erase", map[string]any{"mode": "chip", "confirm": "erase-entire-chip", "backup": good}, codes.PermissionDenied, false},
		"chip erase with region token":       {[]string{"allow_flash", "allow_erase", "allow_chip_erase"}, "erase", map[string]any{"mode": "chip", "confirm": "erase-flash-region", "backup": good}, codes.PermissionDenied, false},
		"restore without allow_erase":        {[]string{"allow_flash"}, "restore", map[string]any{"path": good, "confirm": "restore-flash"}, codes.PermissionDenied, false},
		"restore without token":              {[]string{"allow_flash", "allow_erase"}, "restore", map[string]any{"path": good}, codes.PermissionDenied, false},
		"restore of another device":          {[]string{"allow_flash", "allow_erase"}, "restore", map[string]any{"path": good, "confirm": "restore-flash", "target": "nRF52840_xxAA"}, codes.FailedPrecondition, false},
	} {
		p, f, sim := efrPlugin(t, tc.perms...)
		if tc.stale {
			p, f, sim = efrPlugin(t, tc.perms...)
			sim.main[0x2001] ^= 0x10 // changed after the backup was taken
			tc.args["backup"] = stale
		}
		_, err := invoke(t, p, "FlashProgramming", tc.method, tc.args)
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", name, err, tc.code)
		}
		if writes(f, 0) || len(sim.loads)+len(sim.erases) != 0 {
			t.Errorf("%s: a refused call wrote flash: %s", name, scripts(f, 0))
		}
	}
	// The refusal says how to grant the permission and why the call argument did not.
	_, err := invoke(t, p, "FlashProgramming", "program", prog("allow_flash", true))
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `"config": {"allow_flash": true}`) || !strings.Contains(msg, "--config allow_flash=true") ||
		!strings.Contains(msg, "passed as a call argument, where it is ignored") {
		t.Errorf("unhelpful refusal: %v", err)
	}
	if _, err := invoke(t, p, "FlashProgramming", "program", prog()); strings.Contains(err.Error(), "call argument, where") {
		t.Errorf("no call-argument hint without the argument: %v", err)
	}
	// A tampered backup is refused even when its manifest is intact.
	p, f, _ := efrPlugin(t, "allow_flash", "allow_erase")
	b, _ := os.ReadFile(good)
	b[0] ^= 1
	bad := filepath.Join(t.TempDir(), "bad.bin")
	mb, _ := os.ReadFile(good + ".json")
	_ = os.WriteFile(bad, b, 0o600)
	_ = os.WriteFile(bad+".json", mb, 0o600)
	if _, err := invoke(t, p, "FlashProgramming", "program", prog("backup", bad)); status.Code(err) != codes.FailedPrecondition || writes(f, 0) {
		t.Errorf("tampered backup: %v", err)
	}
}

func TestFlashProgramMergesWritesChangedSectorsAndVerifies(t *testing.T) {
	p, f, sim := efrPlugin(t, "allow_flash", "allow_erase")
	backup := backupOf(t, p)
	before := append([]byte(nil), sim.main...)
	// An Intel HEX image of 6 bytes at 0x00012345 (extended linear address).
	hexImg := ":020000040001F9\n:062345000102030405067D\n:00000001FF\n"
	img := filepath.Join(t.TempDir(), "fw.hex")
	if err := os.WriteFile(img, []byte(hexImg), 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(f.runs)
	res, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"path": img, "confirm": "program-flash", "backup": backup})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), before...)
	copy(want[0x12345:], []byte{1, 2, 3, 4, 5, 6})
	if !bytes.Equal(sim.main, want) {
		t.Errorf("flash differs from the merged image at %#x", firstDiff(want, sim.main))
	}
	if strings.Join(sim.loads, ",") != "0x12000+0x2000" || res["verified"] != true || res["sectors_written"] != float64(1) || res["address"] != "0x00012345" {
		t.Errorf("loads %v result %+v", sim.loads, res)
	}
	if got := scripts(f, n); !strings.Contains(got, "h|IsHalted|loadbin ") || !strings.HasSuffix(got, "|r|g|IsHalted") {
		t.Errorf("program scripts:\n%s", got)
	}

	// Programming the same image again changes nothing and writes nothing:
	// the sector now differs from the backup, so a fresh one is required.
	if _, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"path": img, "confirm": "program-flash", "backup": backup}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale backup after programming: %v", err)
	}
	fresh := backupOf(t, p)
	n = len(sim.loads)
	res, err = invoke(t, p, "FlashProgramming", "program", map[string]any{"path": img, "confirm": "program-flash", "backup": fresh, "reset": "none"})
	if err != nil || res["unchanged"] != true || len(sim.loads) != n {
		t.Errorf("identical image: %+v %v", res, err)
	}

	// A .bin into blank flash needs no erase permission; a bad write is
	// caught by the read-back.
	p, f, sim = efrPlugin(t, "allow_flash")
	backup = backupOf(t, p)
	bin := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(bin, []byte{0x12, 0x34}, 0o600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": bin, "address": "0xF0000", "confirm": "program-flash", "backup": backup, "run": false}
	if res, err = invoke(t, p, "FlashProgramming", "program", args); err != nil || res["reset"] != "halt" {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.HasSuffix(scripts(f, len(f.runs)-1), "|r|IsHalted") {
		t.Errorf("run=false: %s", scripts(f, len(f.runs)-1))
	}
	if !bytes.Equal(sim.main[0xF0000:0xF0003], []byte{0x12, 0x34, 0xFF}) {
		t.Errorf("flash after program: % X", sim.main[0xF0000:0xF0003])
	}
	p, _, sim = efrPlugin(t, "allow_flash")
	sim.corrupt = true
	args["backup"] = backupOf(t, p)
	if _, err := invoke(t, p, "FlashProgramming", "program", args); status.Code(err) != codes.DataLoss {
		t.Errorf("corrupted write: %v", err)
	}
	p, _, sim = efrPlugin(t, "allow_flash")
	sim.failLoad = true
	args["backup"] = backupOf(t, p)
	if _, err := invoke(t, p, "FlashProgramming", "program", args); status.Code(err) != codes.DataLoss || !strings.Contains(err.Error(), "Programming failed") {
		t.Errorf("failed loadbin: %v", err)
	}
}

func TestFlashEraseAndRestore(t *testing.T) {
	p, f, sim := efrPlugin(t, "allow_flash", "allow_erase", "allow_chip_erase")
	backup := backupOf(t, p)
	before := append([]byte(nil), sim.main...)
	n := len(f.runs)
	res, err := invoke(t, p, "FlashProgramming", "erase", map[string]any{"mode": "region", "address": "0x4000", "length": float64(0x4000), "confirm": "erase-flash-region", "backup": backup})
	if err != nil || res["verified_erased"] != true || res["sectors_erased"] != float64(2) {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(sim.erases, ",") != "0x4000..0x7FFF" || !isBlank(sim.main[0x4000:0x8000]) || !bytes.Equal(sim.main[:0x4000], before[:0x4000]) || !bytes.Equal(sim.main[0x8000:], before[0x8000:]) {
		t.Errorf("erases %v", sim.erases)
	}
	// reset defaults to none: the running core is resumed, not reset.
	if got := scripts(f, len(f.runs)-1); strings.Contains(got, "|r|") || !strings.HasSuffix(got, "|g|IsHalted") {
		t.Errorf("erase session %s (all: %s)", got, scripts(f, n))
	}
	// Erased sectors still pass the backup gate; restore writes them back.
	res, err = invoke(t, p, "FlashProgramming", "restore", map[string]any{"path": backup, "confirm": "restore-flash"})
	if err != nil || res["whole_flash_matches_backup"] != true || res["sectors_written"] != float64(2) {
		t.Fatalf("restore %+v %v", res, err)
	}
	if !bytes.Equal(sim.main, before) || sim.loads[len(sim.loads)-1] != "0x4000+0x4000" {
		t.Errorf("restore loads %v", sim.loads)
	}
	res, err = invoke(t, p, "FlashProgramming", "erase", map[string]any{"mode": "chip", "confirm": "erase-entire-chip", "backup": backup})
	if err != nil || !isBlank(sim.main) || sim.erases[len(sim.erases)-1] != "0x0..0xFFFFF" {
		t.Fatalf("chip erase %+v %v", res, err)
	}
	if !bytes.Equal(sim.user, bytes.Repeat([]byte{0xFF}, 0x400)) {
		t.Error("chip erase must leave other banks alone")
	}
}

func TestParseIntelHex(t *testing.T) {
	w, err := parseIntelHex([]byte(":020000040800F2\n:0400000001020304F2\n:0400040005060708DE\n:0400100009090909C8\n:00000001FF\n"))
	if err != nil || len(w) != 2 || w[0].addr != 0x08000000 || hex.EncodeToString(w[0].data) != "0102030405060708" || w[1].addr != 0x08000010 {
		t.Fatalf("%+v %v", w, err)
	}
	for name, in := range map[string]string{
		"checksum": ":0400000001020304F3\n:00000001FF\n",
		"no EOF":   ":0400000001020304F2\n",
		"overlap":  ":0400000001020304F2\n:0400020001020304F0\n:00000001FF\n",
		"no colon": "0400000001020304F2\n:00000001FF\n",
	} {
		if _, err := parseIntelHex([]byte(in)); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReadFailureIsReported(t *testing.T) {
	f := &fakeCmd{reply: func(script []string) string {
		out, halted := efrBanner, false
		for _, l := range script {
			out += "J-Link>" + l + "\n"
			switch {
			case l == "h":
				halted = true
			case l == "IsHalted" && halted:
				out += "CPU is halted (PC = 0x00033948).\n"
			case l == "IsHalted":
				out += "CPU is not halted.\n"
			case strings.HasPrefix(l, "savebin"):
				return out + "Reading 16 bytes from addr 0x00000000 into file...Could not read memory.\n"
			}
		}
		return out
	}}
	p := NewWith(t.TempDir(), f)
	_, err := invoke(t, p, "FlashProgramming", "read", map[string]any{"target": efrTarget, "address": "0x0", "length": float64(16)})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "Could not read memory") {
		t.Fatalf("err=%v", err)
	}
}
