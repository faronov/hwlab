package stm32dfuplugin

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

// STM32WB5x SRAM as the ROM bootloader leaves it (RM0434, AN3156).
const (
	sram1Lo = dfu.BootRAMHi // the bootloader uses SRAM from 0x20000000: the first 16 KiB stay untouched
	sram1Hi = 0x20030000    // SRAM2a (FUS/IPCC tables, secure part) starts here
	spLo    = 0x20000000    // initial SP: anywhere in SRAM1, the top included
	spHi    = sram1Hi
	// vtorAlign is the Cortex-M4 VTOR alignment for the STM32WB vector
	// table (more than 64 entries: 512 bytes).
	vtorAlign = 0x200

	maxWriteHex  = 4 << 10
	maxWriteFile = sram1Hi - sram1Lo // 176 KiB

	tokMemWrite = "write-memory"
	tokLeave    = "leave-dfu"
	// The bootloader jumps to the start of flash by default (AN3156 5.5).
	leaveAddr = flashBase
)

type writeReq struct {
	addr uint32
	data []byte
	src  string
}

// parseWrite validates a memory write before any device is opened.
func parseWrite(args map[string]any) (writeReq, error) {
	if _, ok := args["address"]; !ok {
		return writeReq{}, argErr(errors.New("address: required (SRAM1 0x20004000..0x2002FFFF)"))
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return writeReq{}, argErr(err)
	}
	hs, err := strArg(args, "hex")
	if err != nil {
		return writeReq{}, err
	}
	if alias, err := strArg(args, "data"); err != nil {
		return writeReq{}, err
	} else if alias != "" {
		if hs != "" {
			return writeReq{}, argErr(errors.New("pass hex or its alias data, not both"))
		}
		hs = alias
	}
	path, err := strArg(args, "path")
	if err != nil {
		return writeReq{}, err
	}
	var rq writeReq
	switch {
	case hs != "" && path != "":
		return writeReq{}, argErr(errors.New("pass either hex (inline) or path (a file), not both"))
	case hs != "":
		hs = strings.TrimPrefix(strings.Join(strings.Fields(hs), ""), "0x")
		if len(hs) > 2*maxWriteHex {
			return writeReq{}, argErr(fmt.Errorf("hex: at most %d bytes inline; pass path=<file> for up to %d bytes", maxWriteHex, maxWriteFile))
		}
		b, err := hex.DecodeString(hs)
		if err != nil {
			return writeReq{}, argErr(fmt.Errorf("hex: %w", err))
		}
		rq = writeReq{addr, b, "hex"}
	case path != "":
		st, err := os.Stat(path)
		if err != nil {
			return writeReq{}, argErr(err)
		}
		if !st.Mode().IsRegular() || st.Size() > maxWriteFile {
			return writeReq{}, argErr(fmt.Errorf("path %s: want a regular file of at most %d bytes (the SRAM1 window)", path, maxWriteFile))
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return writeReq{}, argErr(err)
		}
		rq = writeReq{addr, b, path}
	default:
		return writeReq{}, argErr(errors.New("pass hex=<bytes> (up to 4 KiB) or path=<file> (up to 176 KiB)"))
	}
	if len(rq.data) < dfu.MinXfer {
		return writeReq{}, argErr(fmt.Errorf("%d byte(s): a DfuSe download is at least %d bytes (AN3156)", len(rq.data), dfu.MinXfer))
	}
	if err := inSRAM1(addr, len(rq.data)); err != nil {
		return writeReq{}, err
	}
	return rq, nil
}

// inSRAM1 refuses any range that is not entirely inside SRAM1 above the
// bootloader's RAM.
func inSRAM1(addr uint32, n int) error {
	end := uint64(addr) + uint64(n)
	if uint64(addr) >= sram1Lo && end <= sram1Hi {
		return nil
	}
	what := "outside SRAM"
	switch {
	case overlaps(addr, n, dfu.BootRAMLo, sram1Lo-1):
		what = "touches the ROM bootloader's own RAM 0x20000000..0x20003FFF"
	case overlaps(addr, n, sram1Hi, 0x20037FFF):
		what = "touches SRAM2a (0x20030000-: CPU2/FUS tables, partly secure)"
	case overlaps(addr, n, 0x20038000, 0x2003FFFF):
		what = "touches SRAM2b (0x20038000-, partly secure)"
	case overlaps(addr, n, 0, flashRegionHi):
		what = "touches flash or its boot alias (use the FlashProgramming methods)"
	case overlaps(addr, n, periph1Lo, periph1Hi), overlaps(addr, n, periph2Lo, 0xFFFFFFFF):
		what = "touches the peripheral/system region"
	}
	return status.Errorf(codes.OutOfRange, "0x%08x+%d %s: memory writes go only to SRAM1 [0x%08x, 0x%08x)", addr, n, what, uint32(sram1Lo), uint32(sram1Hi))
}

// uploadRefused reports whether an upload failed because the bootloader
// does not allow the address (errTARGET or a plain stall), as opposed to a
// transport failure.
func uploadRefused(err error) bool {
	return errors.Is(err, dfu.ErrTarget) || (errors.Is(err, usb.ErrStall) && !errors.Is(err, dfu.ErrProtected))
}

func (s *session) writeMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseWrite(args)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.memWrite {
		return nil, denied("writing memory needs allow_memory_write=true in the device configuration")
	}
	if err := tokenIs(args, "confirm", tokMemWrite, "writing SRAM1 through the bootloader"); err != nil {
		return nil, err
	}
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	if !t.wb5x {
		return nil, status.Errorf(codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW: %s is not a recognised STM32WB5x (%s): the SRAM1 window is the STM32WB5x one; nothing was written", t.chip, t.idNote)
	}
	alt := s.flashAltSetting()
	if alt == nil {
		return nil, status.Error(codes.FailedPrecondition, "no flash alternate setting at 0x08000000 to download through")
	}
	if err := s.c.SelectAlt(ctx, alt.Alt); err != nil {
		return nil, err
	}
	s.c.SetRAMWindow(alt.Alt, sram1Lo, sram1Hi)
	werr := s.c.Write(ctx, rq.addr, rq.data)
	s.c.CloseRAMWindow()
	if werr != nil {
		if errors.Is(werr, dfu.ErrTarget) {
			return nil, status.Errorf(codes.FailedPrecondition, "the bootloader refused the SRAM download (errTARGET): %v; SRAM1 may be partly written", werr)
		}
		return nil, fmt.Errorf("SRAM1 may be partly written: %w", werr)
	}
	res := map[string]any{"written": true, "address": hexw(rq.addr), "end": hexw(rq.addr + uint32(len(rq.data))), "length": float64(len(rq.data)),
		"sha256": sha(rq.data), "source": rq.src, "region": "SRAM1", "alt_setting": float64(alt.Alt),
		"via": "DfuSe set-address + DNLOAD block 2 per transfer (AN3156), GETSTATUS twice each"}
	got, rerr := s.upload(ctx, rq.addr, len(rq.data))
	switch {
	case rerr != nil && uploadRefused(rerr):
		if e2 := s.c.EnsureIdle(ctx); e2 != nil {
			return nil, e2
		}
		res["verify"] = "unavailable"
		res["verify_note"] = "the bootloader refused UPLOAD from this SRAM range (" + rerr.Error() + "): the write was accepted by the device but NOT read back"
	case rerr != nil:
		return nil, fmt.Errorf("read-back after the write: %w", rerr)
	default:
		if i := firstDiff(rq.data, got); i >= 0 {
			return nil, status.Errorf(codes.DataLoss, "verify=mismatch: SRAM differs from the data first at 0x%08x (wrote 0x%02x, read 0x%02x); SRAM sha256 %s, data sha256 %s",
				rq.addr+uint32(i), rq.data[i], got[i], sha(got), sha(rq.data))
		}
		res["verify"] = "match"
	}
	return s.mark(res), nil
}

type leaveReq struct {
	base         uint32
	sp, pc       uint32
	assertedVecs bool
}

// parseLeave validates the leave arguments before any device is opened.
func parseLeave(args map[string]any) (leaveReq, error) {
	rq := leaveReq{base: leaveAddr}
	if v, ok := args["address"]; ok && v != nil {
		a, err := toAddr(v)
		if err != nil {
			return rq, argErr(fmt.Errorf("address: %w", err))
		}
		rq.base = a
	}
	vs, hasSP := args["vector_sp"]
	vp, hasPC := args["vector_pc"]
	hasSP, hasPC = hasSP && vs != nil, hasPC && vp != nil
	if hasSP != hasPC {
		return rq, argErr(errors.New("pass vector_sp and vector_pc together (the first two words of the image)"))
	}
	if hasSP {
		var err error
		if rq.sp, err = toAddr(vs); err != nil {
			return rq, argErr(fmt.Errorf("vector_sp: %w", err))
		}
		if rq.pc, err = toAddr(vp); err != nil {
			return rq, argErr(fmt.Errorf("vector_pc: %w", err))
		}
		rq.assertedVecs = true
	}
	if rq.base%vtorAlign != 0 {
		return rq, argErr(fmt.Errorf("address 0x%08x: the vector table base must be aligned to 0x%x", rq.base, vtorAlign))
	}
	if rq.base >= sram1Lo && rq.base < sram1Hi {
		return rq, nil
	}
	if rq.base >= flashBase && rq.base <= flashRegionHi {
		return rq, nil // checked against SFSA once the device is identified
	}
	return rq, status.Errorf(codes.OutOfRange, "address 0x%08x: leave jumps only to a vector table in the user flash below SFSA*4K or in SRAM1 [0x%08x, 0x%08x)", rq.base, uint32(sram1Lo), uint32(sram1Hi))
}

// region is where a vector table or entry point may lie.
type region struct {
	name   string
	lo, hi uint32 // [lo, hi)
}

func (r region) has(a uint32, n int) bool { return a >= r.lo && uint64(a)+uint64(n) <= uint64(r.hi) }

// checkVectors applies the jump rules: SP inside SRAM1 (its top included),
// 8-byte aligned; a Thumb reset vector inside the base's region.
func checkVectors(r region, sp, pc uint32) error {
	switch {
	case sp < spLo || sp > spHi:
		return status.Errorf(codes.FailedPrecondition, "initial SP 0x%08x is outside 0x%08x..0x%08x: not a vector table hwlab jumps to", sp, uint32(spLo), uint32(spHi))
	case sp%8 != 0:
		return status.Errorf(codes.FailedPrecondition, "initial SP 0x%08x is not 8-byte aligned (AAPCS)", sp)
	case pc&1 == 0:
		return status.Errorf(codes.FailedPrecondition, "reset vector 0x%08x has the Thumb bit clear: a Cortex-M jump would fault", pc)
	case !r.has(pc&^1, 2):
		return status.Errorf(codes.FailedPrecondition, "reset vector 0x%08x is outside the %s [0x%08x, 0x%08x) that holds the vector table", pc, r.name, r.lo, r.hi)
	}
	return nil
}

func (s *session) leave(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseLeave(args)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.reset {
		return nil, denied("leaving DFU needs allow_reset=true in the device configuration")
	}
	if err := tokenIs(args, "confirm", tokLeave, "leaving DFU (the bootloader jumps to the vector table)"); err != nil {
		return nil, err
	}
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	var r region
	if rq.base >= sram1Lo && rq.base < sram1Hi {
		if !t.wb5x {
			return nil, status.Errorf(codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW: %s is not a recognised STM32WB5x: SRAM1 is the STM32WB5x one", t.chip)
		}
		r = region{"SRAM1", sram1Lo, sram1Hi}
	} else {
		if !t.sfsaOK {
			return nil, status.Errorf(codes.FailedPrecondition, "the user-flash boundary is not established (%s): leave needs SFSA to check the vector table", t.sfsaWhy)
		}
		r = region{"user flash", flashBase, t.secure}
	}
	if !r.has(rq.base, 8) {
		return nil, status.Errorf(codes.OutOfRange, "address 0x%08x: the vector table must lie in the %s [0x%08x, 0x%08x)", rq.base, r.name, r.lo, r.hi)
	}
	src := "upload"
	vb, uerr := s.readAt(ctx, rq.base, 8)
	switch {
	case uerr == nil:
		sp, pc := binary.LittleEndian.Uint32(vb), binary.LittleEndian.Uint32(vb[4:])
		if rq.assertedVecs && (sp != rq.sp || pc != rq.pc) {
			return nil, status.Errorf(codes.FailedPrecondition, "the vector table at 0x%08x holds SP 0x%08x PC 0x%08x, not the asserted vector_sp 0x%08x vector_pc 0x%08x: nothing was sent", rq.base, sp, pc, rq.sp, rq.pc)
		}
		rq.sp, rq.pc = sp, pc
	case r.name == "SRAM1" && uploadRefused(uerr):
		if e2 := s.c.EnsureIdle(ctx); e2 != nil {
			return nil, e2
		}
		if !rq.assertedVecs {
			return nil, status.Errorf(codes.FailedPrecondition, "the bootloader refused UPLOAD of the vector table at 0x%08x (%v): pass vector_sp and vector_pc (the first two words of the image written there) to assert them", rq.base, uerr)
		}
		src = "caller-asserted"
	default:
		return nil, fmt.Errorf("reading the vector table at 0x%08x: %w", rq.base, uerr)
	}
	if err := checkVectors(r, rq.sp, rq.pc); err != nil {
		return nil, err
	}
	alt := s.flashAltSetting()
	if alt == nil {
		return nil, status.Error(codes.FailedPrecondition, "no flash alternate setting at 0x08000000 to leave through")
	}
	if err := s.c.SelectAlt(ctx, alt.Alt); err != nil {
		return nil, err
	}
	lr, err := s.c.Leave(ctx, rq.base)
	if err != nil {
		return nil, err
	}
	s.left = true
	res := map[string]any{
		"left_dfu": true, "jump_address": hexw(rq.base), "vector_table": hexw(rq.base), "region": r.name,
		"initial_sp": hexw(rq.sp), "reset_vector": hexw(rq.pc), "entry_point": hexw(rq.pc &^ 1),
		"vectors_source": src, "manifest_confirmed": lr.Manifest,
		"note": fmt.Sprintf("the bootloader was told to leave DFU and jump through the vector table at 0x%08x (MSP=0x%08x, PC=0x%08x); the device leaves USB. With BOOT0 still selected the next hardware reset or power cycle returns to the ROM bootloader", rq.base, rq.sp, rq.pc),
	}
	if src == "caller-asserted" {
		res["vectors_note"] = "the bootloader does not allow UPLOAD from this SRAM range: SP and PC were asserted by the caller (vector_sp/vector_pc), checked against the jump rules but NOT read from the device"
	}
	if !lr.Manifest {
		res["manifest_note"] = lr.Note
	}
	return s.mark(res), nil
}
