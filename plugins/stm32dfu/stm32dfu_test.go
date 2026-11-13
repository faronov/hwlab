package stm32dfuplugin

import (
	"bytes"
	"context"
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

func TestLeave(t *testing.T) {
	r, id, d := single(t, sim.Config{SFSA: 0xCB}, nil)
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
	if _, err := r.call(id, dbg, "chip_info", nil); err == nil {
		t.Fatal("the device is still reachable after leaving")
	}
}
