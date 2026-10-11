package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx"
)

// Config is the CC23xx part of a probe plugin's device settings. It is set
// when the device is opened; a single call cannot grant it.
type Config struct {
	Perms
	// KeepSACI stops core methods from leaving SACI on their own.
	KeepSACI bool
}

// ConfigKeys are the device settings Config reads.
var ConfigKeys = []string{"allow_flash", "allow_erase", "allow_chip_erase", "allow_ccfg_write", "saci_exit"}

// DangerousKeys are the settings that need the core's confirmation to open.
var DangerousKeys = []string{"allow_flash", "allow_erase", "allow_chip_erase", "allow_ccfg_write"}

// ParseConfig reads the CC23xx settings out of cfg and ignores other keys.
func ParseConfig(cfg map[string]any) (Config, error) {
	var c Config
	for k, dst := range map[string]*bool{
		"allow_flash": &c.Flash, "allow_erase": &c.Erase, "allow_chip_erase": &c.ChipErase, "allow_ccfg_write": &c.CCFGWrite,
	} {
		v, ok := cfg[k]
		if !ok || v == nil {
			continue
		}
		b, isBool := v.(bool)
		if !isBool {
			return c, fmt.Errorf("%s: want true or false, got %v", k, v)
		}
		*dst = b
	}
	if v, ok := cfg["saci_exit"]; ok && v != nil {
		s, _ := v.(string)
		switch s {
		case "run":
		case "none":
			c.KeepSACI = true
		default:
			return c, fmt.Errorf("saci_exit %v: want run or none", v)
		}
	}
	return c, nil
}

// Attach opens the SWD attachment. A CC23xx that waits in its boot ROM has no
// MEM-AP yet; it is recognised by its configuration and security APs and the
// returned Session is non-nil. Other targets get the plain Connect result and
// a nil Session; a failed Connect on a non-CC23xx returns the original error.
func Attach(ctx context.Context, link adiv5.Link, ap uint8, pulse func(context.Context, time.Duration) error) (*adiv5.Target, *cortexm.Core, *cc23xx.Session, error) {
	t, err := adiv5.Connect(link, ap)
	if err != nil {
		t2, err2 := adiv5.ConnectOptional(link, ap)
		if err2 != nil {
			return nil, nil, nil, err
		}
		core := cortexm.New(adiv5.Bus{T: t2})
		s := cc23xx.New(t2, core, pulse)
		if ok, e := s.IsCC23xx(ctx); e != nil || !ok {
			return nil, nil, nil, err
		}
		return t2, core, s, nil
	}
	core := cortexm.New(adiv5.Bus{T: t})
	s := cc23xx.New(t, core, pulse)
	if ok, e := s.IsCC23xx(ctx); e != nil || !ok {
		// A failed identification read may leave a sticky error behind.
		_ = t.Reinit()
		return t, core, nil, nil
	}
	return t, core, s, nil
}

// NeedsAHB reports whether a service method reads the device through the
// AHB-AP. The others (erase, program, restore, ccfg_program, SACI control,
// identity) cope with a device that waits in SACI.
func NeedsAHB(capability, method string) bool {
	switch capability + "." + method {
	case "FlashProgramming.info", "FlashProgramming.read", "FlashProgramming.verify", "FlashProgramming.backup", "FlashProgramming.ccfg_info":
		return true
	}
	return false
}

// EnsureAHB makes core access possible: a device waiting in SACI (every reset
// leaves it there) is let into its application. With keep set, or when SACI
// cannot be left (invalid CCFG, password), the call returns SACI_ACTIVE.
func EnsureAHB(ctx context.Context, s *cc23xx.Session, keep bool) error {
	st, err := s.ReadBootStatus(ctx)
	if err != nil {
		return Err(&cc23xx.Error{Code: cc23xx.CodeSwdLost, Msg: "reading the boot status", Err: err})
	}
	if st.AHB {
		return nil
	}
	if !st.SACI {
		return Err(&cc23xx.Error{Code: cc23xx.CodeAHBAPUnavailable,
			Msg: fmt.Sprintf("the AHB-AP is closed (BOOTSTA 0x%02x %s)", st.BootSta, st.State)})
	}
	if keep {
		return Err(&cc23xx.Error{Code: cc23xx.CodeSACIActive,
			Msg: "the device waits in SACI and the device is open with saci_exit=none; call DebugProbe.saci_exit"})
	}
	if _, err := s.LeaveSACI(ctx, cc23xx.ExitRun); err != nil {
		if cc23xx.CodeOf(err) == "" {
			err = &cc23xx.Error{Code: cc23xx.CodeSACIActive, Msg: "leaving SACI to reach the core", Err: err}
		}
		return Err(err)
	}
	return nil
}

// Reset runs a CC23xx reset for DebugProbe.reset. Every reset ends in SACI
// and is completed through the SACI exit; the reset is observed by the boot
// status reaching SACI, so a kind the part cannot do is refused, not faked.
func Reset(ctx context.Context, s *cc23xx.Session, kind string, halt bool) (map[string]any, error) {
	var via cc23xx.ResetVia
	switch kind {
	case "sysreset":
		via = cc23xx.ResetSys
	case "hardware":
		via = cc23xx.ResetPin
	case "core":
		return nil, Err(&cc23xx.Error{Code: cc23xx.CodeNotSupportedByPart,
			Msg: "the Cortex-M0+ in a CC23xx has no VECTRESET (core reset); use sysreset or hardware"})
	default:
		return nil, status.Errorf(codes.InvalidArgument, "reset kind %q: want sysreset or hardware", kind)
	}
	res, err := s.Reset(ctx, via, halt)
	if err != nil {
		return nil, Err(err)
	}
	out := map[string]any{
		"kind": kind, "halt_requested": halt, "halted": res.Exit.Halted, "reset_seen": true,
		"via": string(via), "duration": res.Duration, "bootsta": res.Exit.BootSta, "state": res.Exit.State,
		"saci_observed": res.Boot.SACI,
		"note":          "every CC23xx reset passes through SACI; the boot ROM was left through the SACI exit",
	}
	if res.Exit.Halted {
		out["pc"] = hex32(res.Exit.PC)
		out["at_reset_vector"] = res.Exit.AtHandler
		out["reset_vector"] = hex32(res.Exit.AppVector)
	}
	return out, nil
}

// Annotate turns a failed core access on a CC23xx into SACI_ACTIVE or
// AHB_AP_UNAVAILABLE when the boot status explains it.
func Annotate(ctx context.Context, s *cc23xx.Session, err error) error {
	if err == nil || cc23xx.CodeOf(err) != "" {
		return err
	}
	if c := status.Code(err); c == codes.InvalidArgument || c == codes.PermissionDenied {
		return err
	}
	st, e := s.ReadBootStatus(context.WithoutCancel(ctx))
	if e != nil || st.AHB {
		return err
	}
	if st.SACI {
		return Err(&cc23xx.Error{Code: cc23xx.CodeSACIActive, Msg: "the device waits in SACI: core access is closed", Err: err})
	}
	return Err(&cc23xx.Error{Code: cc23xx.CodeAHBAPUnavailable, Msg: fmt.Sprintf("the AHB-AP is closed (BOOTSTA 0x%02x %s)", st.BootSta, st.State), Err: err})
}
