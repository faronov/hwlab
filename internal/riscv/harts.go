package riscv

import (
	"context"
	"fmt"
)

// HartReport is everything the debug module reveals about one hart, gathered
// without changing its run state unless Probe asked for a halt/resume cycle.
type HartReport struct {
	Index              int       `json:"index"`
	State              HartState `json:"state"`
	DMStatus           uint32    `json:"dmstatus"`
	HartInfo           uint32    `json:"hartinfo"`
	HartInfoDataAccess bool      `json:"hartinfo_dataaccess"`
	HartInfoDataSize   int       `json:"hartinfo_datasize"`
	HartInfoDataAddr   uint32    `json:"hartinfo_dataaddr"`
	ProgBufSize        int       `json:"progbuf_size"`
	// Filled only when the hart was halted at the time of the report.
	Halted        bool   `json:"halted"`
	XLEN          int    `json:"xlen,omitempty"`
	MISA          uint32 `json:"misa,omitempty"`
	MHartID       uint32 `json:"mhartid"`
	HasMHartID    bool   `json:"has_mhartid"`
	GPRAccess     bool   `json:"gpr_access"`
	CSRAccess     bool   `json:"csr_access"`
	PC            uint32 `json:"pc,omitempty"`
	AbstractError string `json:"abstract_error,omitempty"`
}

// Harts reports on harts 0..max-1 until the first non-existent one, then
// restores the previously selected hart. A hart that is not halted is reported
// from dmstatus and hartinfo only; nothing is halted or resumed.
func (m *DM) Harts(ctx context.Context, max int) ([]HartReport, error) {
	if err := m.Activate(ctx); err != nil {
		return nil, err
	}
	prev := m.hart
	defer func() { _ = m.Select(ctx, prev) }()
	acs, err := m.dtm.Read(ctx, regAbstractCS)
	if err != nil {
		return nil, err
	}
	var out []HartReport
	for h := 0; h < max; h++ {
		rep, ok, err := m.hartReport(ctx, uint32(h), int(acs>>24&0x1F))
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		out = append(out, rep)
	}
	return out, nil
}

func (m *DM) hartReport(ctx context.Context, h uint32, progbuf int) (HartReport, bool, error) {
	if err := m.Select(ctx, h); err != nil {
		return HartReport{}, false, err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return HartReport{}, false, err
	}
	if st.State == HartNonExistent {
		return HartReport{}, false, nil
	}
	hi, err := m.dtm.Read(ctx, regHartInfo)
	if err != nil {
		return HartReport{}, false, err
	}
	rep := HartReport{Index: int(h), State: st.State, DMStatus: st.Raw, HartInfo: hi, ProgBufSize: progbuf,
		HartInfoDataAccess: hi>>16&1 == 1, HartInfoDataSize: int(hi >> 12 & 0xF), HartInfoDataAddr: hi & 0xFFF}
	if st.State != HartHalted {
		return rep, true, nil
	}
	rep.Halted = true
	if v, err := m.ReadReg(ctx, GPR(0)); err == nil {
		rep.GPRAccess = v == 0
	} else {
		rep.AbstractError = err.Error()
	}
	if v, err := m.ReadReg(ctx, CSRMISA); err == nil {
		rep.CSRAccess, rep.MISA = true, v
		switch v >> 30 {
		case 1:
			rep.XLEN = 32
		case 2:
			rep.XLEN = 64
		case 3:
			rep.XLEN = 128
		}
	} else if rep.AbstractError == "" {
		rep.AbstractError = err.Error()
	}
	if v, err := m.ReadReg(ctx, CSRMHartID); err == nil {
		rep.MHartID, rep.HasMHartID = v, true
	}
	if v, err := m.ReadReg(ctx, CSRDPC); err == nil {
		rep.PC = v
	}
	return rep, true, nil
}

// HartCount returns the number of contiguous harts that exist, up to max.
func (m *DM) HartCount(ctx context.Context, max int) (int, error) {
	prev := m.hart
	defer func() { _ = m.Select(ctx, prev) }()
	n := 0
	for h := 0; h < max; h++ {
		if err := m.Select(ctx, uint32(h)); err != nil {
			return n, err
		}
		st, err := m.Status(ctx)
		if err != nil {
			return n, err
		}
		if st.State == HartNonExistent {
			break
		}
		n++
	}
	return n, nil
}

// Hart returns the selected hart index.
func (m *DM) Hart() uint32 { return m.hart }

func (r HartReport) String() string {
	return fmt.Sprintf("hart %d %s", r.Index, r.State)
}
