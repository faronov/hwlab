package tibslplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/tibsl/bsl"
)

const (
	backupFormat = "hwlab-tibsl-flash-backup/1"
	zigbeeWarn   = "never restore this backup onto a device that has joined a Zigbee network since: it holds the network key, frame counters and identity of that time; restore only onto the same chip"
)

// backupManifest is stored next to every backup as <file>.json and ties the
// bytes to one chip.
type backupManifest struct {
	Format      string  `json:"format"`
	CreatedUTC  string  `json:"created_utc"`
	File        string  `json:"file"`
	SizeBytes   int     `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	SecondRead  string  `json:"second_read_sha256"`
	CRC32       string  `json:"crc32"`
	ChipCRC32   string  `json:"chip_crc32"`
	Family      string  `json:"family"`
	Chip        string  `json:"chip"`
	ChipID      string  `json:"chip_id"`
	ICEPick     string  `json:"icepick_device_id,omitempty"`
	UserID      string  `json:"user_id,omitempty"`
	IEEE        string  `json:"ieee"`
	FlashBase   string  `json:"flash_base"`
	Port        string  `json:"port,omitempty"`
	ReadSeconds float64 `json:"read_seconds"`
	Warning     string  `json:"warning"`
}

func defaultBackupDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "tibsl-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "tibsl-flash")
}

func (d *device) identity() backupManifest {
	c := d.chip
	m := backupManifest{Family: c.Family.String(), Chip: c.Name, ChipID: hex32(c.ChipID), IEEE: c.IEEEString(), FlashBase: hex32(c.FlashBase), SizeBytes: c.FlashBytes}
	if c.Family == bsl.FamilyCC26xx {
		m.ICEPick, m.UserID = hex32(c.ICEPick), hex32(c.UserID)
	}
	return m
}

// flashBackup reads the whole flash twice and has the ROM compute its CRC32;
// all must agree. It never modifies flash.
func (d *device) flashBackup(a map[string]any) (map[string]any, error) {
	c := d.chip
	if err := d.flashRange(c.FlashBase, c.FlashBytes); err != nil {
		return nil, err
	}
	dir, _ := a["dir"].(string)
	if dir == "" {
		dir = defaultBackupDir()
	}
	t0 := time.Now()
	first, err := d.cl.Read(c.FlashBase, c.FlashBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := d.cl.Read(c.FlashBase, c.FlashBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	chip, err := d.cl.CRC32(c.FlashBase, c.FlashBytes)
	if err != nil {
		return nil, fmt.Errorf("ROM CRC32: %w", err)
	}
	m := d.identity()
	m.Format, m.SHA256, m.SecondRead = backupFormat, shaHex(first), shaHex(second)
	m.CRC32, m.ChipCRC32 = crcHex(crc32.ChecksumIEEE(first)), crcHex(chip)
	m.Port, m.ReadSeconds, m.Warning = d.entry.Port, time.Since(t0).Seconds(), zigbeeWarn
	if m.SHA256 != m.SecondRead || m.CRC32 != m.ChipCRC32 {
		return nil, status.Errorf(codes.DataLoss, "full-flash reads disagree (read 1 %s, read 2 %s, CRC32 %s, ROM CRC32 %s): no backup was stored", m.SHA256, m.SecondRead, m.CRC32, m.ChipCRC32)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ieee := strings.ToLower(strings.ReplaceAll(m.IEEE, ":", ""))
	m.File = fmt.Sprintf("tibsl-flash-%s-%s.bin", ieee, m.SHA256[:12])
	final := filepath.Join(dir, m.File)
	reused := false
	if old, err := os.ReadFile(final); err == nil {
		if shaHex(old) != m.SHA256 {
			return nil, status.Errorf(codes.AlreadyExists, "%s exists with different content: refusing to overwrite it", final)
		}
		reused = true
	} else if err := os.WriteFile(final, first, 0o600); err != nil {
		return nil, err
	}
	if !reused {
		m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"path": final, "manifest": final + ".json", "size_bytes": float64(len(first)), "sha256": m.SHA256, "second_read_sha256": m.SecondRead,
		"crc32": m.CRC32, "chip_crc32": m.ChipCRC32, "identical_reads": true, "read_seconds": m.ReadSeconds, "chip": c.Name, "ieee": m.IEEE,
		"already_present": reused, "note": "local artifact: it holds the complete firmware and the chip's identity, never commit it; " + zigbeeWarn,
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
	if m.Format != backupFormat {
		return nil, nil, fmt.Errorf("manifest format %q, want %q", m.Format, backupFormat)
	}
	if shaHex(b) != m.SHA256 || len(b) != m.SizeBytes {
		return nil, nil, fmt.Errorf("backup %s does not match the SHA-256/size in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead || m.CRC32 != m.ChipCRC32 {
		return nil, nil, errors.New("the manifest does not record agreeing reads")
	}
	return b, &m, nil
}

// loadGate requires a verified full backup of THIS chip.
func (d *device) loadGate(a map[string]any) ([]byte, map[string]any, error) {
	path, _ := a["backup"].(string)
	if path == "" {
		return nil, nil, status.Error(codes.PermissionDenied, "refusing to modify flash without a verified full backup of this chip: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	id := d.identity()
	if m.Family != id.Family || m.ChipID != id.ChipID || m.ICEPick != id.ICEPick || m.UserID != id.UserID || m.IEEE != id.IEEE ||
		m.FlashBase != id.FlashBase || m.SizeBytes != id.SizeBytes {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another chip (%s %s IEEE %s, %d bytes) than the connected one (%s %s IEEE %s, %d bytes)",
			path, m.Chip, m.ICEPick, m.IEEE, m.SizeBytes, id.Chip, id.ICEPick, id.IEEE, id.SizeBytes)
	}
	return b, map[string]any{"path": path, "sha256": m.SHA256, "crc32": m.CRC32, "ieee": m.IEEE, "chip_matches": true}, nil
}

func (d *device) needWritable() error {
	if !d.chip.Writable {
		return status.Errorf(codes.FailedPrecondition, "%s: modification refused: %s", d.chip.Name, d.chip.ReadOnlyWhy)
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

func (d *device) sector() uint32 { return uint32(d.chip.SectorBytes) }

// plan works out which sectors change when img is written at addr. Fully
// covered sectors are compared by the ROM's CRC32, partly covered ones are
// read so the bytes outside the image are kept. The CCFG/CCA sector goes last.
func (d *device) plan(addr uint32, img []byte) (*writePlan, error) {
	c, sec := d.chip, d.sector()
	p := &writePlan{}
	end := addr + uint32(len(img))
	first := c.FlashBase + (addr-c.FlashBase)/sec*sec
	var cfgSector *sectorPlan
	for s := first; s < end; s += sec {
		lo, hi := max(addr, s), min(end, s+sec)
		var want []byte
		if lo == s && hi == s+sec {
			want = img[s-addr : s-addr+sec]
			chip, err := d.cl.CRC32(s, int(sec))
			if err != nil {
				return nil, err
			}
			if chip == crc32.ChecksumIEEE(want) {
				p.unchanged++
				continue
			}
		} else {
			cur, _, err := d.readChecked(s, int(sec))
			if err != nil {
				return nil, err
			}
			want = append([]byte(nil), cur...)
			copy(want[lo-s:], img[lo-addr:hi-addr])
			if bytes.Equal(cur, want) {
				p.unchanged++
				continue
			}
		}
		sp := sectorPlan{addr: s, want: want}
		if s == c.ConfigSector() {
			cfgSector = &sp
			continue
		}
		p.sectors = append(p.sectors, sp)
	}
	if cfgSector != nil {
		p.sectors = append(p.sectors, *cfgSector)
		if err := d.checkConfig(p, cfgSector.want); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// checkConfig compares the CCFG/CCA the sector would hold with the current one.
func (d *device) checkConfig(p *writePlan, sectorWant []byte) error {
	cur, err := d.config()
	if err != nil {
		return err
	}
	off := d.chip.ConfigAddr - d.chip.ConfigSector()
	p.config, p.cfgCur = true, cur
	p.cfgNext = append([]byte(nil), sectorWant[off:off+uint32(d.chip.ConfigLen)]...)
	p.viol, p.notes = bsl.CheckConfig(d.chip.Family, cur, p.cfgNext)
	return nil
}

// configGate enforces the protected region: confirm_config, then the lockout check.
func configGate(a map[string]any, p *writePlan) error {
	if !p.config {
		return nil
	}
	if err := tokenIs(a, "confirm_config", tokCCFG, "changing the CCFG/CCA sector (the last flash page: bootloader, debug and protection configuration)"); err != nil {
		return err
	}
	if len(p.viol) > 0 {
		if s, _ := a["ccfg_override"].(string); s != tokLockout {
			return status.Errorf(codes.PermissionDenied, "refusing: the new CCFG/CCA would lock this chip out (%s); recovery would need JTAG. Fix the image's CCFG, or pass ccfg_override=%q if this is intended",
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

func (d *device) eraseSector(addr uint32) error {
	if d.chip.Family == bsl.FamilyCC2538 {
		return d.cl.EraseRange(addr, d.chip.SectorBytes)
	}
	return d.cl.SectorErase(addr)
}

// runGap merges programmed runs separated by fewer erased bytes than this.
const runGap = 64

// writeRuns programs the non-erased words of data at addr.
func (d *device) writeRuns(addr uint32, data []byte) error {
	blank := func(i int) bool { return bsl.Blank(data[i : i+4]) }
	for i := 0; i < len(data); {
		if blank(i) {
			i += 4
			continue
		}
		j, gap := i, 0
		for k := i; k < len(data) && gap < runGap; k += 4 {
			if blank(k) {
				gap += 4
				continue
			}
			gap, j = 0, k+4
		}
		if err := d.cl.Write(addr+uint32(i), data[i:j], nil); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// execute erases, rewrites and CRC-checks every planned sector.
func (d *device) execute(p *writePlan) error {
	for n, s := range p.sectors {
		if err := d.eraseSector(s.addr); err != nil {
			return fmt.Errorf("%w (%d of %d sectors done)", err, n, len(p.sectors))
		}
		if err := d.writeRuns(s.addr, s.want); err != nil {
			return fmt.Errorf("%w (sector 0x%08X is erased or partially written; %d of %d sectors done)", err, s.addr, n, len(p.sectors))
		}
		chip, err := d.cl.CRC32(s.addr, len(s.want))
		if err != nil {
			return fmt.Errorf("verify sector 0x%08X: %w", s.addr, err)
		}
		if want := crc32.ChecksumIEEE(s.want); chip != want {
			return status.Errorf(codes.DataLoss, "sector 0x%08X: ROM CRC32 %s after writing, want %s", s.addr, crcHex(chip), crcHex(want))
		}
	}
	return nil
}

func (d *device) flashProgram(a map[string]any) (map[string]any, error) {
	c := d.chip
	dry, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	resetAfter, err := optBool(a, "reset_after")
	if err != nil {
		return nil, argErr(err)
	}
	addr, ok, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		addr = c.FlashBase
	}
	img, path, err := readImage(a, max(c.FlashBytes, 1))
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.flashRange(addr, len(img)); err != nil {
		return nil, err
	}
	if err := d.needWritable(); err != nil {
		return nil, err
	}
	var bk map[string]any
	if !dry {
		if err := tokenIs(a, "confirm", tokProgram, "programming flash"); err != nil {
			return nil, err
		}
		if _, bk, err = d.loadGate(a); err != nil {
			return nil, err
		}
	}
	t0 := time.Now()
	p, err := d.plan(addr, img)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "bytes": float64(len(img)), "image_sha256": shaHex(img), "image_crc32": crcHex(crc32.ChecksumIEEE(img))}
	if path != "" {
		res["path"] = path
	}
	p.report(res)
	if dry {
		res["dry_run"] = true
		res["would_need"] = "confirm=" + tokProgram + ", backup=<FlashProgramming.backup .bin>" + map[bool]string{true: ", confirm_config=" + tokCCFG, false: ""}[p.config]
		if len(p.viol) > 0 {
			res["would_be_refused"] = "the CCFG lockout check fails: ccfg_override=" + tokLockout + " would be needed"
		}
		return res, nil
	}
	if err := configGate(a, p); err != nil {
		return nil, err
	}
	if err := d.execute(p); err != nil {
		return nil, err
	}
	chip, err := d.cl.CRC32(addr, len(img))
	if err != nil {
		return nil, fmt.Errorf("final verify: %w", err)
	}
	if want := crc32.ChecksumIEEE(img); chip != want {
		return nil, status.Errorf(codes.DataLoss, "after programming the ROM CRC32 of 0x%08X+%d is %s, want %s", addr, len(img), crcHex(chip), crcHex(want))
	}
	res["verified"], res["flash_crc32"], res["seconds"], res["backup"] = true, crcHex(chip), time.Since(t0).Seconds(), bk
	if resetAfter {
		r, err := d.reset()
		if err != nil {
			return nil, fmt.Errorf("programmed and verified, but reset failed: %w", err)
		}
		res["reset"] = r
	} else {
		res["next"] = "FlashProgramming.reset starts the new image"
	}
	return res, nil
}

func (d *device) flashRestore(a map[string]any) (map[string]any, error) {
	if _, ok := a["backup"]; !ok {
		if p, ok := a["path"].(string); ok {
			a["backup"] = p
		}
	}
	dry, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	resetAfter, err := optBool(a, "reset_after")
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.needWritable(); err != nil {
		return nil, err
	}
	if !dry {
		if err := tokenIs(a, "confirm", tokRestore, "restoring flash"); err != nil {
			return nil, err
		}
	}
	b, bk, err := d.loadGate(a)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	p, err := d.plan(d.chip.FlashBase, b)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"backup": bk, "warning": zigbeeWarn}
	p.report(res)
	if dry {
		res["dry_run"] = true
		return res, nil
	}
	if err := configGate(a, p); err != nil {
		return nil, err
	}
	if err := d.execute(p); err != nil {
		return nil, fmt.Errorf("restore: %w (the flash may be partially restored)", err)
	}
	chip, err := d.cl.CRC32(d.chip.FlashBase, len(b))
	if err != nil {
		return nil, fmt.Errorf("final verify: %w", err)
	}
	if got := crcHex(chip); got != bk["crc32"] {
		return nil, status.Errorf(codes.DataLoss, "after restore the ROM CRC32 of the flash is %s, the backup's %s", got, bk["crc32"])
	}
	res["matches_backup"], res["flash_crc32"], res["seconds"] = true, crcHex(chip), time.Since(t0).Seconds()
	if resetAfter {
		r, err := d.reset()
		if err != nil {
			return nil, fmt.Errorf("restored and verified, but reset failed: %w", err)
		}
		res["reset"] = r
	}
	return res, nil
}

func (d *device) blankCheck(addr uint32, n int) error {
	chip, err := d.cl.CRC32(addr, n)
	if err != nil {
		return fmt.Errorf("blank check: %w", err)
	}
	if want := crc32.ChecksumIEEE(bytes.Repeat([]byte{0xFF}, n)); chip != want {
		return status.Errorf(codes.DataLoss, "0x%08X+%d is not blank after erasing (ROM CRC32 %s, blank %s)", addr, n, crcHex(chip), crcHex(want))
	}
	return nil
}

func (d *device) flashErase(a map[string]any) (map[string]any, error) {
	c, sec := d.chip, d.sector()
	mode, _ := a["mode"].(string)
	if mode == "" {
		mode = "region"
	}
	if mode != "region" && mode != "bank" {
		return nil, argErr(fmt.Errorf("mode %q: want region or bank", mode))
	}
	if err := d.flashRange(c.FlashBase, c.FlashBytes); err != nil {
		return nil, err
	}
	if err := d.needWritable(); err != nil {
		return nil, err
	}
	addr, n := c.FlashBase, c.FlashBytes
	if mode == "region" {
		var ok bool
		var err error
		if addr, ok, err = u32Arg(a, "address"); err != nil {
			return nil, argErr(err)
		} else if !ok {
			return nil, argErr(errors.New("mode=region needs address and length"))
		}
		nf, ok, err := optNumber(a, "length")
		if err != nil || !ok || nf < 1 || nf > float64(c.FlashBytes) {
			return nil, argErr(fmt.Errorf("length: want 1..%d", c.FlashBytes))
		}
		if (addr-c.FlashBase)%sec != 0 {
			return nil, argErr(fmt.Errorf("address 0x%08X is not aligned to the %d-byte sector", addr, sec))
		}
		n = (int(nf) + int(sec) - 1) / int(sec) * int(sec)
		if err := d.flashRange(addr, n); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokErase, "erasing flash sectors"); err != nil {
			return nil, err
		}
	} else if err := tokenIs(a, "confirm", tokBankErase, "erasing the whole flash bank (including the CCFG)"); err != nil {
		return nil, err
	}
	_, bk, err := d.loadGate(a)
	if err != nil {
		return nil, err
	}
	p := &writePlan{}
	if cs := c.ConfigSector(); cs >= addr && cs < addr+uint32(n) {
		if err := d.checkConfig(p, bytes.Repeat([]byte{0xFF}, int(sec))); err != nil {
			return nil, err
		}
		if err := configGate(a, p); err != nil {
			return nil, err
		}
	}
	t0 := time.Now()
	res := map[string]any{"mode": mode, "erased_from": hex32(addr), "erased_to": hex32(addr + uint32(n)), "sectors": float64(n / int(sec)), "backup": bk, "config_sector_erased": p.config}
	if p.config {
		res["ccfg_notes"] = toAny(p.notes)
	}
	switch {
	case mode == "bank" && c.Family == bsl.FamilyCC26xx:
		if cur := p.cfgCur; len(cur) == bsl.CCFGLen && !bsl.Blank(cur) && cur[bsl.CCFGEraseConf]&1 == 0 {
			return nil, status.Error(codes.FailedPrecondition, "CCFG ERASE_CONF.BANK_ERASE_DIS_N is 0: the ROM refuses BANK_ERASE; erase sector by sector (mode=region)")
		}
		if err := d.cl.BankErase(); err != nil {
			return nil, err
		}
	case c.Family == bsl.FamilyCC2538:
		if err := d.cl.EraseRange(addr, n); err != nil {
			return nil, err
		}
	default:
		order := []uint32{}
		for s := addr; s < addr+uint32(n); s += sec {
			if s != c.ConfigSector() {
				order = append(order, s)
			}
		}
		if p.config {
			order = append(order, c.ConfigSector())
		}
		for i, s := range order {
			if err := d.cl.SectorErase(s); err != nil {
				return nil, fmt.Errorf("%w (%d of %d sectors erased)", err, i, len(order))
			}
		}
	}
	if err := d.blankCheck(addr, n); err != nil {
		return nil, err
	}
	res["blank_verified"], res["erase_seconds"] = true, time.Since(t0).Seconds()
	return res, nil
}
