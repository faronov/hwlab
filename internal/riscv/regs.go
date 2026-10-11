package riscv

import (
	"context"
	"fmt"
)

// abstract command: cmdtype 0 (access register), aarsize 2 (32 bit), transfer.
func regCmd(r RegNum, write bool) uint32 {
	c := uint32(2)<<20 | 1<<17 | uint32(r)
	if write {
		c |= 1 << 16
	}
	return c
}

// ReadRegs reads 32-bit registers of the halted hart with pipelined Access
// Register commands. It never writes a register.
func (m *DM) ReadRegs(ctx context.Context, regs []RegNum) ([]uint32, error) {
	if len(regs) == 0 {
		return nil, nil
	}
	if err := m.requireHalted(ctx); err != nil {
		return nil, err
	}
	out := make([]uint32, 0, len(regs))
	// Keep each batch below a bounded size so a retry stays cheap.
	const perBatch = 24
	for start := 0; start < len(regs); start += perBatch {
		part := regs[start:min(start+perBatch, len(regs))]
		vals, err := m.readRegBatch(ctx, part)
		if err != nil {
			return nil, err
		}
		out = append(out, vals...)
	}
	return out, nil
}

func (m *DM) readRegBatch(ctx context.Context, regs []RegNum) ([]uint32, error) {
	for try := 0; ; try++ {
		ops := make([]DMIOp, 0, 3*len(regs))
		for _, r := range regs {
			ops = append(ops,
				DMIOp{Write: true, Addr: regCommand, Data: regCmd(r, false)},
				DMIOp{Addr: regAbstractCS},
				DMIOp{Addr: regData0})
		}
		res, err := m.dtm.Pipeline(ctx, ops)
		if err != nil {
			return nil, err
		}
		out := make([]uint32, len(regs))
		retry := false
		for i, r := range regs {
			acs := res[3*i+1]
			if e := acs >> 8 & 7; e != 0 {
				if err := m.dtm.Write(ctx, regAbstractCS, 7<<8); err != nil {
					return nil, err
				}
				if e == 1 && try < 3 {
					retry = true
					break
				}
				return nil, fmt.Errorf("reading register %#x: %w: cmderr %d (%s)", uint16(r), ErrAbstract, e, cmdErrName(e))
			}
			if acs>>12&1 == 1 {
				retry = true
				break
			}
			out[i] = res[3*i+2]
		}
		if !retry {
			return out, nil
		}
		if try >= 3 {
			return nil, fmt.Errorf("%w: command unit stayed busy", ErrAbstract)
		}
		m.dtm.idle = min(m.dtm.idle+2, 64)
		if err := m.abstractWait(ctx); err != nil {
			return nil, err
		}
	}
}

// WriteReg writes a 32-bit register of the halted hart.
func (m *DM) WriteReg(ctx context.Context, r RegNum, v uint32) error {
	if err := m.requireHalted(ctx); err != nil {
		return err
	}
	for try := 0; ; try++ {
		res, err := m.dtm.Pipeline(ctx, []DMIOp{
			{Write: true, Addr: regData0, Data: v},
			{Write: true, Addr: regCommand, Data: regCmd(r, true)},
			{Addr: regAbstractCS},
		})
		if err != nil {
			return err
		}
		acs := res[2]
		if e := acs >> 8 & 7; e != 0 {
			if err := m.dtm.Write(ctx, regAbstractCS, 7<<8); err != nil {
				return err
			}
			if e == 1 && try < 3 {
				m.dtm.idle = min(m.dtm.idle+2, 64)
				continue
			}
			return fmt.Errorf("writing register %#x: %w: cmderr %d (%s)", uint16(r), ErrAbstract, e, cmdErrName(e))
		}
		if acs>>12&1 == 1 {
			if try >= 3 {
				return fmt.Errorf("%w: command unit stayed busy", ErrAbstract)
			}
			if err := m.abstractWait(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}
