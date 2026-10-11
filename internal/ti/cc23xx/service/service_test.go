package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

type rig struct {
	sim *xds.Sim
	env *service.Env
	dir string
}

func newRig(t *testing.T, perm service.Perms) *rig {
	t.Helper()
	sim := xds.NewSimCC23xx()
	pr := xds.NewProbe(sim)
	if _, err := pr.Connect(); err != nil {
		t.Fatal(err)
	}
	tgt, err := adiv5.ConnectOptional(xds.NewSWDLink(pr), 0)
	if err != nil {
		t.Fatal(err)
	}
	pulse := func(_ context.Context, low time.Duration) error {
		if err := pr.SetSRST(false); err != nil {
			return err
		}
		time.Sleep(low)
		return pr.SetSRST(true)
	}
	s := cc23xx.New(tgt, cortexm.New(adiv5.Bus{T: tgt}), pulse)
	s.Timing = cc23xx.Timing{ResetLow: time.Millisecond, ResetWait: time.Millisecond, SACIWait: time.Second,
		ExitWait: 200 * time.Millisecond, AppStarted: 500 * time.Millisecond}
	dir := t.TempDir()
	return &rig{sim, &service.Env{S: s, Perm: perm, Dir: dir}, dir}
}

var all = service.Perms{Flash: true, Erase: true, ChipErase: true, CCFGWrite: true}

func (r *rig) call(t *testing.T, c, m string, a map[string]any) (map[string]any, error) {
	t.Helper()
	// Arguments reach the service JSON-decoded.
	var wire map[string]any
	if a != nil {
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
	}
	return r.env.Invoke(context.Background(), c, m, wire)
}

func (r *rig) must(t *testing.T, c, m string, a map[string]any) map[string]any {
	t.Helper()
	res, err := r.call(t, c, m, a)
	if err != nil {
		t.Fatalf("%s.%s: %v", c, m, err)
	}
	return res
}

func wantCode(t *testing.T, err error, c codes.Code, prefix string) {
	t.Helper()
	if err == nil || status.Code(err) != c || !strings.Contains(err.Error(), prefix) {
		t.Fatalf("want %v %q, got %v", c, prefix, err)
	}
}

func (r *rig) run(t *testing.T) {
	t.Helper()
	if _, err := r.env.S.Reset(context.Background(), cc23xx.ResetPin, false); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) backup(t *testing.T) string {
	t.Helper()
	r.run(t)
	return r.must(t, "FlashProgramming", "backup", nil)["path"].(string)
}

func TestInSACIReadsAreRefused(t *testing.T) {
	r := newRig(t, all)
	_, err := r.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 16})
	wantCode(t, err, codes.FailedPrecondition, cc23xx.CodeSACIActive)
	info := r.must(t, "DebugProbe", "cc23xx_info", nil)
	if info["in_saci"] != true {
		t.Fatalf("%v", info)
	}
	if _, ok := info["ccfg"]; ok {
		t.Fatal("CCFG is not readable in SACI")
	}
}

func TestInfoReadVerifyBackup(t *testing.T) {
	r := newRig(t, service.Perms{})
	r.run(t)
	info := r.must(t, "FlashProgramming", "info", nil)
	g := info["geometry"].(map[string]any)
	if g["main_bytes"] != float64(512*1024) || g["sector_bytes"] != float64(2048) || info["ccfg"] == nil {
		t.Fatalf("%v", info)
	}
	rd := r.must(t, "FlashProgramming", "read", map[string]any{"address": "0x0", "length": 8})
	if len(rd["data"].(string)) != 16 {
		t.Fatalf("%v", rd)
	}
	_, err := r.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 5000})
	wantCode(t, err, codes.InvalidArgument, "needs path")
	// The CCFG is not part of MAIN.
	_, err = r.call(t, "FlashProgramming", "read", map[string]any{"address": ccfg.BaseAddr, "length": 16})
	wantCode(t, err, codes.InvalidArgument, cc23xx.CodeUnsafeRequest)
	ci := r.must(t, "FlashProgramming", "ccfg_info", nil)
	if strings.Contains(strings.ToLower(toJSON(ci)), "pwd_hash") {
		t.Fatal("password material leaked")
	}

	bk := r.must(t, "FlashProgramming", "backup", nil)
	p := bk["path"].(string)
	if bk["identical_reads"] != true || filepath.Dir(p) != r.dir {
		t.Fatalf("%v", bk)
	}
	for _, f := range []string{p, p + ".ccfg", p + ".json"} {
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	img, _ := os.ReadFile(p)
	v := filepath.Join(r.dir, "same.bin")
	_ = os.WriteFile(v, img[:4096], 0o600)
	r.must(t, "FlashProgramming", "verify", map[string]any{"path": v, "address": 0})
	img[100] ^= 1
	_ = os.WriteFile(v, img[:4096], 0o600)
	_, err = r.call(t, "FlashProgramming", "verify", map[string]any{"path": v, "address": 0})
	wantCode(t, err, codes.DataLoss, cc23xx.CodeVerifyFailed)
}

func TestModifyingCallsAreGated(t *testing.T) {
	r := newRig(t, service.Perms{})
	bk := r.backup(t)
	img := filepath.Join(r.dir, "img.bin")
	_ = os.WriteFile(img, []byte{1, 2, 3, 4}, 0o600)
	for _, c := range []struct {
		m string
		a map[string]any
	}{
		{"erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk}},
		{"program", map[string]any{"path": img, "address": 0, "confirm": service.TokProgram, "backup": bk}},
		{"restore", map[string]any{"path": bk, "confirm": service.TokRestore}},
	} {
		_, err := r.call(t, "FlashProgramming", c.m, c.a)
		wantCode(t, err, codes.PermissionDenied, cc23xx.CodePermissionDenied)
	}
	// Each permission is needed on its own.
	for _, p := range []service.Perms{{Erase: true, ChipErase: true}, {Flash: true, ChipErase: true}, {Flash: true, Erase: true}} {
		r.env.Perm = p
		_, err := r.call(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk})
		wantCode(t, err, codes.PermissionDenied, cc23xx.CodePermissionDenied)
	}
	r.env.Perm = all
	_, err := r.call(t, "FlashProgramming", "erase", map[string]any{"confirm": "yes", "backup": bk})
	wantCode(t, err, codes.PermissionDenied, "erase-entire-chip")
	_, err = r.call(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip})
	wantCode(t, err, codes.PermissionDenied, "verified full backup")
	_, err = r.call(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": filepath.Join(r.dir, "missing.bin")})
	wantCode(t, err, codes.FailedPrecondition, "backup")
	// Nothing above touched the flash.
	r.must(t, "FlashProgramming", "verify", map[string]any{"path": bk, "address": 0})
}

func TestTamperedBackupIsRefused(t *testing.T) {
	r := newRig(t, all)
	bk := r.backup(t)
	b, _ := os.ReadFile(bk)
	b[0] ^= 1
	_ = os.WriteFile(bk, b, 0o600)
	_, err := r.call(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk})
	wantCode(t, err, codes.FailedPrecondition, "SHA-256")
}

func TestEraseThenRestoreFromSACI(t *testing.T) {
	r := newRig(t, all)
	bk := r.backup(t)
	orig, _ := os.ReadFile(bk)
	res := r.must(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk})
	if res["erased"] != true || res["state"] != "saci" || !strings.Contains(res["retain_policy"].(string), "retain mask") {
		t.Fatalf("%v", res)
	}
	// The erased device waits in SACI and cannot be read or backed up.
	_, err := r.call(t, "FlashProgramming", "backup", nil)
	wantCode(t, err, codes.FailedPrecondition, cc23xx.CodeSACIActive)
	// A second erase on a blank device is a no-op.
	res = r.must(t, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk})
	if res["already_blank"] != true {
		t.Fatalf("%v", res)
	}
	res = r.must(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore})
	if res["restored"] != true || res["already_matched"] != false || res["main_verified"] != true || res["ccfg_verified"] != true {
		t.Fatalf("%v", res)
	}
	got := r.must(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 4096})
	if got["data"] == "" {
		t.Fatal("no data")
	}
	nb := r.must(t, "FlashProgramming", "backup", map[string]any{"dir": filepath.Join(r.dir, "second")})
	again, _ := os.ReadFile(nb["path"].(string))
	if !bytes.Equal(again, orig) {
		t.Fatal("flash differs after restore")
	}
	// Restoring a device that already matches changes nothing.
	res = r.must(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore})
	if res["already_matched"] != true {
		t.Fatalf("%v", res)
	}
}

func TestProgramMergesOverBackupAndKeepsCCFG(t *testing.T) {
	r := newRig(t, all)
	bk := r.backup(t)
	orig, _ := os.ReadFile(bk)
	img := bytes.Repeat([]byte{0xA5, 0x5A, 0x12, 0x34}, 1000)
	path := filepath.Join(r.dir, "new.bin")
	_ = os.WriteFile(path, img, 0o600)
	const at = 0x2000
	_, err := r.call(t, "FlashProgramming", "program", map[string]any{"path": path, "address": at, "confirm": "no", "backup": bk})
	wantCode(t, err, codes.PermissionDenied, service.TokProgram)
	_, err = r.call(t, "FlashProgramming", "program", map[string]any{"path": path, "address": 0x7FFF0, "confirm": service.TokProgram, "backup": bk})
	wantCode(t, err, codes.OutOfRange, "does not fit")
	res := r.must(t, "FlashProgramming", "program", map[string]any{"path": path, "address": at, "confirm": service.TokProgram, "backup": bk})
	if res["merged_over"] != "backup" || res["ccfg_changed"] != false || res["main_verified"] != true || res["ccfg_verified"] != true {
		t.Fatalf("%v", res)
	}
	want := append([]byte(nil), orig...)
	copy(want[at:], img)
	nb := r.must(t, "FlashProgramming", "backup", map[string]any{"dir": filepath.Join(r.dir, "after")})
	got, _ := os.ReadFile(nb["path"].(string))
	if !bytes.Equal(got, want) {
		t.Fatal("flash is not the backup with the image merged over it")
	}
	oc, _ := os.ReadFile(bk + ".ccfg")
	nc, _ := os.ReadFile(nb["path"].(string) + ".ccfg")
	if !bytes.Equal(oc, nc) {
		t.Fatal("the CCFG changed")
	}
	// The old backup no longer matches the flash: a second program is refused.
	_, err = r.call(t, "FlashProgramming", "program", map[string]any{"path": path, "address": at, "confirm": service.TokProgram, "backup": bk})
	wantCode(t, err, codes.FailedPrecondition, "does not match")
	// Restoring the old backup brings the original back.
	r.must(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore})
	nb = r.must(t, "FlashProgramming", "backup", map[string]any{"dir": filepath.Join(r.dir, "third")})
	got, _ = os.ReadFile(nb["path"].(string))
	if !bytes.Equal(got, orig) {
		t.Fatal("restore did not bring the original back")
	}
}

func TestProgramFailureReportsRecovery(t *testing.T) {
	r := newRig(t, all)
	bk := r.backup(t)
	path := filepath.Join(r.dir, "new.bin")
	_ = os.WriteFile(path, bytes.Repeat([]byte{1}, 4096), 0o600)
	r.sim.CC23xx().SACI.ProtectedSectors[1] = true
	_, err := r.call(t, "FlashProgramming", "program", map[string]any{"path": path, "address": 0x800, "confirm": service.TokProgram, "backup": bk})
	if err == nil || !strings.Contains(err.Error(), "restore it with FlashProgramming.restore path="+bk) || status.Code(err) != codes.DataLoss {
		t.Fatalf("%v", err)
	}
	r.sim.CC23xx().SACI.ProtectedSectors[1] = false
	r.must(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore})
}

func TestCCFGProgramGates(t *testing.T) {
	r := newRig(t, all)
	bk := r.backup(t)
	cur, _ := os.ReadFile(bk + ".ccfg")
	path := filepath.Join(r.dir, "ccfg.bin")

	// A change in an undocumented/security field.
	mod := append([]byte(nil), cur...)
	mod[0x100] ^= 0xFF
	if err := ccfg.Finalize(mod); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, mod, 0o600)
	dry := r.must(t, "FlashProgramming", "ccfg_program", map[string]any{"path": path, "backup": bk, "dry_run": true})
	if dry["security_relevant"] != true || len(dry["changes"].([]any)) == 0 {
		t.Fatalf("%v", dry)
	}
	args := map[string]any{"path": path, "backup": bk, "confirm": service.TokCCFG}
	r.env.Perm.CCFGWrite = false
	_, err := r.call(t, "FlashProgramming", "ccfg_program", args)
	wantCode(t, err, codes.PermissionDenied, "allow_ccfg_write")
	r.env.Perm.CCFGWrite = true
	_, err = r.call(t, "FlashProgramming", "ccfg_program", args)
	wantCode(t, err, codes.PermissionDenied, service.TokSecurity)

	// A CCFG with broken CRCs is never written.
	bad := append([]byte(nil), mod...)
	bad[0x7FC] ^= 1
	_ = os.WriteFile(path, bad, 0o600)
	_, err = r.call(t, "FlashProgramming", "ccfg_program", map[string]any{"path": path, "backup": bk, "dry_run": true})
	wantCode(t, err, codes.FailedPrecondition, cc23xx.CodeCCFGInvalid)
	_ = os.WriteFile(path, cur[:100], 0o600)
	_, err = r.call(t, "FlashProgramming", "ccfg_program", map[string]any{"path": path, "backup": bk, "dry_run": true})
	wantCode(t, err, codes.InvalidArgument, "2048")

	// An image that forbids chip erase can never be recovered.
	lock := append([]byte(nil), cur...)
	lock[0x18+1] &^= 0xF0
	if err := ccfg.Finalize(lock); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, lock, 0o600)
	_, err = r.call(t, "FlashProgramming", "ccfg_program", map[string]any{"path": path, "backup": bk, "confirm": service.TokCCFG, "security_ack": service.TokSecurity})
	wantCode(t, err, codes.PermissionDenied, "forbids chip erase")
	// Nothing was erased by the refusals.
	r.must(t, "FlashProgramming", "verify", map[string]any{"path": bk, "address": 0})

	// The acknowledged, safe change goes through and is verified.
	_ = os.WriteFile(path, mod, 0o600)
	args["security_ack"] = service.TokSecurity
	res := r.must(t, "FlashProgramming", "ccfg_program", args)
	if res["ccfg_changed"] != true || res["ccfg_verified"] != true || res["main_verified"] != true {
		t.Fatalf("%v", res)
	}
	got := r.must(t, "FlashProgramming", "ccfg_info", nil)
	if got["sha256"] == "" {
		t.Fatal("no sha")
	}
	// Restoring the original brings the CCFG back too.
	r.must(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore})
	ci, _ := r.sim.CC23xx().SACI.CCFG, 0
	if !bytes.Equal(ci, cur) {
		t.Fatal("restore did not bring the CCFG back")
	}
}

func TestSACIControl(t *testing.T) {
	r := newRig(t, all)
	r.run(t)
	_, err := r.call(t, "DebugProbe", "saci_enter", nil)
	wantCode(t, err, codes.PermissionDenied, service.TokSACI)
	res := r.must(t, "DebugProbe", "saci_enter", map[string]any{"confirm": service.TokSACI, "via": "nreset"})
	if res["saci_active"] != true {
		t.Fatalf("%v", res)
	}
	st := r.must(t, "DebugProbe", "saci_status", nil)
	if st["saci_active"] != true || st["ahb_ap_available"] != false {
		t.Fatalf("%v", st)
	}
	_, err = r.call(t, "DebugProbe", "saci_exit", map[string]any{"mode": "bogus"})
	wantCode(t, err, codes.InvalidArgument, "mode")
	ex := r.must(t, "DebugProbe", "saci_exit", map[string]any{"mode": "reset-halt"})
	if ex["halted"] != true || ex["at_reset_handler"] != true {
		t.Fatalf("%v", ex)
	}
}

func toJSON(m map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				b.WriteString(k + " ")
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(m)
	return b.String()
}
