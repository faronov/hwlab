package cc23xx

import (
	"context"
	"errors"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/saci"
)

func asResult(err error, target **saci.ResultError) bool { return errors.As(err, target) }

const (
	ccfgBase     = 0x4E020000
	ccfgAppVtor  = ccfgBase + 0x08
	invalidWord  = 0xFFFFFFFF
	haltWaitStep = 500 * time.Millisecond
)

// AppResetHandler returns the application reset handler address from the
// CCFG boot configuration (pAppVtor) and the vector table it points to. The
// AHB-AP must be reachable.
func (s *Session) AppResetHandler(ctx context.Context) (handler, vtor uint32, err error) {
	vtor, err = s.Core.Bus.Read32(ctx, ccfgAppVtor)
	if err != nil {
		return 0, 0, err
	}
	if vtor == invalidWord || vtor&3 != 0 {
		return 0, vtor, errf(CodeCCFGInvalid, nil, "CCFG pAppVtor 0x%08x is not a vector table address", vtor)
	}
	h, err := s.Core.Bus.Read32(ctx, vtor+4)
	if err != nil {
		return 0, vtor, err
	}
	if h == invalidWord || h&1 == 0 {
		return 0, vtor, errf(CodeCCFGInvalid, nil, "vector table at 0x%08x has no Thumb reset handler (0x%08x)", vtor, h)
	}
	return h &^ 1, vtor, nil
}

// runToAppHandler sets a breakpoint on the application reset handler, releases
// the boot ROM wait loop and returns with the core halted on the handler.
func (s *Session) runToAppHandler(ctx context.Context, res ExitResult) (ExitResult, error) {
	handler, _, err := s.AppResetHandler(ctx)
	if err != nil {
		res.Note = "stopped in the boot ROM wait loop: " + err.Error()
		res.PC, _ = s.Core.ReadRegister(ctx, cortexm.RegPC)
		return res, nil
	}
	res.AppVector = handler
	fpb, err := s.Core.DiscoverFPB(ctx)
	if err != nil {
		res.Note = "stopped in the boot ROM wait loop: no breakpoint unit: " + err.Error()
		return res, nil
	}
	saved, err := fpb.Save(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = fpb.Restore(context.WithoutCancel(ctx), saved) }()
	if _, err := fpb.Set(ctx, handler); err != nil {
		return res, err
	}
	for i := 0; i < 3; i++ {
		if err := s.Core.WriteRegister(ctx, r3, bootROMWaitReturn); err != nil {
			return res, err
		}
		if _, err := s.Core.Resume(ctx); err != nil {
			return res, err
		}
		stt, werr := s.Core.WaitHalted(ctx, haltWaitStep)
		if werr == nil && stt.Halted {
			pc, err := s.Core.ReadRegister(ctx, cortexm.RegPC)
			if err != nil {
				return res, err
			}
			res.Halted, res.PC = true, pc&^1
			res.AtHandler = res.PC == handler
			if st, e := s.ReadBootStatus(ctx); e == nil {
				res.BootSta, res.State = st.BootSta, st.State
			}
			if res.AtHandler {
				return res, nil
			}
			continue
		}
		// Not halted: either still in a wait loop that wants R3 again, or
		// the application ran past the breakpoint.
		if _, err := s.Core.Halt(ctx); err != nil {
			return res, err
		}
	}
	res.Halted = true
	res.Note = "the core is halted but not on the application reset handler"
	return res, nil
}

// ResetResult is the outcome of Reset.
type ResetResult struct {
	Via      ResetVia   `json:"via"`
	Boot     BootStatus `json:"saci_state"`
	Exit     ExitResult `json:"exit"`
	Duration string     `json:"duration"`
}

// Reset resets the device and brings it back to the requested state:
// reset -> SACI -> exit. halt selects the stop at the application reset
// handler; otherwise the application runs. A password protected debug
// interface stays in SACI and returns TARGET_SECURED.
func (s *Session) Reset(ctx context.Context, via ResetVia, halt bool) (ResetResult, error) {
	t0 := time.Now()
	r := ResetResult{Via: via}
	st, err := s.EnterSACI(ctx, via)
	r.Boot = st
	if err != nil {
		return r, err
	}
	mode := ExitRun
	if halt {
		mode = ExitResetHalt
	}
	r.Exit, err = s.LeaveSACI(ctx, mode)
	r.Duration = time.Since(t0).Round(time.Millisecond).String()
	return r, err
}
