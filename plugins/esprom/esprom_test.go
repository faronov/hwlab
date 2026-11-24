package espromplugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/esprom/rom"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

func fast(t *testing.T) {
	t.Helper()
	a, b, c, d, e := rom.ResetHold, rom.StrapHold, rom.StrapHoldLong, rom.USJStep, rom.EraseAllowance
	rom.ResetHold, rom.StrapHold, rom.StrapHoldLong, rom.USJStep, rom.EraseAllowance = time.Millisecond, time.Millisecond, 30*time.Millisecond, time.Millisecond, 0
	t.Cleanup(func() {
		rom.ResetHold, rom.StrapHold, rom.StrapHoldLong, rom.USJStep, rom.EraseAllowance = a, b, c, d, e
	})
}

func newPlugin(t *testing.T, es ...Entry) *Plugin {
	t.Helper()
	fast(t)
	t.Setenv("HWLAB_HOME", t.TempDir())
	return NewWith(func() []Entry { return es }, defaultOpen)
}

func simEntry(id string, ch *rom.Chip, w rom.Entry, kind Kind) (Entry, *rom.Sim) {
	s := rom.NewSim(ch, w, 4<<20)
	SimFlash(s)
	s.PowerOn()
	return Entry{ID: id, Kind: kind, Entry: w, Baud: kind.defaultBaud(), Sim: s}, s
}

func call(t *testing.T, p *Plugin, dev, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	c, m, _ := strings.Cut(method, ".")
	if args == nil {
		args = map[string]any{}
	}
	st, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: c, Method: m, Args: st})
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

func open(t *testing.T, p *Plugin, dev string, cfg map[string]any) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), dev, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), dev) })
}

var writeCfg = map[string]any{"allow_flash": true, "allow_erase": true, "allow_reset": true}

// restoreCfg: program and restore need allow_flash only.
var restoreCfg = map[string]any{"allow_flash": true}

func backupOf(t *testing.T, p *Plugin, dev string) string {
	t.Helper()
	return must(t, p, dev, "FlashProgramming.backup", map[string]any{"dir": t.TempDir()})["path"].(string)
}

func TestDiscoverAliasesBridgeAndUSJ(t *testing.T) {
	ports := []usb.SerialPort{
		{TTY: "/dev/cu.SLAB_USBtoUART17", Interface: 0, VID: 0x10c4, PID: 0xea60, Serial: "ba653de3e67bf11194bff77a9507fdd7", Product: "SONOFF Dongle Max MG24", Location: "20-7.3"},
		{TTY: "/dev/cu.usbserial-14730", Interface: 0, VID: 0x10c4, PID: 0xea60, Serial: "ba653de3e67bf11194bff77a9507fdd7", Product: "SONOFF Dongle Max MG24", Location: "20-7.3"},
		{TTY: "/dev/cu.usbserial-0001", Interface: 0, VID: 0x10c4, PID: 0xea60, Serial: "0001", Location: "20-7.4.1"},
		{TTY: "/dev/cu.usbmodem7CDFA1FE67F01", Interface: 1, VID: 0x303a, PID: 0x4002, Serial: "7CDFA1FE67F0", Product: "Thread BR H2 bridge", Location: "20-7.4.3"},
		{TTY: "/dev/cu.usbmodem7CDFA1FE67F03", Interface: 3, VID: 0x303a, PID: 0x4002, Serial: "7CDFA1FE67F0", Product: "Thread BR H2 bridge", Location: "20-7.4.3"},
		{TTY: "/dev/cu.usbmodem14601", Interface: 1, VID: 0x303a, PID: 0x1001, Serial: "60:55:F9:F6:96:88", Product: "USB JTAG/serial debug unit", Location: "20-6"},
		{TTY: "/dev/cu.usbmodem147201", Interface: 1, VID: 0x303a, PID: 0x4001, Serial: "X1234567", Product: "TinyUSB CDC", Location: "20-8"},
		{TTY: "/dev/cu.usbmodemLS420BFZ1", Interface: 1, VID: 0x0451, PID: 0xbef3, Serial: "LS420BFZ", Location: "20-1.2"},
	}
	got := map[string]Entry{}
	for _, e := range discover(ports) {
		got[e.ID] = e
	}
	if len(got) != 4 {
		t.Fatalf("%v", got)
	}
	max := got["cp210x-ba653de3e67bf11194bff77a9507fdd7"]
	if max.Port != "/dev/cu.usbserial-14730" || len(max.AltPorts) != 1 || max.AltPorts[0] != "/dev/cu.SLAB_USBtoUART17" || max.Kind != KindUART || max.Entry != rom.EntryClassic || max.Baud != 921600 {
		t.Fatalf("Dongle Max: one device, the Apple node opened, the VCP alias listed: %+v", max)
	}
	if got["cp210x-loc20-7.4.1"].Port != "/dev/cu.usbserial-0001" {
		t.Fatal("generic serial: location id like tibsl")
	}
	br := got["bridge-7CDFA1FE67F0"]
	if br.Port != "/dev/cu.usbmodem7CDFA1FE67F01" || br.Kind != KindBridge || br.Console != "/dev/cu.usbmodem7CDFA1FE67F03" || br.Baud != 460800 {
		t.Fatalf("bridge: CDC 0 only, console never offered: %+v", br)
	}
	usj := got["usj-6055f9f69688"]
	if usj.Kind != KindUSJ || usj.Entry != rom.EntryUSJ || usj.Baud != 0 {
		t.Fatalf("%+v", usj)
	}
	p := newPlugin(t, usj, br)
	devs, _ := p.ListDevices(context.Background())
	if devs[0].Properties["espusbjtag_device"] != "espusbjtag/303a:1001:60_55_F9_F6_96_88" || devs[1].Properties["bridge_console_port"] == "" {
		t.Fatalf("%v %v", devs[0].Properties, devs[1].Properties)
	}
}

func TestParsePorts(t *testing.T) {
	es := parsePorts("a=/dev/ttyUSB0@460800:classic, b=/dev/ttyACM0:usj, c=/dev/x@1, d=COM3")
	if len(es) != 3 || es[0].Baud != 460800 || es[1].Kind != KindUSJ || es[1].Baud != 0 || es[2].Kind != KindUART || es[2].Baud != 921600 {
		t.Fatalf("%+v", es)
	}
}

func TestIdentifyESP32ListingAndCache(t *testing.T) {
	e, s := simEntry("cp210x-x", rom.ESP32, rom.EntryClassic, KindUART)
	e.Candidate, e.Port = true, "/dev/cu.usbserial-1"
	p := newPlugin(t, e)
	devs, _ := p.ListDevices(context.Background())
	if devs[0].Properties["chip"] != "" || len(s.LineLog) != 0 {
		t.Fatal("listing must not touch the port")
	}
	r := must(t, p, "cp210x-x", "TargetIdentify.identify", nil)
	if r["chip"] != "ESP32" || r["mac"] != "24:dc:c3:12:34:56" || r["revision"] != "v3.0" || r["package"] != "ESP32-D0WDR2-V3" ||
		r["flash_bytes"] != float64(4<<20) || r["flash_jedec"] != "c84016" || r["loader"] != "stub" || r["baud"] != float64(921600) ||
		r["lines_atomic"] != true || r["writable"] != true {
		t.Fatalf("%v", r)
	}
	if !strings.Contains(r["rom_boot_message"].(string), "DOWNLOAD_BOOT") || s.Baud() != 921600 {
		t.Fatalf("%v", r)
	}
	devs, _ = p.ListDevices(context.Background())
	if devs[0].Properties["mac"] != "24:dc:c3:12:34:56" || devs[0].Serial != "24dcc3123456" || devs[0].Properties["chip"] != "ESP32" {
		t.Fatalf("%v", devs[0].Properties)
	}
	if s.Commands[rom.CmdFlashData]+s.Commands[rom.CmdEraseRegion]+s.Commands[rom.CmdEraseFlash]+s.Commands[rom.CmdFlashBegin] != 0 {
		t.Fatalf("identify sent modifying commands: %v", s.Commands)
	}
}

func TestInfoLayoutAppsAndOTA(t *testing.T) {
	e, _ := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	r := must(t, p, "dut", "FlashProgramming.info", nil)
	prot := map[string]bool{}
	for _, x := range r["protected_ranges"].([]any) {
		prot[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"bootloader slot", "partition table", "partition nvs (data/nvs)", "partition otadata (data/ota)", "partition phy_init (data/phy)", "partition hubdata (data/0x99)"} {
		if !prot[want] {
			t.Errorf("%q must be protected: %v", want, prot)
		}
	}
	if prot["partition ota_0 (app/ota_0)"] {
		t.Error("app partitions are not protected")
	}
	apps := r["apps"].([]any)
	if a := apps[0].(map[string]any); a["project_name"] != "sim_app" || a["version"] != "1.2.3" || a["partition"] != "ota_0" || a["image_chip_id"] != "0x0000" {
		t.Fatalf("%v", a)
	}
	if apps[1].(map[string]any)["image"] == nil {
		t.Fatal("an empty slot has no image")
	}
	if r["otadata"].(map[string]any)["boot_partition"] != "ota_0" || r["bootloader"].(map[string]any)["offset"] != "0x00001000" {
		t.Fatalf("%v %v", r["otadata"], r["bootloader"])
	}
}

func TestBackupTwoReadsAndRefusal(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", nil)
	s.DropReadPacket = 40 // a lost packet is retried, not fatal
	r := must(t, p, "dut", "FlashProgramming.backup", map[string]any{"dir": t.TempDir()})
	b, _ := os.ReadFile(r["path"].(string))
	if !bytes.Equal(b, s.Flash) || r["identical_reads"] != true || r["md5"] != r["chip_md5"] || r["sha256"] != shaHex(s.Flash) {
		t.Fatalf("%v", r)
	}
	if st, _ := os.Stat(r["path"].(string)); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	var m backupManifest
	mb, _ := os.ReadFile(r["manifest"].(string))
	if err := json.Unmarshal(mb, &m); err != nil || m.MAC != "24:dc:c3:12:34:56" || len(m.Partitions) != 6 || m.Layout["otadata"] == nil {
		t.Fatalf("%+v %v", m, err)
	}
	// Flash that changes between the two reads leaves nothing behind.
	s.ChangeFlashAfter = 2*16 + 16 + 1
	dir := t.TempDir()
	_, err := call(t, p, "dut", "FlashProgramming.backup", map[string]any{"dir": dir})
	wantCode(t, err, codes.DataLoss, "disagree")
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("nothing may be stored: %v", ents)
	}
}

func TestReadAndVerify(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", nil)
	r := must(t, p, "dut", "FlashProgramming.read", map[string]any{"address": "0x8000", "length": float64(64)})
	if r["data"] != hex.EncodeToString(s.Flash[0x8000:0x8040]) {
		t.Fatalf("%v", r)
	}
	_, err := call(t, p, "dut", "FlashProgramming.read", map[string]any{"address": "0"})
	wantCode(t, err, codes.InvalidArgument, "inline")
	path := filepath.Join(t.TempDir(), "app.bin")
	must(t, p, "dut", "FlashProgramming.read", map[string]any{"address": "0x10000", "length": float64(300000), "path": path})
	b, _ := os.ReadFile(path)
	if !bytes.Equal(b, s.Flash[0x10000:0x10000+300000]) {
		t.Fatal("file read")
	}
	r = must(t, p, "dut", "FlashProgramming.verify", map[string]any{"address": "0x10000", "path": path})
	if r["match"] != true {
		t.Fatalf("%v", r)
	}
	b[0x5123] ^= 1
	_ = os.WriteFile(path, b, 0o600)
	r = must(t, p, "dut", "FlashProgramming.verify", map[string]any{"address": "0x10000", "path": path})
	if r["match"] != false || r["first_differing_sector"] != "0x00015000" {
		t.Fatalf("%v", r)
	}
}

func appImage(t *testing.T, ch *rom.Chip, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(path, SimImage(ch, "new_app", version, 20000), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProgramGates(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", nil)
	bk := backupOf(t, p, "dut")
	img := appImage(t, rom.ESP32, "2.0.0")
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"address": "0x110000", "path": img, "confirm": tokProgram, "backup": bk}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	_, err := call(t, p, "dut", "FlashProgramming.program", args(nil))
	wantCode(t, err, codes.PermissionDenied, "allow_flash")
	_ = p.CloseDevice(context.Background(), "dut")
	open(t, p, "dut", writeCfg)
	_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"confirm": "yes"}))
	wantCode(t, err, codes.PermissionDenied, tokProgram)
	_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"backup": nil}))
	wantCode(t, err, codes.PermissionDenied, "backup")
	// Protected regions: nvs, the bootloader slot, a custom data partition.
	for _, addr := range []string{"0x9000", "0x1000", "0x210000"} {
		_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"address": addr, "raw": true}))
		wantCode(t, err, codes.PermissionDenied, "confirm_protected")
	}
	// Raw bytes need raw=true; an image for another chip is refused.
	raw := filepath.Join(t.TempDir(), "raw.bin")
	_ = os.WriteFile(raw, []byte{1, 2, 3}, 0o600)
	_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"path": raw}))
	wantCode(t, err, codes.InvalidArgument, "raw=true")
	_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"path": appImage(t, rom.ESP32C6, "1")}))
	wantCode(t, err, codes.FailedPrecondition, "chip id 0x000D")
	// An image spanning two partitions is not an app image.
	_, err = call(t, p, "dut", "FlashProgramming.program", args(map[string]any{"address": "0x10F000"}))
	wantCode(t, err, codes.InvalidArgument, "one region")
	// reset=run needs allow_reset (the device has it here; a new one not).
	// A backup of another chip is refused.
	other, _ := simEntry("other", rom.ESP32, rom.EntryClassic, KindUART)
	other.Sim.Regs[rom.ESP32.EfuseBase+4] = 0x11111111
	p2 := newPlugin(t, other)
	open(t, p2, "other", writeCfg)
	_, err = call(t, p2, "other", "FlashProgramming.program", args(nil))
	wantCode(t, err, codes.FailedPrecondition, "another chip")
	if s.Commands[rom.CmdFlashBegin]+other.Sim.Commands[rom.CmdFlashBegin] != 0 || len(s.Erased) != 0 {
		t.Fatal("refused calls wrote flash")
	}
}

func TestProgramPlanPreserveAndReset(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", writeCfg)
	bk := backupOf(t, p, "dut")
	before := append([]byte(nil), s.Flash...)
	img := appImage(t, rom.ESP32, "2.0.0")
	b, _ := os.ReadFile(img)
	addr := 0x110000 + 0x123 // unaligned: edge bytes must survive
	a := map[string]any{"address": float64(addr), "path": img, "confirm": tokProgram, "backup": bk}
	r := must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": float64(addr), "path": img, "dry_run": true})
	if r["dry_run"] != true || s.Commands[rom.CmdFlashBegin] != 0 || r["sectors_changed"] == float64(0) {
		t.Fatalf("%v", r)
	}
	r = must(t, p, "dut", "FlashProgramming.program", a)
	want := append([]byte(nil), before...)
	copy(want[addr:], b)
	if r["verified"] != true || !bytes.Equal(s.Flash, want) {
		t.Fatalf("%v", r)
	}
	if s.Flash[addr-1] != before[addr-1] || s.Flash[addr+len(b)] != before[addr+len(b)] {
		t.Fatal("edge bytes changed")
	}
	// The same image again would change nothing.
	r = must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": float64(addr), "path": img, "dry_run": true})
	if r["sectors_changed"] != float64(0) {
		t.Fatalf("%v", r)
	}
	// The flash changed after the backup: it no longer protects that region.
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": float64(addr), "path": appImage(t, rom.ESP32, "3.0.0"), "confirm": tokProgram, "backup": bk})
	wantCode(t, err, codes.FailedPrecondition, "no longer matches")
	// reset=run boots the application.
	bk2 := backupOf(t, p, "dut")
	r = must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": float64(addr), "path": appImage(t, rom.ESP32, "3.0.0"), "confirm": tokProgram, "backup": bk2, "reset": "run"})
	if s.Mode() != "app" || r["reset"] == nil {
		t.Fatalf("%v %v", r, s)
	}
	// The next call enters the loader again by itself.
	if must(t, p, "dut", "TargetIdentify.identify", nil)["chip"] != "ESP32" || s.Mode() != "stub" {
		t.Fatal(s)
	}
	j, _ := os.ReadFile(p.journalPath())
	if n := strings.Count(string(j), `"op":"program"`); n != 4 || !strings.Contains(string(j), `"phase":"done"`) {
		t.Fatalf("journal: %d program records\n%s", n, j)
	}
}

func TestProgramDeferredWriteErrorIsReported(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", writeCfg)
	bk := backupOf(t, p, "dut")
	s.FailWriteAt = 0x112000
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x110000", "path": appImage(t, rom.ESP32, "2"), "confirm": tokProgram, "backup": bk})
	wantCode(t, err, codes.Aborted, "SPI flash operation failed")
	j, _ := os.ReadFile(p.journalPath())
	if !strings.Contains(string(j), `"phase":"failed"`) {
		t.Fatalf("%s", j)
	}
}

func TestEraseModesAndRestore(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", writeCfg)
	bk := backupOf(t, p, "dut")
	before := append([]byte(nil), s.Flash...)
	_, err := call(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "app", "partition": "nvs", "confirm": tokEraseApp, "backup": bk})
	wantCode(t, err, codes.InvalidArgument, "not an app partition")
	r := must(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "app", "partition": "ota_0", "confirm": tokEraseApp, "backup": bk})
	if r["blank_verified"] != true || !bytes.Equal(s.Flash[0x10000:0x110000], bytes.Repeat([]byte{0xFF}, 0x100000)) {
		t.Fatalf("%v", r)
	}
	_, err = call(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "region", "address": "0x9000", "length": float64(0x1000), "confirm": tokEraseReg, "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "confirm_protected")
	_, err = call(t, p, "dut", "FlashProgramming.erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": bk, "confirm_protected": tokProtected})
	wantCode(t, err, codes.PermissionDenied, "allow_chip_erase")
	// Restore: only the erased app sectors differ; none is protected.
	r = must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "dry_run": true})
	if strings.Contains(r["would_need"].(string), "confirm_protected") || r["sectors_changed"] == float64(0) {
		t.Fatalf("%v", r)
	}
	r = must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "confirm": tokRestore})
	if r["matches_backup"] != true || !bytes.Equal(s.Flash, before) {
		t.Fatalf("%v", r)
	}
	// Restoring an nvs that changed since needs confirm_protected.
	s.Flash[0x9010] ^= 0xFF
	_, err = call(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "confirm": tokRestore})
	wantCode(t, err, codes.PermissionDenied, "partition nvs")
	must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "confirm": tokRestore, "confirm_protected": tokProtected})
	if !bytes.Equal(s.Flash, before) {
		t.Fatal("restore")
	}
}

func TestResetWithoutROMAndConsole(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	_, err := call(t, p, "dut", "FlashProgramming.reset", nil)
	wantCode(t, err, codes.PermissionDenied, "allow_reset")
	r := must(t, p, "dut", "FlashProgramming.reset", map[string]any{"allow_reset": true, "console_ms": float64(50)})
	if s.Commands[rom.CmdSync] != 0 || s.Mode() != "app" || !strings.Contains(r["console"].(string), "SYNTHETIC application") {
		t.Fatalf("a run reset only drives the lines: %v %v", r, s.Commands)
	}
	if r["sequence"] != "DTR=0 RTS=0 -> DTR=0 RTS=1 1ms -> DTR=0 RTS=0" {
		t.Fatalf("%v", r["sequence"])
	}
	r = must(t, p, "dut", "FlashProgramming.reset", map[string]any{"allow_reset": true, "mode": "bootloader"})
	if r["chip"] != "ESP32" || s.Mode() != "rom" {
		t.Fatalf("%v %v", r, s)
	}
	_, err = call(t, p, "dut", "FlashProgramming.reset", map[string]any{"allow_reset": true, "entry": "none"})
	wantCode(t, err, codes.FailedPrecondition, "by hand")
}

func TestUSJResetReenumeratesAndIdentify(t *testing.T) {
	e, s := simEntry("usj-x", rom.ESP32S3, rom.EntryUSJ, KindUSJ)
	s.Reenumerate = true
	p := newPlugin(t, e)
	open(t, p, "usj-x", map[string]any{"allow_reset": true})
	r := must(t, p, "usj-x", "TargetIdentify.identify", nil)
	if r["chip"] != "ESP32-S3" || r["baud"] != float64(115200) || r["chip_basis"] != "GET_SECURITY_INFO chip id 9" || r["loader"] != "stub" {
		t.Fatalf("%v", r)
	}
	r = must(t, p, "usj-x", "FlashProgramming.reset", map[string]any{"console_ms": float64(30)})
	if s.Mode() != "app" || r["sequence"] != "DTR=0 RTS=0 1ms -> DTR=0 RTS=1 1ms -> DTR=0 RTS=0" {
		t.Fatalf("%v %v", r, s)
	}
	if !strings.Contains(r["console"].(string), "SYNTHETIC application") {
		t.Fatalf("console after re-enumeration: %v", r)
	}
}

func TestBridgeH2(t *testing.T) {
	e, s := simEntry("bridge-x", rom.ESP32H2, rom.EntryClassic, KindBridge)
	p := newPlugin(t, e)
	r := must(t, p, "bridge-x", "TargetIdentify.identify", nil)
	if r["chip"] != "ESP32-H2" || r["eui64"] != "74:4d:bd:ff:fe:64:1e:ce" || r["baud"] != float64(460800) || s.Baud() != 460800 || r["transport"] != "h2_bridge" {
		t.Fatalf("%v", r)
	}
}

func TestTIBSLGuardAndEncryption(t *testing.T) {
	e, s := simEntry("cp210x-loc1", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	dir := filepath.Join(hwlabHome(), "cache")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "tibsl-identity.json"), []byte(`{"cp210x-loc1":{"chip":"CC2652P","identified_utc":"2026-11-19T00:00:00Z"}}`), 0o600)
	_, err := call(t, p, "cp210x-loc1", "TargetIdentify.identify", nil)
	wantCode(t, err, codes.FailedPrecondition, "CC2652P")
	if len(s.LineLog) != 0 {
		t.Fatal("a refused call must not toggle lines")
	}
	s.Regs[rom.ESP32.EfuseBase] = 1 << 20 // FLASH_CRYPT_CNT = 1: encrypted
	r := must(t, p, "cp210x-loc1", "TargetIdentify.identify", map[string]any{"allow_other_chip": true})
	if r["flash_encrypted"] != true || r["writable"] != false {
		t.Fatalf("%v", r)
	}
}

// After a program, one partition is restored from the backup taken before
// it, without a new backup and without touching anything else.
func TestRegionRestoreAfterProgram(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	open(t, p, "dut", restoreCfg)
	bk := backupOf(t, p, "dut")
	before := append([]byte(nil), s.Flash...)
	must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x110000", "path": appImage(t, rom.ESP32, "9.9.9"), "confirm": tokProgram, "backup": bk})
	s.Flash[0x9010] ^= 0xFF // nvs changed since the backup too
	nvs := s.Flash[0x9010]
	r := must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "partition": "ota_1", "dry_run": true})
	if r["restores"] != "partition ota_1 (app/ota_1)" || strings.Contains(r["would_need"].(string), "confirm_protected") || r["sectors_changed"] == float64(0) {
		t.Fatalf("%v", r)
	}
	writes := s.Writes
	r = must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "partition": "ota_1", "confirm": tokRestore})
	if r["matches_backup"] != true || !bytes.Equal(s.Flash[0x110000:0x210000], before[0x110000:0x210000]) || s.Flash[0x9010] != nvs || s.Writes == writes {
		t.Fatalf("%v", r)
	}
	// The nvs region is protected; address+length must be sector aligned.
	_, err := call(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "partition": "nvs", "confirm": tokRestore})
	wantCode(t, err, codes.PermissionDenied, "confirm_protected")
	_, err = call(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "address": "0x9010", "length": float64(4096), "confirm": tokRestore})
	wantCode(t, err, codes.InvalidArgument, "multiples")
	_, err = call(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "partition": "nope", "confirm": tokRestore})
	wantCode(t, err, codes.NotFound, "nope")
	must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bk, "address": "0x9000", "length": float64(4096), "confirm": tokRestore, "confirm_protected": tokProtected})
	if !bytes.Equal(s.Flash, before) {
		t.Fatal("after both region restores the flash equals the backup")
	}
}

// A FLASH_DATA lost on the way restarts at that block; the program succeeds
// and the restart is reported and journaled. A device timeout never maps to
// Unavailable, which the core reports as a crashed plugin.
func TestProgramSurvivesALostBlock(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	cfg := map[string]any{"allow_flash": true, "timeout_ms": float64(100)}
	open(t, p, "dut", cfg)
	bk := backupOf(t, p, "dut")
	s.DropFlashDataAnswer = s.Commands[rom.CmdFlashData] + 2 // the second of two blocks
	img := appImage(t, rom.ESP32, "4.0.0")
	r := must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x110000", "path": img, "confirm": tokProgram, "backup": bk})
	b, _ := os.ReadFile(img)
	if r["verified"] != true || r["write_retries"] != float64(1) || !bytes.Equal(s.Flash[0x110000:0x110000+len(b)], b) {
		t.Fatalf("%v", r)
	}
	j, _ := os.ReadFile(p.journalPath())
	if !strings.Contains(string(j), `"write_retries":1`) {
		t.Fatalf("%s", j)
	}
	if c := status.Code(wrap(rom.ErrTimeout)); c != codes.DeadlineExceeded {
		t.Fatal(c)
	}
	// A chip that never answers: a precondition, not Unavailable.
	s.PowerOn()
	_, err := call(t, p, "dut", "TargetIdentify.identify", nil)
	if err == nil {
		_ = p.CloseDevice(context.Background(), "dut")
		open(t, p, "dut", map[string]any{"entry": "none", "timeout_ms": float64(100)})
		_, err = call(t, p, "dut", "TargetIdentify.identify", nil)
	}
	if status.Code(err) == codes.Unavailable || err == nil {
		t.Fatalf("%v", err)
	}
}

// After a restore rewrote the partition table, the next gate uses the new
// layout, and info shows it.
func TestLayoutIsReadAgainAfterAModification(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	// Backup B: ota_1's range is a data partition in that layout.
	tableB := append([]esp.Partition(nil), SimPartitions...)
	tableB[4] = esp.Partition{Label: "nvs2", Type: 0x01, Subtype: 0x02, Offset: 0x110000, Size: 0x100000}
	copy(s.Flash[esp.PartitionTableOffset:], esp.BuildPartitionTable(tableB))
	open(t, p, "dut", restoreCfg)
	bkB := backupOf(t, p, "dut")
	_ = p.CloseDevice(context.Background(), "dut")
	SimFlash(s) // layout A again (ota_1 is an app)
	open(t, p, "dut", restoreCfg)
	if r := must(t, p, "dut", "FlashProgramming.info", nil); !strings.Contains(fmtParts(r), "ota_1") {
		t.Fatalf("%v", r["partitions"])
	}
	must(t, p, "dut", "FlashProgramming.restore", map[string]any{"path": bkB, "confirm": tokRestore, "confirm_protected": tokProtected})
	if r := must(t, p, "dut", "FlashProgramming.info", nil); !strings.Contains(fmtParts(r), "nvs2") {
		t.Fatalf("info shows the stale table: %v", r["partitions"])
	}
	_, err := call(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x110000", "path": appImage(t, rom.ESP32, "5"), "confirm": tokProgram, "backup": bkB})
	wantCode(t, err, codes.PermissionDenied, "partition nvs2")
}

func fmtParts(r map[string]any) string {
	var l []string
	for _, x := range r["partitions"].([]any) {
		l = append(l, x.(map[string]any)["label"].(string))
	}
	return strings.Join(l, ",")
}

// FlashProgramming.reset drives the same lines as an entry: refused on a
// port where tibsl found a TI chip.
func TestResetRefusedOnATIDongle(t *testing.T) {
	e, s := simEntry("cp210x-loc1", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	dir := filepath.Join(hwlabHome(), "cache")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "tibsl-identity.json"), []byte(`{"cp210x-loc1":{"chip":"CC2652P","identified_utc":"2026-11-19T00:00:00Z"}}`), 0o600)
	for _, mode := range []string{"run", "bootloader"} {
		_, err := call(t, p, "cp210x-loc1", "FlashProgramming.reset", map[string]any{"allow_reset": true, "mode": mode})
		wantCode(t, err, codes.FailedPrecondition, "CC2652P")
	}
	if len(s.LineLog) != 0 {
		t.Fatalf("lines toggled: %v", s.LineLog)
	}
	if _, err := os.Stat(p.journalPath()); err == nil {
		t.Fatal("a refused reset must not be journaled as started")
	}
	must(t, p, "cp210x-loc1", "FlashProgramming.reset", map[string]any{"allow_reset": true, "allow_other_chip": true})
}

// A dry run writes nothing and, like identify, leaves the chip in the loader.
func TestDryRunLeavesTheChipInTheLoader(t *testing.T) {
	e, s := simEntry("dut", rom.ESP32, rom.EntryClassic, KindUART)
	p := newPlugin(t, e)
	r := must(t, p, "dut", "FlashProgramming.program", map[string]any{"address": "0x110000", "path": appImage(t, rom.ESP32, "6"), "dry_run": true})
	if s.Mode() != "stub" || r["note"] != dryNote || s.Commands[rom.CmdFlashBegin]+s.Commands[rom.CmdEraseRegion] != 0 {
		t.Fatalf("%v %v", r, s)
	}
	if _, err := os.Stat(p.journalPath()); err == nil {
		t.Fatal("a dry run is not journaled: it neither writes nor resets on its own")
	}
}

// A backup table entry that runs past the image is refused, not a panic.
func TestRestoreRangeRejectsBadPartitionEntries(t *testing.T) {
	b := make([]byte, 0x20000)
	for _, p := range []esp.Partition{
		{Label: "big", Type: 0x00, Offset: 0x10000, Size: 0x100000},
		{Label: "empty", Type: 0x01, Subtype: 0x02, Offset: 0x9000, Size: 0},
		{Label: "odd", Type: 0x01, Subtype: 0x02, Offset: 0x9100, Size: 0x1000},
	} {
		_, _, _, err := restoreRange(map[string]any{"partition": p.Label}, b, []esp.Partition{p}, nil)
		wantCode(t, err, codes.FailedPrecondition, "outside")
	}
}
