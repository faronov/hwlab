package cc23xx

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
	"github.com/faronov/hwlab/internal/ti/cc23xx/saci"
)

// SectorSize is the MAIN flash sector size (SWCU193: 2 KiB sectors).
const SectorSize = saci.SectorSize

// verifySpan caps the sectors one VERIFY_MAIN command covers.
const verifySpan = 16

// Geometry describes the flash of the attached device. Sizes come from the
// VIMS DESCEX register, not from a part-number table.
type Geometry struct {
	MainBase   uint32 `json:"main_base"`
	MainBytes  uint32 `json:"main_bytes"`
	SectorSize int    `json:"sector_bytes"`
	Sectors    int    `json:"sectors"`
	Banks      int    `json:"banks"`
	CCFGBase   uint32 `json:"ccfg_base"`
	CCFGBytes  int    `json:"ccfg_bytes"`
	Program    int    `json:"program_granule_bytes"`
	Note       string `json:"note"`
}

// Geometry derives the geometry from an identity read with the AHB-AP open.
func (id *Identity) Geometry() (Geometry, error) {
	if id.FlashBytes == 0 {
		return Geometry{}, errf(CodeAHBAPUnavailable, nil, "flash size is read through the AHB-AP, which is closed (BOOTSTA 0x%02x)", id.Boot.BootSta)
	}
	return Geometry{
		MainBase: FlashBase, MainBytes: id.FlashBytes, SectorSize: SectorSize, Sectors: int(id.FlashBytes) / SectorSize,
		Banks: id.FlashBanks, CCFGBase: ccfg.BaseAddr, CCFGBytes: ccfg.Size, Program: saci.ProgramGranule,
		Note: "protected sectors (CCFG flashProt) are enforced by SACI and reported as SECTOR_PROTECTED; their bitmaps are not decoded",
	}, nil
}

// ReadFlash reads MAIN flash through the AHB-AP.
func (s *Session) ReadFlash(ctx context.Context, g Geometry, addr uint32, n int) ([]byte, error) {
	if n < 0 || uint64(addr)+uint64(n) > uint64(g.MainBase)+uint64(g.MainBytes) || addr < g.MainBase {
		return nil, errf(CodeUnsafeRequest, nil, "range %#x+%d is outside MAIN flash", addr, n)
	}
	const chunk = 4096
	out := make([]byte, 0, n)
	for len(out) < n {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l := n - len(out)
		if l > chunk {
			l = chunk
		}
		b, err := s.T.ReadMem(addr+uint32(len(out)), l)
		if err != nil {
			return nil, errf(CodeAHBAPUnavailable, err, "reading flash at %#x", addr+uint32(len(out)))
		}
		out = append(out, b...)
	}
	return out, nil
}

// ReadCCFG reads the CCFG sector through the AHB-AP.
func (s *Session) ReadCCFG(ctx context.Context) ([]byte, error) {
	b, err := s.T.ReadMem(ccfg.BaseAddr, ccfg.Size)
	if err != nil {
		return nil, errf(CodeAHBAPUnavailable, err, "reading the CCFG sector")
	}
	return b, nil
}

// SectorPlan is one sector of a programming plan.
type SectorPlan struct {
	Sector int    `json:"sector"`
	Addr   uint32 `json:"addr"`
	// Data is the span to program: the non-erased part of the sector, aligned
	// to the programming granule. Empty when the sector stays erased.
	Data     []byte `json:"-"`
	DataAddr uint32 `json:"data_addr"`
	Bytes    int    `json:"bytes"`
	// Image is the full 2 KiB sector content expected after programming.
	Image []byte `json:"-"`
	CRC   uint32 `json:"crc32"`
}

// Plan is the erase/program/verify plan for an image on an erased device.
type Plan struct {
	Base         uint32       `json:"base"`
	Length       int          `json:"length"`
	FirstSector  int          `json:"first_sector"`
	LastSector   int          `json:"last_sector"`
	Sectors      []SectorPlan `json:"sectors"`
	ProgramBytes int          `json:"program_bytes"`
	BlankSectors int          `json:"blank_sectors"`
	SHA256       string       `json:"sha256,omitempty"`
}

// PlanImage splits image, placed at base, into sectors for programming onto
// an erased MAIN flash. It never touches the CCFG: a range outside MAIN is an
// error.
func PlanImage(g Geometry, base uint32, image []byte) (*Plan, error) {
	if len(image) == 0 {
		return nil, errf(CodeUnsafeRequest, nil, "empty image")
	}
	end := uint64(base) + uint64(len(image))
	if base < g.MainBase || end > uint64(g.MainBase)+uint64(g.MainBytes) {
		return nil, errf(CodeUnsafeRequest, nil, "image %#x+%d does not fit MAIN flash %#x+%#x (the CCFG is programmed only by its own operation)", base, len(image), g.MainBase, g.MainBytes)
	}
	p := &Plan{Base: base, Length: len(image)}
	first := int((base - g.MainBase) / uint32(g.SectorSize))
	last := int((uint32(end-1) - g.MainBase) / uint32(g.SectorSize))
	p.FirstSector, p.LastSector = first, last
	for sec := first; sec <= last; sec++ {
		sa := g.MainBase + uint32(sec*g.SectorSize)
		full := make([]byte, g.SectorSize)
		for i := range full {
			full[i] = 0xFF
		}
		lo, hi := sa, sa+uint32(g.SectorSize)
		if lo < base {
			lo = base
		}
		if uint64(hi) > end {
			hi = uint32(end)
		}
		copy(full[lo-sa:hi-sa], image[lo-base:hi-base])
		sp := SectorPlan{Sector: sec, Addr: sa, Image: full, CRC: crc32.ChecksumIEEE(full)}
		first, last := -1, -1
		for i, b := range full {
			if b != 0xFF {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first < 0 {
			p.BlankSectors++
		} else {
			g16 := saci.ProgramGranule
			first -= first % g16
			last = (last/g16 + 1) * g16
			sp.Data, sp.DataAddr, sp.Bytes = full[first:last], sa+uint32(first), last-first
			p.ProgramBytes += sp.Bytes
		}
		p.Sectors = append(p.Sectors, sp)
	}
	return p, nil
}

// flashErr turns a SACI error into a coded error.
func flashErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var re *saci.ResultError
	if errors.As(err, &re) {
		switch re.Result {
		case saci.ResNotAllowed:
			return errf(CodePermissionDenied, err, "%s is not allowed by the device (CCFG permissions or SACI session state)", op)
		case saci.ResCRC32Mismatch, saci.ResBlankCheckFailed:
			return errf(CodeVerifyFailed, err, "%s: the flash content does not match", op)
		case saci.ResFlashFSMError:
			return errf(CodeFlashFailed, err, "%s: flash state machine error (write/erase protected sector or flash not erased)", op)
		}
		return errf(CodeFlashFailed, err, "%s failed", op)
	}
	if errors.Is(err, saci.ErrTimeout) {
		return errf(CodeSACITimeout, err, "%s: no SACI response", op)
	}
	return err
}

// FlashTxn is one flash programming session in SACI.
type FlashTxn struct {
	s *Session
	c *saci.Client
	// Tainted is set after chip erase or CCFG programming: SACI then refuses
	// to boot until the device is reset.
	Tainted bool
	// Progress, when set, is called after each sector with sectors done/total.
	Progress func(done, total int)
}

// BeginFlash resets the device into SACI (nRESET when the probe has it, else
// SYSRESETREQ) and returns a transaction. A device already in SACI is used as
// it is.
func (s *Session) BeginFlash(ctx context.Context) (*FlashTxn, error) {
	via := ResetVia("")
	if st, err := s.ReadBootStatus(ctx); err != nil || !st.SACI {
		via = ResetPin
		if s.PulseReset == nil {
			via = ResetSys
		}
	}
	if _, err := s.EnterSACI(ctx, via); err != nil {
		return nil, err
	}
	return &FlashTxn{s: s, c: s.SACI()}, nil
}

// ChipErase erases MAIN flash and the CCFG. retain is the SACI retain mask.
func (t *FlashTxn) ChipErase(ctx context.Context, retain uint16) error {
	t.Tainted = true
	return flashErr("chip erase", t.c.EraseChip(ctx, retain))
}

// Blank checks sectors [first, first+n) are erased.
func (t *FlashTxn) Blank(ctx context.Context, first, n int) error {
	for done := 0; done < n; {
		k := n - done
		if k > verifySpan {
			k = verifySpan
		}
		if err := t.c.VerifyMain(ctx, uint32((first+done)*SectorSize), uint32(k*SectorSize), 0, true); err != nil {
			return flashErr(fmt.Sprintf("blank check of sectors %d..%d", first+done, first+done+k-1), err)
		}
		done += k
	}
	return nil
}

// Program writes the plan sector by sector. It stops at the first error.
func (t *FlashTxn) Program(ctx context.Context, p *Plan) error {
	n := 0
	for _, sp := range p.Sectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(sp.Data) > 0 {
			if err := t.c.ProgramMain(ctx, sp.DataAddr, sp.Data); err != nil {
				return flashErr(fmt.Sprintf("programming sector %d", sp.Sector), err)
			}
		}
		n++
		if t.Progress != nil {
			t.Progress(n, len(p.Sectors))
		}
	}
	return nil
}

// Verify checks every sector of the plan against its CRC32 with SACI (the
// device never returns flash content).
func (t *FlashTxn) Verify(ctx context.Context, p *Plan) error {
	for i := 0; i < len(p.Sectors); {
		k := len(p.Sectors) - i
		if k > verifySpan {
			k = verifySpan
		}
		var buf []byte
		for _, sp := range p.Sectors[i : i+k] {
			buf = append(buf, sp.Image...)
		}
		if err := t.c.VerifyMain(ctx, p.Sectors[i].Addr, uint32(len(buf)), crc32.ChecksumIEEE(buf), false); err != nil {
			return flashErr(fmt.Sprintf("verify of sectors %d..%d", p.Sectors[i].Sector, p.Sectors[i+k-1].Sector), err)
		}
		i += k
	}
	return nil
}

// VerifyRange checks whole sectors [addr, addr+len) against crc.
func (t *FlashTxn) VerifyRange(ctx context.Context, addr, length, crc uint32) error {
	return flashErr("verify", t.c.VerifyMain(ctx, addr, length, crc, false))
}

// ProgramCCFG programs a complete CCFG sector image onto an erased CCFG. The
// image must be internally consistent (valid CRCs) and it is the caller's job
// to have shown the security-impact diff.
func (t *FlashTxn) ProgramCCFG(ctx context.Context, img []byte) error {
	info, err := ccfg.Parse(img)
	if err != nil {
		return errf(CodeUnsafeRequest, err, "invalid CCFG image")
	}
	if !info.Valid {
		return errf(CodeCCFGInvalid, nil, "refusing to program a CCFG whose CRCs are not valid")
	}
	t.Tainted = true
	return flashErr("CCFG program", t.c.ProgramCCFG(ctx, img, false))
}

// VerifyCCFG performs the SACI identity check of the CCFG against img.
func (t *FlashTxn) VerifyCCFG(ctx context.Context, img []byte) error {
	b, c, u, d, err := ccfg.CRCs(img)
	if err != nil {
		return err
	}
	return flashErr("CCFG verify", t.c.VerifyCCFG(ctx, false, saci.CCFGCRCs{BootCfg: b, Central: c, UserRec: u, DebugCfg: d}, false))
}

// BlankCCFG checks that the CCFG sector is erased.
func (t *FlashTxn) BlankCCFG(ctx context.Context) error {
	return flashErr("CCFG blank check", t.c.VerifyCCFG(ctx, true, saci.CCFGCRCs{}, false))
}

// Finish resets the device (a tainted session cannot boot) and leaves SACI
// in the requested mode. An empty mode resets into SACI and stays there.
func (t *FlashTxn) Finish(ctx context.Context, mode ExitMode) (ResetResult, error) {
	via := ResetPin
	if t.s.PulseReset == nil {
		via = ResetSACI
	}
	t0 := time.Now()
	r := ResetResult{Via: via}
	st, err := t.s.EnterSACI(ctx, via)
	r.Boot = st
	if err != nil || mode == "" {
		r.Duration = time.Since(t0).Round(time.Millisecond).String()
		return r, err
	}
	r.Exit, err = t.s.LeaveSACI(ctx, mode)
	r.Duration = time.Since(t0).Round(time.Millisecond).String()
	return r, err
}
