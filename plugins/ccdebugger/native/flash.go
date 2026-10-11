package native

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/flashplan"
	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// Provenance: the flash-controller procedure below is derived from the TI
// CC253x/4x User's Guide (SWRU191F: flash controller, DMA and CRC chapters),
// the BSD-3-Clause cc2530-zigbee debug tools and real-hardware observation.
// No cc-tool (GPLv2) code is copied or translated. See native/NOTICE.

func timeoutErr() error { return fmt.Errorf("%w: %w", ErrTransport, ErrTimeout) }

func (d *Debugger) overdue(dl *deadline) error {
	if _, err := dl.remaining(); err != nil {
		return timeoutErr()
	}
	return nil
}

// ChipErase erases the whole chip: all flash, the lock bits (clearing a debug
// lock) and the secondary IEEE address. It is destructive and is never
// followed by a resume; the debug session must be re-established afterwards.
func (d *Debugger) ChipErase(opt ChipEraseOptions) (res ChipEraseResult, err error) {
	if opt.Confirm != ConfirmChipErase {
		return res, policyf("chip erase requires the confirmation %q", ConfirmChipErase)
	}
	start := d.cfg.Now()
	err = d.target(need{chip: true}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		res.StatusBefore = Status(v)
		res.LockedBefore = res.StatusBefore&DebugLocked != 0
		if res.StatusBefore&ChipEraseBusy != 0 {
			return errPreflightf("a chip erase is already in progress (status=0x%02x)", v)
		}
		if e := d.writePacket([]byte{0x1C, cmdChipErase}, dl); e != nil {
			return e
		}
		// The target's state is unknown from here on, whatever happens next.
		d.ready, d.dmaEligible, d.needsReset, d.geom = false, false, true, nil
		res.ReattachFirst = true
		for res.PollCount = 1; ; res.PollCount++ {
			d.cfg.Sleep(chipEraseWait)
			if res.PollCount > pollMaxIter {
				return timeoutErr()
			}
			if e := d.overdue(dl); e != nil {
				return e
			}
			v, e := d.exchangeByte(cmdReadStatus, dl)
			if e != nil {
				return e
			}
			res.StatusAfter = Status(v)
			if res.StatusAfter&ChipEraseBusy == 0 {
				break
			}
		}
		if res.StatusAfter&DebugLocked != 0 {
			return fmt.Errorf("%w: %w: debug lock still set after erase (status=0x%02x)", ErrTransport, ErrEraseFailed, uint8(res.StatusAfter))
		}
		return nil
	})
	res.Elapsed = d.cfg.Now().Sub(start)
	return
}

// flashOp runs fn on the halted CPU with RAM-staging permissions, DPS cleared
// and the CPU state verified unchanged afterwards. Any flash operation marks
// the session as needing a reset before the CPU executes again.
func (d *Debugger) flashOp(n need, fn func(dl *deadline) error) error {
	n.mem, n.write = true, true
	return d.stopped(n, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			d.needsReset = true
			return fn(dl)
		})
	})
}

func lenBytes(n int) []byte { return []byte{byte(n >> 8 & 0x1F), byte(n)} }

// descriptors for the burst-to-RAM channel (1), the RAM-to-flash channel (2)
// and the CRC channel (0). Layout per TI: SRC(2) DEST(2) LEN(2) cfg(2).
func descCh1(n int) []byte {
	return append(append([]byte{0x62, 0x60, byte(scratchBuf >> 8), byte(scratchBuf & 0xFF)}, lenBytes(n)...), 0x1F, 0x12)
}
func descCh2(n int) []byte {
	return append(append([]byte{byte(scratchBuf >> 8), byte(scratchBuf & 0xFF), 0x62, 0x73}, lenBytes(n)...), 0x12, 0x42)
}
func descCRC(src uint16, n int) []byte {
	return append(append([]byte{byte(src >> 8), byte(src), 0x70, 0xBD}, lenBytes(n)...), 0x20, 0x42)
}

// flashPrep checks the flash controller is idle, the clock is slow enough
// and DMA is not paused, then installs the staging DMA descriptors.
func (d *Debugger) flashPrep(dl *deadline) error {
	cfg, err := d.exchangeByte(cmdReadConfig, dl)
	if err != nil {
		return err
	}
	if cfg&0x04 != 0 {
		return errPreflightf("DMA is paused in the debug configuration (0x%02x); run enable_dma right after reset-attach", cfg)
	}
	if clk, err := d.sfrGet(0x9E, dl); err != nil { // CLKCONSTA, read-only status
		return err
	} else if clk&7 > 1 {
		return errPreflightf("CLKCONSTA.CLKSPD=%d: flash writes need the 32 MHz or 16 MHz clock", clk&7)
	}
	fctl, err := d.xget(regFCTL, 1, false, dl)
	if err != nil {
		return err
	}
	if fctl[0]&fctlBusy != 0 {
		return errPreflightf("flash controller is busy (FCTL=0x%02x)", fctl[0])
	}
	if err := d.xput(scratchDesc, descCh1(burstMax), dl); err != nil {
		return err
	}
	if err := d.xput(scratchDesc+8, descCh2(burstMax), dl); err != nil {
		return err
	}
	for _, w := range [][2]byte{{sfrDMA1CFGL, byte(scratchDesc & 0xFF)}, {sfrDMA1CFGH, byte(scratchDesc >> 8)}} {
		if err := d.sfrPut(w[0], w[1], dl); err != nil {
			return err
		}
	}
	got, err := d.xget(scratchDesc, 16, false, dl)
	if err != nil {
		return err
	}
	if !bytes.Equal(got[:8], descCh1(burstMax)) || !bytes.Equal(got[8:], descCh2(burstMax)) {
		return transportf("DMA descriptor readback mismatch")
	}
	return nil
}

// fctlWait polls FCTL until the controller is idle, then checks ABORT.
func (d *Debugger) fctlWait(dl *deadline, fail error) error {
	for i := 0; i < pollMaxIter; i++ {
		if err := d.overdue(dl); err != nil {
			return err
		}
		b, err := d.xget(regFCTL, 1, false, dl)
		if err != nil {
			return err
		}
		if b[0]&fctlBusy != 0 {
			continue
		}
		if b[0]&fctlAbort != 0 {
			return fmt.Errorf("%w: %w: flash controller aborted (FCTL=0x%02x), page may be locked", ErrTransport, fail, b[0])
		}
		return nil
	}
	return timeoutErr()
}

func (d *Debugger) erasePage(g target.Geometry, page int, dl *deadline) error {
	w, err := g.FlashWordAddr(page * g.PageSize)
	if err != nil {
		return err
	}
	if err := d.xput(regFADDRL, []byte{byte(w), byte(w >> 8)}, dl); err != nil {
		return err
	}
	if err := d.xput1(regFCTL, fctlErase, dl); err != nil {
		return err
	}
	return d.fctlWait(dl, ErrEraseFailed)
}

// blank reports the first non-0xFF byte offset in flash [addr, addr+n), or -1.
func (d *Debugger) blank(addr, n int, dl *deadline) (int, error) {
	for off := 0; off < n; {
		_, o := target.Bank(addr + off)
		c := min(n-off, flashChunk, target.BankSize-o)
		b, err := d.flashRead(addr+off, c, dl)
		if err != nil {
			return 0, err
		}
		for i, v := range b {
			if v != 0xFF {
				return off + i, nil
			}
		}
		off += c
	}
	return -1, nil
}

func (d *Debugger) erasePages(g target.Geometry, pages []int, dl *deadline) error {
	for _, pg := range pages {
		if err := d.erasePage(g, pg, dl); err != nil {
			return err
		}
		bad, err := d.blank(pg*g.PageSize, g.PageSize, dl)
		if err != nil {
			return err
		}
		if bad >= 0 {
			return fmt.Errorf("%w: %w: page %d not blank at +0x%X after erase", ErrTransport, ErrEraseFailed, pg, bad)
		}
	}
	return nil
}

func (d *Debugger) writeBlock(g target.Geometry, addr int, data []byte, cur *int, dl *deadline) error {
	n := len(data)
	if *cur != n {
		for _, w := range []struct {
			at   uint16
			desc []byte
		}{{scratchDesc + 4, lenBytes(n)}, {scratchDesc + 8 + 4, lenBytes(n)}} {
			if err := d.xput(w.at, w.desc, dl); err != nil {
				return err
			}
		}
		*cur = n
	}
	w, err := g.FlashWordAddr(addr)
	if err != nil {
		return err
	}
	if err := d.sfrPut(sfrDMAARM, 0x02, dl); err != nil {
		return err
	}
	if err := d.burstWrite(data, dl); err != nil {
		return err
	}
	if err := d.dmaWait(0x02, dl); err != nil {
		return err
	}
	if err := d.xput(regFADDRL, []byte{byte(w), byte(w >> 8)}, dl); err != nil {
		return err
	}
	if err := d.sfrPut(sfrDMAARM, 0x04, dl); err != nil {
		return err
	}
	if err := d.xput1(regFCTL, fctlWrite, dl); err != nil {
		return err
	}
	if err := d.fctlWait(dl, ErrWriteFailed); err != nil {
		return err
	}
	return d.dmaWait(0x04, dl)
}

// dmaWait polls DMAARM until the given channel bit clears (transfer done).
func (d *Debugger) dmaWait(mask byte, dl *deadline) error {
	for i := 0; i < pollMaxIter; i++ {
		if err := d.overdue(dl); err != nil {
			return err
		}
		v, err := d.sfrGet(sfrDMAARM, dl)
		if err != nil {
			return err
		}
		if v&mask == 0 {
			return nil
		}
	}
	return timeoutErr()
}

// ErasePages erases the given pages of main flash and blank-checks them. The
// lock-bit page and pages protected by their lock bits are refused.
func (d *Debugger) ErasePages(pages []int, opt ErasePagesOptions) (res ErasePagesResult, err error) {
	if opt.Confirm != ConfirmErasePages {
		return res, policyf("page erase requires the confirmation %q", ConfirmErasePages)
	}
	g, err := d.geometry()
	if err != nil {
		return res, err
	}
	if !g.Programmable {
		return res, invalidf("flash erase is not implemented for %s", g.Chip)
	}
	if len(pages) == 0 || len(pages) > g.Pages {
		return res, invalidf("page list must contain 1..%d pages", g.Pages)
	}
	pages = append([]int(nil), pages...)
	sort.Ints(pages)
	for i, pg := range pages {
		if pg < 0 || pg >= g.LockPage() || (i > 0 && pages[i-1] == pg) {
			return res, invalidf("page %d is invalid, duplicated or the lock-bit page (use write_config)", pg)
		}
	}
	lock, err := d.ReadLock()
	if err != nil {
		return res, err
	}
	for _, pg := range pages {
		if lock.PageLocked(pg) {
			return res, policyf("page %d is write-protected by its lock bit; only a chip erase clears it", pg)
		}
	}
	for i := 0; i < len(pages); i += groupPages {
		grp := pages[i:min(i+groupPages, len(pages))]
		err = d.flashOp(need{fErase: true}, func(dl *deadline) error {
			if err := d.flashPrep(dl); err != nil {
				return err
			}
			return d.erasePages(g, grp, dl)
		})
		if err != nil {
			return res, err
		}
		if opt.Progress != nil {
			opt.Progress("erase", i+len(grp), len(pages))
		}
	}
	res.Pages, res.BlankCheck = pages, true
	return res, nil
}

// planCheck re-validates a plan against the live chip and lock state so a
// hand-built or stale plan cannot write outside what Build allowed.
func planCheck(plan flashplan.Plan, live target.Geometry, lock target.Lock) error {
	if plan.Geometry != live {
		return errPreflightf("plan was built for %s (id 0x%02X) but the connected chip is %s (id 0x%02X)",
			plan.Geometry.Chip, plan.Geometry.ChipID, live.Chip, live.ChipID)
	}
	if !live.Programmable {
		return errPreflightf("flash programming is not implemented for %s", live.Chip)
	}
	if _, err := flashplan.ParseMode(string(plan.Mode)); err != nil {
		return errPreflightf("%v", err)
	}
	if plan.ChipErase && plan.Mode != flashplan.FullChipImage {
		return errPreflightf("chip-erase plans require full-chip-image mode")
	}
	if len(plan.Blocks) == 0 {
		return errPreflightf("plan has no blocks to program")
	}
	erased := map[int]bool{}
	for _, pg := range plan.ErasePages {
		if pg < 0 || pg >= live.Pages {
			return errPreflightf("erase page %d outside 0..%d", pg, live.Pages-1)
		}
		if !plan.ChipErase && lock.PageLocked(pg) {
			return errPreflightf("page %d is write-protected by its lock bit", pg)
		}
		erased[pg] = true
	}
	lockStart, macStart := live.LockPageAddr(), live.SecondaryIEEEAddr()
	prev := -1
	for _, b := range plan.Blocks {
		n := len(b.Data)
		if n == 0 || n > flashplan.BlockSize || b.Addr%target.WordSize != 0 || n%target.WordSize != 0 {
			return errPreflightf("block 0x%X+%d is empty, too large or not word aligned", b.Addr, n)
		}
		if b.Addr < prev {
			return errPreflightf("blocks are not ascending and disjoint")
		}
		prev = b.Addr + n
		if b.Addr < 0 || b.Addr+n > live.FlashSize {
			return errPreflightf("block 0x%X+%d is outside the %d-byte flash", b.Addr, n, live.FlashSize)
		}
		first, last, err := live.PageRange(b.Addr, n)
		if err != nil || first != last {
			return errPreflightf("block 0x%X+%d crosses a flash page", b.Addr, n)
		}
		if !plan.ChipErase && !erased[first] {
			return errPreflightf("block 0x%X lies in page %d, which the plan does not erase", b.Addr, first)
		}
		if b.Addr+n > lockStart {
			switch {
			case plan.Mode == flashplan.PreserveDeviceConfig:
				return errPreflightf("preserve-device-config forbids writing the lock-bit page")
			case !plan.TouchesConfig:
				return errPreflightf("block in the lock-bit page but the plan does not declare a configuration write")
			case plan.Mode == flashplan.PreserveMAC && b.Addr < macStart+target.IEEESize && b.Addr+n > macStart:
				return errPreflightf("preserve-mac forbids overwriting the secondary IEEE address")
			}
		}
	}
	if !plan.ChipErase {
		for _, pg := range plan.ErasePages {
			if pg == live.LockPage() && plan.Mode == flashplan.PreserveDeviceConfig {
				return errPreflightf("preserve-device-config forbids erasing the lock-bit page")
			}
		}
	}
	return nil
}

// Program programs a validated plan: preflight against the live chip, back up
// preserved ranges, erase the planned pages, program blocks through the
// debug-interface burst and flash-controller DMA path, then verify. A default
// (readback) verification is always performed unless VerifyNone is explicitly
// requested, in which case Verified is false. The target must be reset before
// it runs code again; Program never resumes it.
func (d *Debugger) Program(plan flashplan.Plan, opt ProgramOptions) (res ProgramResult, err error) {
	if opt.Confirm != ConfirmProgram {
		return res, policyf("flash program requires the confirmation %q", ConfirmProgram)
	}
	if plan.TouchesConfig && opt.ConfirmConfig != ConfirmConfig {
		return res, policyf("the plan modifies lock bits or the secondary IEEE address and requires the confirmation %q", ConfirmConfig)
	}
	if opt.Verify == "" {
		opt.Verify = VerifyReadback
	}
	if _, err := ParseVerify(string(opt.Verify)); err != nil {
		return res, err
	}
	start := d.cfg.Now()
	res.Chip, res.Mode, res.Verify = plan.Geometry.Chip, plan.Mode, opt.Verify
	live, err := d.ReadChipInfo()
	if err != nil {
		return res, err
	}
	lock, err := d.ReadLock()
	if err != nil {
		return res, err
	}
	if lock.DebugLocked {
		return res, fmt.Errorf("%w: %w: debug interface is locked", ErrTransport, ErrTargetLocked)
	}
	if err := planCheck(plan, live.Geometry, lock); err != nil {
		return res, err
	}
	g := live.Geometry
	blocks := append([]flashplan.Block(nil), plan.Blocks...)
	for _, r := range plan.Preserve {
		saved, err := d.ReadFlashBytes(r.Addr, r.Len)
		if err != nil {
			return res, err
		}
		blocks = append(blocks, flashplan.Block{Addr: r.Addr, Data: saved})
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Addr < blocks[j].Addr })
	// Preserved data replaces, never duplicates, image bytes (Build rejects overlap).
	for i := 1; i < len(blocks); i++ {
		if blocks[i].Addr < blocks[i-1].Addr+len(blocks[i-1].Data) {
			return res, errPreflightf("preserved range overlaps an image block")
		}
	}
	pages := plan.ErasePages
	if plan.ChipErase {
		seen := map[int]bool{}
		for _, b := range blocks {
			seen[b.Addr/g.PageSize] = true
		}
		pages = nil
		for pg := range seen {
			pages = append(pages, pg)
		}
		sort.Ints(pages)
	}
	if err := d.flashOp(need{fErase: true, fProg: true, cfg: plan.TouchesConfig}, func(dl *deadline) error {
		return d.flashPrep(dl)
	}); err != nil {
		return res, err
	}
	for i := 0; i < len(pages); i += groupPages {
		grp := pages[i:min(i+groupPages, len(pages))]
		if plan.ChipErase {
			err = d.flashOp(need{fProg: true, cfg: plan.TouchesConfig}, func(dl *deadline) error {
				for _, pg := range grp {
					if bad, e := d.blank(pg*g.PageSize, g.PageSize, dl); e != nil {
						return e
					} else if bad >= 0 {
						return errPreflightf("page %d is not blank after the chip erase (+0x%X)", pg, bad)
					}
				}
				return nil
			})
		} else {
			err = d.flashOp(need{fErase: true, cfg: plan.TouchesConfig}, func(dl *deadline) error {
				return d.erasePages(g, grp, dl)
			})
		}
		if err != nil {
			return res, err
		}
		res.ErasedPages = append(res.ErasedPages, grp...)
		if opt.Progress != nil {
			opt.Progress("erase", i+len(grp), len(pages))
		}
	}
	cur := burstMax
	for i := 0; i < len(blocks); i += groupBlocks {
		grp := blocks[i:min(i+groupBlocks, len(blocks))]
		err = d.flashOp(need{fProg: true, cfg: plan.TouchesConfig}, func(dl *deadline) error {
			for _, b := range grp {
				if allFFBytes(b.Data) {
					continue
				}
				if e := d.writeBlock(g, b.Addr, b.Data, &cur, dl); e != nil {
					return e
				}
				res.BlocksProgrammed++
				res.BytesProgrammed += len(b.Data)
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		if opt.Progress != nil {
			opt.Progress("program", i+len(grp), len(blocks))
		}
	}
	res.SkippedBlocks = plan.SkippedBlocks
	res.ResetRequired = true
	if opt.Verify != VerifyNone {
		if res.CRCs, err = d.verifyBlocks(g, blocks, opt.Verify, opt.Progress); err != nil {
			return res, err
		}
		res.Verified = true
	}
	res.Elapsed = d.cfg.Now().Sub(start)
	return res, nil
}

func allFFBytes(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return false
		}
	}
	return true
}

// VerifyFlash verifies flash ranges against expected blocks without writing
// to flash. It needs only memory access, not any flash permission.
func (d *Debugger) VerifyFlash(blocks []flashplan.Block, mode VerifyMode, progress Progress) (crcs int, err error) {
	g, err := d.geometry()
	if err != nil {
		return 0, err
	}
	if mode == VerifyNone {
		return 0, invalidf("verify mode none verifies nothing")
	}
	for _, b := range blocks {
		if len(b.Data) == 0 || len(b.Data) > flashplan.BlockSize || b.Addr < 0 || b.Addr+len(b.Data) > g.FlashSize {
			return 0, invalidf("verify block 0x%X+%d outside the flash or larger than %d bytes", b.Addr, len(b.Data), flashplan.BlockSize)
		}
		if first, last, e := g.PageRange(b.Addr, len(b.Data)); e != nil || first != last {
			return 0, invalidf("verify block 0x%X+%d crosses a flash page", b.Addr, len(b.Data))
		}
	}
	crcs, err = d.verifyBlocks(g, blocks, mode, progress)
	if errors.Is(err, ErrVerify) {
		// A read-only comparison that finds a difference reports data, it does
		// not leave the target in an unknown state; keep the session usable so
		// the caller can still reset and resume the target.
		d.mu.Lock()
		if d.state == StateFaulted {
			d.state = StateOpen
		}
		d.mu.Unlock()
	}
	return crcs, err
}

// verifyBlocks checks blocks by readback, CRC or both. Readback and CRC use
// separate bounded operations; the readback comparison is authoritative.
func (d *Debugger) verifyBlocks(g target.Geometry, blocks []flashplan.Block, mode VerifyMode, progress Progress) (crcs int, err error) {
	for i := 0; i < len(blocks); i += groupBlocks {
		grp := blocks[i:min(i+groupBlocks, len(blocks))]
		if mode == VerifyReadback || mode == VerifyBoth {
			err = d.stopped(need{mem: true}, func(dl *deadline) error {
				return d.preserve(dl, func() error {
					if _, e := d.instruction([]byte{0x75, 0x92, 0x00}, dl); e != nil {
						return e
					}
					for _, b := range grp {
						got, e := d.flashRead(b.Addr, len(b.Data), dl)
						if e != nil {
							return e
						}
						if j := firstDiff(got, b.Data); j >= 0 {
							return fmt.Errorf("%w: %w: readback differs at 0x%X (read 0x%02X, expected 0x%02X)",
								ErrTransport, ErrVerify, b.Addr+j, got[j], b.Data[j])
						}
					}
					return nil
				})
			})
			if err != nil {
				return crcs, err
			}
		}
		if mode == VerifyCRC || mode == VerifyBoth {
			err = d.flashOp(need{}, func(dl *deadline) error {
				for _, b := range grp {
					want := flashplan.CRC16(b.Data)
					got, e := d.crcFlash(b.Addr, len(b.Data), dl)
					if e != nil {
						return e
					}
					if got != want {
						return fmt.Errorf("%w: %w: CRC16 of 0x%X+%d is 0x%04X, expected 0x%04X",
							ErrTransport, ErrVerify, b.Addr, len(b.Data), got, want)
					}
					crcs++
				}
				return nil
			})
			if err != nil {
				return crcs, err
			}
		}
		if progress != nil {
			progress("verify", i+len(grp), len(blocks))
		}
	}
	return crcs, nil
}

func firstDiff(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}

// crcFlash computes the CRC-16 of flash [addr, addr+n) with the CC2530's
// hardware CRC (DMA channel 0 into RNDH from the XBANK flash window). The
// range must be inside one bank.
func (d *Debugger) crcFlash(addr, n int, dl *deadline) (uint16, error) {
	bank, off := target.Bank(addr)
	if off+n > target.BankSize || n < 1 || n > flashplan.BlockSize {
		return 0, invalidf("CRC range must stay in one bank and be at most %d bytes", flashplan.BlockSize)
	}
	memctr, err := d.sfrGet(sfrMEMCTR, dl)
	if err != nil {
		return 0, err
	}
	if memctr&0x08 != 0 {
		return 0, errPreflightf("MEMCTR.XMAP is set; refusing CRC over the flash window")
	}
	sel := memctr&0xF8 | byte(bank)
	if err := d.sfrPut(sfrMEMCTR, sel, dl); err != nil {
		return 0, err
	}
	desc := descCRC(uint16(xbankWindow+off), n)
	if err := d.xput(scratchDesc+0x20, desc, dl); err != nil {
		return 0, err
	}
	for _, w := range [][2]byte{{sfrDMA0CFGL, byte((scratchDesc + 0x20) & 0xFF)}, {sfrDMA0CFGH, byte((scratchDesc + 0x20) >> 8)},
		{sfrRNDL, 0xFF}, {sfrRNDL, 0xFF}} {
		if err := d.sfrPut(w[0], w[1], dl); err != nil {
			return 0, err
		}
	}
	if err := d.sfrPut(sfrDMAARM, 0x01, dl); err != nil {
		return 0, err
	}
	if err := d.sfrPut(sfrDMAREQ, 0x01, dl); err != nil {
		return 0, err
	}
	if err := d.dmaWait(0x01, dl); err != nil {
		return 0, err
	}
	lo, err := d.sfrGet(sfrRNDL, dl)
	if err != nil {
		return 0, err
	}
	hi, err := d.sfrGet(sfrRNDH, dl)
	if err != nil {
		return 0, err
	}
	if err := d.sfrPut(sfrMEMCTR, memctr, dl); err != nil {
		return 0, err
	}
	if v, err := d.sfrGet(sfrMEMCTR, dl); err != nil {
		return 0, err
	} else if v != memctr {
		return 0, transportf("MEMCTR restoration failed after CRC; do not resume")
	}
	return uint16(hi)<<8 | uint16(lo), nil
}

const xbankWindow = 0x8000

// ConfigChange selects device-identity fields to rewrite. Nil fields stay as
// they are.
type ConfigChange struct {
	SecondaryIEEE []byte // 8 bytes, LSB first as stored
	Lock          []byte // the whole 16-byte lock-bit structure
}

// ConfigOptions controls WriteDeviceConfig.
type ConfigOptions struct {
	Confirm      string // ConfirmConfig
	ConfirmLock  string // ConfirmDebugLock, needed when the result debug-locks the target
	Verify       VerifyMode
	BackupBefore func(lockPage []byte) error // mandatory; failure aborts before any erase
}

// WriteDeviceConfig rewrites the secondary IEEE address and/or lock bits. It
// is the only path that writes them: read the lock page, call BackupBefore,
// overlay the change, erase the lock-bit page, program it, verify. A power
// loss between erase and program loses the page, so the backup is mandatory.
func (d *Debugger) WriteDeviceConfig(ch ConfigChange, opt ConfigOptions) (ProgramResult, error) {
	var res ProgramResult
	if opt.Confirm != ConfirmConfig {
		return res, policyf("device-configuration write requires the confirmation %q", ConfirmConfig)
	}
	if opt.BackupBefore == nil {
		return res, policyf("device-configuration write requires a backup callback")
	}
	if ch.SecondaryIEEE == nil && ch.Lock == nil {
		return res, invalidf("nothing to change")
	}
	if ch.SecondaryIEEE != nil && len(ch.SecondaryIEEE) != target.IEEESize {
		return res, invalidf("secondary IEEE address must be %d bytes", target.IEEESize)
	}
	if ch.Lock != nil && len(ch.Lock) != target.LockSize {
		return res, invalidf("lock structure must be %d bytes", target.LockSize)
	}
	live, err := d.ReadChipInfo()
	if err != nil {
		return res, err
	}
	g := live.Geometry
	page, err := d.ReadFlashBytes(g.LockPageAddr(), g.PageSize)
	if err != nil {
		return res, err
	}
	cur, err := target.DecodeLock(g, page[len(page)-target.LockSize:])
	if err != nil {
		return res, err
	}
	if cur.DebugLocked {
		return res, fmt.Errorf("%w: %w: debug interface is locked", ErrTransport, ErrTargetLocked)
	}
	if err := opt.BackupBefore(append([]byte(nil), page...)); err != nil {
		return res, policyf("backup failed, nothing was changed: %v", err)
	}
	next := append([]byte(nil), page...)
	if ch.SecondaryIEEE != nil {
		copy(next[len(next)-target.LockSize-target.IEEESize:], ch.SecondaryIEEE)
	}
	if ch.Lock != nil {
		copy(next[len(next)-target.LockSize:], ch.Lock)
	}
	if next[len(next)-1]&0x80 == 0 && opt.ConfirmLock != ConfirmDebugLock {
		return res, policyf("this change debug-locks the target; it requires the confirmation %q", ConfirmDebugLock)
	}
	if bytes.Equal(next, page) {
		return res, invalidf("requested configuration equals the current one")
	}
	img, err := flashplan.FromBinary(next, g.LockPageAddr())
	if err != nil {
		return res, err
	}
	plan, err := flashplan.Build(g, &cur, img, flashplan.Options{Mode: flashplan.FullChipImage})
	if err != nil {
		return res, err
	}
	return d.Program(plan, ProgramOptions{Verify: opt.Verify, Confirm: ConfirmProgram, ConfirmConfig: ConfirmConfig})
}
