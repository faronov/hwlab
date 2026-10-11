package stm32

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
)

// STM32F1 flash interface registers (RM0008 "Embedded Flash memory").
const (
	f1Base  = 0x40022000
	f1KeyR  = f1Base + 0x04
	f1SR    = f1Base + 0x0C
	f1CR    = f1Base + 0x10
	f1AR    = f1Base + 0x14
	f1OBR   = f1Base + 0x1C
	f1WRPR  = f1Base + 0x20
	f1OptBy = 0x1FFFF800 // option bytes, read only here

	f1CRPG   = 1 << 0
	f1CRPER  = 1 << 1
	f1CRSTRT = 1 << 6
	f1CRLock = 1 << 7

	f1SRBSY    = 1 << 0
	f1SRPGErr  = 1 << 2
	f1SRWRPErr = 1 << 4
	f1SREOP    = 1 << 5
	f1SRErrs   = f1SRPGErr | f1SRWRPErr
	f1SRW1C    = f1SRErrs | f1SREOP

	// f1WRPGroup is the flash covered by one FLASH_WRPR bit (bit 31 covers
	// everything above).
	f1WRPGroup = 4096
)

// ErrNotErased means a halfword that must be programmed does not read as
// erased, so programming it would raise PGERR.
var ErrNotErased = errors.New("target halfword is not erased")

// The loader runs from SRAM. It is hwlab's own code, 19 Thumb instructions
// (verified with an independent disassembler):
//
//	0x00  movs r7, #1
//	0x02  str  r7, [r3, #0x10]    ; FLASH_CR = PG
//	0x04  cmp  r2, #0             ; loop: halfwords left?
//	0x06  beq  0x20
//	0x08  ldrh r4, [r1]
//	0x0a  strh r4, [r0]
//	0x0c  ldr  r4, [r3, #0xc]     ; busy: FLASH_SR
//	0x0e  lsrs r5, r4, #1         ; carry = BSY
//	0x10  bhs  0x0c
//	0x12  movs r5, #0x14          ; PGERR | WRPRTERR
//	0x14  tst  r4, r5
//	0x16  bne  0x20
//	0x18  adds r0, #2
//	0x1a  adds r1, #2
//	0x1c  subs r2, #1
//	0x1e  b    0x04
//	0x20  movs r5, #0             ; done / error: FLASH_CR = 0
//	0x22  str  r5, [r3, #0x10]
//	0x24  bkpt #0
//
// In: r0 destination, r1 source (SRAM), r2 halfword count, r3 0x40022000.
// Out: r2 == 0 on success; otherwise r0 is the failing address and r4 the
// FLASH_SR that stopped it.
var loaderHW = [...]uint16{
	0x2701, 0x611F, 0x2A00, 0xD00B, 0x880C, 0x8004, 0x68DC, 0x0865, 0xD2FC, 0x2514,
	0x422C, 0xD103, 0x3002, 0x3102, 0x3A01, 0xE7F1, 0x2500, 0x611D, 0xBE00,
}

const (
	loaderBase   = 0x20000000
	loaderBufOff = 0x40
	loaderEnd    = 0x24 // offset of the BKPT
)

// LoaderBytes returns the loader image.
func LoaderBytes() []byte {
	b := make([]byte, 2*len(loaderHW))
	for i, h := range loaderHW {
		binary.LittleEndian.PutUint16(b[2*i:], h)
	}
	return b
}

func (f *Flash) f1Wait(ctx context.Context) (uint32, error) {
	deadline := time.Now().Add(f.Timeout)
	for {
		sr, err := f.C.Bus.Read32(ctx, f1SR)
		if err != nil {
			return 0, err
		}
		if sr&f1SRBSY == 0 {
			return sr, nil
		}
		if time.Now().After(deadline) {
			return sr, fmt.Errorf("%w: FLASH_SR.BSY still set after %s (SR 0x%08x)", ErrController, f.Timeout, sr)
		}
		select {
		case <-ctx.Done():
			return sr, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func f1CheckSR(sr uint32, what string) error {
	if sr&f1SRErrs != 0 {
		return fmt.Errorf("%w: %s: FLASH_SR 0x%08x (PGERR=%d WRPRTERR=%d)", ErrController, what, sr, sr>>2&1, sr>>4&1)
	}
	return nil
}

func (f *Flash) f1Protection(ctx context.Context) (Protection, error) {
	var p Protection
	var err error
	if p.OBR, err = f.C.Bus.Read32(ctx, f1OBR); err != nil {
		return p, err
	}
	if p.WRPR, err = f.C.Bus.Read32(ctx, f1WRPR); err != nil {
		return p, err
	}
	if p.OptionBytes, err = f.C.Read(ctx, f1OptBy, 16); err != nil {
		return p, err
	}
	// OBR.RDPRT (bit 1) is the live readout protection. An option-byte RDP
	// that is not 0xA5 takes effect at the next reset, so it counts too.
	if p.OBR&2 != 0 || p.OptionBytes[0] != 0xA5 {
		p.RDP = 1
	}
	for _, s := range f.P.Sectors {
		g := (s.Addr - FlashBase) / f1WRPGroup
		if g > 31 {
			g = 31
		}
		if p.WRPR>>g&1 == 0 {
			p.WriteProtected = append(p.WriteProtected, s.Index)
		}
	}
	return p, nil
}

func (f *Flash) f1Unlock(ctx context.Context) error {
	cr, err := f.C.Bus.Read32(ctx, f1CR)
	if err != nil {
		return err
	}
	if cr&f1CRLock == 0 {
		return nil
	}
	if err := f.C.Bus.Write32(ctx, f1KeyR, key1); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f1KeyR, key2); err != nil {
		return err
	}
	if cr, err = f.C.Bus.Read32(ctx, f1CR); err != nil {
		return err
	}
	if cr&f1CRLock != 0 {
		return ErrLocked
	}
	return nil
}

func (f *Flash) f1Lock(ctx context.Context) error {
	return f.C.Bus.Write32(ctx, f1CR, f1CRLock)
}

// f1Begin checks a halted core and no readout protection, then unlocks the
// controller. OPTKEYR is never written.
func (f *Flash) f1Begin(ctx context.Context) (Protection, error) {
	st, err := f.C.Status(ctx)
	if err != nil {
		return Protection{}, err
	}
	if !st.Halted {
		return Protection{}, errors.New("the core must be halted before flash is modified")
	}
	prot, err := f.f1Protection(ctx)
	if err != nil {
		return prot, err
	}
	if prot.RDP != 0 {
		return prot, fmt.Errorf("%w: readout protection is active or pending (FLASH_OBR 0x%08x, option RDP byte 0x%02x); this tool never changes option bytes", ErrProtected, prot.OBR, prot.OptionBytes[0])
	}
	if _, err := f.f1Wait(ctx); err != nil {
		return prot, err
	}
	if err := f.C.Bus.Write32(ctx, f1SR, f1SRW1C); err != nil {
		return prot, err
	}
	return prot, f.f1Unlock(ctx)
}

func protectedIn(prot Protection, secs []Sector) (int, bool) {
	for _, s := range secs {
		for _, w := range prot.WriteProtected {
			if w == s.Index {
				return s.Index, true
			}
		}
	}
	return 0, false
}

// f1Erase erases one page (PER + AR + STRT) and checks it reads back erased.
func (f *Flash) f1Erase(ctx context.Context, s Sector) (err error) {
	prot, err := f.f1Begin(ctx)
	if err != nil {
		return err
	}
	if i, bad := protectedIn(prot, []Sector{s}); bad {
		return fmt.Errorf("%w: page %d is write-protected (FLASH_WRPR 0x%08x)", ErrProtected, i, prot.WRPR)
	}
	defer func() {
		if lerr := f.f1Lock(context.WithoutCancel(ctx)); lerr != nil && err == nil {
			err = fmt.Errorf("erase done but the controller could not be re-locked: %w", lerr)
		}
	}()
	if err := f.C.Bus.Write32(ctx, f1CR, f1CRPER); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f1AR, s.Addr); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f1CR, f1CRPER|f1CRSTRT); err != nil {
		return err
	}
	sr, err := f.f1Wait(ctx)
	if err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f1CR, 0); err != nil {
		return err
	}
	if err := f1CheckSR(sr, fmt.Sprintf("erase of page %d", s.Index)); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f1SR, f1SRW1C); err != nil {
		return err
	}
	data, err := f.C.Read(ctx, s.Addr, int(s.Size))
	if err != nil {
		return fmt.Errorf("page %d: erase finished but the read-back failed: %w", s.Index, err)
	}
	for i, b := range data {
		if b != 0xFF {
			return fmt.Errorf("%w: page %d: erase finished but 0x%08x reads 0x%02x", ErrVerify, s.Index, s.Addr+uint32(i), b)
		}
	}
	return nil
}

// loaderChunk is the most data one loader run programs; it is limited so the
// loader and buffer fit even the smallest F1 SRAM (4 KiB).
func (f *Flash) loaderChunk() int {
	if f.P.SRAMKB > 0 && f.P.SRAMKB < 4 {
		return 512
	}
	return 2048
}

// savedState is what the loader run disturbs: a few registers and the SRAM
// at its base. Both are put back afterwards.
type savedState struct {
	regs map[cortexm.RegisterID]uint32
	sram []byte
}

var loaderRegs = []cortexm.RegisterID{0, 1, 2, 3, 4, 5, 7, cortexm.RegPC, cortexm.RegXPSR}

func (f *Flash) saveState(ctx context.Context, n int) (*savedState, error) {
	st := &savedState{regs: map[cortexm.RegisterID]uint32{}}
	for _, r := range loaderRegs {
		v, err := f.C.ReadRegister(ctx, r)
		if err != nil {
			return nil, err
		}
		st.regs[r] = v
	}
	var err error
	if st.sram, err = f.C.Read(ctx, loaderBase, n); err != nil {
		return nil, err
	}
	return st, nil
}

func (f *Flash) restoreState(ctx context.Context, st *savedState) error {
	ctx = context.WithoutCancel(ctx)
	if err := f.C.Write(ctx, loaderBase, st.sram); err != nil {
		return err
	}
	for _, r := range loaderRegs {
		if err := f.C.WriteRegister(ctx, r, st.regs[r]); err != nil {
			return err
		}
	}
	// The BKPT that ended the loader is not a debug event the user caused.
	if err := f.C.ClearCause(ctx); err != nil {
		return err
	}
	_, err := f.C.Halt(ctx) // drops C_MASKINTS and keeps the core halted
	return err
}

// runLoader programs data (halfword aligned, non-empty) at dest with the
// core halted, using the SRAM already holding the loader.
func (f *Flash) runLoader(ctx context.Context, st *savedState, dest uint32, data []byte) error {
	if err := f.C.Write(ctx, loaderBase+loaderBufOff, data); err != nil {
		return err
	}
	xpsr := st.regs[cortexm.RegXPSR]
	set := []struct {
		r cortexm.RegisterID
		v uint32
	}{{0, dest}, {1, loaderBase + loaderBufOff}, {2, uint32(len(data) / 2)}, {3, f1Base}, {cortexm.RegPC, loaderBase}, {cortexm.RegXPSR, xpsr | 1<<24}}
	for _, w := range set {
		if err := f.C.WriteRegister(ctx, w.r, w.v); err != nil {
			return err
		}
	}
	if err := f.C.RunMasked(ctx); err != nil {
		return err
	}
	timeout := f.Timeout
	if _, err := f.C.WaitHalted(ctx, timeout); err != nil {
		_, _ = f.C.Halt(context.WithoutCancel(ctx))
		return fmt.Errorf("%w: the flash loader did not finish", ErrController)
	}
	pc, err := f.C.ReadRegister(ctx, cortexm.RegPC)
	if err != nil {
		return err
	}
	left, err := f.C.ReadRegister(ctx, 2)
	if err != nil {
		return err
	}
	if pc&^1 != loaderBase+loaderEnd {
		return fmt.Errorf("%w: the loader halted at 0x%08x, not at its BKPT 0x%08x", ErrController, pc&^1, uint32(loaderBase+loaderEnd))
	}
	if left != 0 {
		at, _ := f.C.ReadRegister(ctx, 0)
		sr, _ := f.C.ReadRegister(ctx, 4)
		return f1CheckSR(sr, fmt.Sprintf("programming at 0x%08x (%d halfwords left)", at, left))
	}
	return nil
}

// f1Program programs data at a halfword-aligned address. Halfwords already
// equal to the data are skipped, blank ones are not written, and a halfword
// that differs but is not erased is an error (the controller would raise
// PGERR).
func (f *Flash) f1Program(ctx context.Context, addr uint32, data []byte) (err error) {
	if addr&1 != 0 || len(data)&1 != 0 {
		return fmt.Errorf("%w: address 0x%08x, length %d (F1 programs halfwords)", ErrAlign, addr, len(data))
	}
	if !f.Contains(addr, len(data)) {
		return fmt.Errorf("%w: [0x%08x, +%d)", ErrRange, addr, len(data))
	}
	if len(data) == 0 {
		return nil
	}
	cur, err := f.C.Read(ctx, addr, len(data))
	if err != nil {
		return err
	}
	type run struct{ off, n int }
	var runs []run
	for i := 0; i < len(data); i += 2 {
		want, have := binary.LittleEndian.Uint16(data[i:]), binary.LittleEndian.Uint16(cur[i:])
		switch {
		case want == have, want == 0xFFFF:
			continue
		case have != 0xFFFF:
			return fmt.Errorf("%w: 0x%08x holds 0x%04x, the image wants 0x%04x: erase the page first", ErrNotErased, addr+uint32(i), have, want)
		}
		if n := len(runs); n > 0 && runs[n-1].off+runs[n-1].n == i {
			runs[n-1].n += 2
		} else {
			runs = append(runs, run{i, 2})
		}
	}
	if len(runs) == 0 {
		return nil
	}
	prot, err := f.f1Begin(ctx)
	if err != nil {
		return err
	}
	if i, bad := protectedIn(prot, f.P.Covering(addr, len(data))); bad {
		return fmt.Errorf("%w: page %d is write-protected (FLASH_WRPR 0x%08x)", ErrProtected, i, prot.WRPR)
	}
	chunk := f.loaderChunk()
	st, err := f.saveState(ctx, loaderBufOff+chunk)
	if err != nil {
		return err
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_ = f.C.Bus.Write32(ctx, f1SR, f1SRW1C)
		if lerr := f.f1Lock(ctx); lerr != nil && err == nil {
			err = fmt.Errorf("programming done but the controller could not be re-locked: %w", lerr)
		}
		if rerr := f.restoreState(ctx, st); rerr != nil && err == nil {
			err = fmt.Errorf("programming done but the core state could not be restored: %w", rerr)
		}
	}()
	if err := f.C.Write(ctx, loaderBase, LoaderBytes()); err != nil {
		return err
	}
	for _, r := range runs {
		for off := 0; off < r.n; off += chunk {
			n := min(chunk, r.n-off)
			at := r.off + off
			if err := f.runLoader(ctx, st, addr+uint32(at), data[at:at+n]); err != nil {
				return fmt.Errorf("programming 0x%08x: %w", addr+uint32(at), err)
			}
		}
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
