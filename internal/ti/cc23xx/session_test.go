package cc23xx_test

import (
	"context"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

type rig struct {
	sim  *xds.Sim
	s    *cc23xx.Session
	core *cortexm.Core
}

func newRig(t *testing.T, sim *xds.Sim) *rig {
	t.Helper()
	pr := xds.NewProbe(sim)
	if _, err := pr.Connect(); err != nil {
		t.Fatal(err)
	}
	tgt, err := adiv5.ConnectOptional(xds.NewSWDLink(pr), 0)
	if err != nil {
		t.Fatal(err)
	}
	core := cortexm.New(adiv5.Bus{T: tgt})
	pulse := func(_ context.Context, low time.Duration) error {
		if err := pr.SetSRST(false); err != nil {
			return err
		}
		time.Sleep(low)
		return pr.SetSRST(true)
	}
	s := cc23xx.New(tgt, core, pulse)
	s.Timing = cc23xx.Timing{ResetLow: time.Millisecond, ResetWait: time.Millisecond, SACIWait: time.Second,
		ExitWait: 200 * time.Millisecond, AppStarted: 500 * time.Millisecond}
	return &rig{sim, s, core}
}

func TestIdentifyInSACI(t *testing.T) {
	r := newRig(t, xds.NewSimCC23xx())
	id, err := r.s.Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !id.SACI || id.Boot.AHB || id.CCFG != nil || id.CfgAPDeviceID != 0x1BB8402F {
		t.Fatalf("%+v", id)
	}
}

func TestResetRunAndIdentify(t *testing.T) {
	for _, via := range []cc23xx.ResetVia{cc23xx.ResetPin, cc23xx.ResetSys} {
		r := newRig(t, xds.NewSimCC23xx())
		ctx := context.Background()
		res, err := r.s.Reset(ctx, via, false)
		if err != nil {
			t.Fatalf("%s: %v", via, err)
		}
		if res.Exit.Halted || res.Boot.BootSta != cc23xx.BootSACI {
			t.Fatalf("%s: %+v", via, res)
		}
		id, err := r.s.Identify(ctx)
		if err != nil || id.SACI || !id.Boot.AHB || id.FlashBytes != 512*1024 || id.FlashBanks != 2 || id.CCFG == nil || !id.CCFG.Valid {
			t.Fatalf("%s: %+v %v", via, id, err)
		}
		// Reset again from the running state: the device must land in SACI again.
		if _, err := r.s.Reset(ctx, via, false); err != nil {
			t.Fatalf("second %s: %v", via, err)
		}
	}
}

func TestResetHaltStopsAtResetHandler(t *testing.T) {
	r := newRig(t, xds.NewSimCC23xx())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := r.s.Reset(ctx, cc23xx.ResetPin, true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Exit.Halted || !res.Exit.AtHandler || res.Exit.PC != 0x8934 {
			t.Fatalf("%+v", res)
		}
		pc, err := r.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil || pc != 0x8934 {
			t.Fatalf("pc %#x %v", pc, err)
		}
	}
}

func TestSecuredTargetStaysInSACI(t *testing.T) {
	sim := xds.NewSimCC23xx()
	sim.CC23xx().SACI.DebugAuth = 0xA5
	r := newRig(t, sim)
	_, err := r.s.Reset(context.Background(), cc23xx.ResetPin, false)
	if cc23xx.CodeOf(err) != cc23xx.CodeTargetSecured {
		t.Fatalf("%v", err)
	}
	if !sim.CC23xx().InSACI() {
		t.Fatal("secured target left SACI")
	}
}

func TestInvalidCCFGRefusesExit(t *testing.T) {
	sim := xds.NewSimCC23xx()
	sim.CC23xx().SACI.CCFGValid = false
	r := newRig(t, sim)
	_, err := r.s.Reset(context.Background(), cc23xx.ResetPin, false)
	if cc23xx.CodeOf(err) != cc23xx.CodeCCFGInvalid {
		t.Fatalf("%v", err)
	}
}

func TestStuckBootROM(t *testing.T) {
	sim := xds.NewSimCC23xx()
	sim.CC23xx().NeverExit = true
	r := newRig(t, sim)
	_, err := r.s.Reset(context.Background(), cc23xx.ResetPin, false)
	if err == nil || cc23xx.CodeOf(err) == "" {
		t.Fatalf("want a coded error, got %v", err)
	}
}

func TestNotCC23xx(t *testing.T) {
	r := newRig(t, xds.NewSim())
	_, err := r.s.Identify(context.Background())
	if cc23xx.CodeOf(err) != cc23xx.CodeNotCC23xx {
		t.Fatalf("%v", err)
	}
}

func TestSysresetCapsTheClockForSACI(t *testing.T) {
	for _, tc := range []struct {
		via  cc23xx.ResetVia
		caps int
	}{{cc23xx.ResetSys, 1}, {cc23xx.ResetPin, 0}} {
		r := newRig(t, xds.NewSimCC23xx())
		var caps, restores int
		r.s.CapClock = func(hz uint32) (func(), error) {
			if hz != cc23xx.SysresetSACIClockHz {
				t.Errorf("cap %d Hz", hz)
			}
			caps++
			return func() { restores++ }, nil
		}
		if _, err := r.s.Reset(context.Background(), tc.via, false); err != nil {
			t.Fatal(err)
		}
		if caps != tc.caps || restores != tc.caps {
			t.Fatalf("%s: capped %d restored %d, want %d", tc.via, caps, restores, tc.caps)
		}
	}
}
