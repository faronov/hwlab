package bflbplugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/bflb/isp"
	"github.com/faronov/hwlab/plugins/uart"
)

func newPlugin(t *testing.T) (*Plugin, *isp.Sim) {
	t.Helper()
	sim := isp.NewSim()
	l := func() []Entry { return []Entry{{ID: SimID, Sim: sim, USBCDC: true}} }
	return NewWith(l, defaultOpen, defaultHelper), sim
}

func inv(t *testing.T, p *Plugin, cap, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: SimID, Capability: cap, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func mustInv(t *testing.T, p *Plugin, cap, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := inv(t, p, cap, method, args)
	if err != nil {
		t.Fatalf("%s.%s: %v", cap, method, err)
	}
	return r
}

func TestIdentifyLoadsHelperOnce(t *testing.T) {
	p, sim := newPlugin(t)
	r := mustInv(t, p, "TargetIdentify", "identify", map[string]any{"load_helper": false})
	if r["helper_running"] != false || r["chip_id"] == nil || r["mac"] != nil {
		t.Fatalf("identify without helper: %v", r)
	}
	r = mustInv(t, p, "TargetIdentify", "identify", nil)
	if r["mac"] != "0000112233445566" || r["flash_jedec"] != "0xc22014" || r["flash_bytes"] != float64(1<<20) || r["helper_loaded_now"] != true {
		t.Fatalf("identify: %v", r)
	}
	if !sim.HelperRunning() {
		t.Fatal("helper not running")
	}
	// A new lease reopens the port: the resident helper is found again.
	r = mustInv(t, p, "FlashProgramming", "info", nil)
	if r["helper_loaded_now"] != nil || r["helper_running"] != true {
		t.Fatalf("info: %v", r)
	}
}

func TestModificationNeedsBackupOfThisBoard(t *testing.T) {
	p, sim := newPlugin(t)
	t.Setenv("HWLAB_HOME", t.TempDir())
	_, err := inv(t, p, "FlashProgramming", "program", map[string]any{"address": 0, "data": "42464e50"})
	if status.Code(err) != codes.PermissionDenied || sim.Flash[0] != 0xff {
		t.Fatalf("program without backup: %v", err)
	}
	bk := mustInv(t, p, "FlashProgramming", "backup", nil)
	path := bk["path"].(string)

	other := isp.NewSim()
	other.MAC[7] ^= 1
	po := NewWith(func() []Entry { return []Entry{{ID: SimID, Sim: other, USBCDC: true}} }, defaultOpen, defaultHelper)
	_, err = inv(t, po, "FlashProgramming", "program", map[string]any{"address": 0, "data": "42464e50", "backup": path})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("backup of another board accepted: %v", err)
	}
}

func TestProgramVerifyRestore(t *testing.T) {
	p, sim := newPlugin(t)
	t.Setenv("HWLAB_HOME", t.TempDir())
	for i := range sim.Flash[:3*isp.Sector] {
		sim.Flash[i] = byte(i)
	}
	bk := mustInv(t, p, "FlashProgramming", "backup", nil)
	if bk["identical_reads"] != true {
		t.Fatal(bk)
	}
	original := append([]byte(nil), sim.Flash...)

	img := make([]byte, 5000)
	for i := range img {
		img[i] = byte(255 - i%251)
	}
	copy(img, "BFNP")
	f := filepath.Join(t.TempDir(), "fw.bin")
	if err := os.WriteFile(f, img, 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustInv(t, p, "FlashProgramming", "program", map[string]any{"address": "0x1000", "path": f, "backup": bk["path"]})
	if r["verified"] != true || r["erased_to"] != "0x003000" {
		t.Fatalf("program: %v", r)
	}
	if !bytes.Equal(sim.Flash[0x1000:0x1000+len(img)], img) {
		t.Fatal("image not in flash")
	}
	// The tail of the last sector is erased, not kept.
	if sim.Flash[0x1000+len(img)] != 0xff {
		t.Fatal("tail of the last sector not erased")
	}
	v := mustInv(t, p, "FlashProgramming", "verify", map[string]any{"address": 0x1000, "path": f})
	if v["match"] != true {
		t.Fatalf("verify: %v", v)
	}
	v = mustInv(t, p, "FlashProgramming", "verify", map[string]any{"address": 0, "path": f})
	if v["match"] != false {
		t.Fatalf("verify at the wrong address matched: %v", v)
	}

	res := mustInv(t, p, "FlashProgramming", "restore", map[string]any{"path": bk["path"]})
	if res["matches_backup"] != true || res["sectors_restored"] != float64(2) {
		t.Fatalf("restore: %v", res)
	}
	if !bytes.Equal(sim.Flash, original) {
		t.Fatal("flash differs from the backup after restore")
	}
}

func TestRangeChecks(t *testing.T) {
	p, _ := newPlugin(t)
	t.Setenv("HWLAB_HOME", t.TempDir())
	bk := mustInv(t, p, "FlashProgramming", "backup", nil)
	cases := []map[string]any{
		{"address": 0x10, "data": "00", "backup": bk["path"]},    // unaligned
		{"address": 1 << 20, "data": "00", "backup": bk["path"]}, // past the end
	}
	for i, c := range cases {
		if _, err := inv(t, p, "FlashProgramming", "program", c); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	if _, err := inv(t, p, "FlashProgramming", "read", map[string]any{"address": 1<<20 - 4, "length": 8}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("read past the end: %v", err)
	}
}

func TestHelperMustBeConfigured(t *testing.T) {
	sim := isp.NewSim()
	l := func() []Entry { return []Entry{{ID: SimID, Sim: sim, USBCDC: true}} }
	// A non-sim entry without HWLAB_BFLB_EFLASH_LOADER is refused before
	// anything is loaded.
	p := NewWith(l, defaultOpen, func(Entry) ([]byte, string, error) {
		return defaultHelper(Entry{Port: "/dev/null"})
	})
	t.Setenv(HelperEnv, "")
	_, err := inv(t, p, "FlashProgramming", "info", nil)
	if status.Code(err) != codes.FailedPrecondition || sim.HelperRunning() {
		t.Fatalf("info without a helper: %v", err)
	}
}

func TestResetLeavesISP(t *testing.T) {
	p, sim := newPlugin(t)
	mustInv(t, p, "FlashProgramming", "reset", nil)
	if sim.HelperRunning() || sim.Resets != 1 {
		t.Fatal("reset did not restart the chip")
	}
}

func TestCandidatesOnlyROMUSB(t *testing.T) {
	enum := func() ([]uart.Info, error) {
		return []uart.Info{
			{Path: "/dev/cu.usbmodemEXAMPLE", VID: "FFFF", PID: "FFFF", USB: true, Product: "BLIOT CDC Virtual ComPort"},
			{Path: "/dev/cu.usbserial-1420", VID: "1a86", PID: "7523", USB: true},
		}, nil
	}
	got := candidates(nil, enum)
	if len(got) != 1 || got[0].Port != "/dev/cu.usbmodemEXAMPLE" || !got[0].USBCDC || !got[0].Candidate {
		t.Fatalf("candidates: %+v", got)
	}
	es := parsePorts("xt=/dev/cu.usbmodemEXAMPLE,uart=/dev/cu.usbserial-1420@230400,bad", enum)
	if len(es) != 2 || !es[0].USBCDC || es[1].USBCDC || es[1].Baud != 230400 {
		t.Fatalf("parsePorts: %+v", es)
	}
	if len(candidates(es, enum)) != 0 {
		t.Fatal("explicit port listed twice")
	}
}

func TestUnknownMethod(t *testing.T) {
	p, _ := newPlugin(t)
	_, err := inv(t, p, "DebugProbe", "halt", nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatal(err)
	}
}
