package stm32dfuplugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/stm32dfu/sim"
)

const (
	dbg   = "DebugProbe"
	mem   = "MemoryAccess"
	flash = "FlashProgramming"
	ident = "TargetIdentify"
)

var allowAll = map[string]any{"allow_flash": true, "allow_erase": true, "allow_reset": true}

type rig struct {
	*Plugin
	devs []*sim.Device
}

func newRig(t *testing.T, cfgs ...sim.Config) *rig {
	t.Helper()
	t.Setenv("HWLAB_HOME", t.TempDir())
	r := &rig{}
	for _, c := range cfgs {
		r.devs = append(r.devs, sim.New(c))
	}
	list := func() []Entry {
		var es []Entry
		for i, d := range r.devs {
			if !d.Gone() {
				u := d.USBDevice()
				u.SysName = fmt.Sprintf("%s-%d", u.SysName, i)
				es = append(es, Entry{Dev: u, Serial: d.Serial(), Sim: d})
			}
		}
		return nameEntries(es)
	}
	r.Plugin = NewWith(list, defaultOpen)
	t.Cleanup(func() {
		for _, d := range r.devs {
			_ = r.CloseDevice(context.Background(), d.Serial())
			if n := d.UnsafeCount(); n != 0 {
				t.Errorf("%s: %d unsafe command(s): %v", d.Serial(), n, d.Log)
			}
		}
	})
	return r
}

// single opens one simulated device with cfg.
func single(t *testing.T, c sim.Config, cfg map[string]any) (*rig, string, *sim.Device) {
	t.Helper()
	if c.Serial == "" {
		c.Serial = "SIMDFU0001"
	}
	if c.FUSVersion == 0 {
		c.FUSVersion = sim.FUS053
	}
	r := newRig(t, c)
	if err := r.OpenDevice(context.Background(), c.Serial, cfg); err != nil {
		t.Fatal(err)
	}
	return r, c.Serial, r.devs[0]
}

func (r *rig) call(id, capName, method string, args map[string]any) (map[string]any, error) {
	st, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	resp, err := r.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return resp.GetResult().AsMap(), nil
}

func (r *rig) must(t *testing.T, id, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	m, err := r.call(id, capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s %v: %v", capName, method, args, err)
	}
	return m
}

func (r *rig) fail(t *testing.T, want codes.Code, sub, id, capName, method string, args map[string]any) {
	t.Helper()
	_, err := r.call(id, capName, method, args)
	if err == nil || status.Code(err) != want || !strings.Contains(err.Error(), sub) {
		t.Fatalf("%s.%s %v: want %v containing %q, got %v", capName, method, args, want, sub, err)
	}
}

func writeFile(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.bin")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListAndNaming(t *testing.T) {
	r := newRig(t, sim.Config{Serial: "205F31A8", SFSA: 0xCB}, sim.Config{Serial: "DUP", SFSA: 0xCB}, sim.Config{Serial: "DUP", SFSA: 0xF4})
	ds, err := r.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range ds {
		ids = append(ids, d.GetId())
	}
	if len(ids) != 3 || ids[0] != "205F31A8" || !strings.HasPrefix(ids[1], "DUP@") || !strings.HasPrefix(ids[2], "DUP@") || ids[1] == ids[2] {
		t.Fatalf("ids %v", ids)
	}
}

func TestIdentifyAndBoundaryPerSFSA(t *testing.T) {
	for _, sfsa := range SimSFSAs {
		t.Run(fmt.Sprintf("0x%02x", sfsa), func(t *testing.T) {
			r, id, _ := single(t, sim.Config{SFSA: sfsa, Xfer: 2048}, nil)
			info := r.must(t, id, dbg, "info", nil)
			if f, _ := info["dfu_functional"].(map[string]any); f["transfer_size"] != float64(2048) || info["bootloader_version"] == nil {
				t.Fatalf("info %v", info)
			}
			idn := r.must(t, id, ident, "identify", nil)
			if idn["chip"] != "STM32WB5x" || idn["dev_id"] != "0x495" || idn["sfsa"] != fmt.Sprintf("0x%02x", sfsa) {
				t.Fatalf("identify %v", idn)
			}
			fi := r.must(t, id, flash, "info", nil)
			secure := 0x08000000 + uint32(sfsa)*4096
			if fi["secure_base"] != fmt.Sprintf("0x%08x", secure) || fi["user_flash_bytes"] != float64(int(sfsa)*4096) || fi["modification_allowed"] != true ||
				fi["write_window"] != fmt.Sprintf("[0x08000000, 0x%08x)", secure) {
				t.Fatalf("flash info %v", fi)
			}
			// Reads stop at the secure start.
			r.fail(t, codes.OutOfRange, "secure", id, flash, "read", map[string]any{"address": fmt.Sprintf("0x%x", secure-4), "length": 8})
			r.fail(t, codes.OutOfRange, "secure", id, mem, "read", map[string]any{"address": fmt.Sprintf("0x%x", secure), "length": 4})
			r.must(t, id, mem, "read", map[string]any{"address": fmt.Sprintf("0x%x", secure-4), "length": 4})
		})
	}
}

func TestMemoryRead(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, nil)
	m := r.must(t, id, mem, "read", map[string]any{"address": "0x1FFF8070", "length": 8})
	want := hex.EncodeToString(d.OptionBytes()[0x70:0x78])
	if m["hex"] != want {
		t.Fatalf("option bytes %v want %s", m, want)
	}
	r.fail(t, codes.InvalidArgument, "", id, mem, "read", map[string]any{"address": "0x20000000", "length": 4097})
	r.fail(t, codes.InvalidArgument, "allow_peripheral", id, mem, "read", map[string]any{"address": "0xE0042000", "length": 4})
	r.must(t, id, mem, "read", map[string]any{"address": "0xE0042000", "length": 4, "allow_peripheral": true})
	r.fail(t, codes.InvalidArgument, "alias", id, mem, "read", map[string]any{"address": 0, "length": 4})
	p := filepath.Join(t.TempDir(), "sram.bin")
	m = r.must(t, id, mem, "read", map[string]any{"address": "0x20000000", "length": 65536, "path": p})
	if b, err := os.ReadFile(p); err != nil || len(b) != 65536 || m["hex"] != nil {
		t.Fatalf("path read %v %v", m, err)
	}
}

func TestFUSInfo(t *testing.T) {
	for _, v := range []struct {
		v        uint32
		fus, hex string
	}{{sim.FUS053, "0.5.3", "0x00050300"}, {sim.FUS120, "1.2.0", "0x01020000"}} {
		r, id, _ := single(t, sim.Config{SFSA: 0xCB, FUSVersion: v.v}, nil)
		m := r.must(t, id, flash, "fus_info", nil)
		if m["word_0x20030030"] != v.hex || m["fus_version"] != v.fus || m["stack_version"] != "1.0.0" || m["sfsa"] != "0xcb" {
			t.Fatalf("%v", m)
		}
		if di := m["device_info"].(map[string]any); di["layout"] != "fus" {
			t.Fatalf("%v", di)
		}
	}
	_ = r0(t)
}

// r0 checks an empty SRAM2a decodes as no table.
func r0(t *testing.T) bool {
	t.Helper()
	r := newRig(t, sim.Config{Serial: "EMPTY", SFSA: 0xCB})
	m := r.must(t, "EMPTY", flash, "fus_info", nil)
	if di := m["device_info"].(map[string]any); di["layout"] != "none" {
		t.Fatalf("%v", m)
	}
	return true
}

func TestAmbiguousBoundaryIsReadOnly(t *testing.T) {
	cases := map[string]sim.Config{
		"bad complement":     {SFSA: 0xCB, BadOBComplement: true},
		"no option bytes":    {SFSA: 0xCB, NoOBAlt: true},
		"flash alt mismatch": {SFSA: 0xCB, FlashAltPages: 200},
		"sfsa zero":          {SFSA: 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, id, d := single(t, c, allowAll)
			fi := r.must(t, id, flash, "info", nil)
			if fi["modification_allowed"] != false || fi["write_window"] != "closed" || fi["read_only_reason"] == nil {
				t.Fatalf("%v", fi)
			}
			img := writeFile(t, make([]byte, 8))
			r.fail(t, codes.FailedPrecondition, "secure", id, flash, "program", map[string]any{"path": img, "address": "0x08002000", "confirm": "program-flash", "backup": "x"})
			r.fail(t, codes.FailedPrecondition, "secure", id, flash, "erase", map[string]any{"mode": "region", "address": "0x08002000", "length": 4096, "confirm": "erase-flash-region", "backup": "x"})
			r.fail(t, codes.FailedPrecondition, "", id, flash, "backup", map[string]any{"dir": t.TempDir()})
			r.fail(t, codes.FailedPrecondition, "unknown", id, mem, "read", map[string]any{"address": "0x08000000", "length": 4})
			if d.Erases != 0 || d.Programs != 0 {
				t.Fatal("modified")
			}
		})
	}
}

func TestInferredChipWithoutDBGMCU(t *testing.T) {
	r, id, _ := single(t, sim.Config{SFSA: 0xE1, NoDBGMCU: true}, nil)
	m := r.must(t, id, ident, "identify", nil)
	if m["chip"] != "STM32WB5x" || m["sfsa"] != "0xe1" {
		t.Fatalf("%v", m)
	}
}

func backup(t *testing.T, r *rig, id string) string {
	t.Helper()
	m := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	return m["path"].(string)
}

func TestBackupManifest(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xF4}, nil)
	p := backup(t, r, id)
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(b, d.Flash()[:0xF4*4096]) {
		t.Fatalf("backup content: %v", err)
	}
	mb, _ := os.ReadFile(p + ".json")
	var m map[string]any
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if m["sfsa"] != "0xf4" || m["secure_base"] != "0x080f4000" || !strings.Contains(m["warning"].(string), "Zigbee") || m["sha256"] != m["second_read_sha256"] {
		t.Fatalf("manifest %v", m)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestEraseGates(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, map[string]any{"allow_flash": true})
	bk := backup(t, r, id)
	args := func(kv ...any) map[string]any {
		m := map[string]any{"mode": "region", "address": "0x08001000", "length": 4096, "confirm": "erase-flash-region", "backup": bk}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	r.fail(t, codes.PermissionDenied, "allow_erase", id, flash, "erase", args())
	r.must(t, id, dbg, "info", nil)
	_ = r.CloseDevice(context.Background(), id)
	if err := r.OpenDevice(context.Background(), id, allowAll); err != nil {
		t.Fatal(err)
	}
	r.fail(t, codes.PermissionDenied, "", id, flash, "erase", args("confirm", "yes"))
	r.fail(t, codes.PermissionDenied, "mass erase", id, flash, "erase", args("mode", "chip"))
	r.fail(t, codes.PermissionDenied, "backup", id, flash, "erase", args("backup", ""))
	r.fail(t, codes.InvalidArgument, "page", id, flash, "erase", args("address", "0x08001004"))
	r.fail(t, codes.OutOfRange, "secure", id, flash, "erase", args("address", "0x080CA000", "length", 8192))
	r.fail(t, codes.OutOfRange, "", id, flash, "erase", args("address", "0x07FFF000"))
	// A backup whose page no longer matches the flash.
	d.SetFlash(0x08001000, []byte{1, 2, 3})
	r.fail(t, codes.FailedPrecondition, "no longer matches", id, flash, "erase", args())
	// A backup taken under another SFSA.
	r2, id2, _ := single(t, sim.Config{Serial: "OTHER", SFSA: 0xF4}, allowAll)
	other := backup(t, r2, id2)
	r.fail(t, codes.FailedPrecondition, "secure boundary", id, flash, "erase", args("backup", other, "address", "0x08000000"))
	// A tampered backup.
	b, _ := os.ReadFile(other)
	b[0] ^= 1
	_ = os.WriteFile(other, b, 0o600)
	r2.fail(t, codes.FailedPrecondition, "SHA-256", id2, flash, "erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 4096, "confirm": "erase-flash-region", "backup": other})
	if d.Erases != 0 {
		t.Fatal("erased")
	}
	bk = backup(t, r, id)
	m := r.must(t, id, flash, "erase", args("length", 8192))
	if len(m["erased_pages"].([]any)) != 2 || d.Erases != 2 || !isBlank(d.Flash()[0x1000:0x3000]) || d.Flash()[0x3000] == 0xFF && d.Flash()[0x3001] == 0xFF {
		t.Fatalf("%v", m)
	}
}

func TestProgramVerifyRestore(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xE1}, map[string]any{"allow_flash": true})
	bk := backup(t, r, id)
	orig := d.Flash()
	// Page 2 (0x08002000) is erased: programming it needs no erase.
	img := make([]byte, 1500)
	for i := range img {
		img[i] = byte(i*5 + 1)
	}
	p := writeFile(t, img)
	prog := func(path, addr string) map[string]any {
		return map[string]any{"path": path, "address": addr, "confirm": "program-flash", "backup": bk}
	}
	r.fail(t, codes.PermissionDenied, "", id, flash, "program", map[string]any{"path": p, "address": "0x08002000", "confirm": "flash", "backup": bk})
	m := r.must(t, id, flash, "program", prog(p, "0x08002000"))
	if m["pages_erased"] != float64(0) || m["verified"] != true || !bytes.Equal(d.Flash()[0x2000:0x2000+1500], img) {
		t.Fatalf("%v", m)
	}
	r.must(t, id, flash, "verify", map[string]any{"path": p, "address": "0x08002000"})
	r.fail(t, codes.DataLoss, "differs", id, flash, "verify", map[string]any{"path": p, "address": "0x08000000"})
	// Page 0 holds data: changing it needs an erase, hence allow_erase.
	r.fail(t, codes.PermissionDenied, "allow_erase", id, flash, "program", prog(p, "0x08000000"))
	_ = r.CloseDevice(context.Background(), id)
	if err := r.OpenDevice(context.Background(), id, allowAll); err != nil {
		t.Fatal(err)
	}
	before := d.Flash()
	m = r.must(t, id, flash, "program", prog(p, "0x08000100"))
	after := d.Flash()
	if m["pages_erased"] != float64(1) || !bytes.Equal(after[0x100:0x100+1500], img) || !bytes.Equal(after[:0x100], before[:0x100]) || !bytes.Equal(after[0x100+1500:0x1000], before[0x100+1500:0x1000]) {
		t.Fatalf("program with erase: %v", m)
	}
	// Images reaching the secure area are refused.
	big := writeFile(t, make([]byte, 8192))
	r.fail(t, codes.OutOfRange, "secure", id, flash, "program", prog(big, "0x080E0000"))
	// Failures are reported as data loss.
	d.FailProgram = true
	img2 := writeFile(t, []byte{9, 9, 9, 9, 9, 9, 9, 9})
	r.fail(t, codes.DataLoss, "", id, flash, "program", prog(img2, "0x08005000"))
	d.FailProgram = false
	d.CorruptProgram = true
	r.fail(t, codes.DataLoss, "", id, flash, "program", prog(img2, "0x08008000"))
	d.CorruptProgram = false
	// Restore brings the whole user area back.
	r.fail(t, codes.PermissionDenied, "", id, flash, "restore", map[string]any{"path": bk, "confirm": "program-flash"})
	m = r.must(t, id, flash, "restore", map[string]any{"path": bk, "confirm": "restore-flash"})
	if m["user_flash_matches_backup"] != true || !bytes.Equal(d.Flash()[:0xE1*4096], orig[:0xE1*4096]) || !strings.Contains(m["warning"].(string), "Zigbee") {
		t.Fatalf("restore %v", m)
	}
	// The secure area was never written.
	if !bytes.Equal(d.Flash()[0xE1*4096:], orig[0xE1*4096:]) {
		t.Fatal("secure area changed")
	}
}

// vec is a two-word vector table (initial SP, reset vector).
func vec(sp, pc uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, sp)
	binary.LittleEndian.PutUint32(b[4:], pc)
	return b
}

func TestLeave(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, nil)
	d.SetFlash(0x08000000, vec(0x20030000, 0x08000101))
	r.fail(t, codes.PermissionDenied, "allow_reset", id, flash, "leave", map[string]any{"confirm": "leave-dfu"})
	_ = r.CloseDevice(context.Background(), id)
	if err := r.OpenDevice(context.Background(), id, allowAll); err != nil {
		t.Fatal(err)
	}
	r.fail(t, codes.PermissionDenied, "", id, flash, "leave", map[string]any{"confirm": "reset"})
	m := r.must(t, id, flash, "leave", map[string]any{"confirm": "leave-dfu"})
	if m["left_dfu"] != true || !d.Gone() || d.Leaves != 1 || d.Erases != 0 || d.Programs != 0 {
		t.Fatalf("%v", m)
	}
	if m["vector_table"] != "0x08000000" || m["initial_sp"] != "0x20030000" || m["reset_vector"] != "0x08000101" || m["entry_point"] != "0x08000100" ||
		m["vectors_source"] != "upload" || m["manifest_confirmed"] != true || m["region"] != "user flash" {
		t.Fatalf("%v", m)
	}
	if d.JumpBase != 0x08000000 || d.JumpSP != 0x20030000 || d.JumpPC != 0x08000101 {
		t.Fatalf("jump 0x%08x 0x%08x 0x%08x", d.JumpBase, d.JumpSP, d.JumpPC)
	}
	if _, err := r.call(id, dbg, "chip_info", nil); err == nil {
		t.Fatal("the device is still reachable after leaving")
	}
}

func TestLeaveToSRAM(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, allowAll)
	d.SetSRAM(0x20008000, vec(0x20010000, 0x20008201))
	m := r.must(t, id, flash, "leave", map[string]any{"confirm": "leave-dfu", "address": "0x20008000"})
	if m["region"] != "SRAM1" || m["initial_sp"] != "0x20010000" || m["entry_point"] != "0x20008200" || m["vectors_source"] != "upload" || d.JumpBase != 0x20008000 || d.JumpPC != 0x20008201 {
		t.Fatalf("%v jump 0x%08x", m, d.JumpBase)
	}
}

func TestLeaveRefusals(t *testing.T) {
	type c struct {
		name   string
		base   uint32
		sp, pc uint32
		args   map[string]any
		code   codes.Code
		sub    string
	}
	cases := []c{
		{"misaligned", 0x08000100, 0x20030000, 0x08000301, nil, codes.InvalidArgument, "aligned to 0x200"},
		{"boot RAM", 0x20000000, 0x20030000, 0x20000201, nil, codes.OutOfRange, "SRAM1"},
		{"SRAM2a", 0x20030000, 0x20030000, 0x20030201, nil, codes.OutOfRange, "SRAM1"},
		{"system memory", 0x1FFF0000, 0x20030000, 0x1FFF0201, nil, codes.OutOfRange, "SRAM1"},
		{"at SFSA", 0x080CB000, 0x20030000, 0x080CB201, nil, codes.OutOfRange, "user flash"},
		{"SP above SRAM1", 0x08000000, 0x20030008, 0x08000101, nil, codes.FailedPrecondition, "initial SP"},
		{"SP below SRAM", 0x08000000, 0x1FFFFFF8, 0x08000101, nil, codes.FailedPrecondition, "initial SP"},
		{"erased SP", 0x08000000, 0xFFFFFFFF, 0xFFFFFFFF, nil, codes.FailedPrecondition, "initial SP"},
		{"SP unaligned", 0x08000000, 0x20010004, 0x08000101, nil, codes.FailedPrecondition, "8-byte"},
		{"no Thumb bit", 0x08000000, 0x20030000, 0x08000100, nil, codes.FailedPrecondition, "Thumb"},
		{"PC in SRAM from flash", 0x08000000, 0x20030000, 0x20004001, nil, codes.FailedPrecondition, "user flash"},
		{"PC in secure flash", 0x08000000, 0x20030000, 0x080CB001, nil, codes.FailedPrecondition, "user flash"},
		{"PC in flash from SRAM", 0x20004000, 0x20030000, 0x08000101, nil, codes.FailedPrecondition, "SRAM1"},
		{"PC in boot RAM", 0x20004000, 0x20030000, 0x20000101, nil, codes.FailedPrecondition, "SRAM1"},
		{"asserted mismatch", 0x08000000, 0x20030000, 0x08000101, map[string]any{"vector_sp": "0x20030000", "vector_pc": "0x08000201"}, codes.FailedPrecondition, "asserted"},
		{"half asserted", 0x08000000, 0x20030000, 0x08000101, map[string]any{"vector_sp": "0x20030000"}, codes.InvalidArgument, "together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, id, d := single(t, sim.Config{SFSA: 0xCB}, allowAll)
			if tc.base >= 0x20000000 {
				d.SetSRAM(tc.base, vec(tc.sp, tc.pc))
			} else if tc.base >= 0x08000000 && tc.base < 0x080CB000 {
				d.SetFlash(tc.base, vec(tc.sp, tc.pc))
			}
			args := map[string]any{"confirm": "leave-dfu", "address": fmt.Sprintf("0x%08x", tc.base)}
			for k, v := range tc.args {
				args[k] = v
			}
			r.fail(t, tc.code, tc.sub, id, flash, "leave", args)
			if d.Gone() || d.Leaves != 0 {
				t.Fatal("left DFU")
			}
		})
	}
}

func TestLeaveCallerAssertedVectors(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB, NoRAMUpload: true}, allowAll)
	d.SetSRAM(0x20004000, vec(0x20030000, 0x20004101))
	r.fail(t, codes.FailedPrecondition, "vector_sp", id, flash, "leave", map[string]any{"confirm": "leave-dfu", "address": "0x20004000"})
	r.fail(t, codes.FailedPrecondition, "Thumb", id, flash, "leave", map[string]any{"confirm": "leave-dfu", "address": "0x20004000", "vector_sp": "0x20030000", "vector_pc": "0x20004100"})
	if d.Gone() {
		t.Fatal("left")
	}
	m := r.must(t, id, flash, "leave", map[string]any{"confirm": "leave-dfu", "address": "0x20004000", "vector_sp": "0x20030000", "vector_pc": "0x20004101"})
	if m["vectors_source"] != "caller-asserted" || m["vectors_note"] == nil || m["initial_sp"] != "0x20030000" || d.JumpPC != 0x20004101 {
		t.Fatalf("%v", m)
	}
}

func TestLeaveOutcomes(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, allowAll)
	d.SetFlash(0x08000000, vec(0x20030000, 0x08000101))
	d.LeaveDrop = true
	m := r.must(t, id, flash, "leave", map[string]any{"confirm": "leave-dfu"})
	if m["manifest_confirmed"] != false || m["manifest_note"] == nil || !d.Gone() {
		t.Fatalf("%v", m)
	}
	r, id, d = single(t, sim.Config{SFSA: 0xCB}, allowAll)
	d.SetFlash(0x08000000, vec(0x20030000, 0x08000101))
	d.LeaveState = 5
	if _, err := r.call(id, flash, "leave", map[string]any{"confirm": "leave-dfu"}); err == nil || !strings.Contains(err.Error(), "dfuMANIFEST") || d.Gone() {
		t.Fatalf("%v", err)
	}
	r.must(t, id, dbg, "chip_info", nil)
}

var allowMem = map[string]any{"allow_memory_write": true}

func TestMemoryWrite(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, allowMem)
	m := r.must(t, id, mem, "write", map[string]any{"address": "0x20004000", "hex": "0x01 02 03 04 05", "confirm": "write-memory"})
	if m["verify"] != "match" || m["length"] != float64(5) || m["end"] != "0x20004005" || !bytes.Equal(d.SRAM(0x20004000, 5), []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("%v", m)
	}
	m = r.must(t, id, mem, "write", map[string]any{"address": "0x2002FFFE", "data": "aabb", "confirm": "write-memory"})
	if m["verify"] != "match" || !bytes.Equal(d.SRAM(0x2002FFFE, 2), []byte{0xAA, 0xBB}) {
		t.Fatalf("%v", m)
	}
	img := make([]byte, 0x2C000)
	for i := range img {
		img[i] = byte(i*13 + 5)
	}
	m = r.must(t, id, mem, "write", map[string]any{"address": "0x20004000", "path": writeFile(t, img), "confirm": "write-memory"})
	if m["verify"] != "match" || !bytes.Equal(d.SRAM(0x20004000, len(img)), img) {
		t.Fatalf("%v", m)
	}
	if d.Erases != 0 || d.Programs != 0 || d.OBWrites != 0 {
		t.Fatal("flash touched")
	}
}

func TestMemoryWriteRefusals(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, allowMem)
	ok := map[string]any{"address": "0x20004000", "hex": "0102", "confirm": "write-memory"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range ok {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	r.fail(t, codes.PermissionDenied, "", id, mem, "write", with("confirm", "write"))
	r.fail(t, codes.PermissionDenied, "", id, mem, "write", with("confirm", nil))
	r.fail(t, codes.InvalidArgument, "address", id, mem, "write", with("address", nil))
	r.fail(t, codes.InvalidArgument, "not both", id, mem, "write", with("data", "0304"))
	r.fail(t, codes.InvalidArgument, "not both", id, mem, "write", with("path", writeFile(t, []byte{1, 2})))
	r.fail(t, codes.InvalidArgument, "pass hex", id, mem, "write", with("hex", nil))
	r.fail(t, codes.InvalidArgument, "at most", id, mem, "write", with("hex", strings.Repeat("00", 4097)))
	r.fail(t, codes.InvalidArgument, "at most", id, mem, "write", with("hex", nil, "path", writeFile(t, make([]byte, 0x2C001))))
	r.fail(t, codes.InvalidArgument, "at least 2", id, mem, "write", with("hex", "01"))
	r.fail(t, codes.InvalidArgument, "hex", id, mem, "write", with("hex", "zz"))
	for addr, sub := range map[string]string{
		"0x20003FF0": "bootloader's own RAM", "0x20000000": "bootloader's own RAM", "0x2002FFFF": "SRAM2a", "0x20030000": "SRAM2a",
		"0x20038000": "SRAM2b", "0x08000000": "flash", "0x00000000": "flash", "0x40000000": "peripheral", "0xE0042000": "peripheral", "0x10000000": "outside SRAM",
	} {
		r.fail(t, codes.OutOfRange, sub, id, mem, "write", with("address", addr))
	}
	if d.RAMWrites != 0 || d.Erases != 0 || d.Programs != 0 || d.BadDownloads != 0 {
		t.Fatalf("sent: ram %d", d.RAMWrites)
	}
	// Without allow_memory_write.
	r2, id2, d2 := single(t, sim.Config{Serial: "NOMEM", SFSA: 0xCB}, allowAll)
	r2.fail(t, codes.PermissionDenied, "allow_memory_write", id2, mem, "write", ok)
	// Not an STM32WB5x.
	r3, id3, d3 := single(t, sim.Config{Serial: "NOTWB", SFSA: 0xCB, NoDBGMCU: true, NoOBAlt: true}, allowMem)
	r3.fail(t, codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW", id3, mem, "write", ok)
	if d2.RAMWrites != 0 || d3.RAMWrites != 0 {
		t.Fatal("written")
	}
}

func TestMemoryWriteVerify(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB, NoRAMUpload: true}, allowMem)
	m := r.must(t, id, mem, "write", map[string]any{"address": "0x20004000", "hex": "01020304", "confirm": "write-memory"})
	if m["verify"] != "unavailable" || m["verify_note"] == nil || d.RAMWrites != 1 || !bytes.Equal(d.SRAM(0x20004000, 4), []byte{1, 2, 3, 4}) {
		t.Fatalf("%v", m)
	}
	r.must(t, id, dbg, "chip_info", nil)
	r, id, d = single(t, sim.Config{SFSA: 0xCB}, allowMem)
	d.CorruptProgram = true
	r.fail(t, codes.DataLoss, "verify=mismatch", id, mem, "write", map[string]any{"address": "0x20004000", "hex": "01020304", "confirm": "write-memory"})
}

func TestCommandListAndVersion(t *testing.T) {
	r, id, _ := single(t, sim.Config{SFSA: 0xCB}, nil)
	m := r.must(t, id, ident, "identify", nil)
	names := fmt.Sprint(m["dfuse_commands_decoded"])
	if m["dfuse_commands"] != "00214192" || names != "[get-command set-address-pointer erase read-unprotect]" || m["read_unprotect_supported"] != true {
		t.Fatalf("%v", m)
	}
	info := r.must(t, id, dbg, "info", nil)
	if info["bootloader_version"] != "2.2" || info["bcd_device"] != "0x2200" || info["dfuse_commands"] != "00214192" {
		t.Fatalf("%v", info)
	}
	r, id, _ = single(t, sim.Config{Serial: "H5", SFSA: 0xCB, NoReadUnprotect: true}, nil)
	m = r.must(t, id, ident, "identify", nil)
	if m["dfuse_commands"] != "002141" || m["read_unprotect_supported"] != false {
		t.Fatalf("%v", m)
	}
}

func TestReadProtectionAndMinimumRead(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, nil)
	r.fail(t, codes.InvalidArgument, "2..", id, mem, "read", map[string]any{"address": "0x08000000", "length": 1})
	m := r.must(t, id, mem, "read", map[string]any{"address": "0x08000001", "length": 3})
	if m["hex"] != hex.EncodeToString(d.Flash()[1:4]) {
		t.Fatalf("%v", m)
	}
	r, id, _ = single(t, sim.Config{Serial: "RDP", SFSA: 0xCB, RDPActive: true}, nil)
	r.fail(t, codes.FailedPrecondition, "errVENDOR", id, mem, "read", map[string]any{"address": "0x20004000", "length": 4})
}
