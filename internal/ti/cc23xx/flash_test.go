package cc23xx_test

import (
	"bytes"
	"context"
	"hash/crc32"
	"testing"

	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

func testImage(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	copy(b[0:], []byte{0x00, 0x90, 0x00, 0x20, 0x35, 0x89, 0x00, 0x00})
	return b
}

func geometry(t *testing.T, r *rig) cc23xx.Geometry {
	t.Helper()
	if _, err := r.s.Reset(context.Background(), cc23xx.ResetPin, false); err != nil {
		t.Fatal(err)
	}
	id, err := r.s.Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g, err := id.Geometry()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPlanImage(t *testing.T) {
	g := cc23xx.Geometry{MainBytes: 512 * 1024, SectorSize: 2048, Sectors: 256}
	img := make([]byte, 5000)
	for i := range img {
		img[i] = 0xFF
	}
	img[3] = 1
	img[4999] = 2
	p, err := cc23xx.PlanImage(g, 0, img)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sectors) != 3 || p.BlankSectors != 1 || p.Sectors[0].Bytes != 16 || p.Sectors[1].Bytes != 0 {
		t.Fatalf("%+v", p)
	}
	if p.Sectors[0].DataAddr != 0 || p.Sectors[2].DataAddr%16 != 0 {
		t.Fatalf("granule: %+v", p.Sectors)
	}
	// An unaligned base keeps the rest of the sector erased.
	p, err = cc23xx.PlanImage(g, 0x1000+4, []byte{1, 2, 3})
	if err != nil || p.FirstSector != 2 || p.Sectors[0].Image[3] != 0xFF || p.Sectors[0].Image[4] != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, c := range []struct {
		base uint32
		n    int
	}{{512*1024 - 2, 4}, {0x4E020000, 16}} {
		if _, err := cc23xx.PlanImage(g, c.base, make([]byte, c.n)); cc23xx.CodeOf(err) != cc23xx.CodeUnsafeRequest {
			t.Fatalf("%#x: %v", c.base, err)
		}
	}
	if _, err := cc23xx.PlanImage(g, 0, nil); err == nil {
		t.Fatal("empty image accepted")
	}
}

func TestEraseProgramVerifyBoot(t *testing.T) {
	sim := xds.NewSimCC23xx()
	r := newRig(t, sim)
	ctx := context.Background()
	g := geometry(t, r)
	oldCCFG, err := r.s.ReadCCFG(ctx)
	if err != nil {
		t.Fatal(err)
	}
	img := testImage(10000)
	plan, err := cc23xx.PlanImage(g, 0, img)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.s.BeginFlash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ChipErase(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Blank(ctx, 0, g.Sectors); err != nil {
		t.Fatal(err)
	}
	// A wrong CRC must fail verification.
	if err := tx.VerifyRange(ctx, 0, 2048, 0x12345678); cc23xx.CodeOf(err) != cc23xx.CodeVerifyFailed {
		t.Fatalf("wrong CRC accepted: %v", err)
	}
	if err := tx.Program(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := tx.Blank(ctx, 0, 1); cc23xx.CodeOf(err) != cc23xx.CodeVerifyFailed {
		t.Fatalf("programmed sector is blank: %v", err)
	}
	if err := tx.Verify(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := tx.ProgramCCFG(ctx, oldCCFG); err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifyCCFG(ctx, oldCCFG); err != nil {
		t.Fatal(err)
	}
	// A different CCFG does not match.
	other := append([]byte(nil), oldCCFG...)
	other[0x100] ^= 0xFF
	if err := ccfg.Finalize(other); err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifyCCFG(ctx, other); cc23xx.CodeOf(err) != cc23xx.CodeVerifyFailed {
		t.Fatalf("%v", err)
	}
	res, err := tx.Finish(ctx, cc23xx.ExitRun)
	if err != nil || res.Exit.Halted {
		t.Fatalf("%+v %v", res, err)
	}
	got, err := r.s.ReadFlash(ctx, g, 0, len(img))
	if err != nil || !bytes.Equal(got, img) {
		t.Fatalf("flash content differs after program: %v", err)
	}
	if crc32.ChecksumIEEE(got) != crc32.ChecksumIEEE(img) {
		t.Fatal("crc")
	}
	id, err := r.s.Identify(ctx)
	if err != nil || id.CCFG == nil || !id.CCFG.Valid {
		t.Fatalf("CCFG after program: %+v %v", id, err)
	}
}

func TestExitRefusedAfterEraseUntilCCFG(t *testing.T) {
	r := newRig(t, xds.NewSimCC23xx())
	ctx := context.Background()
	geometry(t, r)
	tx, err := r.s.BeginFlash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ChipErase(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// The erased device boots nowhere: SACI refuses the exit.
	_, err = tx.Finish(ctx, cc23xx.ExitRun)
	if cc23xx.CodeOf(err) != cc23xx.CodeCCFGInvalid {
		t.Fatalf("%v", err)
	}
	// The device is still in SACI and can be recovered.
	st, err := r.s.ReadBootStatus(ctx)
	if err != nil || !st.SACI {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestPermissionDenied(t *testing.T) {
	sim := xds.NewSimCC23xx()
	r := newRig(t, sim)
	ctx := context.Background()
	geometry(t, r)
	sim.CC23xx().SACI.AllowErase = false
	tx, err := r.s.BeginFlash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ChipErase(ctx, 0); cc23xx.CodeOf(err) != cc23xx.CodePermissionDenied {
		t.Fatalf("%v", err)
	}
}

func TestProgramOverDataFails(t *testing.T) {
	r := newRig(t, xds.NewSimCC23xx())
	ctx := context.Background()
	g := geometry(t, r)
	plan, _ := cc23xx.PlanImage(g, 0, testImage(4096))
	tx, err := r.s.BeginFlash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No erase: the sim image already holds a program at these addresses.
	if err := tx.Program(ctx, plan); cc23xx.CodeOf(err) != cc23xx.CodeFlashFailed {
		t.Fatalf("%v", err)
	}
}

func TestCCFGProgramRejectsBadImage(t *testing.T) {
	r := newRig(t, xds.NewSimCC23xx())
	ctx := context.Background()
	geometry(t, r)
	tx, err := r.s.BeginFlash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bad := make([]byte, ccfg.Size)
	if err := tx.ProgramCCFG(ctx, bad); cc23xx.CodeOf(err) != cc23xx.CodeCCFGInvalid {
		t.Fatalf("%v", err)
	}
	if err := tx.ProgramCCFG(ctx, bad[:100]); cc23xx.CodeOf(err) != cc23xx.CodeUnsafeRequest {
		t.Fatalf("%v", err)
	}
}
