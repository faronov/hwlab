package stm32dfuplugin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

// STM32WB5x memory map (RM0434).
const (
	flashBase     = 0x08000000
	flashRegionHi = 0x0FFFFFFF // the flash region including its unused tail
	bootAliasHi   = 0x000FFFFF // boot alias: mirrors the boot memory
	pageSize      = 4096
	obBase        = 0x1FFF8000 // option bytes as stored (value, complement)
	obSize        = 0x80
	flashSizeAddr = 0x1FFF75E0 // FLASHSIZE_BASE, u16 KiB
	uid64Addr     = 0x1FFF7580 // UID64
	dbgmcuIDCODE  = 0xE0042000
	wbDevID       = 0x495
)

// Offsets of the option-byte double words (value, then its complement) in
// the 0x1FFF8000 area. They mirror the FLASH registers stlink reads:
// FLASH_OPTR 0x58004020, FLASH_SFR 0x58004080, FLASH_SRRVR 0x58004084.
var obWords = []struct {
	name string
	off  int
}{
	{"optr", 0x00}, {"pcrop1asr", 0x08}, {"pcrop1aer", 0x10}, {"wrp1ar", 0x18}, {"wrp1br", 0x20},
	{"pcrop1bsr", 0x28}, {"pcrop1ber", 0x30}, {"ipccbr", 0x68}, {"sfr", 0x70}, {"srrvr", 0x78},
}

// target is what the session learned about the chip. Nothing in it comes
// from a write.
type target struct {
	idcode   uint32
	devID    uint16
	revID    uint16
	idSource string
	idNote   string
	chip     string
	wb5x     bool

	flashKB       int
	flashKBSource string
	uid64         uint64
	uidOK         bool

	flashAlt *dfu.AltSetting
	obAlt    *dfu.AltSetting

	ob        []byte
	obErr     string
	obWord    map[string]uint32
	obCompOK  map[string]bool
	sfsa      uint32
	sfsaOK    bool
	sfsaWhy   string // why the boundary is not established
	secure    uint32 // SFSA*4K + flash base when sfsaOK
	userBytes int
}

// writable reports whether erase/program may run at all.
func (t *target) writable() error {
	switch {
	case !t.wb5x:
		return status.Errorf(codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW: %s is not a recognised STM32WB5x (%s): this plugin only modifies STM32WB5x user flash; nothing was written", t.chip, t.idNote)
	case !t.sfsaOK:
		return status.Errorf(codes.FailedPrecondition, "read-only: the STM32WB secure flash start (SFSA) is not established unambiguously (%s); every erase/program is refused", t.sfsaWhy)
	}
	return nil
}

// readAt uploads from any address through the alternate setting that maps
// it (the flash alt otherwise). It refuses flash at or above the secure
// start and the whole flash while the boundary is unknown.
func (s *session) readAt(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if s.tgt != nil {
		if err := s.tgt.readAllowed(addr, n); err != nil {
			return nil, err
		}
	} else if overlaps(addr, n, 0, flashRegionHi) {
		return nil, errors.New("internal: flash read before the secure boundary is known")
	}
	return s.upload(ctx, addr, n)
}

func (s *session) upload(ctx context.Context, addr uint32, n int) ([]byte, error) {
	alt := s.altCovering(addr, n)
	if alt == nil {
		alt = s.flashAltSetting()
	}
	if alt == nil {
		return nil, fmt.Errorf("%w: no alternate setting to read through", dfu.ErrProtocol)
	}
	if err := s.c.SelectAlt(ctx, alt.Alt); err != nil {
		return nil, err
	}
	return s.c.Read(ctx, addr, n)
}

func overlaps(addr uint32, n int, lo, hi uint64) bool {
	end := uint64(addr) + uint64(n) - 1
	return uint64(addr) <= hi && end >= lo
}

func (t *target) readAllowed(addr uint32, n int) error {
	if overlaps(addr, n, 0, bootAliasHi) {
		return status.Errorf(codes.InvalidArgument, "0x%08x+%d touches the boot alias at 0x00000000, which can mirror flash: read the real address instead", addr, n)
	}
	if !overlaps(addr, n, flashBase, flashRegionHi) {
		return nil
	}
	if !t.sfsaOK {
		return status.Errorf(codes.FailedPrecondition, "flash reads are refused while the secure flash start is unknown (%s)", t.sfsaWhy)
	}
	if addr < flashBase || uint64(addr)+uint64(n) > uint64(t.secure) {
		return status.Errorf(codes.OutOfRange, "0x%08x+%d is outside the STM32WB user flash 0x%08x..0x%08x: the secure area from SFSA 0x%02x (CPU2 FUS and wireless stack) is never read", addr, n, uint32(flashBase), t.secure, t.sfsa)
	}
	return nil
}

func (s *session) flashAltSetting() *dfu.AltSetting {
	for i := range s.cfg.Alts {
		a := &s.cfg.Alts[i]
		if int(a.Interface) == s.iface && a.Memory != nil && a.Memory.Start() == flashBase {
			return a
		}
	}
	return nil
}

// target identifies the chip and the secure boundary once per session and
// opens the client's write window only when both are certain.
func (s *session) target(ctx context.Context) (*target, error) {
	if s.tgt != nil {
		return s.tgt, nil
	}
	s.c.CloseWriteWindow()
	t := &target{obWord: map[string]uint32{}, obCompOK: map[string]bool{}}
	t.flashAlt = s.flashAltSetting()
	t.obAlt = s.altCovering(obBase, obSize)

	// Chip identity: DBGMCU_IDCODE when the ROM lets it be read.
	if b, err := s.upload(ctx, dbgmcuIDCODE, 4); err == nil {
		t.idcode = binary.LittleEndian.Uint32(b)
		t.devID, t.revID, t.idSource = uint16(t.idcode&0xFFF), uint16(t.idcode>>16), "DBGMCU_IDCODE (0xE0042000)"
	} else if errors.Is(err, usb.ErrDisconnected) {
		return nil, err
	} else {
		t.idNote = "DBGMCU_IDCODE unreadable through DFU: " + err.Error()
		if e2 := s.c.EnsureIdle(ctx); e2 != nil {
			return nil, e2
		}
	}
	if b, err := s.upload(ctx, flashSizeAddr, 2); err == nil {
		if kb := int(binary.LittleEndian.Uint16(b)); kb > 0 && kb <= 1024 {
			t.flashKB, t.flashKBSource = kb, "FLASHSIZE register (0x1FFF75E0)"
		}
	} else if errors.Is(err, usb.ErrDisconnected) {
		return nil, err
	} else if e2 := s.c.EnsureIdle(ctx); e2 != nil {
		return nil, e2
	}
	if t.flashKB == 0 && t.flashAlt != nil {
		t.flashKB, t.flashKBSource = int((t.flashAlt.Memory.End()-flashBase)>>10), "flash alternate-setting string"
	}
	if b, err := s.upload(ctx, uid64Addr, 8); err == nil {
		t.uid64, t.uidOK = binary.LittleEndian.Uint64(b), true
	} else if errors.Is(err, usb.ErrDisconnected) {
		return nil, err
	} else if e2 := s.c.EnsureIdle(ctx); e2 != nil {
		return nil, e2
	}
	mapWB := t.flashAlt != nil && t.obAlt != nil && t.flashAlt.Memory.Segments[0].PageSize == pageSize &&
		t.obAlt.Memory.Start() == obBase && t.obAlt.Memory.End()-obBase == obSize
	switch {
	case t.idSource != "" && t.devID == wbDevID:
		t.chip, t.wb5x = "STM32WB5x", true
	case t.idSource != "":
		t.chip = fmt.Sprintf("STM32 dev 0x%03x", t.devID)
		t.idNote = fmt.Sprintf("DBGMCU_IDCODE 0x%08x is dev 0x%03x, not the STM32WB5x 0x495", t.idcode, t.devID)
	case mapWB:
		t.chip, t.wb5x, t.devID, t.idSource = "STM32WB5x", true, wbDevID, "inferred from the DFU memory map (4 KiB flash pages at 0x08000000, 128-byte option bytes at 0x1FFF8000)"
	default:
		t.chip = "unknown STM32"
		t.idNote += "; the DFU memory map is not the STM32WB5x one either"
	}

	s.readSecureBoundary(ctx, t)
	if t.sfsaOK && t.wb5x && t.flashAlt != nil {
		seg, ok := t.flashAlt.Memory.Find(flashBase)
		if !ok || !seg.Erasable() || !seg.Writable() {
			t.sfsaOK, t.sfsaWhy = false, "the flash alternate setting does not mark the flash erasable and writable"
		} else {
			s.c.SetWriteWindow(t.flashAlt.Alt, flashBase, t.secure)
		}
	}
	s.tgt = t
	return t, nil
}

// readSecureBoundary reads FLASH_SFR from the option bytes, checks its
// complement and cross-checks the flash size and the flash alternate
// setting. Any doubt leaves sfsaOK false.
func (s *session) readSecureBoundary(ctx context.Context, t *target) {
	fail := func(format string, a ...any) { t.sfsaOK, t.sfsaWhy = false, fmt.Sprintf(format, a...) }
	if t.obAlt == nil {
		fail("there is no option-bytes alternate setting at 0x1FFF8000 (128 bytes) to read FLASH_SFR through")
		return
	}
	b, err := s.upload(ctx, obBase, obSize)
	if err != nil {
		t.obErr = err.Error()
		_ = s.c.EnsureIdle(ctx)
		fail("the option bytes could not be read: %v", err)
		return
	}
	t.ob = b
	for _, w := range obWords {
		v, c := binary.LittleEndian.Uint32(b[w.off:]), binary.LittleEndian.Uint32(b[w.off+4:])
		t.obWord[w.name], t.obCompOK[w.name] = v, v^c == 0xFFFFFFFF
	}
	sfr := t.obWord["sfr"]
	t.sfsa = sfr & 0xFF
	switch {
	case !t.obCompOK["sfr"]:
		fail("the FLASH_SFR option word 0x%08x at 0x1FFF8070 does not match its complement 0x%08x", sfr, binary.LittleEndian.Uint32(b[0x74:]))
		return
	case sfr&(1<<8) != 0:
		fail("FLASH_SFR 0x%08x has FSD set (secure flash disabled): not the expected STM32WB coprocessor layout", sfr)
		return
	case t.flashKB == 0:
		fail("the flash size is unknown")
		return
	case t.sfsa == 0 || t.sfsa*pageSize >= uint32(t.flashKB)<<10:
		fail("SFSA 0x%02x is outside the %d KiB flash", t.sfsa, t.flashKB)
		return
	}
	secure := flashBase + t.sfsa*pageSize
	if t.flashAlt == nil {
		fail("there is no flash alternate setting at 0x08000000")
		return
	}
	altEnd, flashEnd := t.flashAlt.Memory.End(), flashBase+uint32(t.flashKB)<<10
	if altEnd != flashEnd && altEnd != secure {
		fail("the flash alternate setting ends at 0x%08x, which is neither the secure start 0x%08x (SFSA 0x%02x) nor the end of the %d KiB flash 0x%08x", altEnd, secure, t.sfsa, t.flashKB, flashEnd)
		return
	}
	t.secure, t.userBytes, t.sfsaOK = secure, int(secure-flashBase), true
}

func (t *target) boundarySource() string {
	src := "FLASH_SFR option word at 0x1FFF8070 (complement at 0x1FFF8074 verified)"
	if t.flashAlt != nil && t.flashAlt.Memory.End() == t.secure {
		return src + "; the flash alternate setting ends at the same address"
	}
	return src + "; the flash alternate setting reports the whole flash (no contradiction)"
}

func rdpLevel(optr uint32) float64 {
	switch optr & 0xFF {
	case 0xAA:
		return 0
	case 0xCC:
		return 2
	}
	return 1
}

// facts reports the boundary and option bytes (read only).
func (t *target) facts() map[string]any {
	res := map[string]any{
		"chip": t.chip, "stm32wb5x": t.wb5x, "id_source": t.idSource,
		"flash_kb": float64(t.flashKB), "flash_size_source": t.flashKBSource,
		"secure_boundary_established": t.sfsaOK,
	}
	if t.idcode != 0 {
		res["dbgmcu_idcode"], res["dev_id"], res["rev_id"] = hexw(t.idcode), fmt.Sprintf("0x%03x", t.devID), fmt.Sprintf("0x%04x", t.revID)
	} else if t.wb5x {
		res["dev_id"] = fmt.Sprintf("0x%03x", t.devID)
	}
	if t.idNote != "" {
		res["id_note"] = t.idNote
	}
	if t.uidOK {
		res["uid64"] = fmt.Sprintf("0x%016x", t.uid64)
	}
	if t.ob != nil {
		ob := map[string]any{}
		for _, w := range obWords {
			ob[w.name] = hexw(t.obWord[w.name])
		}
		bad := []any{}
		for _, w := range obWords {
			if !t.obCompOK[w.name] {
				bad = append(bad, w.name)
			}
		}
		ob["complement_mismatch"] = bad
		res["option_bytes"] = ob
		optr, sfr, srrvr := t.obWord["optr"], t.obWord["sfr"], t.obWord["srrvr"]
		prot := map[string]any{
			"rdp_level": rdpLevel(optr), "rdp_byte": fmt.Sprintf("0x%02x", optr&0xFF),
			"wrp1a":      fmt.Sprintf("pages 0x%02x..0x%02x", t.obWord["wrp1ar"]&0xFF, t.obWord["wrp1ar"]>>16&0xFF),
			"wrp1b":      fmt.Sprintf("pages 0x%02x..0x%02x", t.obWord["wrp1br"]&0xFF, t.obWord["wrp1br"]>>16&0xFF),
			"wrp_active": t.obWord["wrp1ar"]&0xFF <= t.obWord["wrp1ar"]>>16&0xFF || t.obWord["wrp1br"]&0xFF <= t.obWord["wrp1br"]>>16&0xFF,
			"pcrop1a":    fmt.Sprintf("0x%03x..0x%03x", t.obWord["pcrop1asr"]&0x1FF, t.obWord["pcrop1aer"]&0x1FF),
			"pcrop1b":    fmt.Sprintf("0x%03x..0x%03x", t.obWord["pcrop1bsr"]&0x1FF, t.obWord["pcrop1ber"]&0x1FF),
			"sfsa":       fmt.Sprintf("0x%02x", sfr&0xFF), "fsd": sfr>>8&1 != 0, "dds": sfr>>12&1 != 0,
			"sram2a_secure_start": hexw(0x20030000 + (srrvr>>18&0x1F)<<10), "sram2b_secure_start": hexw(0x20038000 + (srrvr>>25&0x1F)<<10),
			"sbrv": hexw(srrvr & 0x3FFFF), "brsd": srrvr>>23&1 != 0, "nbrsd": srrvr>>30&1 != 0, "c2opt": srrvr>>31&1 != 0,
			"ipccdba":   hexw(t.obWord["ipccbr"] & 0x3FFF),
			"read_only": true,
		}
		res["protection"] = prot
	} else if t.obErr != "" {
		res["option_bytes_error"] = t.obErr
	}
	if t.sfsaOK {
		res["sfsa"] = fmt.Sprintf("0x%02x", t.sfsa)
		res["secure_base"] = hexw(t.secure)
		res["user_flash_bytes"] = float64(t.userBytes)
		res["sfsa_source"] = t.boundarySource()
		res["secure_area"] = "the CPU2 wireless coprocessor area (FUS and wireless stack); never read, erased or written by this plugin"
	} else {
		res["read_only_reason"] = t.sfsaWhy
		if t.ob != nil {
			res["sfsa_unconfirmed"] = fmt.Sprintf("0x%02x", t.sfsa)
		}
	}
	return res
}
