package stm32

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
)

// STM32WB5x flash interface (RM0434 "Embedded flash memory (FLASH)") and the
// few PWR bits the driver must read. Option bytes (OPTKEYR, OPTR writes,
// OPTSTRT, OBL_LAUNCH) are never written.
const (
	wbFlashBase = 0x58004000
	wbKeyR      = wbFlashBase + 0x08
	wbSR        = wbFlashBase + 0x10
	wbCR        = wbFlashBase + 0x14
	wbOPTR      = wbFlashBase + 0x20
	wbPCROP1ASR = wbFlashBase + 0x24
	wbPCROP1AER = wbFlashBase + 0x28
	wbWRP1AR    = wbFlashBase + 0x2C
	wbWRP1BR    = wbFlashBase + 0x30
	wbPCROP1BSR = wbFlashBase + 0x34
	wbPCROP1BER = wbFlashBase + 0x38
	wbIPCCBR    = wbFlashBase + 0x3C
	wbSFR       = wbFlashBase + 0x80
	wbSRRVR     = wbFlashBase + 0x84

	wbPWRCR4   = 0x5800040C
	wbC2BOOT   = 1 << 15
	wbPWREXTSC = 0x58000488

	wbPage = 4096

	wbCRPG   = 1 << 0
	wbCRPER  = 1 << 1
	wbCRPNB  = 3 // shift, 8 bits
	wbCRSTRT = 1 << 16
	wbCRLock = 1 << 31

	wbSREOP    = 1 << 0
	wbSROPERR  = 1 << 1
	wbSRPROG   = 1 << 3
	wbSRWRP    = 1 << 4
	wbSRPGA    = 1 << 5
	wbSRSIZ    = 1 << 6
	wbSRPGS    = 1 << 7
	wbSRMISS   = 1 << 8
	wbSRFAST   = 1 << 9
	wbSRRD     = 1 << 14
	wbSROPTV   = 1 << 15
	wbSRBSY    = 1 << 16
	wbSRCFGBSY = 1 << 18
	wbSRPESD   = 1 << 19
	wbSRErrs   = wbSROPERR | wbSRPROG | wbSRWRP | wbSRPGA | wbSRSIZ | wbSRPGS | wbSRMISS | wbSRFAST | wbSRRD
	// OPTVERR is cleared too: RM0434 notes it can be set after reset without
	// any option-byte operation, and ST's HAL clears it before every
	// operation. It is reported, never acted on.
	wbSRW1C = wbSRErrs | wbSREOP | wbSROPTV
)

// ErrCPU2Running means CPU2 may be executing (PWR_CR4.C2BOOT is set): CPU1
// flash operations would then have to arbitrate with it through HSEM, which
// this driver does not implement.
var ErrCPU2Running = errors.New("CPU2 (the wireless coprocessor) has been booted since the last reset")

// detectWB reads FLASH_SFR/SRRVR and builds the page list of the user area.
func detectWB(ctx context.Context, c *cortexm.Core, p *Profile) error {
	sfr, err := c.Bus.Read32(ctx, wbSFR)
	if err != nil {
		return fmt.Errorf("FLASH_SFR is unreadable: %w", err)
	}
	srrvr, err := c.Bus.Read32(ctx, wbSRRVR)
	if err != nil {
		return fmt.Errorf("FLASH_SRRVR is unreadable: %w", err)
	}
	end := uint32(FlashBase) + p.FlashSize()
	sec := FlashBase + (sfr&0xFF)*wbPage
	if sec > end {
		sec = end
	}
	if sec <= FlashBase {
		return fmt.Errorf("FLASH_SFR 0x%08x puts the secure area at the start of flash: there is no CPU1 user flash", sfr)
	}
	p.SFR, p.SRRVR, p.SecureBase = sfr, srrvr, sec
	p.PageSize = wbPage
	p.Sectors = f1Layout(int((sec-FlashBase)>>10), wbPage)
	p.SRAMKB = 192 // SRAM1; SRAM2a/b are reported by the FUS layer
	p.Programmable = true
	p.Note = fmt.Sprintf("STM32WB: only the CPU1 user flash 0x%08x..0x%08x (below FLASH_SFR.SFSA=0x%02x) is read, backed up or modified; "+
		"the CPU2 secure area above it is never touched. Flash is only modified while CPU2 has not been booted since reset (PWR_CR4.C2BOOT=0). "+
		"Option bytes are never written", uint32(FlashBase), sec, sfr&0xFF)
	return nil
}

// WBProtection is the decoded STM32WB protection state (read only).
type WBProtection struct {
	OPTR, WRP1AR, WRP1BR              uint32
	PCROP1ASR, PCROP1AER              uint32
	PCROP1BSR, PCROP1BER              uint32
	IPCCBR, SFR, SRRVR, PWRCR4, SRNow uint32
}

func (f *Flash) wbProtection(ctx context.Context) (Protection, error) {
	var w WBProtection
	for _, r := range []struct {
		addr uint32
		dst  *uint32
	}{
		{wbOPTR, &w.OPTR}, {wbWRP1AR, &w.WRP1AR}, {wbWRP1BR, &w.WRP1BR},
		{wbPCROP1ASR, &w.PCROP1ASR}, {wbPCROP1AER, &w.PCROP1AER}, {wbPCROP1BSR, &w.PCROP1BSR}, {wbPCROP1BER, &w.PCROP1BER},
		{wbIPCCBR, &w.IPCCBR}, {wbSFR, &w.SFR}, {wbSRRVR, &w.SRRVR}, {wbPWRCR4, &w.PWRCR4}, {wbSR, &w.SRNow},
	} {
		v, err := f.C.Bus.Read32(ctx, r.addr)
		if err != nil {
			return Protection{}, err
		}
		*r.dst = v
	}
	p := Protection{WB: &w}
	switch byte(w.OPTR) {
	case 0xAA:
		p.RDP = 0
	case 0xCC:
		p.RDP = 2
	default:
		p.RDP = 1
	}
	// WRP areas are page ranges [STRT, END]; an area with STRT > END is off.
	for _, ar := range []uint32{w.WRP1AR, w.WRP1BR} {
		lo, hi := int(ar&0xFF), int(ar>>16&0xFF)
		for i := lo; i <= hi && i < len(f.P.Sectors); i++ {
			p.WriteProtected = append(p.WriteProtected, i)
		}
	}
	p.PCROPActive = w.PCROP1ASR&0x1FF <= w.PCROP1AER&0x1FF || w.PCROP1BSR&0x1FF <= w.PCROP1BER&0x1FF
	return p, nil
}

// WBSecureBase re-reads FLASH_SFR now. FUS operations move it.
func WBSecureBase(ctx context.Context, c *cortexm.Core, flashBytes uint32) (uint32, uint32, error) {
	sfr, err := c.Bus.Read32(ctx, wbSFR)
	if err != nil {
		return 0, 0, err
	}
	sec := FlashBase + (sfr&0xFF)*wbPage
	if end := FlashBase + flashBytes; sec > end {
		sec = end
	}
	return sec, sfr, nil
}

// CPU2Booted reports PWR_CR4.C2BOOT. A system reset clears it, and CPU2 does
// not execute before CPU1 sets it again.
func CPU2Booted(ctx context.Context, c *cortexm.Core) (bool, error) {
	v, err := c.Bus.Read32(ctx, wbPWRCR4)
	if err != nil {
		return false, err
	}
	return v&wbC2BOOT != 0, nil
}

func (f *Flash) wbWait(ctx context.Context) (uint32, error) {
	deadline := time.Now().Add(f.Timeout)
	for {
		sr, err := f.C.Bus.Read32(ctx, wbSR)
		if err != nil {
			return 0, err
		}
		if sr&(wbSRBSY|wbSRCFGBSY|wbSRPESD) == 0 {
			return sr, nil
		}
		if time.Now().After(deadline) {
			return sr, fmt.Errorf("%w: FLASH_SR busy (BSY=%d CFGBSY=%d PESD=%d) after %s (SR 0x%08x)", ErrController,
				sr>>16&1, sr>>18&1, sr>>19&1, f.Timeout, sr)
		}
		select {
		case <-ctx.Done():
			return sr, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func wbCheckSR(sr uint32, what string) error {
	if sr&wbSRErrs != 0 {
		return fmt.Errorf("%w: %s: FLASH_SR 0x%08x (OPERR=%d PROGERR=%d WRPERR=%d PGAERR=%d SIZERR=%d PGSERR=%d MISSERR=%d FASTERR=%d RDERR=%d)",
			ErrController, what, sr, sr>>1&1, sr>>3&1, sr>>4&1, sr>>5&1, sr>>6&1, sr>>7&1, sr>>8&1, sr>>9&1, sr>>14&1)
	}
	return nil
}

func (f *Flash) wbUnlock(ctx context.Context) error {
	cr, err := f.C.Bus.Read32(ctx, wbCR)
	if err != nil {
		return err
	}
	if cr&wbCRLock == 0 {
		return nil
	}
	if err := f.C.Bus.Write32(ctx, wbKeyR, key1); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, wbKeyR, key2); err != nil {
		return err
	}
	if cr, err = f.C.Bus.Read32(ctx, wbCR); err != nil {
		return err
	}
	if cr&wbCRLock != 0 {
		return ErrLocked
	}
	return nil
}

func (f *Flash) wbLock(ctx context.Context) error {
	return f.C.Bus.Write32(ctx, wbCR, wbCRLock)
}

// wbBegin checks: CPU1 halted, CPU2 not booted, no RDP, no PCROP, the secure
// boundary unchanged since detection; then waits for the controller and
// unlocks it.
func (f *Flash) wbBegin(ctx context.Context) (Protection, error) {
	st, err := f.C.Status(ctx)
	if err != nil {
		return Protection{}, err
	}
	if !st.Halted {
		return Protection{}, errors.New("the core must be halted before flash is modified")
	}
	prot, err := f.wbProtection(ctx)
	if err != nil {
		return prot, err
	}
	if prot.WB.PWRCR4&wbC2BOOT != 0 {
		return prot, fmt.Errorf("%w (PWR_CR4 0x%08x): it may access flash concurrently, and this driver does not arbitrate through HSEM. "+
			"Do a system reset with halt=true first (that clears C2BOOT and keeps CPU2 stopped), then retry", ErrCPU2Running, prot.WB.PWRCR4)
	}
	if prot.RDP != 0 {
		return prot, fmt.Errorf("%w: readout protection level %d is active (FLASH_OPTR 0x%08x); this tool never changes option bytes", ErrProtected, prot.RDP, prot.WB.OPTR)
	}
	if prot.PCROPActive {
		return prot, fmt.Errorf("%w: a PCROP area is configured (PCROP1A 0x%03x..0x%03x, PCROP1B 0x%03x..0x%03x): not modelled, refusing", ErrProtected,
			prot.WB.PCROP1ASR&0x1FF, prot.WB.PCROP1AER&0x1FF, prot.WB.PCROP1BSR&0x1FF, prot.WB.PCROP1BER&0x1FF)
	}
	if sec := FlashBase + (prot.WB.SFR&0xFF)*wbPage; min(sec, FlashBase+f.P.FlashSize()) != f.P.SecureBase {
		return prot, fmt.Errorf("%w: the secure flash start moved from 0x%08x to 0x%08x since detection (FLASH_SFR 0x%08x)", ErrProtected, f.P.SecureBase, sec, prot.WB.SFR)
	}
	if _, err := f.wbWait(ctx); err != nil {
		return prot, err
	}
	if err := f.C.Bus.Write32(ctx, wbSR, wbSRW1C); err != nil {
		return prot, err
	}
	return prot, f.wbUnlock(ctx)
}

func (f *Flash) wbInUser(addr uint32, n int) bool {
	return n >= 0 && addr >= FlashBase && uint64(addr)+uint64(n) <= uint64(f.P.SecureBase)
}

// wbErase erases one 4 KiB page (PER + PNB + STRT) and checks it reads back
// erased.
func (f *Flash) wbErase(ctx context.Context, s Sector) (err error) {
	if s.Size != wbPage || (s.Addr-FlashBase)%wbPage != 0 || !f.wbInUser(s.Addr, int(s.Size)) {
		return fmt.Errorf("%w: page 0x%08x+%d is not a user page below the secure start 0x%08x", ErrRange, s.Addr, s.Size, f.P.SecureBase)
	}
	prot, err := f.wbBegin(ctx)
	if err != nil {
		return err
	}
	// The page number comes from the address, not from the caller's index.
	if i, bad := protectedIn(prot, []Sector{{Index: int((s.Addr - FlashBase) / wbPage)}}); bad {
		return fmt.Errorf("%w: page %d is write-protected (WRP1AR 0x%08x WRP1BR 0x%08x)", ErrProtected, i, prot.WB.WRP1AR, prot.WB.WRP1BR)
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_ = f.C.Bus.Write32(ctx, wbCR, 0)
		if lerr := f.wbLock(ctx); lerr != nil && err == nil {
			err = fmt.Errorf("erase done but the controller could not be re-locked: %w", lerr)
		}
	}()
	page := (s.Addr - FlashBase) / wbPage
	cr := uint32(wbCRPER) | page<<wbCRPNB
	if err := f.C.Bus.Write32(ctx, wbCR, cr); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, wbCR, cr|wbCRSTRT); err != nil {
		return err
	}
	sr, err := f.wbWait(ctx)
	if err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, wbCR, 0); err != nil {
		return err
	}
	if err := wbCheckSR(sr, fmt.Sprintf("erase of page %d", page)); err != nil {
		return err
	}
	data, err := f.C.Read(ctx, s.Addr, int(s.Size))
	if err != nil {
		return fmt.Errorf("page %d: erase finished but the read-back failed: %w", page, err)
	}
	for i, b := range data {
		if b != 0xFF {
			return fmt.Errorf("%w: page %d: erase finished but 0x%08x reads 0x%02x", ErrVerify, page, s.Addr+uint32(i), b)
		}
	}
	return nil
}

// wbProgram programs 64-bit double-words. A double-word that already holds
// the wanted value is skipped; one that must change has to read as erased
// (RM0434: programming a non-erased double-word raises PROGERR).
func (f *Flash) wbProgram(ctx context.Context, addr uint32, data []byte) (err error) {
	if addr&7 != 0 || len(data)&7 != 0 {
		return fmt.Errorf("%w: address 0x%08x, length %d (STM32WB programs 64-bit double-words)", ErrAlign, addr, len(data))
	}
	if !f.wbInUser(addr, len(data)) {
		return fmt.Errorf("%w: [0x%08x, +%d) is not inside the user flash 0x%08x..0x%08x", ErrRange, addr, len(data), uint32(FlashBase), f.P.SecureBase)
	}
	if len(data) == 0 {
		return nil
	}
	cur, err := f.C.Read(ctx, addr, len(data))
	if err != nil {
		return err
	}
	var todo []int
	for i := 0; i < len(data); i += 8 {
		want, have := binary.LittleEndian.Uint64(data[i:]), binary.LittleEndian.Uint64(cur[i:])
		switch {
		case want == have:
			continue
		case have != ^uint64(0):
			return fmt.Errorf("%w: 0x%08x holds 0x%016x, the image wants 0x%016x: erase the page first", ErrNotErased, addr+uint32(i), have, want)
		}
		todo = append(todo, i)
	}
	if len(todo) == 0 {
		return nil
	}
	prot, err := f.wbBegin(ctx)
	if err != nil {
		return err
	}
	if i, bad := protectedIn(prot, f.P.Covering(addr, len(data))); bad {
		return fmt.Errorf("%w: page %d is write-protected", ErrProtected, i)
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_ = f.C.Bus.Write32(ctx, wbCR, 0)
		if lerr := f.wbLock(ctx); lerr != nil && err == nil {
			err = fmt.Errorf("programming done but the controller could not be re-locked: %w", lerr)
		}
	}()
	if err := f.C.Bus.Write32(ctx, wbCR, wbCRPG); err != nil {
		return err
	}
	for _, i := range todo {
		a := addr + uint32(i)
		// Two 32-bit writes in ascending order: the controller starts the
		// programming when the second word of the double-word arrives.
		if err := f.C.Bus.Write32(ctx, a, binary.LittleEndian.Uint32(data[i:])); err != nil {
			return fmt.Errorf("programming 0x%08x: %w", a, err)
		}
		if err := f.C.Bus.Write32(ctx, a+4, binary.LittleEndian.Uint32(data[i+4:])); err != nil {
			return fmt.Errorf("programming 0x%08x: %w", a+4, err)
		}
		sr, err := f.wbWait(ctx)
		if err != nil {
			return err
		}
		if err := wbCheckSR(sr, fmt.Sprintf("programming 0x%08x", a)); err != nil {
			return err
		}
	}
	if err := f.C.Bus.Write32(ctx, wbCR, 0); err != nil {
		return err
	}
	got, err := f.C.Read(ctx, addr, len(data))
	if err != nil {
		return fmt.Errorf("programming finished but the read-back failed: %w", err)
	}
	for i := range data {
		if got[i] != data[i] {
			return fmt.Errorf("%w: 0x%08x wrote 0x%02x, read 0x%02x", ErrVerify, addr+uint32(i), data[i], got[i])
		}
	}
	return nil
}

// NeedsErase reports whether turning cur into want needs the page erased
// first. F4 can clear bits in place; STM32WB can only program a double-word
// that reads erased, so any change to a non-erased double-word needs one.
func (f *Flash) NeedsErase(cur, want []byte) bool {
	if f.P.Family == WB {
		for i := 0; i+8 <= len(cur) && i+8 <= len(want); i += 8 {
			c, w := binary.LittleEndian.Uint64(cur[i:]), binary.LittleEndian.Uint64(want[i:])
			if c != w && c != ^uint64(0) {
				return true
			}
		}
		return false
	}
	for i := range want {
		if want[i]&^cur[i] != 0 {
			return true
		}
	}
	return false
}
