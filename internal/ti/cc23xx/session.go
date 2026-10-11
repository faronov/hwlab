// Package cc23xx is the target-family layer for TI CC23xx (CC2340 class)
// devices. It sits on adiv5 and the cortexm core and is shared by every debug
// probe transport: reset and SACI entry/exit, the CFG-AP/SEC-AP access, device
// identification, the CCFG parser and the flash service.
//
// A CC23xx boots into the Secure Application Command Interface after any
// reset. The AHB-AP, and with it the CPU debug registers, is only reachable
// once SACI has been left with a debug exit command. See SWCU193 chapter 8.
package cc23xx

import (
	"context"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/saci"
)

// Access port numbers (SWCU193 table 5-1) and their identification registers.
const (
	APAHB = 0
	APCfg = 1
	APSec = 2

	idrCfgAP = 0x002E0001
	idrSecAP = 0x002E0000
	idrAHB   = 0x84770001
)

// CFG-AP registers.
const (
	cfgDeviceID     = 0x00
	cfgPartID       = 0x04
	cfgDeviceStatus = 0x0C
	apIDR           = 0xFC
)

// BOOTSTA values (SWCU193 table 8-1) the layer acts on.
const (
	BootSACI          = 0x20
	BootBldrWaitLoop  = 0x81
	BootAppEntry      = 0xC0
	BootAppWaitLoop   = 0xC1
	BootAppStart      = 0xC2
	BootFailNoApp     = 0xFD
	BootFailTransfer  = 0xFE
	BootFaultHandler  = 0xFF
	bootFailLow       = 0x3E
	bootFailHigh      = 0x3F
	r3                = cortexm.RegisterID(3)
	bootROMWaitReturn = 0 // value R3 takes to leave the wait loop
)

// Timing holds the delays of the reset flow. The defaults are the values
// verified on a CC2340 through both probe transports.
type Timing struct {
	ResetLow   time.Duration // nRESET asserted
	ResetWait  time.Duration // from release to the first SWD line reset
	SACIWait   time.Duration // wait for BOOTSTA == SACI after a reset
	ExitWait   time.Duration // wait for the boot ROM after leaving SACI
	AppStarted time.Duration // wait for BOOTSTA >= 0xC2 after R3 = 0
}

// DefaultTiming returns the verified defaults.
func DefaultTiming() Timing {
	return Timing{ResetLow: 5 * time.Millisecond, ResetWait: 60 * time.Millisecond, SACIWait: 2 * time.Second,
		ExitWait: 2 * time.Second, AppStarted: 2 * time.Second}
}

// Session is one CC23xx target behind an SWD probe. The caller serialises
// calls; it is not safe for concurrent use.
type Session struct {
	T    *adiv5.Target
	Core *cortexm.Core
	// PulseReset drives the probe's nRESET low for the given time and releases it.
	PulseReset func(ctx context.Context, low time.Duration) error
	Timing     Timing
	// CapClock, when set, lowers the SWD clock to at most hz and returns a
	// function that restores the previous clock (a no-op when the clock is
	// already at or below hz).
	CapClock func(hz uint32) (restore func(), err error)

	sc      *saci.Client
	restore func()
}

// SysresetSACIClockHz is the SWD clock used for a SYSRESETREQ re-entry into
// SACI. On the real CC2340 (XDS110, clean start after nRESET) the SACI mailbox
// answered with the clock at 400 and 500 kHz for the whole sequence, while
// 600 kHz and above left the first command unread. After an nRESET it worked
// up to 4 MHz. The reason is not understood; the cap is empirical.
const SysresetSACIClockHz = 400_000

func (s *Session) capClock(hz uint32) error {
	s.restoreClock()
	if s.CapClock == nil {
		return nil
	}
	r, err := s.CapClock(hz)
	if err != nil {
		return err
	}
	s.restore = r
	return nil
}

func (s *Session) restoreClock() {
	if s.restore != nil {
		s.restore()
		s.restore = nil
	}
}

// New returns a session. pulse may be nil when the probe has no nRESET output.
func New(t *adiv5.Target, core *cortexm.Core, pulse func(context.Context, time.Duration) error) *Session {
	return &Session{T: t, Core: core, PulseReset: pulse, Timing: DefaultTiming()}
}

// SACI returns the SACI client over the SEC-AP.
func (s *Session) SACI() *saci.Client {
	if s.sc == nil {
		s.sc = saci.New(saci.NewAPMailbox(s.T))
	}
	return s.sc
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// IsCC23xx reports whether the CFG-AP and SEC-AP identification registers
// match a CC23xx.
func (s *Session) IsCC23xx(ctx context.Context) (bool, error) {
	cfg, err := s.T.ReadAPOn(APCfg, apIDR)
	if err != nil {
		return false, err
	}
	sec, err := s.T.ReadAPOn(APSec, apIDR)
	if err != nil {
		return false, err
	}
	return cfg == idrCfgAP && sec == idrSecAP, nil
}

// BootStatus is the CFG-AP DEVICESTATUS view.
type BootStatus struct {
	DeviceStatus uint32 `json:"device_status"`
	BootSta      uint8  `json:"bootsta"`
	State        string `json:"state"`
	SACI         bool   `json:"saci_active"`
	AHB          bool   `json:"ahb_ap_available"`
}

// StateName names a BOOTSTA value.
func StateName(b uint8) string {
	switch {
	case b == 0x00:
		return "reset"
	case b == 0x01:
		return "cold-boot"
	case b == BootSACI:
		return "saci"
	case b == 0x36:
		return "wait-swd-disconnect"
	case b == 0x37:
		return "saci-exited"
	case b == bootFailLow || b == bootFailHigh:
		return "boot-failure"
	case b == 0x80:
		return "bootloader-entry"
	case b == BootBldrWaitLoop:
		return "bootloader-wait-for-probe"
	case b >= 0xBA && b <= 0xBF:
		return "serial-rom-bootloader"
	case b == BootAppEntry:
		return "app-entry"
	case b == BootAppWaitLoop:
		return "app-wait-for-probe"
	case b == BootAppStart:
		return "app-running"
	case b >= 0xC3 && b <= 0xFC:
		return "app-defined"
	case b == BootFailNoApp || b == BootFailTransfer || b == BootFaultHandler:
		return "app-boot-failure"
	}
	return fmt.Sprintf("0x%02x", b)
}

// ReadBootStatus reads DEVICESTATUS and the AHB-AP IDR.
func (s *Session) ReadBootStatus(ctx context.Context) (BootStatus, error) {
	ds, err := s.T.ReadAPOn(APCfg, cfgDeviceStatus)
	if err != nil {
		return BootStatus{}, err
	}
	idr, err := s.T.ReadAPOn(APAHB, apIDR)
	if err != nil {
		return BootStatus{}, err
	}
	b := uint8(ds >> 8)
	if idr == idrAHB && s.T.APIDR == 0 {
		if err := s.T.RefreshAP(); err != nil {
			return BootStatus{}, err
		}
	}
	return BootStatus{DeviceStatus: ds, BootSta: b, State: StateName(b), SACI: b == BootSACI, AHB: idr == idrAHB}, nil
}

func (s *Session) waitBoot(ctx context.Context, timeout time.Duration, what string, ok func(BootStatus) bool) (BootStatus, error) {
	dl := time.Now().Add(timeout)
	var last BootStatus
	for {
		st, err := s.ReadBootStatus(ctx)
		if err == nil {
			last = st
			if ok(st) {
				return st, nil
			}
			if st.BootSta == bootFailLow || st.BootSta == bootFailHigh {
				return st, errf(CodeBootFailure, nil, "boot code reports failure BOOTSTA 0x%02x while waiting for %s", st.BootSta, what)
			}
		}
		if time.Now().After(dl) {
			if err != nil {
				return last, errf(CodeSwdLost, err, "reading DEVICESTATUS while waiting for %s", what)
			}
			return last, errf(CodeSACITimeout, nil, "waited %v for %s, BOOTSTA is 0x%02x (%s)", timeout, what, last.BootSta, last.State)
		}
		if err := sleep(ctx, 3*time.Millisecond); err != nil {
			return last, err
		}
	}
}

// ResetVia selects how a reset is driven.
type ResetVia string

// Reset sources.
const (
	ResetPin  ResetVia = "nreset"   // the probe's nRESET line
	ResetSys  ResetVia = "sysreset" // AIRCR.SYSRESETREQ (needs the AHB-AP)
	ResetSACI ResetVia = "saci"     // SACI BLDR_APP_RESET_DEVICE (needs SACI active)
)

// EnterSACI resets the device and waits until the boot ROM sits in SACI.
// A device already in SACI is left alone when via is empty.
func (s *Session) EnterSACI(ctx context.Context, via ResetVia) (BootStatus, error) {
	if via == "" {
		if st, err := s.ReadBootStatus(ctx); err == nil && st.SACI {
			return st, nil
		}
		via = ResetPin
	}
	switch via {
	case ResetPin:
		s.restoreClock()
		if s.PulseReset == nil {
			return BootStatus{}, errf(CodeNotSupportedByPart, nil, "this probe has no nRESET output")
		}
		if err := s.PulseReset(ctx, s.Timing.ResetLow); err != nil {
			return BootStatus{}, err
		}
	case ResetSys:
		if err := s.capClock(SysresetSACIClockHz); err != nil {
			return BootStatus{}, err
		}
		// The write may be answered with a fault or a dropped link while the
		// device resets; what counts is the SACI state afterwards.
		_ = s.Core.Bus.Write32(ctx, cortexm.AddrAIRCR, 0x05FA0004)
	case ResetSACI:
		if err := s.SACI().ResetDevice(ctx, false); err != nil {
			return BootStatus{}, err
		}
	default:
		return BootStatus{}, errf(CodeUnsafeRequest, nil, "unknown reset source %q", via)
	}
	if err := sleep(ctx, s.Timing.ResetWait); err != nil {
		return BootStatus{}, err
	}
	if err := s.T.Reinit(); err != nil {
		return BootStatus{}, errf(CodeSwdLost, err, "SWD did not come back after the %s reset", via)
	}
	st, err := s.waitBoot(ctx, s.Timing.SACIWait, "SACI after reset", func(b BootStatus) bool { return b.SACI })
	if err != nil {
		return st, errf(CodeResetNotObserved, err, "%s reset driven, SACI not entered", via)
	}
	return st, nil
}

// ExitMode selects how SACI is left.
type ExitMode string

// SACI exit modes.
const (
	// ExitRun lets the boot flow run the application (BLDR_APP_EXIT_SACI_RUN).
	ExitRun ExitMode = "run"
	// ExitHalt leaves SACI with the core halted in the boot ROM wait loop
	// (BOOTSTA 0x81/0xC1) so breakpoints can be set before the application starts.
	ExitHalt ExitMode = "halt"
	// ExitResetHalt does ExitHalt, sets a breakpoint on the application reset
	// handler, runs, and returns with the core halted there.
	ExitResetHalt ExitMode = "reset-halt"
)

// ExitResult describes the state after leaving SACI.
type ExitResult struct {
	Mode      ExitMode `json:"mode"`
	BootSta   uint8    `json:"bootsta"`
	State     string   `json:"state"`
	Halted    bool     `json:"halted"`
	PC        uint32   `json:"pc,omitempty"`
	AppVector uint32   `json:"app_reset_handler,omitempty"`
	AtHandler bool     `json:"at_reset_handler,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// CheckDebugOpen asks SACI whether debug needs a password.
func (s *Session) CheckDebugOpen(ctx context.Context) error {
	req, err := s.SACI().RequestPasswordID(ctx)
	if err != nil {
		return err
	}
	if req.Needed {
		return errf(CodeTargetSecured, nil, "debug is protected by a password (CCFG debugCfg.authorization); no password is stored or guessed")
	}
	return nil
}

// LeaveSACI exits SACI with the requested mode. SACI must be active.
func (s *Session) LeaveSACI(ctx context.Context, mode ExitMode) (ExitResult, error) {
	res := ExitResult{Mode: mode}
	st, err := s.ReadBootStatus(ctx)
	if err != nil {
		return res, err
	}
	if err := s.CheckDebugOpen(ctx); err != nil {
		return res, err
	}
	sc := s.SACI()
	switch mode {
	case ExitRun:
		if err := sc.ExitRun(ctx); err != nil {
			return res, exitErr(err)
		}
		st, err = s.waitBoot(ctx, s.Timing.ExitWait, "the AHB-AP after SACI exit", func(b BootStatus) bool { return b.AHB })
		if err != nil {
			return res, err
		}
		s.restoreClock()
		if err := s.T.Reinit(); err != nil {
			return res, err
		}
		res.BootSta, res.State = st.BootSta, st.State
		return res, nil
	case ExitHalt, ExitResetHalt:
		if err := sc.ExitHalt(ctx); err != nil {
			return res, exitErr(err)
		}
		st, err = s.waitBoot(ctx, s.Timing.ExitWait, "the boot ROM wait loop", func(b BootStatus) bool {
			return b.AHB && (b.BootSta == BootBldrWaitLoop || b.BootSta == BootAppWaitLoop)
		})
		if err != nil {
			return res, err
		}
		s.restoreClock()
		if err := s.T.Reinit(); err != nil {
			return res, err
		}
		if _, err := s.Core.Halt(ctx); err != nil {
			return res, errf(CodeAHBAPUnavailable, err, "halting the core in the boot ROM wait loop")
		}
		res.BootSta, res.State, res.Halted = st.BootSta, st.State, true
		if mode == ExitHalt {
			res.PC, _ = s.Core.ReadRegister(ctx, cortexm.RegPC)
			return res, nil
		}
		return s.runToAppHandler(ctx, res)
	}
	return res, errf(CodeUnsafeRequest, nil, "unknown exit mode %q", mode)
}

func exitErr(err error) error {
	var re *saci.ResultError
	if asResult(err, &re) && re.Result == saci.ResNotAllowed {
		return errf(CodeCCFGInvalid, err, "SACI refuses to exit: the CCFG is invalid or a chip erase/CCFG program ran in this SACI session; reset to start a new session")
	}
	return err
}

// ReleaseBootWait gets a core that waits in the boot ROM loop (after ExitHalt)
// to continue: R3 = 0, resume, and wait for the application start. It
// handles both the bootloader and the application wait loops.
func (s *Session) ReleaseBootWait(ctx context.Context) (BootStatus, error) {
	var st BootStatus
	for i := 0; i < 3; i++ {
		if err := s.Core.WriteRegister(ctx, r3, bootROMWaitReturn); err != nil {
			return st, err
		}
		if _, err := s.Core.Resume(ctx); err != nil {
			return st, err
		}
		var err error
		st, err = s.waitBoot(ctx, s.Timing.AppStarted, "the application start", func(b BootStatus) bool {
			return b.BootSta == BootAppStart || b.BootSta == BootBldrWaitLoop || b.BootSta == BootAppWaitLoop || b.BootSta >= 0xC3
		})
		if err != nil {
			return st, err
		}
		if st.BootSta != BootBldrWaitLoop && st.BootSta != BootAppWaitLoop {
			return st, nil
		}
		// Another wait loop (bootloader then application): halt and release again.
		if _, err := s.Core.Halt(ctx); err != nil {
			return st, err
		}
	}
	return st, errf(CodeBootFailure, nil, "boot ROM wait loops did not release (BOOTSTA 0x%02x)", st.BootSta)
}
