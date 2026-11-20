package xds110plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc26xx"
	"github.com/faronov/hwlab/internal/ti/icepick"
)

// CC13xx/CC26xx over cJTAG: identity, CCFG, flash read/backup/verify and the
// modifying operations. The confirmation tokens and the CCFG rules are the
// ones of the tibsl plugin (ROM serial bootloader), so a chip is protected
// the same way whichever path reaches it; in addition every modification
// needs the device-level permissions of this plugin (allow_flash,
// allow_erase, allow_ccfg_write, allow_chip_erase) and a verified backup.
const (
	tokErase        = "erase-flash-region"
	tokBankErase    = "bank-erase"
	tokProgram      = "program-flash"
	tokRestore      = "restore-flash"
	tokCCFG         = "modify-ccfg"
	tokLockout      = "allow-bsl-lockout"
	tokICEPickErase = "icepick-chip-erase"

	cc26BackupFormat = "hwlab-xds110-cc26xx-flash-backup/1"
	cc26ZigbeeWarn   = "never restore this backup onto a device that has joined a Zigbee network since: it holds the network key, frame counters and identity of that time; restore only onto the same chip"
	maxImageFile     = 2 << 20
)

// cc26Handles lists the methods the CC13xx/CC26xx path implements once
// attached over cJTAG.
var cc26Handles = map[string]bool{
	"DebugProbe.chip_info": true, "FlashProgramming.info": true, "FlashProgramming.ccfg_info": true,
	"FlashProgramming.read": true, "FlashProgramming.verify": true, "FlashProgramming.backup": true,
	"FlashProgramming.erase": true, "FlashProgramming.program": true, "FlashProgramming.restore": true,
}

// memReader adapts the core to cc26xx.Memory.
type memReader struct {
	ctx context.Context
	c   *cortexm.Core
}

func (m memReader) Word(addr uint32) (uint32, error) { return m.c.Bus.Read32(m.ctx, addr) }

func (m memReader) Read(addr uint32, n int, _ func(int)) ([]byte, error) {
	return m.c.Read(m.ctx, addr, n)
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func tokenIs(a map[string]any, key, want, what string) error {
	if s, _ := a[key].(string); s != want {
		return status.Errorf(codes.PermissionDenied, "%s needs %s=%q", what, key, want)
	}
	return nil
}

// cc26 is one call on an attached CC13xx/CC26xx.
type cc26 struct {
	d    *device
	ctx  context.Context
	chip *cc26xx.Chip
}

func (d *device) cc26(ctx context.Context) (*cc26, error) {
	ch, err := cc26xx.Identify(memReader{ctx, d.core})
	if err != nil {
		return nil, fmt.Errorf("identify the CC13xx/CC26xx: %w", err)
	}
	return &cc26{d: d, ctx: ctx, chip: ch}, nil
}

func (c *cc26) read(addr uint32, n int) ([]byte, error) { return c.d.core.Read(c.ctx, addr, n) }

func (c *cc26) config() ([]byte, error) {
	b, err := c.read(c.chip.ConfigAddr, cc26xx.CCFGLen)
	if err != nil {
		return nil, fmt.Errorf("read CCFG: %w", err)
	}
	return b, nil
}

func (c *cc26) fields(res map[string]any) {
	ch := c.chip
	res["family"] = "CC26xx"
	res["chip"] = ch.Name
	res["series"] = ch.Series
	res["icepick_device_id"] = hex32(ch.ICEPick)
	res["wafer_id"] = fmt.Sprintf("0x%04X", ch.WaferID)
	res["pg_rev"] = float64(ch.PGRev)
	res["user_id"] = hex32(ch.UserID)
	res["package"] = ch.Package
	res["pa"] = ch.PA
	res["protocols"] = toAny(ch.Protocols)
	res["flash_base"] = hex32(0)
	res["flash_bytes"] = float64(ch.FlashBytes)
	res["flash_kib"] = float64(ch.FlashBytes >> 10)
	res["sector_bytes"] = float64(ch.SectorBytes)
	res["ieee"] = ch.IEEEString()
	res["writable"] = ch.Writable && ch.Agama()
	switch {
	case ch.ReadOnlyWhy != "":
		res["read_only_reason"] = ch.ReadOnlyWhy
	case !ch.Agama():
		res["read_only_reason"] = ch.Series + ": the debug flash path is implemented for CC13x2/CC26x2 (ROM_API_FLASH_TABLE) only; reads work"
	}
	if cj := c.d.cj; cj != nil {
		res["interface"] = "cJTAG " + formatName(cj.format) + " via ICEPick"
		res["jtag_idcode"] = hex32(cj.icepickID)
		res["cpu_dap_idcode"] = hex32(cj.dapID)
		res["clock_hz"] = float64(c.d.attached.clockHz)
	}
}

// ccfgSummary is the security-relevant digest of a CCFG.
func ccfgSummary(cfg []byte) map[string]any {
	dec := cc26xx.DecodeCCFG(cfg)
	prot := cc26xx.ProtectedSectors(cfg)
	ps := make([]any, len(prot))
	for i, p := range prot {
		ps[i] = float64(p)
	}
	return map[string]any{
		"bootloader": dec["bootloader"], "image_valid": dec["image_valid"], "cpu_dap_enabled": dec["cpu_dap_enabled"],
		"debug_locked_after_reset": cc26xx.DebugLocked(cfg), "taps": dec["taps"], "ti_fa_enabled": dec["ti_fa_enabled"],
		"chip_erase_enabled": dec["chip_erase_enabled"], "bank_erase_enabled": dec["bank_erase_enabled"],
		"write_protected_sectors": ps,
	}
}

func (c *cc26) identify() (map[string]any, error) {
	res := map[string]any{}
	c.fields(res)
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	dec := cc26xx.DecodeCCFG(cfg)
	res["bootloader"] = dec["bootloader"]
	if o, ok := dec["ieee_override"]; ok {
		res["ieee_ccfg_override"] = o
	}
	res["ccfg"] = ccfgSummary(cfg)
	res["debug_locked"] = false
	if st, err := c.d.core.Status(c.ctx); err == nil {
		res["core_halted"] = st.Halted
	}
	if ci, err := c.d.core.ReadIdentity(c.ctx); err == nil {
		res["core"], res["cpuid"] = ci.Name, hex32(ci.CPUID)
	}
	res["note"] = "read through the CPU DAP behind the ICEPick; the core was not halted or written"
	return res, nil
}

func (c *cc26) flashInfo() (map[string]any, error) {
	res, err := c.identify()
	if err != nil {
		return nil, err
	}
	delete(res, "note")
	res["config_address"] = hex32(c.chip.ConfigAddr)
	res["config_bytes"] = float64(cc26xx.CCFGLen)
	res["config_sector"] = hex32(c.chip.ConfigSector())
	if c.chip.Agama() {
		if e, err := cc26xx.ReadROMEntries(c.ctx, cc26xx.CoreMem{C: c.d.core}); err == nil {
			res["rom"] = map[string]any{"rom_version": hex32(e.ROMVersion), "flash_table": hex32(e.FlashTable), "sector_erase": hex32(e.SectorErase),
				"program": hex32(e.Program), "hapi_crc32": hex32(e.HAPICrc32), "hapi_sector_erase": hex32(e.HAPIErase), "hapi_program": hex32(e.HAPIProgram)}
		} else {
			res["rom_error"] = err.Error()
		}
	}
	res["method"] = "erase/program run the ROM flash API (ROM_API_FLASH_TABLE FlashSectorErase/FlashProgram) on the halted core from SRAM; SRAM 0x20000000..0x20003000 is overwritten and the target is reset afterwards (reset_after=false leaves it halted)"
	res["backup_required_for_modification"] = true
	res["device_permissions"] = "program/restore: allow_flash+allow_erase; erase: allow_erase; CCFG sector: also allow_ccfg_write; ICEPick chip erase: allow_chip_erase"
	res["tokens"] = map[string]any{"erase": tokErase, "bank_erase": tokBankErase, "program": tokProgram, "restore": tokRestore,
		"config_sector": "confirm_config=" + tokCCFG, "lockout_override": "ccfg_override=" + tokLockout, "icepick_chip_erase": tokICEPickErase}
	return res, nil
}

func (c *cc26) ccfgInfo() (map[string]any, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	res := cc26xx.DecodeCCFG(cfg)
	res["address"] = hex32(c.chip.ConfigAddr)
	res["bytes"] = float64(len(cfg))
	res["chip"] = c.chip.Name
	res["raw_hex"] = hex.EncodeToString(cfg)
	res["debug_locked_after_reset"] = cc26xx.DebugLocked(cfg)
	return res, nil
}

// flashRange validates [addr, addr+n) against the flash.
func (c *cc26) flashRange(addr uint32, n int) error {
	if n < 1 || uint64(addr)+uint64(n) > uint64(c.chip.FlashBytes) {
		return argErr(fmt.Errorf("0x%08X+%d is outside the flash 0x00000000..0x%08X", addr, n, c.chip.FlashBytes))
	}
	return nil
}

func optAddr(a map[string]any, key string) (uint32, bool, error) {
	if v, ok := a[key]; !ok || v == nil {
		return 0, false, nil
	}
	v, err := u32(a, key)
	return v, err == nil, err
}

func writeNew(path string, b []byte, overwrite bool) error {
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func (c *cc26) flashRead(a map[string]any) (map[string]any, error) {
	addr, _, err := optAddr(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	path, _ := a["path"].(string)
	nf, ok, err := optNumber(a, "length")
	if err != nil {
		return nil, argErr(err)
	}
	n := c.chip.FlashBytes - int(min(addr, uint32(c.chip.FlashBytes)))
	if ok {
		if nf < 1 || nf != math.Trunc(nf) || nf > float64(c.chip.FlashBytes) {
			return nil, argErr(fmt.Errorf("length %v: want an integer 1..%d", nf, c.chip.FlashBytes))
		}
		n = int(nf)
	}
	if err := c.flashRange(addr, n); err != nil {
		return nil, err
	}
	if path == "" && n > inlineBytes {
		return nil, argErr(fmt.Errorf("length %d: at most %d bytes inline; pass path to read more", n, inlineBytes))
	}
	t0 := time.Now()
	b, err := c.read(addr, n)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(n), "sha256": shaHex(b), "read_seconds": time.Since(t0).Seconds()}
	if path != "" {
		ow, _, _ := optBool(a, "overwrite")
		if err := writeNew(path, b, ow); err != nil {
			return nil, err
		}
		res["path"] = path
		return res, nil
	}
	res["data"] = hex.EncodeToString(b)
	return res, nil
}

func readImage(a map[string]any, max int) ([]byte, string, error) {
	path, _ := a["path"].(string)
	_, hasData := a["data"]
	switch {
	case path != "" && hasData:
		return nil, "", errors.New("give either path or data, not both")
	case path != "":
		st, err := os.Stat(path)
		if err != nil {
			return nil, "", err
		}
		if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > int64(max) {
			return nil, "", fmt.Errorf("%s is %d bytes; want a regular file of 1..%d bytes (the flash size)", path, st.Size(), max)
		}
		b, err := os.ReadFile(path)
		return b, path, err
	case hasData:
		s, _ := a["data"].(string)
		s = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), "0x")
		b, err := hex.DecodeString(s)
		if err != nil || len(b) == 0 || len(b) > min(max, inlineBytes) {
			return nil, "", fmt.Errorf("data: want a hex string of 1..%d bytes", min(max, inlineBytes))
		}
		return b, "", nil
	}
	return nil, "", errors.New("path or data is required")
}

func (c *cc26) flashVerify(a map[string]any) (map[string]any, error) {
	addr, _, err := optAddr(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	img, path, err := readImage(a, max(c.chip.FlashBytes, 1))
	if err != nil {
		return nil, argErr(err)
	}
	if err := c.flashRange(addr, len(img)); err != nil {
		return nil, err
	}
	got, err := c.read(addr, len(img))
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(len(img)), "match": bytes.Equal(got, img), "image_sha256": shaHex(img),
		"flash_sha256": shaHex(got), "method": "read back through the MEM-AP"}
	if path != "" {
		res["path"] = path
	}
	if !bytes.Equal(got, img) {
		for i := range img {
			if got[i] != img[i] {
				res["first_difference"] = hex32(addr + uint32(i))
				break
			}
		}
	}
	return res, nil
}

// backupManifest is stored next to every backup as <file>.json and ties the
// bytes to one chip.
type backupManifest struct {
	Format      string  `json:"format"`
	CreatedUTC  string  `json:"created_utc"`
	File        string  `json:"file"`
	SizeBytes   int     `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	SecondRead  string  `json:"second_read_sha256"`
	Chip        string  `json:"chip"`
	ICEPick     string  `json:"icepick_device_id"`
	UserID      string  `json:"user_id"`
	IEEE        string  `json:"ieee"`
	FlashBase   string  `json:"flash_base"`
	JTAGIDCode  string  `json:"jtag_idcode,omitempty"`
	Probe       string  `json:"probe,omitempty"`
	ReadSeconds float64 `json:"read_seconds"`
	Warning     string  `json:"warning"`
}

func (c *cc26) identity() backupManifest {
	return backupManifest{Chip: c.chip.Name, ICEPick: hex32(c.chip.ICEPick), UserID: hex32(c.chip.UserID), IEEE: c.chip.IEEEString(),
		FlashBase: hex32(0), SizeBytes: c.chip.FlashBytes}
}

func cc26BackupDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "cc26xx-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "cc26xx-flash")
}

// flashBackup reads the whole flash twice through the MEM-AP; both reads must
// agree. It never halts the core or modifies the target.
func (c *cc26) flashBackup(a map[string]any) (map[string]any, error) {
	dir, _ := a["dir"].(string)
	if dir == "" {
		dir = cc26BackupDir()
	}
	t0 := time.Now()
	first, err := c.read(0, c.chip.FlashBytes)
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := c.read(0, c.chip.FlashBytes)
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	m := c.identity()
	m.Format, m.SHA256, m.SecondRead = cc26BackupFormat, shaHex(first), shaHex(second)
	m.ReadSeconds, m.Warning, m.Probe = time.Since(t0).Seconds(), cc26ZigbeeWarn, c.d.entry.ID
	if c.d.cj != nil {
		m.JTAGIDCode = hex32(c.d.cj.icepickID)
	}
	if m.SHA256 != m.SecondRead {
		return nil, status.Errorf(codes.DataLoss, "full-flash reads disagree (read 1 %s, read 2 %s): no backup was stored. A running firmware that writes its NV area makes reads differ; halt the core (DebugProbe.halt) and back up again", m.SHA256, m.SecondRead)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ieee := strings.ToLower(strings.ReplaceAll(m.IEEE, ":", ""))
	m.File = fmt.Sprintf("cc26xx-flash-%s-%s.bin", ieee, m.SHA256[:12])
	final := filepath.Join(dir, m.File)
	reused := false
	if old, err := os.ReadFile(final); err == nil {
		if shaHex(old) != m.SHA256 {
			return nil, status.Errorf(codes.AlreadyExists, "%s exists with different content: refusing to overwrite it", final)
		}
		reused = true
	} else if err := writeNew(final, first, false); err != nil {
		return nil, err
	}
	if !reused {
		m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := writeNew(final+".json", append(mb, '\n'), false); err != nil {
			return nil, err
		}
	}
	cfg := first[c.chip.ConfigAddr:]
	return map[string]any{
		"path": final, "manifest": final + ".json", "size_bytes": float64(len(first)), "sha256": m.SHA256, "second_read_sha256": m.SecondRead,
		"identical_reads": true, "read_seconds": m.ReadSeconds, "chip": c.chip.Name, "ieee": m.IEEE, "already_present": reused,
		"ccfg": ccfgSummary(cfg),
		"note": "local artifact: it holds the complete firmware and the chip's identity, never commit it; " + cc26ZigbeeWarn,
	}, nil
}

func loadBackup(path string) ([]byte, *backupManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	mb, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, nil, fmt.Errorf("no manifest %s.json: only backups written by FlashProgramming.backup are accepted", path)
	}
	var m backupManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Format != cc26BackupFormat {
		return nil, nil, fmt.Errorf("manifest format %q, want %q", m.Format, cc26BackupFormat)
	}
	if shaHex(b) != m.SHA256 || len(b) != m.SizeBytes {
		return nil, nil, fmt.Errorf("backup %s does not match the SHA-256/size in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead {
		return nil, nil, errors.New("the manifest does not record agreeing reads")
	}
	return b, &m, nil
}

// loadGate requires a verified full backup of THIS chip.
func (c *cc26) loadGate(a map[string]any) ([]byte, map[string]any, error) {
	path, _ := a["backup"].(string)
	if path == "" {
		return nil, nil, status.Error(codes.PermissionDenied, "refusing to modify flash without a verified full backup of this chip: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	id := c.identity()
	if m.ICEPick != id.ICEPick || m.UserID != id.UserID || m.IEEE != id.IEEE || m.FlashBase != id.FlashBase || m.SizeBytes != id.SizeBytes {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another chip (%s %s IEEE %s, %d bytes) than the connected one (%s %s IEEE %s, %d bytes)",
			path, m.Chip, m.ICEPick, m.IEEE, m.SizeBytes, id.Chip, id.ICEPick, id.IEEE, id.SizeBytes)
	}
	return b, map[string]any{"path": path, "sha256": m.SHA256, "ieee": m.IEEE, "chip_matches": true}, nil
}

func (c *cc26) needWritable() error {
	if !c.chip.Writable || !c.chip.Agama() {
		why := c.chip.ReadOnlyWhy
		if why == "" {
			why = c.chip.Series + " is not a CC13x2/CC26x2: the debug flash path uses their ROM flash API"
		}
		return status.Errorf(codes.FailedPrecondition, "%s: modification refused: %s", c.chip.Name, why)
	}
	return nil
}

// perm checks the device-level permissions set when the device was opened.
func (c *cc26) perm(flash, erase, ccfg bool) error {
	p := c.d.o.cc.Perms
	var miss []string
	if flash && !p.Flash {
		miss = append(miss, "allow_flash")
	}
	if erase && !p.Erase {
		miss = append(miss, "allow_erase")
	}
	if ccfg && !p.CCFGWrite {
		miss = append(miss, "allow_ccfg_write")
	}
	if len(miss) > 0 {
		return status.Errorf(codes.PermissionDenied, "this needs the device opened with %s=true (device-level permissions cannot be granted per call)", strings.Join(miss, "=true, "))
	}
	return nil
}

// sectorPlan is one sector to erase and rewrite with want.
type sectorPlan struct {
	addr uint32
	want []byte
}

type writePlan struct {
	sectors   []sectorPlan
	unchanged int
	config    bool
	cfgCur    []byte
	cfgNext   []byte
	viol      []string
	notes     []string
}

func (c *cc26) sector() uint32 { return uint32(c.chip.SectorBytes) }

// plan works out which sectors change when img is written at addr; the bytes
// outside the image are kept. The CCFG sector goes last.
func (c *cc26) plan(addr uint32, img []byte) (*writePlan, error) {
	sec := c.sector()
	p := &writePlan{}
	end := addr + uint32(len(img))
	var cfgSector *sectorPlan
	for s := addr / sec * sec; s < end; s += sec {
		cur, err := c.read(s, int(sec))
		if err != nil {
			return nil, err
		}
		lo, hi := max(addr, s), min(end, s+sec)
		want := append([]byte(nil), cur...)
		copy(want[lo-s:], img[lo-addr:hi-addr])
		if bytes.Equal(cur, want) {
			p.unchanged++
			continue
		}
		sp := sectorPlan{addr: s, want: want}
		if s == c.chip.ConfigSector() {
			cfgSector = &sp
			continue
		}
		p.sectors = append(p.sectors, sp)
	}
	if cfgSector != nil {
		p.sectors = append(p.sectors, *cfgSector)
		if err := c.checkConfig(p, cfgSector.want); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// blankDebugRisk is the extra rule of the debug path: a blank CCFG leaves
// CPU_DAP_ENABLE at 0xFF.
const blankDebugRisk = "the CCFG would be blank: CPU_DAP_ENABLE reads 0xFF, so the debug port may stay closed after the next reset (recovery: ROM bootloader, which a blank image starts, or the ICEPick chip erase)"

// checkConfig compares the CCFG the sector would hold with the current one.
func (c *cc26) checkConfig(p *writePlan, sectorWant []byte) error {
	cur, err := c.config()
	if err != nil {
		return err
	}
	off := c.chip.ConfigAddr - c.chip.ConfigSector()
	p.config, p.cfgCur = true, cur
	p.cfgNext = append([]byte(nil), sectorWant[off:off+cc26xx.CCFGLen]...)
	p.viol, p.notes = cc26xx.CheckCCFG(cur, p.cfgNext)
	if cc26xx.Blank(p.cfgNext) {
		p.viol = append(p.viol, blankDebugRisk)
	}
	return nil
}

// configGate enforces the protected region: confirm_config and the CCFG
// write permission, then the lockout check.
func (c *cc26) configGate(a map[string]any, p *writePlan) error {
	if !p.config {
		return nil
	}
	if err := c.perm(false, false, true); err != nil {
		return err
	}
	if err := tokenIs(a, "confirm_config", tokCCFG, "changing the CCFG sector (the last flash sector: bootloader, debug and protection configuration)"); err != nil {
		return err
	}
	if len(p.viol) > 0 {
		if s, _ := a["ccfg_override"].(string); s != tokLockout {
			return status.Errorf(codes.PermissionDenied, "refusing: the new CCFG would lock this chip out (%s). Fix the image's CCFG, or pass ccfg_override=%q if this is intended",
				strings.Join(p.viol, "; "), tokLockout)
		}
	}
	return nil
}

func (p *writePlan) report(res map[string]any) {
	addrs := make([]any, len(p.sectors))
	for i, s := range p.sectors {
		addrs[i] = hex32(s.addr)
	}
	res["sectors_changed"] = float64(len(p.sectors))
	res["sectors_unchanged"] = float64(p.unchanged)
	res["sector_addresses"] = addrs
	res["config_sector_changes"] = p.config
	if p.config {
		res["ccfg_lockout_violations"] = toAny(p.viol)
		if len(p.notes) > 0 {
			res["ccfg_notes"] = toAny(p.notes)
		}
	}
}

// runs returns the 16-byte aligned spans of data that hold non-erased bytes,
// merging spans separated by fewer than 64 erased bytes.
func runs(data []byte) [][2]int {
	const gran, gap = 16, 64
	var out [][2]int
	for i := 0; i < len(data); i += gran {
		if cc26xx.Blank(data[i:min(i+gran, len(data))]) {
			continue
		}
		if n := len(out); n > 0 && i-out[n-1][1] < gap {
			out[n-1][1] = min(i+gran, len(data))
			continue
		}
		out = append(out, [2]int{i, min(i+gran, len(data))})
	}
	return out
}

// flasher runs the ROM flash API for one modifying call.
type flasher struct {
	c   *cc26
	rom *cc26xx.ROMFlash
}

func (c *cc26) flasher() (*flasher, error) {
	rom, err := cc26xx.NewROMFlash(c.ctx, c.d.core, cc26xx.CoreMem{C: c.d.core})
	if err != nil {
		return nil, err
	}
	if err := rom.Prepare(c.ctx); err != nil {
		return nil, err
	}
	return &flasher{c: c, rom: rom}, nil
}

func (f *flasher) finish() error { return f.rom.Finish(context.WithoutCancel(f.c.ctx)) }

// writeSector erases a sector, programs want and reads it back.
func (f *flasher) writeSector(addr uint32, want []byte) error {
	if err := f.rom.EraseSector(f.c.ctx, addr); err != nil {
		return err
	}
	for _, r := range runs(want) {
		if err := f.rom.Program(f.c.ctx, addr+uint32(r[0]), want[r[0]:r[1]]); err != nil {
			return fmt.Errorf("%w (sector 0x%08X is erased or partially written)", err, addr)
		}
	}
	got, err := f.c.read(addr, len(want))
	if err != nil {
		return fmt.Errorf("verify sector 0x%08X: %w", addr, err)
	}
	if !bytes.Equal(got, want) {
		return status.Errorf(codes.DataLoss, "sector 0x%08X reads back different data after programming (sha256 %s, want %s)", addr, shaHex(got), shaHex(want))
	}
	return nil
}

func (f *flasher) execute(p *writePlan) error {
	for n, s := range p.sectors {
		if err := f.writeSector(s.addr, s.want); err != nil {
			return fmt.Errorf("%w (%d of %d sectors done)", err, n, len(p.sectors))
		}
	}
	return nil
}

// after finishes a modifying call: restore the cache and, unless the caller
// asked otherwise, reset the target into its new image.
func (c *cc26) after(f *flasher, a map[string]any, res map[string]any) error {
	if err := f.finish(); err != nil {
		return fmt.Errorf("restore the flash cache mode: %w", err)
	}
	keep := false
	if v, set, err := optBool(a, "reset_after"); err != nil {
		return argErr(err)
	} else if set {
		keep = !v
	}
	if keep {
		res["core"] = "halted; SRAM 0x20000000..0x20003000 was used by the flash driver: reset before running the firmware"
		return nil
	}
	r, err := c.d.resetCJTAG(c.ctx, "sysreset", false)
	if err != nil {
		return fmt.Errorf("flash done and verified, but the reset failed: %w", err)
	}
	res["reset"] = r
	return nil
}

func (c *cc26) flashProgram(a map[string]any) (map[string]any, error) {
	dry, _, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	addr, _, err := optAddr(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	img, path, err := readImage(a, min(max(c.chip.FlashBytes, 1), maxImageFile))
	if err != nil {
		return nil, argErr(err)
	}
	if err := c.flashRange(addr, len(img)); err != nil {
		return nil, err
	}
	if err := c.needWritable(); err != nil {
		return nil, err
	}
	var bk map[string]any
	if !dry {
		if err := c.perm(true, true, false); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokProgram, "programming flash"); err != nil {
			return nil, err
		}
		if _, bk, err = c.loadGate(a); err != nil {
			return nil, err
		}
	}
	t0 := time.Now()
	p, err := c.plan(addr, img)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "bytes": float64(len(img)), "image_sha256": shaHex(img)}
	if path != "" {
		res["path"] = path
	}
	p.report(res)
	if dry {
		res["dry_run"] = true
		res["would_need"] = "allow_flash+allow_erase, confirm=" + tokProgram + ", backup=<FlashProgramming.backup .bin>" + map[bool]string{true: ", allow_ccfg_write, confirm_config=" + tokCCFG, false: ""}[p.config]
		if len(p.viol) > 0 {
			res["would_be_refused"] = "the CCFG lockout check fails: ccfg_override=" + tokLockout + " would be needed"
		}
		return res, nil
	}
	if err := c.configGate(a, p); err != nil {
		return nil, err
	}
	if len(p.sectors) == 0 {
		res["verified"], res["seconds"], res["backup"] = true, time.Since(t0).Seconds(), bk
		res["note"] = "the flash already holds the image: nothing was written and the core was not touched"
		return res, nil
	}
	f, err := c.flasher()
	if err != nil {
		return nil, err
	}
	if err := f.execute(p); err != nil {
		_ = f.finish()
		return nil, err
	}
	got, err := c.read(addr, len(img))
	if err != nil {
		_ = f.finish()
		return nil, fmt.Errorf("final verify: %w", err)
	}
	if !bytes.Equal(got, img) {
		_ = f.finish()
		return nil, status.Errorf(codes.DataLoss, "after programming 0x%08X+%d reads back sha256 %s, want %s", addr, len(img), shaHex(got), shaHex(img))
	}
	res["verified"], res["flash_sha256"], res["backup"] = true, shaHex(got), bk
	if err := c.after(f, a, res); err != nil {
		return nil, err
	}
	res["seconds"] = time.Since(t0).Seconds()
	return res, nil
}

func (c *cc26) flashRestore(a map[string]any) (map[string]any, error) {
	if _, ok := a["backup"]; !ok {
		if p, ok := a["path"].(string); ok {
			a["backup"] = p
		}
	}
	dry, _, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	if err := c.needWritable(); err != nil {
		return nil, err
	}
	if !dry {
		if err := c.perm(true, true, false); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokRestore, "restoring flash"); err != nil {
			return nil, err
		}
	}
	b, bk, err := c.loadGate(a)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	p, err := c.plan(0, b)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"backup": bk, "warning": cc26ZigbeeWarn}
	p.report(res)
	if dry {
		res["dry_run"] = true
		return res, nil
	}
	if err := c.configGate(a, p); err != nil {
		return nil, err
	}
	if len(p.sectors) == 0 {
		res["matches_backup"], res["seconds"] = true, time.Since(t0).Seconds()
		res["note"] = "the flash already equals the backup: nothing was written"
		return res, nil
	}
	f, err := c.flasher()
	if err != nil {
		return nil, err
	}
	if err := f.execute(p); err != nil {
		_ = f.finish()
		return nil, fmt.Errorf("restore: %w (the flash may be partially restored)", err)
	}
	got, err := c.read(0, len(b))
	if err != nil {
		_ = f.finish()
		return nil, fmt.Errorf("final verify: %w", err)
	}
	if shaHex(got) != bk["sha256"] {
		_ = f.finish()
		return nil, status.Errorf(codes.DataLoss, "after restore the flash reads sha256 %s, the backup's %s", shaHex(got), bk["sha256"])
	}
	res["matches_backup"] = true
	if err := c.after(f, a, res); err != nil {
		return nil, err
	}
	res["seconds"] = time.Since(t0).Seconds()
	return res, nil
}

// securityAfterBankErase is the CCFG a bank erase leaves: blank, with TI's
// default security block (debug and every TAP enabled, bootloader enabled)
// and the current bootloader backdoor kept when the current CCFG enables it.
func securityAfterBankErase(cur []byte) []byte {
	next := bytes.Repeat([]byte{0xFF}, cc26xx.CCFGLen)
	copy(next[cc26xx.SecurityOffset:], cc26xx.DefaultSecurity)
	if bd := cc26xx.BackdoorOf(cur); bd.BootloaderEnabled && !cc26xx.Blank(cur) {
		copy(next[cc26xx.CCFGBLConfig:cc26xx.CCFGBLConfig+4], cur[cc26xx.CCFGBLConfig:cc26xx.CCFGBLConfig+4])
	}
	return next
}

func (c *cc26) flashErase(a map[string]any) (map[string]any, error) {
	sec := c.sector()
	mode, _ := a["mode"].(string)
	if mode == "" {
		mode = "region"
	}
	if mode != "region" && mode != "bank" {
		return nil, argErr(fmt.Errorf("mode %q: want region or bank", mode))
	}
	if err := c.needWritable(); err != nil {
		return nil, err
	}
	if err := c.perm(false, true, false); err != nil {
		return nil, err
	}
	addr, n := uint32(0), c.chip.FlashBytes
	if mode == "region" {
		var ok bool
		var err error
		if addr, ok, err = optAddr(a, "address"); err != nil {
			return nil, argErr(err)
		} else if !ok {
			return nil, argErr(errors.New("mode=region needs address and length"))
		}
		nf, ok, err := optNumber(a, "length")
		if err != nil || !ok || nf < 1 || nf > float64(c.chip.FlashBytes) {
			return nil, argErr(fmt.Errorf("length: want 1..%d", c.chip.FlashBytes))
		}
		if addr%sec != 0 {
			return nil, argErr(fmt.Errorf("address 0x%08X is not aligned to the %d-byte sector", addr, sec))
		}
		n = (int(nf) + int(sec) - 1) / int(sec) * int(sec)
		if err := c.flashRange(addr, n); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokErase, "erasing flash sectors"); err != nil {
			return nil, err
		}
	} else if err := tokenIs(a, "confirm", tokBankErase, "erasing the whole flash (including the CCFG)"); err != nil {
		return nil, err
	}
	_, bk, err := c.loadGate(a)
	if err != nil {
		return nil, err
	}
	p := &writePlan{}
	cs := c.chip.ConfigSector()
	var cfgWant []byte
	if cs >= addr && cs < addr+uint32(n) {
		cfgWant = bytes.Repeat([]byte{0xFF}, int(sec))
		if mode == "bank" {
			cur, err := c.config()
			if err != nil {
				return nil, err
			}
			copy(cfgWant[c.chip.ConfigAddr-cs:], securityAfterBankErase(cur))
		}
		if err := c.checkConfig(p, cfgWant); err != nil {
			return nil, err
		}
		if err := c.configGate(a, p); err != nil {
			return nil, err
		}
	}
	t0 := time.Now()
	res := map[string]any{"mode": mode, "erased_from": hex32(addr), "erased_to": hex32(addr + uint32(n)), "sectors": float64(n / int(sec)), "backup": bk, "config_sector_erased": p.config}
	if p.config {
		res["ccfg_lockout_violations"] = toAny(p.viol)
		res["ccfg_notes"] = toAny(p.notes)
	}
	f, err := c.flasher()
	if err != nil {
		return nil, err
	}
	var order []uint32
	for s := addr; s < addr+uint32(n); s += sec {
		if s != cs {
			order = append(order, s)
		}
	}
	for i, s := range order {
		if err := f.rom.EraseSector(c.ctx, s); err != nil {
			_ = f.finish()
			return nil, fmt.Errorf("%w (%d of %d sectors erased)", err, i, len(order))
		}
	}
	if p.config {
		if err := f.writeSector(cs, cfgWant); err != nil {
			_ = f.finish()
			return nil, fmt.Errorf("CCFG sector: %w", err)
		}
		if mode == "bank" {
			res["ccfg_written"] = "TI default security block (debug and TAPs enabled, bootloader enabled, current backdoor kept); IMAGE_VALID_CONF stays erased, so the ROM starts the bootloader"
		}
	}
	got, err := c.read(addr, n)
	if err != nil {
		_ = f.finish()
		return nil, fmt.Errorf("blank check: %w", err)
	}
	for s := addr; s < addr+uint32(n); s += sec {
		want := bytes.Repeat([]byte{0xFF}, int(sec))
		if s == cs && p.config {
			want = cfgWant
		}
		if !bytes.Equal(got[s-addr:s-addr+sec], want) {
			_ = f.finish()
			return nil, status.Errorf(codes.DataLoss, "sector 0x%08X does not read back as erased", s)
		}
	}
	res["blank_verified"] = true
	if err := c.after(f, a, res); err != nil {
		return nil, err
	}
	res["erase_seconds"] = time.Since(t0).Seconds()
	return res, nil
}

// chipEraseICEPick erases the whole chip through the ICEPick's chip erase
// test TAP. It needs no CPU DAP, so it recovers a chip whose CCFG locks the
// debug port. Everything is erased: firmware, NV, CCFG (and with it any
// IEEE override); the factory FCFG1 stays.
func (d *device) chipEraseICEPick(o options, a map[string]any) (map[string]any, error) {
	if !o.cc.ChipErase {
		return nil, status.Error(codes.PermissionDenied, "the ICEPick chip erase needs the device opened with allow_chip_erase=true")
	}
	if err := tokenIs(a, "confirm", tokICEPickErase, "erasing the whole chip (flash, NV and CCFG) through the ICEPick"); err != nil {
		return nil, err
	}
	if o.iface != ifaceCJTAG {
		return nil, argErr(errors.New("the ICEPick chip erase needs interface=cjtag"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.detach(); err != nil {
		return nil, err
	}
	if _, err := d.probe.SetClock(o.clockHz / 1000); err != nil {
		return nil, err
	}
	eng, err := d.activateCJTAG(o)
	if err != nil {
		return nil, err
	}
	r := &icepick.Router{S: eng}
	id, _, err := r.Identify(ctx)
	if err != nil {
		_ = d.probe.CJTAGDisconnect()
		return nil, noICEPickHint(err)
	}
	t0 := time.Now()
	if err := r.ChipErase(ctx); err != nil {
		_ = d.probe.CJTAGDisconnect()
		return nil, err
	}
	_ = d.probe.CJTAGDisconnect()
	res := map[string]any{"icepick_idcode": hex32(id), "requested": true}
	// The erase resets the device; give it time, then see whether the DAP
	// is reachable again.
	time.Sleep(500 * time.Millisecond)
	if err := d.attach(o); err != nil {
		res["reattached"] = false
		res["reattach_error"] = err.Error()
		res["note"] = "the chip erase was requested; the debug port did not come back yet. Power-cycle the target and run DebugProbe.chip_info with interface=cjtag"
		return res, nil
	}
	res["reattached"] = true
	res["seconds"] = time.Since(t0).Seconds()
	if c, err := d.cc26(ctx); err == nil {
		if cfg, err := c.config(); err == nil {
			res["ccfg_blank"] = cc26xx.Blank(cfg)
			res["ccfg"] = ccfgSummary(cfg)
		}
		if b, err := c.read(0, 4096); err == nil {
			res["first_4k_blank"] = cc26xx.Blank(b)
		}
	}
	res["note"] = "VALIDATED ON A CC2650 by the OpenOCD procedure this follows, NOT on hardware by hwlab; check ccfg_blank/first_4k_blank"
	return res, nil
}
