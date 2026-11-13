package stm32_test

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

func newFUS(t *testing.T) (*stm32.SimWB, *cortexm.Core, *stm32.FUS) {
	t.Helper()
	s, c, f := newUnparkedFUS(t)
	copy(s.Flash, stm32.ParkStub)
	return s, c, f
}

func newUnparkedFUS(t *testing.T) (*stm32.SimWB, *cortexm.Core, *stm32.FUS) {
	t.Helper()
	s, c, _, _ := setupWB(t)
	f := stm32.NewFUS(c, 1024)
	f.Reattach = func(context.Context) error { s.Reattach(); return nil }
	f.Poll, f.BusyPoll = 0, time.Millisecond
	f.ReadyTimeout, f.CmdTimeout, f.OpTimeout = time.Second, time.Second, 5*time.Second
	restore, err := f.Hold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	return s, c, f
}

// coproImage builds a synthetic image with an ST-style footer.
func coproImage(n int, kind uint32) []byte {
	b := image(n)
	binary.LittleEndian.PutUint32(b[n-8:], kind)
	binary.LittleEndian.PutUint32(b[n-4:], stm32.FUSImageMagic)
	return b
}

const (
	fusKind = 0x200000FF
	macKind = 0x22110045
)

func TestFUSEnterFromWirelessStack(t *testing.T) {
	s, _, f := newFUS(t)
	st, di, err := f.EnterFUS(context.Background())
	if err != nil {
		t.Fatal(err, f.Log)
	}
	if st != stm32.FUSStateIdle || di.Layout != "fus" || di.FUSVersion.String() != "1.1.1" || di.StackVersion.String() != "1.10.0" {
		t.Fatalf("state 0x%02x %+v", st, di)
	}
	if s.CPU2Resets != 1 || len(s.Commands) != 3 {
		t.Fatalf("resets %d commands %x", s.CPU2Resets, s.Commands)
	}
	if s.Released == 0 {
		t.Fatal("event buffers were never returned to CPU2")
	}
}

func TestFUSFirstStartUnknown(t *testing.T) {
	s, _, f := newFUS(t)
	s.FirstStartUnknown = true
	if st, _, err := f.EnterFUS(context.Background()); err != nil || st != stm32.FUSStateIdle {
		t.Fatal(st, err, f.Log)
	}
}

func TestFUSUpgradeDeleteInstallStart(t *testing.T) {
	s, c, f := newFUS(t)
	ctx := context.Background()

	r, err := f.Upgrade(ctx, coproImage(24492, fusKind), 0, nil)
	if err != nil {
		t.Fatal(err, f.Log)
	}
	if r.Before.FUSVersion.String() != "1.1.1" || r.After.FUSVersion.String() != "1.2.0" || r.After.StackVersion != r.Before.StackVersion {
		t.Fatalf("%+v", r)
	}
	if want := stm32.StagingAddress(0x080CB000, 24492); r.StagedAt != want || s.LastStagedImage != want+24492-4 {
		t.Fatalf("staged at 0x%08x (FUS found 0x%08x), want 0x%08x", r.StagedAt, s.LastStagedImage, want)
	}
	if s.FlashWhileC2 != 0 {
		t.Fatal("flash was modified while CPU2 was booted")
	}

	r, err = f.DeleteStack(ctx, func(di stm32.DeviceInfo, _ stm32.Security) error {
		if di.StackVersion.String() != "1.10.0" {
			return errors.New("unexpected stack")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err, f.Log)
	}
	if r.After.StackVersion != 0 || r.SecAfter.SecureBase != 0x080F4000 {
		t.Fatalf("%+v", r)
	}

	// The secure start moved: the image goes below the new one.
	r, err = f.Upgrade(ctx, coproImage(74100, macKind), 0, nil)
	if err != nil {
		t.Fatal(err, f.Log)
	}
	if r.After.StackVersion.String() != "1.24.0" || r.SecAfter.SecureBase != 0x080E0000 || r.StagedAt != stm32.StagingAddress(0x080F4000, 74100) {
		t.Fatalf("%+v", r)
	}

	if err := f.StartStack(ctx); err != nil {
		t.Fatal(err, f.Log)
	}
	di, err := f.ReadDeviceInfo(ctx)
	if err != nil || di.Layout != "wireless_stack" || di.StackVersion.String() != "1.24.0" {
		t.Fatalf("%+v %v", di, err)
	}
	if st, _ := c.Status(ctx); !st.Halted {
		t.Fatal("CPU1 is not held")
	}
	if s.FlashWhileC2 != 0 {
		t.Fatal("flash was modified while CPU2 was booted")
	}
}

func TestFUSPreCheckChangesNothing(t *testing.T) {
	s, _, f := newFUS(t)
	want := errors.New("version mismatch")
	if _, err := f.Upgrade(context.Background(), coproImage(1024, fusKind), 0, func(stm32.DeviceInfo, stm32.Security) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if _, err := f.DeleteStack(context.Background(), func(stm32.DeviceInfo, stm32.Security) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if s.Erases != 0 || s.Programs != 0 || s.StackVersion == 0 || s.FUSVersion != 0x01010100 {
		t.Fatal("state changed")
	}
	for _, op := range s.Commands {
		if op != 0xFC52 {
			t.Fatalf("command 0x%04x sent", op)
		}
	}
}

func TestFUSRejectsBadImageAndAddress(t *testing.T) {
	s, _, f := newFUS(t)
	ctx := context.Background()
	if _, err := f.Upgrade(ctx, image(4096), 0, nil); err == nil {
		t.Fatal("image without footer accepted")
	}
	if _, err := f.Upgrade(ctx, coproImage(8192, fusKind), 0x080CA000, nil); !errors.Is(err, stm32.ErrRange) {
		t.Fatalf("image across the secure start: %v", err)
	}
	if s.Erases != 0 {
		t.Fatal("flash erased")
	}
}

func TestFUSResetWithCPU1RunningAborts(t *testing.T) {
	s, c, f := newFUS(t)
	ctx := context.Background()
	if err := f.SystemReset(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := stm32.Detect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	img := coproImage(1024, fusKind)
	if err := f.Stage(ctx, stm32.NewFlash(c, p, 0), stm32.StagingAddress(p.SecureBase, len(img)), img); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.EnterFUS(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Request(ctx, stm32.OpFWUpgrade, 1); err != nil {
		t.Fatal(err)
	}
	// CPU1's boot page no longer holds the park stub when the FUS's
	// power-on-like reset lets CPU1 run.
	copy(s.Flash, image(len(stm32.ParkStub)))
	if _, err := f.WaitIdle(ctx, true); !errors.Is(err, stm32.ErrFUSUnexpected) {
		t.Fatalf("got %v", err)
	}
	if st, _ := c.Status(ctx); !st.Halted {
		t.Fatal("CPU1 was left running")
	}
	if s.FUSVersion != 0x01010100 {
		t.Fatal("the simulated upgrade completed")
	}
}

func TestFUSRequiresParkedCPU1(t *testing.T) {
	s, _, f := newUnparkedFUS(t)
	if _, _, err := f.EnterFUS(context.Background()); !errors.Is(err, stm32.ErrNotParked) {
		t.Fatal(err)
	}
	if len(s.Commands) != 0 || s.CPU2Resets != 0 {
		t.Fatal("CPU2 was booted although CPU1 is not parked")
	}
}

func TestFUSResetsThatResetTheDebugPort(t *testing.T) {
	ctx := context.Background()
	// The link stays down until the debugger re-attaches.
	s, c, f := newFUS(t)
	s.LinkLossAccesses = 1 << 20
	if _, _, err := f.EnterFUS(ctx); err != nil {
		t.Fatal(err, f.Log)
	}
	if s.Reattaches == 0 || s.CPU2Resets != 1 {
		t.Fatalf("reattaches %d, CPU2 resets %d", s.Reattaches, s.CPU2Resets)
	}
	if st, err := c.Status(ctx); err != nil || !st.Halted {
		t.Fatal("CPU1 not halted after the reset", err)
	}
	// Without a way to re-attach the operation stops.
	s2, _, f2 := newFUS(t)
	s2.LinkLossAccesses = 1 << 20
	f2.Reattach = nil
	if _, _, err := f2.EnterFUS(ctx); err == nil {
		t.Fatal("succeeded without a debug link")
	}
	// Plain system resets (debug logic kept) work too.
	s3, _, f3 := newFUS(t)
	s3.PORResets = false
	if _, _, err := f3.EnterFUS(ctx); err != nil || s3.Reattaches != 0 {
		t.Fatal(err, s3.Reattaches)
	}
}
