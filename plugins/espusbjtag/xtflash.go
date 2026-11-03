package espusbjtag

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/xtensa"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// ESP32-S3 flash: the same SPI1 driver, watchdog quiesce and eFuse gates as
// on the ESP32-C6, with every memory access done by a halted Xtensa core
// (esp.XtensaBus, batched through the debug module) and the chip profile
// selecting the S3 register map. The ROM download mode leaves the flash pins
// in GPIO mode; esp.OpenFlashChip switches them to the SPI function for the
// call and puts them back afterwards.

// xtFlashSession is flashSession for an ESP32-S3: the addressed core is
// halted (the chip profile can only be read through a halted core), any other
// running core is halted too so that nothing fetches from flash while SPI1 is
// borrowed, the watchdogs are quiesced and the SPI1 controller opened. A
// read-only call resumes what it halted; a modifying call leaves the cores
// halted, like the RISC-V path (resume with DebugProbe.resume).
func (s *session) xtFlashSession(ctx context.Context, x *xtTarget, args map[string]any, modify bool, fn func(*flashCtx) error) (err error) {
	c, h, err := s.coreFor(ctx, x, args)
	if err != nil {
		return err
	}
	wasHalted, err := xtAttach(ctx, c)
	if err != nil {
		return err
	}
	var halted []*xtensa.Core
	bus := esp.XtensaBus{C: c}
	fc := &flashCtx{bus: bus, hart: h, core: c}
	var q esp.Quiesced
	quiesced := false
	defer func() {
		bg := context.WithoutCancel(ctx)
		if fc.f != nil {
			if cerr := fc.f.Close(bg); cerr != nil && err == nil {
				err = fmt.Errorf("flash call done but the SPI1/IO_MUX registers were not restored: %w", cerr)
			}
		}
		if quiesced {
			if rerr := q.Restore(bg, bus); rerr != nil && err == nil {
				err = fmt.Errorf("flash call done but the watchdog state was not restored: %w", rerr)
			}
		}
		if modify {
			return
		}
		for i := len(halted) - 1; i >= 0; i-- {
			if rerr := halted[i].Resume(bg); rerr != nil && err == nil {
				err = fmt.Errorf("flash call done but a core did not resume: %w", rerr)
			}
		}
	}()
	if !wasHalted {
		if _, err := c.Halt(ctx); err != nil {
			return err
		}
		halted = append(halted, c)
	}
	for i, oc := range x.cores {
		if i == h {
			continue
		}
		if err := x.examine(ctx, i); err != nil {
			if errors.Is(err, xtensa.ErrNoDebugModule) || x.examined[i] == nil {
				// Held in reset or powered down (core 1 in ROM download
				// mode): it cannot fetch anything.
				continue
			}
			return err
		}
		on, err := xtAttach(ctx, oc)
		if err != nil {
			return err
		}
		if !on {
			if _, err := oc.Halt(ctx); err != nil {
				return fmt.Errorf("halting core %d for the flash call: %w", i, err)
			}
			halted = append(halted, oc)
		}
	}
	chip, err := s.xtChip(ctx, x, c)
	if err != nil {
		return err
	}
	fc.chip, fc.ev = chip, x.ev
	if q, err = esp.Quiesce(ctx, bus, chip); err != nil {
		return err
	}
	quiesced = true
	if fc.f, err = esp.OpenFlashChip(ctx, bus, chip); err != nil {
		return err
	}
	return s.flashPrepare(ctx, fc, modify, fn)
}

// EXTMEM data-cache registers of the ESP32-S3 (ESP-IDF soc/esp32s3
// extmem_reg.h): DCACHE_CTRL bit 0 DCACHE_ENABLE, DCACHE_CTRL1 bit n
// DCACHE_SHUT_COREn_BUS. MMU entries (ext_mem_defs.h): bit 14 invalid, bit 15
// external RAM, bits 0..13 the 64 KiB flash page; the data window 0x3C000000
// uses entry 0.
const (
	s3DCacheCtrl  = 0x600C4000
	s3DCacheCtrl1 = 0x600C4004
	s3DBusWindow  = 0x3C000000
	s3MMUInvalid  = 1 << 14
	s3MMUSPIRAM   = 1 << 15
	s3MMUPage     = 0x3FFF
)

// s3MappedWindow compares the start of the CPU's data-bus flash window with
// the physical flash page its MMU entry names, but only reads the window when
// the data cache is enabled, the core's bus is not shut and the entry is a
// valid flash page; otherwise it says why the window is not mapped (the
// normal state in ROM download mode). The instruction-bus window 0x42000000 is
// never read with data loads.
func (fc *flashCtx) s3MappedWindow(ctx context.Context) map[string]any {
	res := map[string]any{"window": fmt.Sprintf("0x%08x (data bus)", s3DBusWindow)}
	r, err := fc.bus.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusRead, Addr: s3DCacheCtrl, Words: 2},
		{Kind: riscv.BusRead, Addr: fc.chip.MMUTable, Words: 1},
	})
	if err != nil {
		res["error"] = err.Error()
		return res
	}
	ctrl, ctrl1, ent := r[0][0], r[0][1], r[1][0]
	res["dcache_ctrl"], res["dcache_ctrl1"], res["mmu_entry0"] = hex32(ctrl), hex32(ctrl1), hex32(ent)
	why := ""
	switch {
	case ctrl&1 == 0:
		why = "the data cache is disabled (EXTMEM DCACHE_CTRL bit 0 clear)"
	case ctrl1&(1<<uint(fc.hart)) != 0:
		why = fmt.Sprintf("core %d's data bus to the cache is shut (DCACHE_CTRL1 bit %d)", fc.hart, fc.hart)
	case ent&s3MMUInvalid != 0:
		why = "MMU entry 0 is invalid"
	case ent&s3MMUSPIRAM != 0:
		why = "MMU entry 0 maps external RAM, not flash"
	}
	if why != "" {
		res["mapped"], res["reason"] = false, why+": nothing to compare (normal in ROM download mode)"
		return res
	}
	page := (ent & s3MMUPage) * 0x10000
	res["mapped"], res["flash_offset"] = true, fmt.Sprintf("0x%x", page)
	if int(page)+256 > fc.info.SizeBytes {
		res["reason"] = "the entry points beyond the flash size"
		return res
	}
	phys, e1 := fc.f.Read(ctx, page, 256)
	win, e2 := fc.bus.ReadMem(ctx, s3DBusWindow, 256)
	if e := errors.Join(e1, e2); e != nil {
		res["error"] = e.Error()
		return res
	}
	res["window_equals_flash"] = bytes.Equal(phys, win)
	return res
}
