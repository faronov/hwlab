package esp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/riscv"
)

// ErrROMCall is returned when a call into the mask ROM did not return as
// expected. The hart's registers are put back either way; on a timeout the
// hart is halted again first.
var ErrROMCall = errors.New("rom call")

// ROMAttach is what AttachFlashROM did, for the caller's result.
type ROMAttach struct {
	Function   string `json:"function"`
	Address    string `json:"address"`
	Args       string `json:"args"`
	HaltedAtPC string `json:"halted_at_pc"`
	ReturnedMS int64  `json:"returned_ms"`
	Basis      string `json:"basis"`
}

// InDownloadLoader reports whether a halted pc is in the chip's mask ROM,
// which is where a hart sits while the ROM serial/USB download loader runs.
// It is part of the evidence AttachFlashROM requires; it does not by itself
// prove download mode.
func (c *Chip) InDownloadLoader(pc uint32) bool { return c.Classify(pc) == ClassROM }

// AttachFlashROM calls the mask ROM's esp_rom_spiflash_attach(0, false)
// (ROM symbol spi_flash_attach) on the halted hart, as the ROM download
// loader's SPI_ATTACH command does for esptool: it selects the SPI flash pad
// functions and configures the SPI0/SPI1 controllers for the default (eFuse
// 0) pin set. The ROM download loader does not attach the flash until a host
// asks it to, so until then every SPI1 command reads all-ones.
//
// The call is made with debug-module register access only: the caller-saved
// state (all GPRs, dpc, mstatus, dcsr) is saved, a0/a1 are set, ra is set to
// the halted pc, interrupts are masked (mstatus.MIE cleared), an execute
// breakpoint is placed on the return address and the hart is resumed at the
// function. It must halt at the return address with the stack pointer it
// started with; anything else is an error. Every saved register and the
// trigger slot are restored before returning. No target memory is written.
//
// The caller must have halted the hart, quiesced the watchdogs and checked
// that the pc is in ROM. The attach is not undone afterwards (as after
// esptool's SPI_ATTACH); the next reset clears it.
func AttachFlashROM(ctx context.Context, dm *riscv.DM, chip *Chip) (ROMAttach, error) {
	if chip.ROMSPIAttach == 0 {
		return ROMAttach{}, fmt.Errorf("%w: the %s profile has no ROM spi_flash_attach address", ErrROMCall, chip.Name)
	}
	regs := []riscv.RegNum{riscv.CSRDPC, riscv.CSRMStatus, riscv.CSRDCSR}
	for n := 1; n < 32; n++ {
		regs = append(regs, riscv.GPR(n))
	}
	saved, err := dm.ReadRegs(ctx, regs)
	if err != nil {
		return ROMAttach{}, err
	}
	pc, mstatus, dcsr, sp := saved[0], saved[1], saved[2], saved[4] // regs[4] is x2
	if !chip.InDownloadLoader(pc) {
		return ROMAttach{}, fmt.Errorf("%w: the hart is halted at %#x, outside the ROM: not the download loader", ErrROMCall, pc)
	}
	bg := context.WithoutCancel(ctx)
	restore := func() error {
		var first error
		for i, r := range regs {
			if r == riscv.CSRDCSR {
				continue
			}
			if err := dm.WriteReg(bg, r, saved[i]); err != nil && first == nil {
				first = err
			}
		}
		if err := dm.WriteReg(bg, riscv.CSRDCSR, dcsr); err != nil && first == nil {
			first = err
		}
		return first
	}
	bp, err := dm.SetBreakpoint(ctx, pc)
	if err != nil {
		return ROMAttach{}, fmt.Errorf("%w: no breakpoint for the return address: %w", ErrROMCall, err)
	}
	fail := func(e error) (ROMAttach, error) {
		if st, err := dm.Status(bg); err == nil && st.State != riscv.HartHalted {
			_, _ = dm.Halt(bg)
		}
		_ = dm.ClearBreakpoint(bg, bp)
		if rerr := restore(); rerr != nil {
			e = fmt.Errorf("%w (and restoring the registers failed: %w)", e, rerr)
		}
		return ROMAttach{}, e
	}
	for _, w := range []struct {
		r riscv.RegNum
		v uint32
	}{
		{riscv.GPR(10), 0}, // a0: ishspi = 0, the eFuse/default SPI pins
		{riscv.GPR(11), 0}, // a1: legacy = false
		{riscv.GPR(1), pc}, // ra: back to where the loader was halted
		{riscv.CSRMStatus, mstatus &^ (1 << 3)},
		{riscv.CSRDCSR, dcsr &^ (1 << 2)}, // no single step
		{riscv.CSRDPC, chip.ROMSPIAttach},
	} {
		if err := dm.WriteReg(ctx, w.r, w.v); err != nil {
			return fail(err)
		}
	}
	start := time.Now()
	if err := dm.ResumeNoWait(ctx); err != nil {
		return fail(err)
	}
	hi, err := dm.WaitHalt(ctx, 2*time.Second)
	if err != nil {
		return fail(fmt.Errorf("%w: spi_flash_attach did not return: %w", ErrROMCall, err))
	}
	took := time.Since(start)
	nsp, err := dm.ReadReg(ctx, riscv.GPR(2))
	if err != nil {
		return fail(err)
	}
	if hi.PC != pc || nsp != sp {
		return fail(fmt.Errorf("%w: spi_flash_attach stopped at pc %#x sp %#x (cause %s), expected the return to %#x with sp %#x", ErrROMCall, hi.PC, nsp, hi.CauseStr, pc, sp))
	}
	if err := dm.ClearBreakpoint(ctx, bp); err != nil {
		_ = restore()
		return ROMAttach{}, err
	}
	if err := restore(); err != nil {
		return ROMAttach{}, fmt.Errorf("%w: attached, but restoring the registers failed: %w", ErrROMCall, err)
	}
	return ROMAttach{
		Function: "spi_flash_attach (esp_rom_spiflash_attach)", Address: fmt.Sprintf("0x%08x", chip.ROMSPIAttach),
		Args: "ishspi=0, legacy=false", HaltedAtPC: fmt.Sprintf("0x%08x", pc), ReturnedMS: took.Milliseconds(),
		Basis: "the flash did not answer JEDEC (all-ones) with the hart halted in ROM, i.e. the ROM download loader had not attached it; the ROM attach was called as esptool's SPI_ATTACH does, and stays in effect until the next reset",
	}, nil
}
