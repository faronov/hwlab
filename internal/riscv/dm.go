package riscv

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Debug Module registers (debug spec 0.13/1.0).
const (
	regData0      = 0x04
	regDMControl  = 0x10
	regDMStatus   = 0x11
	regHartInfo   = 0x12
	regAbstractCS = 0x16
	regCommand    = 0x17
	regSBCS       = 0x38
	regSBAddress0 = 0x39
	regSBData0    = 0x3C
)

// dmcontrol bits.
const (
	dmcActive    = 1 << 0
	dmcHaltReq   = 1 << 31
	dmcResumeReq = 1 << 30
)

// Errors.
var (
	ErrNotHalted    = errors.New("riscv: hart is not halted")
	ErrHaltTimeout  = errors.New("riscv: hart did not halt in time")
	ErrNoSBA        = errors.New("riscv: this debug module has no 32-bit System Bus Access")
	ErrAbstract     = errors.New("riscv: abstract command failed")
	ErrBusAccess    = errors.New("riscv: system bus access failed")
	ErrNoHart       = errors.New("riscv: selected hart does not exist")
	ErrUnauthorized = errors.New("riscv: debug module requires authentication")
)

// HartState is the run state of the selected hart.
type HartState string

// Hart states.
const (
	HartRunning     HartState = "running"
	HartHalted      HartState = "halted"
	HartUnavailable HartState = "unavailable"
	HartNonExistent HartState = "nonexistent"
	HartUnknown     HartState = "unknown"
)

// Status is a decoded dmstatus.
type Status struct {
	Raw             uint32    `json:"raw"`
	Version         int       `json:"version"` // 2 = 0.13, 3 = 1.0
	Authenticated   bool      `json:"authenticated"`
	ImpEBreak       bool      `json:"impebreak"`
	HasResetHaltReq bool      `json:"has_reset_halt_req"`
	HaveReset       bool      `json:"have_reset"`
	ResumeAck       bool      `json:"resume_ack"`
	State           HartState `json:"state"`
}

// VersionName names the debug spec version.
func (s Status) VersionName() string {
	switch s.Version {
	case 1:
		return "0.11"
	case 2:
		return "0.13"
	case 3:
		return "1.0"
	}
	return fmt.Sprintf("unknown(%d)", s.Version)
}

func decodeStatus(v uint32) Status {
	s := Status{
		Raw: v, Version: int(v & 0xF), Authenticated: v>>7&1 == 1, ImpEBreak: v>>22&1 == 1,
		HasResetHaltReq: v>>5&1 == 1, HaveReset: v>>18&1 == 1, ResumeAck: v>>17&1 == 1,
	}
	switch {
	case v>>14&1 == 1:
		s.State = HartNonExistent
	case v>>12&1 == 1:
		s.State = HartUnavailable
	case v>>9&1 == 1:
		s.State = HartHalted
	case v>>11&1 == 1:
		s.State = HartRunning
	default:
		s.State = HartUnknown
	}
	return s
}

// Info is what Identify learns about the debug module. Nothing in it needs a
// halted hart.
type Info struct {
	DTM           DTMCS  `json:"dtm"`
	Status        Status `json:"status"`
	HartInfo      uint32 `json:"hartinfo"`
	Harts         int    `json:"harts"` // contiguous harts 0..n-1 that exist
	ProgBufSize   int    `json:"progbuf_size"`
	DataCount     int    `json:"data_count"`
	SBVersion     int    `json:"sb_version"`
	SBASize       int    `json:"sba_bits"`
	SBA32         bool   `json:"sba32"`
	ActivatedByUs bool   `json:"activated_by_us"`
}

// DM is the debug module of one hart (hartsel 0 unless Select is called).
type DM struct {
	dtm       *DTM
	hart      uint32
	activated bool
	wasActive bool
	checked   bool
	sba       SBAInfo
	sbaOK     bool
	// Timeout bounds each polling loop (default 1 s).
	Timeout time.Duration
}

// NewDM wraps a DTM.
func NewDM(dtm *DTM) *DM { return &DM{dtm: dtm, Timeout: time.Second} }

// DTM returns the underlying transport module.
func (m *DM) DTM() *DTM { return m.dtm }

func (m *DM) control(extra uint32) uint32 {
	return dmcActive | extra | (m.hart&0x3FF)<<16 | (m.hart>>10&0x3FF)<<6
}

// Activate sets dmactive. If the module was already active it is left as is,
// and Deactivate will not touch it.
func (m *DM) Activate(ctx context.Context) error {
	if m.checked {
		return nil
	}
	v, err := m.dtm.Read(ctx, regDMControl)
	if err != nil {
		return err
	}
	m.wasActive = v&dmcActive != 0
	if !m.wasActive {
		if err := m.dtm.Write(ctx, regDMControl, dmcActive); err != nil {
			return err
		}
		m.activated = true
	}
	m.checked = true
	return nil
}

// Release undoes what this session changed: if it activated the module and the
// hart is running, dmactive is cleared again. A halted hart or a module that
// was already active is left alone.
func (m *DM) Release(ctx context.Context) error {
	if !m.activated {
		return nil
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if st.State == HartRunning {
		if err := m.dtm.Write(ctx, regDMControl, 0); err != nil {
			return err
		}
		m.activated, m.checked = false, false
	}
	return nil
}

// Select picks the hart for later operations.
func (m *DM) Select(ctx context.Context, hart uint32) error {
	if hart > 0xFFFFF {
		return fmt.Errorf("riscv: hart index %d out of range", hart)
	}
	if err := m.Activate(ctx); err != nil {
		return err
	}
	m.hart = hart
	return m.dtm.Write(ctx, regDMControl, m.control(0))
}

// Status reads dmstatus for the selected hart.
func (m *DM) Status(ctx context.Context) (Status, error) {
	if err := m.Activate(ctx); err != nil {
		return Status{}, err
	}
	v, err := m.dtm.Read(ctx, regDMStatus)
	if err != nil {
		return Status{}, err
	}
	return decodeStatus(v), nil
}

// Identify reads the debug module configuration. It enumerates harts by
// selecting them one at a time (at most maxHarts) and restores hart 0.
func (m *DM) Identify(ctx context.Context, maxHarts int) (Info, error) {
	var info Info
	if err := m.Activate(ctx); err != nil {
		return info, err
	}
	info.DTM, info.ActivatedByUs = m.dtm.CS(), m.activated
	st, err := m.Status(ctx)
	if err != nil {
		return info, err
	}
	info.Status = st
	if !st.Authenticated {
		return info, ErrUnauthorized
	}
	if info.HartInfo, err = m.dtm.Read(ctx, regHartInfo); err != nil {
		return info, err
	}
	acs, err := m.dtm.Read(ctx, regAbstractCS)
	if err != nil {
		return info, err
	}
	info.ProgBufSize, info.DataCount = int(acs>>24&0x1F), int(acs&0xF)
	sbcs, err := m.dtm.Read(ctx, regSBCS)
	if err != nil {
		return info, err
	}
	info.SBVersion, info.SBASize, info.SBA32 = int(sbcs>>29), int(sbcs>>5&0x7F), sbcs>>2&1 == 1
	prev := m.hart
	for h := uint32(0); h < uint32(maxHarts); h++ {
		if err := m.Select(ctx, h); err != nil {
			return info, err
		}
		s, err := m.Status(ctx)
		if err != nil {
			return info, err
		}
		if s.State == HartNonExistent {
			break
		}
		info.Harts++
	}
	if err := m.Select(ctx, prev); err != nil {
		return info, err
	}
	if info.Status, err = m.Status(ctx); err != nil {
		return info, err
	}
	return info, nil
}

func (m *DM) poll(ctx context.Context, what string, done func(Status) bool) (Status, error) {
	deadline := time.Now().Add(m.Timeout)
	for {
		st, err := m.Status(ctx)
		if err != nil {
			return st, err
		}
		if st.State == HartNonExistent {
			return st, ErrNoHart
		}
		if done(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("riscv: timed out waiting for %s (dmstatus=%#08x)", what, st.Raw)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Halt requests a halt of the selected hart and waits until it is halted.
// haltreq is cleared again afterwards.
func (m *DM) Halt(ctx context.Context) (Status, error) {
	if err := m.Activate(ctx); err != nil {
		return Status{}, err
	}
	if err := m.dtm.Write(ctx, regDMControl, m.control(dmcHaltReq)); err != nil {
		return Status{}, err
	}
	st, err := m.poll(ctx, "halt", func(s Status) bool { return s.State == HartHalted })
	// Always drop haltreq, even if the wait failed.
	if werr := m.dtm.Write(ctx, regDMControl, m.control(0)); err == nil {
		err = werr
	}
	if err != nil && errors.Is(err, ErrNoHart) {
		return st, err
	}
	if err != nil {
		return st, fmt.Errorf("%w: %v", ErrHaltTimeout, err)
	}
	return st, nil
}

// Resume resumes the selected hart and waits for the acknowledge.
func (m *DM) Resume(ctx context.Context) (Status, error) {
	if err := m.Activate(ctx); err != nil {
		return Status{}, err
	}
	if err := m.dtm.Write(ctx, regDMControl, m.control(dmcResumeReq)); err != nil {
		return Status{}, err
	}
	st, err := m.poll(ctx, "resume acknowledge", func(s Status) bool { return s.ResumeAck && s.State == HartRunning })
	if werr := m.dtm.Write(ctx, regDMControl, m.control(0)); err == nil {
		err = werr
	}
	return st, err
}

func (m *DM) requireHalted(ctx context.Context) error {
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if st.State != HartHalted {
		return fmt.Errorf("%w (state %s)", ErrNotHalted, st.State)
	}
	return nil
}

// abstractStatus waits for the abstract command unit to go idle and returns
// cmderr. A non-zero cmderr is cleared.
func (m *DM) abstractWait(ctx context.Context) error {
	deadline := time.Now().Add(m.Timeout)
	for {
		v, err := m.dtm.Read(ctx, regAbstractCS)
		if err != nil {
			return err
		}
		if v>>12&1 == 0 {
			if e := v >> 8 & 7; e != 0 {
				if err := m.dtm.Write(ctx, regAbstractCS, 7<<8); err != nil {
					return err
				}
				return fmt.Errorf("%w: cmderr %d (%s)", ErrAbstract, e, cmdErrName(e))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: busy for %s", ErrAbstract, m.Timeout)
		}
	}
}

func cmdErrName(e uint32) string {
	switch e {
	case 1:
		return "busy"
	case 2:
		return "not supported"
	case 3:
		return "exception"
	case 4:
		return "halt/resume"
	case 5:
		return "bus error"
	case 7:
		return "other"
	}
	return "reserved"
}

// RegNum selects an abstract register: CSRs are 0x0000-0x0FFF, GPRs x0..x31
// are 0x1000-0x101F, FPRs 0x1020-0x103F.
type RegNum uint16

// GPR returns the register number of xN.
func GPR(n int) RegNum { return RegNum(0x1000 + n) }

// Well-known CSR numbers.
const (
	CSRTSelect   RegNum = 0x7A0
	CSRTData1    RegNum = 0x7A1
	CSRTData2    RegNum = 0x7A2
	CSRTInfo     RegNum = 0x7A4
	CSRMStatus   RegNum = 0x300
	CSRMISA      RegNum = 0x301
	CSRMTVec     RegNum = 0x305
	CSRMEPC      RegNum = 0x341
	CSRMCause    RegNum = 0x342
	CSRDCSR      RegNum = 0x7B0
	CSRDPC       RegNum = 0x7B1
	CSRMVendorID RegNum = 0xF11
	CSRMArchID   RegNum = 0xF12
	CSRMImpID    RegNum = 0xF13
	CSRMHartID   RegNum = 0xF14
)

// ReadReg reads a 32-bit register of the halted hart with an Access Register
// abstract command. It never writes the register.
func (m *DM) ReadReg(ctx context.Context, r RegNum) (uint32, error) {
	v, err := m.ReadRegs(ctx, []RegNum{r})
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

func sbErrName(e uint32) string {
	switch e {
	case 1:
		return "timeout"
	case 2:
		return "bad address"
	case 3:
		return "alignment"
	case 4:
		return "unsupported size"
	case 7:
		return "other"
	}
	return "reserved"
}
