package tibslplugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/tibsl/bsl"
)

func newPlugin(t *testing.T, es ...Entry) *Plugin {
	t.Helper()
	s, h := bsl.EntryStep, bsl.EntryHold
	bsl.EntryStep, bsl.EntryHold = time.Millisecond, time.Millisecond
	t.Cleanup(func() { bsl.EntryStep, bsl.EntryHold = s, h })
	t.Setenv("HWLAB_HOME", t.TempDir())
	return NewWith(func() []Entry { return es }, defaultOpen)
}

func wiredSim() *bsl.Sim {
	s := bsl.NewSim()
	s.Wired = true
	s.PowerOn()
	return s
}

func call(t *testing.T, p *Plugin, dev, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	capName, m, _ := strings.Cut(method, ".")
	if args == nil {
		args = map[string]any{}
	}
	args["timeout_ms"] = float64(300)
	st, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: capName, Method: m, Args: st})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func must(t *testing.T, p *Plugin, dev, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := call(t, p, dev, method, args)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}

func wantCode(t *testing.T, err error, c codes.Code, sub string) {
	t.Helper()
	if status.Code(err) != c || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want %v containing %q, got %v", c, sub, err)
	}
}

func backupOf(t *testing.T, p *Plugin, dev string) string {
	t.Helper()
	r := must(t, p, dev, "FlashProgramming.backup", map[string]any{"dir": t.TempDir()})
	return r["path"].(string)
}

func TestParsePorts(t *testing.T) {
	es := parsePorts("cc=/dev/cu.usbserial-0001@115200:dtr-rts, b=/dev/ttyUSB1, bad=, c=COM3:rts-dtr, d=/dev/x@99")
	if len(es) != 3 || es[0].Port != "/dev/cu.usbserial-0001" || es[0].Baud != 115200 || es[0].Entry != bsl.EntryDTRRTS ||
		es[1].Entry != bsl.EntryManual || es[1].Baud != defaultBaud || es[2].Port != "COM3" || es[2].Entry != bsl.EntryRTSDTR {
		t.Fatalf("%+v", es)
	}
}

func TestIdentifyBackupAndReset(t *testing.T) {
	s := wiredSim()
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	r := must(t, p, "dut", "TargetIdentify.identify", nil)
	if r["chip"] != "CC2652P" || r["ieee"] != "00:12:4B:00:20:CD:1D:7D" || r["flash_bytes"] != float64(360448) || r["lines_atomic"] != true {
		t.Fatalf("%v", r)
	}
	if bd := r["bootloader"].(map[string]any); bd["summary"] != "backdoor on pin 15, active low" {
		t.Fatalf("%v", bd)
	}
	r = must(t, p, "dut", "FlashProgramming.backup", map[string]any{"dir": t.TempDir()})
	if r["sha256"] != shaHex(s.Flash) || r["identical_reads"] != true || r["chip_crc32"] != r["crc32"] {
		t.Fatalf("%v", r)
	}
	b, _ := os.ReadFile(r["path"].(string))
	if !bytes.Equal(b, s.Flash) {
		t.Fatal("backup bytes")
	}
	if st, _ := os.Stat(r["path"].(string)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	r = must(t, p, "dut", "FlashProgramming.read", map[string]any{"address": "0x100", "length": float64(16)})
	if r["data"] != hex.EncodeToString(s.Flash[0x100:0x110]) || r["crc_verified"] != true {
		t.Fatalf("%v", r)
	}
	r = must(t, p, "dut", "FlashProgramming.verify", map[string]any{"address": float64(0x100), "data": hex.EncodeToString(s.Flash[0x100:0x140])})
	if r["match"] != true {
		t.Fatalf("%v", r)
	}
	must(t, p, "dut", "FlashProgramming.reset", nil)
	if s.InBootloader() || s.Boots[len(s.Boots)-1] != "app" {
		t.Fatalf("reset must boot the application: %v", s.Boots)
	}
	if s.Commands[bsl.CmdMemoryWrite]+s.Commands[bsl.CmdSetCCFG]+s.Commands[bsl.CmdSectorErase]+s.Commands[bsl.CmdBankErase]+s.Commands[bsl.CmdSendData] != 0 {
		t.Fatalf("read-only calls sent modifying commands: %v", s.Commands)
	}
}

func TestBackupRefusesDisagreeingReads(t *testing.T) {
	s := wiredSim()
	s.FlipReadAt = 900
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	dir := t.TempDir()
	_, err := call(t, p, "dut", "FlashProgramming.backup", map[string]any{"dir": dir})
	wantCode(t, err, codes.DataLoss, "disagree")
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal("nothing may be stored")
	}
}

func TestMemoryReadWindows(t *testing.T) {
	p := newPlugin(t, Entry{ID: "dut", Sim: bsl.NewSim()})
	r := must(t, p, "dut", "MemoryAccess.read", map[string]any{"address": "0x50001318", "length": float64(4)})
	if r["data"] != "2f10b43b" || r["region"] != "FCFG1" {
		t.Fatalf("%v", r)
	}
	_, err := call(t, p, "dut", "MemoryAccess.read", map[string]any{"address": "0x40082250", "length": float64(4)})
	wantCode(t, err, codes.PermissionDenied, "allow_peripheral")
	r = must(t, p, "dut", "MemoryAccess.read", map[string]any{"address": "0x40082250", "length": float64(4), "allow_peripheral": true})
	if r["data"] != "03000000" {
		t.Fatalf("%v", r)
	}
}

func TestProgramGates(t *testing.T) {
	s := wiredSim()
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	bk := backupOf(t, p, "dut")
	img := hex.EncodeToString(bytes.Repeat([]byte{0xA5}, 64))
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x2010", "data": img, "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "program-flash")
	_, err = call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x2010", "data": img, "confirm": tokProgram})
	wantCode(t, err, codes.PermissionDenied, "backup")

	// A backup of another chip is refused.
	other := wiredSim()
	binary.LittleEndian.PutUint32(other.FCFG[bsl.RegMAC154-bsl.FCFG1Base:], 0x11111111)
	p2 := newPlugin(t, Entry{ID: "other", Sim: other, Entry: bsl.EntryDTRRTS})
	_, err = call(t, p2, "other", "FlashProgramming.program", map[string]any{"address": "0x2010", "data": img, "confirm": tokProgram, "backup": bk})
	wantCode(t, err, codes.FailedPrecondition, "another chip")
	if len(s.Erased)+len(other.Erased) != 0 {
		t.Fatal("refused calls erased flash")
	}
}

func TestProgramPreservesAndSkips(t *testing.T) {
	s := wiredSim()
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	bk := backupOf(t, p, "dut")
	before := append([]byte(nil), s.Flash...)
	data := bytes.Repeat([]byte{0x5A}, 64)
	args := func() map[string]any {
		return map[string]any{"address": "0x2010", "data": hex.EncodeToString(data), "confirm": tokProgram, "backup": bk}
	}
	r := must(t, p, "dut", "FlashProgramming.program", args())
	if r["verified"] != true || r["sectors_changed"] != float64(1) || len(s.Erased) != 1 || s.Erased[0] != 0x2000 {
		t.Fatalf("%v erased %x", r, s.Erased)
	}
	want := append([]byte(nil), before...)
	copy(want[0x2010:], data)
	if !bytes.Equal(s.Flash, want) {
		t.Fatal("bytes outside the image changed")
	}
	r = must(t, p, "dut", "FlashProgramming.program", args())
	if r["sectors_changed"] != float64(0) || len(s.Erased) != 1 {
		t.Fatalf("an identical image must not erase: %v", r)
	}
	// Restore puts the backup back, changing only that sector.
	r = must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "confirm": tokRestore, "reset_after": true})
	if r["matches_backup"] != true || r["sectors_changed"] != float64(1) || !bytes.Equal(s.Flash, before) || s.InBootloader() {
		t.Fatalf("%v", r)
	}
}

func fullImage(s *bsl.Sim) []byte {
	img := append([]byte(nil), s.Flash...)
	for i := 0x8000; i < 0x9000; i++ {
		img[i] = byte(i)
	}
	return img
}

func TestCCFGIsAProtectedRegion(t *testing.T) {
	s := wiredSim()
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	bk := backupOf(t, p, "dut")
	dir := t.TempDir()
	cfgOff := len(s.Flash) - bsl.CCFGLen
	write := func(name string, img []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, img, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// An image that changes app sectors and the CCFG (IEEE override).
	good := fullImage(s)
	copy(good[cfgOff+bsl.CCFGIEEEMAC0:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	goodPath := write("good.bin", good)
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"path": goodPath, "confirm": tokProgram, "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "confirm_config")
	if len(s.Erased) != 0 {
		t.Fatal("refused program erased flash")
	}

	// An image whose CCFG disables the backdoor is refused even with confirm_config.
	bad := fullImage(s)
	binary.LittleEndian.PutUint32(bad[cfgOff+bsl.CCFGBLConfig:], 0xC5FE0F00)
	badPath := write("bad.bin", bad)
	r := must(t, p, "dut", "FlashProgramming.program", map[string]any{"path": badPath, "dry_run": true})
	if r["would_be_refused"] == nil || len(r["ccfg_lockout_violations"].([]any)) != 1 {
		t.Fatalf("%v", r)
	}
	_, err = call(t, p, "dut", "FlashProgramming.program", map[string]any{"path": badPath, "confirm": tokProgram, "backup": bk, "confirm_config": tokCCFG})
	wantCode(t, err, codes.PermissionDenied, "lock this chip out")
	if len(s.Erased) != 0 {
		t.Fatal("refused program erased flash")
	}

	r = must(t, p, "dut", "FlashProgramming.program", map[string]any{"path": goodPath, "confirm": tokProgram, "backup": bk, "confirm_config": tokCCFG, "reset_after": true})
	if !bytes.Equal(s.Flash, good) || r["sectors_changed"] != float64(2) || s.Erased[len(s.Erased)-1] != 0x56000 {
		t.Fatalf("%v erased %x", r, s.Erased)
	}
	if s.InBootloader() {
		t.Fatal("the new image must boot")
	}
	// The bootloader is still reachable through the backdoor.
	r = must(t, p, "dut", "TargetIdentify.identify", nil)
	if r["ieee_ccfg_override"] != "08:07:06:05:04:03:02:01" {
		t.Fatalf("%v", r)
	}
	if s.Commands[bsl.CmdMemoryWrite]+s.Commands[bsl.CmdSetCCFG] != 0 {
		t.Fatal("MEMORY_WRITE/SET_CCFG must never be sent")
	}
}

func TestEraseRegionAndBank(t *testing.T) {
	s := wiredSim()
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	bk := backupOf(t, p, "dut")
	r := must(t, p, "dut", "FlashProgramming.erase", map[string]any{"address": "0x2000", "length": float64(100), "confirm": tokErase, "backup": bk})
	if r["sectors"] != float64(1) || r["blank_verified"] != true || !bsl.Blank(s.Flash[0x2000:0x4000]) {
		t.Fatalf("%v", r)
	}
	_, err := call(t, p, "dut", "FlashProgramming.erase", map[string]any{"address": "0x2001", "length": float64(4), "confirm": tokErase, "backup": bk})
	wantCode(t, err, codes.InvalidArgument, "aligned")
	_, err = call(t, p, "dut", "FlashProgramming.erase", map[string]any{"address": "0x56000", "length": float64(8192), "confirm": tokErase, "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "confirm_config")
	_, err = call(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "bank", "confirm": tokErase, "backup": bk, "confirm_config": tokCCFG})
	wantCode(t, err, codes.PermissionDenied, "bank-erase")
	_, err = call(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "bank", "confirm": tokBankErase, "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "confirm_config")
	if s.BankErases != 0 {
		t.Fatal("refused bank erase ran")
	}
	r = must(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "bank", "confirm": tokBankErase, "backup": bk, "confirm_config": tokCCFG})
	if s.BankErases != 1 || !bsl.Blank(s.Flash) || r["config_sector_erased"] != true {
		t.Fatalf("%v", r)
	}
	// A blank chip boots into the bootloader: restore brings the image back.
	r = must(t, p, "dut", "FlashProgramming.restore", map[string]any{"backup": bk, "confirm": tokRestore, "confirm_config": tokCCFG, "reset_after": true})
	if r["matches_backup"] != true || s.InBootloader() {
		t.Fatalf("%v %v", r, s.Boots)
	}
}

func TestCC2538ProgramAndCCA(t *testing.T) {
	s := bsl.NewSim2538()
	p := newPlugin(t, Entry{ID: "cc2538", Sim: s, Entry: bsl.EntryManual})
	r := must(t, p, "cc2538", "FlashProgramming.info", nil)
	if r["chip"] != "CC2538" || r["config_address"] != "0x0027FFD4" || r["sector_bytes"] != float64(2048) {
		t.Fatalf("%v", r)
	}
	bk := backupOf(t, p, "cc2538")
	data := hex.EncodeToString([]byte{1, 2, 3, 4, 5, 6})
	r = must(t, p, "cc2538", "FlashProgramming.program", map[string]any{"address": "0x200802", "data": data, "confirm": tokProgram, "backup": bk})
	if r["verified"] != true || s.Flash[0x802] != 1 || s.Flash[0x807] != 6 || s.Flash[0x801] != byte((0x801*3+1)&0xFF) {
		t.Fatalf("%v", r)
	}
	// Setting the debug lock bit in the CCA is a lockout.
	last := hex.EncodeToString([]byte{0x7F})
	_, err := call(t, p, "cc2538", "FlashProgramming.program", map[string]any{"address": "0x27FFFF", "data": last, "confirm": tokProgram, "backup": bk, "confirm_config": tokCCFG})
	wantCode(t, err, codes.PermissionDenied, "debug lock")
}

func TestUnknownPartIsReadOnly(t *testing.T) {
	s := wiredSim()
	binary.LittleEndian.PutUint32(s.FCFG[bsl.RegICEPickDeviceID-bsl.FCFG1Base:], 0x3BB9902F)
	p := newPlugin(t, Entry{ID: "dut", Sim: s, Entry: bsl.EntryDTRRTS})
	r := must(t, p, "dut", "TargetIdentify.identify", nil)
	if r["writable"] != false || !strings.Contains(r["read_only_reason"].(string), "0xBB99") {
		t.Fatalf("%v", r)
	}
	bk := backupOf(t, p, "dut")
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x2000", "data": "00000000", "confirm": tokProgram, "backup": bk})
	wantCode(t, err, codes.FailedPrecondition, "modification refused")
}
