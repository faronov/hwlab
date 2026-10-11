package stlinkplugin

import (
	"bytes"
	"context"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/faronov/hwlab/internal/ownedtemp"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

// Confirmation tokens for destructive calls.
const (
	tokProgram   = "program-flash"
	tokRestore   = "restore-flash"
	tokEraseReg  = "erase-flash-region"
	tokEraseChip = "erase-entire-chip"
)

const (
	maxFlashInline = 4096
	maxFlashFile   = 16 << 20
	flashChunk     = 4096
)

// unsupportedHW is the status label for flash calls on a target that is not
// a recognised STM32. Nothing is written in that case.
const unsupportedHW = "UNSUPPORTED_BY_ATTACHED_HW"

// manifest describes a backup. It holds no firmware bytes.
type manifest struct {
	Format      string  `json:"format"`
	CreatedUTC  string  `json:"created_utc"`
	File        string  `json:"file"`
	SizeBytes   int     `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	SecondRead  string  `json:"second_read_sha256"`
	Chip        string  `json:"chip"`
	DevID       uint16  `json:"dev_id"`
	RevID       uint16  `json:"rev_id"`
	FlashKB     int     `json:"flash_kb"`
	ReadSeconds float64 `json:"read_seconds"`
	ProbeSerial string  `json:"probe_serial,omitempty"` // local artifact only
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// flashTarget returns the flash driver for a positively recognised STM32 and
// otherwise an explicit error that names the evidence.
func (s *session) flashTarget(ctx context.Context) (*stm32.Flash, *stm32.Profile, error) {
	p, err := stm32.Detect(ctx, s.core)
	if err != nil {
		hint, _, _ := s.vendorHint(ctx)
		what := "an Arm Cortex-M of unknown vendor"
		if hint != "" {
			what = hint
		}
		return nil, nil, status.Errorf(codes.FailedPrecondition,
			"%s: this flash driver is STM32-specific and the attached target is %s (%v); nothing was read or written. Flash methods never run against a target they did not positively recognise",
			unsupportedHW, what, err)
	}
	s.chip = p
	mv := 0
	if s.info.VoltageOK {
		mv = s.info.VoltageMV
	}
	return stm32.NewFlash(s.core, p, mv), p, nil
}

func (s *session) needFlash(erase bool) error {
	if !s.o.perm.flash {
		return denied("flash modification needs allow_flash=true in the device configuration")
	}
	if erase && !s.o.perm.erase {
		return denied("erasing flash needs allow_erase=true in the device configuration")
	}
	return nil
}

func tokenIs(args map[string]any, key, want, why string) error {
	got, err := strArg(args, key)
	if err != nil {
		return err
	}
	if got != want {
		return denied("%s: pass %s=%q", why, key, want)
	}
	return nil
}

func flashErr(err error) error {
	switch {
	case errors.Is(err, stm32.ErrRange):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, stm32.ErrAlign):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, stm32.ErrVerify):
		return status.Error(codes.DataLoss, err.Error())
	case errors.Is(err, stm32.ErrProtected):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, stm32.ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

func (s *session) defaultDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "stlink-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "stlink-flash")
}

// requireHalted makes a modifying flash call run on a halted core: the
// firmware must not execute out of flash that is being changed.
func (s *session) requireHalted(ctx context.Context) (wasRunning bool, err error) {
	st, err := s.core.Status(ctx)
	if err != nil {
		return false, err
	}
	if st.Halted {
		return false, nil
	}
	if _, err := s.core.Halt(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func imageArgs(args map[string]any) ([]byte, string, error) {
	path, err := strArg(args, "path")
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		return nil, "", argErr(errors.New("path: the image file is required"))
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return nil, "", argErr(serr)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxFlashFile {
		return nil, "", argErr(fmt.Errorf("path %s: want a non-empty regular file up to %d bytes", path, maxFlashFile))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", argErr(err)
	}
	return data, path, nil
}

func rangeArgs(args map[string]any, fl *stm32.Flash, what string) (uint32, int, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return 0, 0, argErr(err)
	}
	n, err := intArg(args, "length", 0, 0, maxFlashFile)
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, argErr(fmt.Errorf("%s: length must be at least 1 byte", what))
	}
	if !fl.Contains(addr, n) {
		return 0, 0, status.Errorf(codes.OutOfRange, "%s: [0x%08x, +%d) is outside the flash 0x%08x..0x%08x", what, addr, n, uint32(stm32.FlashBase), stm32.FlashBase+fl.P.FlashSize())
	}
	return addr, n, nil
}

// readFlash reads n bytes in chunks.
func readFlash(ctx context.Context, fl *stm32.Flash, addr uint32, n int) ([]byte, error) {
	out := make([]byte, 0, n)
	for off := 0; off < n; off += flashChunk {
		c := min(flashChunk, n-off)
		b, err := fl.Read(ctx, addr+uint32(off), c)
		if err != nil {
			return nil, fmt.Errorf("reading 0x%08x: %w", addr+uint32(off), err)
		}
		out = append(out, b...)
	}
	return out, nil
}

// ---- info / read / verify ----

func (s *session) flashInfo(ctx context.Context, _ map[string]any) (map[string]any, error) {
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		hint, word, _ := s.vendorHint(ctx)
		return s.mark(map[string]any{
			"supported": false, "status": unsupportedHW, "reason": err.Error(),
			"vendor_hint": hint, "vendor_register_word": hexw(word),
			"note": "no flash was touched; MemoryAccess.read still reads any address",
		}), nil
	}
	sectors := make([]any, len(p.Sectors))
	for i, sc := range p.Sectors {
		sectors[i] = map[string]any{"index": float64(sc.Index), "addr": hexw(sc.Addr), "size": float64(sc.Size)}
	}
	res := map[string]any{
		"supported": true, "chip": p.Name, "family": string(p.Family), "dev_id": fmt.Sprintf("0x%03x", p.DevID), "rev_id": fmt.Sprintf("0x%04x", p.RevID),
		"flash_base": hexw(stm32.FlashBase), "flash_bytes": float64(p.FlashSize()), "flash_kb": float64(p.FlashKB),
		"programmable": p.Programmable, "sector_count": float64(len(sectors)), "sectors": sectors,
		"jedec": "not applicable: STM32 on-chip flash has no JEDEC identification",
		"basis": "DBGMCU_IDCODE and the flash-size register, both read-only; the geometry comes from the family table",
	}
	if p.Note != "" {
		res["note"] = p.Note
	}
	res["flash_size_source"] = p.FlashSizeSource
	if p.SRAMKB > 0 {
		res["sram_kb_readable"] = float64(p.SRAMKB)
	}
	if p.PageSize > 0 {
		res["page_size"] = float64(p.PageSize)
		res["program_granularity_bytes"] = float64(2)
		res["program_method"] = "hwlab-owned Thumb loader in SRAM drives FLASH_CR.PG halfword by halfword"
	}
	if p.Family == stm32.F4 || p.Family == stm32.F1 {
		pr, err := fl.ReadProtection(ctx)
		if err != nil {
			return nil, err
		}
		ws := []any{}
		for _, i := range pr.WriteProtected {
			ws = append(ws, float64(i))
		}
		prot := map[string]any{"rdp_level": float64(pr.RDP), "write_protected_sectors": ws, "read_only": true}
		if p.Family == stm32.F4 {
			prot["optcr"] = hexw(pr.OPTCR)
		} else {
			prot["flash_obr"], prot["flash_wrpr"] = hexw(pr.OBR), hexw(pr.WRPR)
			prot["option_bytes"] = hex.EncodeToString(pr.OptionBytes)
			prot["rdp_option_byte"] = fmt.Sprintf("0x%02x", pr.OptionBytes[0])
			prot["readout_protection_active"] = pr.OBR&2 != 0
		}
		res["protection"] = prot
	}
	return s.mark(res), nil
}

func (s *session) flashRead(ctx context.Context, args map[string]any) (map[string]any, error) {
	fl, _, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	addr, n, err := rangeArgs(args, fl, "read")
	if err != nil {
		return nil, err
	}
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	if path == "" && n > maxFlashInline {
		return nil, argErr(fmt.Errorf("length %d: at most %d bytes inline; pass path=<new file> for more", n, maxFlashInline))
	}
	t0 := time.Now()
	data, err := readFlash(ctx, fl, addr, n)
	if err != nil {
		return nil, flashErr(err)
	}
	el := time.Since(t0)
	res := map[string]any{"address": hexw(addr), "length": float64(n), "sha256": sha(data), "elapsed_ms": float64(el.Milliseconds()),
		"via": "MEM-AP reads of the memory-mapped flash (STM32 flash is memory mapped; readout protection makes the read fail rather than return data)"}
	if el > 0 && n >= 1024 {
		res["throughput_kib_s"] = float64(n) / 1024 / el.Seconds()
	}
	if path == "" {
		res["hex"] = hex.EncodeToString(data)
		return s.mark(res), nil
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return nil, err
	}
	if err := ownedtemp.WriteFile(path, data, overwrite); err != nil {
		return nil, argErr(err)
	}
	res["path"] = path
	return s.mark(res), nil
}

func (s *session) flashVerify(ctx context.Context, args map[string]any) (map[string]any, error) {
	fl, _, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	img, path, err := imageArgs(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	if !fl.Contains(addr, len(img)) {
		return nil, status.Errorf(codes.OutOfRange, "image %s (%d bytes) at 0x%08x does not fit the flash", path, len(img), addr)
	}
	got, err := readFlash(ctx, fl, addr, len(img))
	if err != nil {
		return nil, flashErr(err)
	}
	if i := firstDiff(img, got); i >= 0 {
		return nil, status.Errorf(codes.DataLoss, "verify FAILED: flash differs from %s first at 0x%08x (file 0x%02x, flash 0x%02x); flash sha256 %s, file sha256 %s",
			path, addr+uint32(i), img[i], got[i], sha(got), sha(img))
	}
	return s.mark(map[string]any{"verified": true, "address": hexw(addr), "length": float64(len(img)), "sha256": sha(img)}), nil
}

func isBlank(b []byte) bool {
	for _, c := range b {
		if c != 0xFF {
			return false
		}
	}
	return true
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return -1
}

// ---- backup ----

func (s *session) flashBackup(ctx context.Context, args map[string]any) (map[string]any, error) {
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := strArg(args, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = s.defaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	size := int(p.FlashSize())
	t0 := time.Now()
	first, err := readFlash(ctx, fl, stm32.FlashBase, size)
	if err != nil {
		return nil, fmt.Errorf("first read: %w", flashErr(err))
	}
	second, err := readFlash(ctx, fl, stm32.FlashBase, size)
	if err != nil {
		return nil, fmt.Errorf("second read: %w", flashErr(err))
	}
	m := manifest{Format: "hwlab-stm32-flash-backup/1", SizeBytes: size, SHA256: sha(first), SecondRead: sha(second),
		Chip: p.Name, DevID: p.DevID, RevID: p.RevID, FlashKB: p.FlashKB, ReadSeconds: time.Since(t0).Seconds(), ProbeSerial: s.entry.Serial}
	if m.SHA256 != m.SecondRead {
		return nil, status.Errorf(codes.DataLoss, "the two full-flash reads differ (%s vs %s): no backup was stored", m.SHA256, m.SecondRead)
	}
	m.File = fmt.Sprintf("stm32-flash-%03x-%dK-%s.bin", p.DevID, p.FlashKB, m.SHA256[:12])
	final := filepath.Join(dir, m.File)
	if err := os.WriteFile(final, first, 0o600); err != nil {
		return nil, err
	}
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
		return nil, err
	}
	return s.mark(map[string]any{
		"path": final, "manifest": final + ".json", "size_bytes": float64(size), "sha256": m.SHA256, "second_read_sha256": m.SecondRead,
		"identical_reads": true, "read_seconds": m.ReadSeconds, "chip": p.Name,
		"note": "local artifact: it holds the complete firmware, never commit it",
	}), nil
}

// loadBackup reads a backup file and checks it against its manifest.
func loadBackup(path string) ([]byte, *manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	mb, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, nil, fmt.Errorf("no manifest %s.json: only backups written by FlashProgramming.backup are accepted", path)
	}
	var m manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, nil, fmt.Errorf("manifest: %w", err)
	}
	if sha(b) != m.SHA256 {
		return nil, nil, fmt.Errorf("backup %s does not match the SHA-256 in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead {
		return nil, nil, errors.New("the manifest does not record two identical reads")
	}
	return b, &m, nil
}

// backupGate requires a verified full backup of THIS chip whose bytes in the
// affected sectors equal what is in flash now. A sector that is completely
// erased also passes: it is what an earlier erase of that sector (already
// covered by the backup) leaves behind, and it holds nothing to lose.
func (s *session) backupGate(ctx context.Context, args map[string]any, fl *stm32.Flash, secs []stm32.Sector) (map[string]any, error) {
	path, err := strArg(args, "backup")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, denied("refusing to modify flash without a verified full backup: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	if m.DevID != fl.P.DevID || len(b) != int(fl.P.FlashSize()) {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another chip (%s dev 0x%03x, %d bytes) than the connected one (%s dev 0x%03x, %d bytes)",
			path, m.Chip, m.DevID, len(b), fl.P.Name, fl.P.DevID, fl.P.FlashSize())
	}
	for _, sc := range secs {
		cur, err := readFlash(ctx, fl, sc.Addr, int(sc.Size))
		if err != nil {
			return nil, err
		}
		off := sc.Addr - stm32.FlashBase
		if !bytes.Equal(cur, b[off:off+sc.Size]) && !isBlank(cur) {
			return nil, status.Errorf(codes.FailedPrecondition, "the backup %s no longer matches sector %d (0x%08x): the flash changed after the backup, take a new backup", path, sc.Index, sc.Addr)
		}
	}
	return map[string]any{"path": path, "sha256": m.SHA256, "affected_sectors_match_flash": true}, nil
}

// modifiable checks the chip can be changed at all.
func modifiable(p *stm32.Profile) error {
	if !p.Programmable {
		return status.Errorf(codes.Unimplemented, "%s (family %s): %s", p.Name, p.Family, p.Note)
	}
	return nil
}

// ---- erase ----

func (s *session) flashErase(ctx context.Context, args map[string]any) (map[string]any, error) {
	mode, err := strArg(args, "mode")
	if err != nil {
		return nil, err
	}
	switch mode {
	case "region", "chip":
	case "app":
		return nil, argErr(errors.New("mode app: STM32 has no partition table, use mode=region"))
	default:
		return nil, argErr(fmt.Errorf("mode %q: want region or chip (there is no implicit mode)", mode))
	}
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if mode == "chip" {
		if !s.o.perm.chipErase {
			return nil, denied("erasing the whole flash needs allow_chip_erase=true in the device configuration")
		}
		if err := tokenIs(args, "confirm", tokEraseChip, "whole-flash erase"); err != nil {
			return nil, err
		}
	} else if err := tokenIs(args, "confirm", tokEraseReg, "erasing a flash region"); err != nil {
		return nil, err
	}
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	if err := modifiable(p); err != nil {
		return nil, err
	}
	var secs []stm32.Sector
	if mode == "chip" {
		secs = p.Sectors
	} else {
		addr, n, err := rangeArgs(args, fl, "erase")
		if err != nil {
			return nil, err
		}
		secs = p.Covering(addr, n)
		if len(secs) == 0 || secs[0].Addr != addr || secs[len(secs)-1].Addr+secs[len(secs)-1].Size != addr+uint32(n) {
			return nil, argErr(fmt.Errorf("[0x%08x, +%d) does not start and end on sector boundaries (the covering sectors are 0x%08x..0x%08x)",
				addr, n, secs[0].Addr, secs[len(secs)-1].Addr+secs[len(secs)-1].Size))
		}
	}
	bk, err := s.backupGate(ctx, args, fl, secs)
	if err != nil {
		return nil, err
	}
	wasRunning, err := s.requireHalted(ctx)
	if err != nil {
		return nil, err
	}
	erased := []any{}
	for _, sc := range secs {
		if err := fl.EraseSector(ctx, sc); err != nil {
			if len(erased) == 0 && errors.Is(err, stm32.ErrProtected) {
				return nil, flashErr(err) // refused before anything was erased
			}
			return nil, status.Errorf(codes.DataLoss, "erase stopped at sector %d (0x%08x) after %d sector(s): %v; the core stays halted", sc.Index, sc.Addr, len(erased), flashErr(err))
		}
		erased = append(erased, float64(sc.Index))
	}
	return s.mark(map[string]any{
		"mode": mode, "erased_sectors": erased, "verified_erased": true, "backup": bk, "core_halted": true, "was_running": wasRunning,
		"note": "every erased word was read back as 0xFF; the core stays halted",
	}), nil
}

// ---- program / restore ----

// sectorPlan describes what one sector needs.
type sectorPlan struct {
	sec     stm32.Sector
	merged  []byte
	changed bool
	erase   bool
}

// planProgram merges image into the covering sectors, preserving the bytes
// outside it, and decides which sectors need an erase (a bit must go 0→1).
func planProgram(ctx context.Context, fl *stm32.Flash, addr uint32, img []byte) ([]sectorPlan, error) {
	var out []sectorPlan
	for _, sc := range fl.P.Covering(addr, len(img)) {
		cur, err := readFlash(ctx, fl, sc.Addr, int(sc.Size))
		if err != nil {
			return nil, err
		}
		merged := append([]byte(nil), cur...)
		lo, hi := max(addr, sc.Addr), min(addr+uint32(len(img)), sc.Addr+sc.Size)
		copy(merged[lo-sc.Addr:hi-sc.Addr], img[lo-addr:hi-addr])
		pl := sectorPlan{sec: sc, merged: merged, changed: !bytes.Equal(cur, merged)}
		for i := range merged {
			if merged[i]&^cur[i] != 0 {
				pl.erase = true
				break
			}
		}
		out = append(out, pl)
	}
	return out, nil
}

func applyPlan(ctx context.Context, fl *stm32.Flash, plan []sectorPlan) (erased, programmed int, err error) {
	for _, pl := range plan {
		if !pl.changed {
			continue
		}
		if pl.erase {
			if err := fl.EraseSector(ctx, pl.sec); err != nil {
				return erased, programmed, fmt.Errorf("sector %d: %w", pl.sec.Index, err)
			}
			erased++
		}
		if err := fl.Program(ctx, pl.sec.Addr, pl.merged); err != nil {
			return erased, programmed, fmt.Errorf("sector %d: %w", pl.sec.Index, err)
		}
		programmed++
	}
	return erased, programmed, nil
}

func (s *session) afterReset(ctx context.Context, args map[string]any, res map[string]any) error {
	mode, err := strArg(args, "reset")
	if err != nil {
		return err
	}
	switch mode {
	case "", "none":
		res["core_halted"] = true
		return nil
	case "run", "halt":
		rr, err := s.reset(ctx, map[string]any{"kind": "system", "halt": mode == "halt"})
		if err != nil {
			return fmt.Errorf("flash written and verified but the reset failed: %w", err)
		}
		res["reset"] = rr
		return nil
	}
	return argErr(fmt.Errorf("reset %q: want run, halt or none", mode))
}

func (s *session) flashProgram(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.needFlash(false); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokProgram, "programming flash"); err != nil {
		return nil, err
	}
	img, path, err := imageArgs(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	if rs, _ := strArg(args, "reset"); rs != "" && rs != "none" && !s.o.perm.reset {
		return nil, denied("reset=%s needs allow_reset=true in the device configuration", rs)
	}
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	if err := modifiable(p); err != nil {
		return nil, err
	}
	if !fl.Contains(addr, len(img)) {
		return nil, status.Errorf(codes.OutOfRange, "image %s (%d bytes) at 0x%08x does not fit the flash 0x%08x..0x%08x", path, len(img), addr, uint32(stm32.FlashBase), stm32.FlashBase+p.FlashSize())
	}
	plan, err := planProgram(ctx, fl, addr, img)
	if err != nil {
		return nil, flashErr(err)
	}
	needErase := false
	secs := make([]stm32.Sector, len(plan))
	for i, pl := range plan {
		secs[i] = pl.sec
		needErase = needErase || (pl.changed && pl.erase)
	}
	if needErase && !s.o.perm.erase {
		return nil, denied("this image needs sectors erased (a bit must go from 0 to 1): that needs allow_erase=true in the device configuration")
	}
	bk, err := s.backupGate(ctx, args, fl, secs)
	if err != nil {
		return nil, err
	}
	wasRunning, err := s.requireHalted(ctx)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	erased, programmed, err := applyPlan(ctx, fl, plan)
	if err != nil {
		return nil, status.Errorf(codes.DataLoss, "programming stopped after %d erase(s) and %d sector(s): %v; the core stays halted and the flash may be partly written: restore the backup", erased, programmed, flashErr(err))
	}
	el := time.Since(t0)
	got, err := readFlash(ctx, fl, addr, len(img))
	if err != nil {
		return nil, flashErr(err)
	}
	if i := firstDiff(img, got); i >= 0 {
		return nil, status.Errorf(codes.DataLoss, "final verify FAILED at 0x%08x (image 0x%02x, flash 0x%02x)", addr+uint32(i), img[i], got[i])
	}
	res := map[string]any{
		"address": hexw(addr), "length": float64(len(img)), "sha256": sha(img), "verified": true, "sectors_touched": float64(len(plan)),
		"sectors_erased": float64(erased), "sectors_programmed": float64(programmed), "elapsed_ms": float64(el.Milliseconds()),
		"backup": bk, "was_running": wasRunning,
		"basis": "per covering sector: read, merge the image over it (the bytes outside the image are preserved), erase only when a bit must be set, program, read back; then the whole image is compared again. Option bytes and RDP are never written",
	}
	if err := s.afterReset(ctx, args, res); err != nil {
		return nil, err
	}
	return s.mark(res), nil
}

func (s *session) flashRestore(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokRestore, "restoring flash"); err != nil {
		return nil, err
	}
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, argErr(errors.New("path: the backup .bin is required"))
	}
	if rs, _ := strArg(args, "reset"); rs != "" && rs != "none" && !s.o.perm.reset {
		return nil, denied("reset=%s needs allow_reset=true in the device configuration", rs)
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		return nil, err
	}
	if err := modifiable(p); err != nil {
		return nil, err
	}
	if m.DevID != p.DevID || len(b) != int(p.FlashSize()) {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another chip (%s dev 0x%03x, %d bytes) than the connected one (%s dev 0x%03x, %d bytes): refusing to restore",
			path, m.Chip, m.DevID, len(b), p.Name, p.DevID, p.FlashSize())
	}
	plan, err := planProgram(ctx, fl, stm32.FlashBase, b)
	if err != nil {
		return nil, flashErr(err)
	}
	wasRunning, err := s.requireHalted(ctx)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	erased, programmed, err := applyPlan(ctx, fl, plan)
	if err != nil {
		return nil, status.Errorf(codes.DataLoss, "restore stopped after %d erase(s) and %d sector(s): %v; the core stays halted", erased, programmed, flashErr(err))
	}
	el := time.Since(t0)
	got, err := readFlash(ctx, fl, stm32.FlashBase, len(b))
	if err != nil {
		return nil, flashErr(err)
	}
	if sha(got) != m.SHA256 {
		return nil, status.Errorf(codes.DataLoss, "restore FAILED: whole-flash SHA-256 %s differs from the backup %s", sha(got), m.SHA256)
	}
	res := map[string]any{
		"restored": true, "sha256": m.SHA256, "whole_flash_matches_backup": true, "sectors_erased": float64(erased),
		"sectors_programmed": float64(programmed), "elapsed_ms": float64(el.Milliseconds()), "was_running": wasRunning,
	}
	if err := s.afterReset(ctx, args, res); err != nil {
		return nil, err
	}
	return s.mark(res), nil
}
