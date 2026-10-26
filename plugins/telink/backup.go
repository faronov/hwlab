package telinkplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/telink/sws"
	"github.com/faronov/hwlab/plugins/telink/target"
)

const backupFormat = "hwlab-telink-flash-backup/1"

// backupManifest is stored next to every backup as <file>.json. It ties the
// bytes to one physical board: the flash unique id, the JEDEC id and the
// target chip id must all match before a backup is accepted as evidence.
type backupManifest struct {
	Format             string  `json:"format"`
	CreatedUTC         string  `json:"created_utc"`
	File               string  `json:"file"`
	SizeBytes          int     `json:"size_bytes"`
	SHA256             string  `json:"sha256"`
	SecondRead         string  `json:"second_read_sha256"`
	ChipID             string  `json:"target_chip_id"`
	ChipRevision       string  `json:"target_revision"`
	Family             string  `json:"target_family"`
	FlashJEDEC         string  `json:"flash_jedec"`
	FlashUID           string  `json:"flash_uid"`
	ProgrammerFirmware string  `json:"programmer_firmware"`
	Port               string  `json:"port,omitempty"`
	ReadSeconds        float64 `json:"read_seconds"`
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func defaultBackupDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "telink-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "telink-flash")
}

// boardIdentity is what identifies the connected board for backups.
type boardIdentity struct {
	chipID   uint16
	rev      byte
	jedec    uint32
	size     uint32
	uid      string
	firmware uint16
}

func (d *device) identity() (boardIdentity, error) {
	var b boardIdentity
	size, jedec, err := d.flashBytes()
	if err != nil {
		return b, err
	}
	uid, err := d.prog.FlashUID()
	if err != nil {
		return b, status.Errorf(codes.FailedPrecondition, "cannot read the flash unique id that ties a backup to this board: %v", err)
	}
	rev, id, err := d.prog.ReadChipID()
	if err != nil {
		return b, err
	}
	b = boardIdentity{chipID: id, rev: rev, jedec: jedec, size: size, uid: hex.EncodeToString(uid)}
	if v, ok := d.prog.Cached(); ok {
		b.firmware = v.Firmware
	}
	if b.uid == "ffffffffffffffff" || b.uid == "0000000000000000" {
		return b, status.Errorf(codes.FailedPrecondition, "flash unique id reads %s: it cannot identify this board", b.uid)
	}
	return b, nil
}

// flashBackup reads the whole flash twice, requires identical SHA-256 and
// stores it with a manifest. It never modifies flash.
func (d *device) flashBackup(a map[string]any) (map[string]any, error) {
	if err := d.requireCaptured(); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error()+" (flash is only readable reliably with the CPU captured)")
	}
	dir, _ := a["dir"].(string)
	if dir == "" {
		dir = defaultBackupDir()
	}
	id, err := d.identity()
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	first, err := d.prog.FlashRead(0, int(id.size))
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := d.prog.FlashRead(0, int(id.size))
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	m := backupManifest{
		Format: backupFormat, SizeBytes: len(first), SHA256: shaHex(first), SecondRead: shaHex(second),
		ChipID: fmt.Sprintf("0x%04x", id.chipID), ChipRevision: fmt.Sprintf("0x%02x", id.rev), Family: familyName(id.chipID),
		FlashJEDEC: fmt.Sprintf("0x%06x", id.jedec), FlashUID: id.uid, ProgrammerFirmware: fmt.Sprintf("0x%04x", id.firmware),
		Port: d.entry.Port, ReadSeconds: time.Since(t0).Seconds(),
	}
	if m.SHA256 != m.SecondRead {
		return nil, status.Errorf(codes.DataLoss, "the two full-flash reads differ (%s vs %s): no backup was stored", m.SHA256, m.SecondRead)
	}
	// The second read must also agree with the board identity read before.
	if after, err := d.identity(); err != nil || after != id {
		return nil, status.Errorf(codes.DataLoss, "the board identity changed during the backup (%v): no backup was stored", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m.File = fmt.Sprintf("telink-flash-%s-%s.bin", id.uid, m.SHA256[:12])
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
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	if !reused {
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"path": final, "manifest": final + ".json", "size_bytes": float64(len(first)), "sha256": m.SHA256,
		"second_read_sha256": m.SecondRead, "identical_reads": true, "read_seconds": m.ReadSeconds,
		"flash_uid": id.uid, "flash_jedec": m.FlashJEDEC, "target_chip_id": m.ChipID, "already_present": reused,
		"note": "local artifact: it holds the complete firmware and board identity, never commit it",
	}, nil
}

func familyName(id uint16) string {
	if f := target.Lookup(id); f != nil {
		return f.Name
	}
	return "unknown"
}

// loadBackup reads a backup and checks it against its manifest.
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
	if m.SHA256 != m.SecondRead {
		return nil, nil, errors.New("the manifest does not record two identical reads")
	}
	return b, &m, nil
}

// backupGate requires a verified full backup of THIS board (same flash unique
// id, JEDEC id, target chip id and size) whose factory regions still equal
// the flash. With that, the board's original state stays restorable with
// FlashProgramming.restore whatever the modification does to application
// and journal sectors; factory data can never be rewritten by this plugin,
// so it must already be what the backup holds.
func (d *device) backupGate(a map[string]any, prot target.Protection) ([]byte, map[string]any, error) {
	path, _ := a["backup"].(string)
	if path == "" {
		return nil, nil, status.Error(codes.PermissionDenied, "refusing to modify flash without a verified full backup of this board: take one with FlashProgramming.backup (CPU halted) and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	id, err := d.identity()
	if err != nil {
		return nil, nil, err
	}
	cur := backupManifest{ChipID: fmt.Sprintf("0x%04x", id.chipID), FlashJEDEC: fmt.Sprintf("0x%06x", id.jedec), FlashUID: id.uid, SizeBytes: int(id.size)}
	if m.FlashUID != cur.FlashUID || m.FlashJEDEC != cur.FlashJEDEC || m.ChipID != cur.ChipID || m.SizeBytes != cur.SizeBytes {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another board (uid %s, jedec %s, chip %s, %d bytes) than the connected one (uid %s, jedec %s, chip %s, %d bytes)",
			path, m.FlashUID, m.FlashJEDEC, m.ChipID, m.SizeBytes, cur.FlashUID, cur.FlashJEDEC, cur.ChipID, cur.SizeBytes)
	}
	checked := 0
	for _, r := range prot.Layout.Regions {
		if r.Class != sws.ClassFactory {
			continue
		}
		got, err := d.prog.FlashRead(r.Start, int(r.End-r.Start))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s for the backup check: %w", r.Name, err)
		}
		if !bytes.Equal(got, b[r.Start:r.End]) {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "%s (0x%05x-0x%05x) differs from backup %s: factory data changed after the backup; refusing to modify flash", r.Name, r.Start, r.End, path)
		}
		checked++
	}
	return b, map[string]any{"path": path, "sha256": m.SHA256, "flash_uid": m.FlashUID, "board_matches": true, "factory_regions_match": float64(checked)}, nil
}

// flashRestore writes back every application/journal sector that differs from
// the backup, then reads the whole flash and compares it with the backup.
// Factory and undescribed sectors are never written: if they differ, nothing
// is written at all.
func (d *device) flashRestore(a map[string]any) (map[string]any, error) {
	if err := d.requireCaptured(); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if _, ok := a["backup"]; !ok {
		// hwlab target flash restore --path BACKUP.bin
		if p, ok := a["path"].(string); ok {
			a["backup"] = p
		}
	}
	pol, err := d.policy(a)
	if err != nil {
		return nil, argErr(err)
	}
	prot, err := d.modifiable("restore")
	if err != nil {
		return nil, err
	}
	skipJ, err := optBool(a, "skip_journals")
	if err != nil {
		return nil, argErr(err)
	}
	if skipJ && pol.Journals {
		return nil, argErr(errors.New("allow_journals and skip_journals exclude each other"))
	}
	b, bk, err := d.backupGate(a, prot)
	if err != nil {
		return nil, err
	}
	journal := func(s uint32) string {
		for _, r := range prot.Layout.Regions {
			if r.Class == sws.ClassJournal && s >= r.Start && s < r.End {
				return r.Name
			}
		}
		return ""
	}
	cur, err := d.prog.FlashRead(0, len(b))
	if err != nil {
		return nil, err
	}
	type run struct{ start, end uint32 }
	var runs []run
	var skipped []any
	differing := 0
	for s := uint32(0); s < uint32(len(b)); s += sws.FlashSector {
		e := s + sws.FlashSector
		if bytes.Equal(cur[s:e], b[s:e]) {
			continue
		}
		differing++
		if name := journal(s); skipJ && name != "" {
			skipped = append(skipped, fmt.Sprintf("%s 0x%05x", name, s))
			continue
		}
		if err := prot.Layout.CheckModify(s, sws.FlashSector, pol); err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "sector 0x%05x differs from the backup but may not be written: %v; nothing was written", s, err)
		}
		if n := len(runs); n > 0 && runs[n-1].end == s {
			runs[n-1].end = e
		} else {
			runs = append(runs, run{s, e})
		}
	}
	res := map[string]any{"backup": bk, "sectors_differing": float64(differing)}
	written := 0
	for _, r := range runs {
		rep, err := d.prog.Program(prot.Layout, r.start, b[r.start:r.end], pol)
		if err != nil {
			return nil, fmt.Errorf("restore 0x%05x-0x%05x: %w (%d sectors restored before; the flash may be partially restored)", r.start, r.end, err, written)
		}
		written += rep.SectorsErased
	}
	after, err := d.prog.FlashRead(0, len(b))
	if err != nil {
		return nil, fmt.Errorf("final read: %w", err)
	}
	res["sectors_restored"] = float64(written)
	res["flash_sha256"] = shaHex(after)
	res["matches_backup"] = bytes.Equal(after, b)
	res["journal_sectors_kept"] = skipped
	for s := uint32(0); s < uint32(len(b)); s += sws.FlashSector {
		e := s + sws.FlashSector
		if bytes.Equal(after[s:e], b[s:e]) {
			continue
		}
		if skipJ && journal(s) != "" && bytes.Equal(after[s:e], cur[s:e]) {
			continue // deliberately kept, and unchanged by the restore
		}
		return nil, status.Errorf(codes.DataLoss, "after restore sector 0x%05x still differs from the backup", s)
	}
	return res, nil
}

// flashInfo describes the flash and its protection map without changing it.
func (d *device) flashInfo() (map[string]any, error) {
	size, jedec, err := d.flashBytes()
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"flash_jedec": fmt.Sprintf("0x%06x", jedec), "flash_bytes": float64(size), "flash_kib": float64(size >> 10),
		"sector_bytes": float64(sws.FlashSector), "page_bytes": float64(sws.FlashPage), "layout_selected": d.o.layout,
		"backup_required_for_modification": true, "layouts_available": anySlice(target.AllLayoutNames()),
	}
	if uid, err := d.prog.FlashUID(); err == nil {
		res["flash_uid"] = hex.EncodeToString(uid)
	}
	prot, perr := d.protection()
	if perr != nil {
		res["protection_error"] = perr.Error()
		return res, nil
	}
	res["protection_verified"], res["protection_reason"], res["layout"] = prot.Verified, prot.Reason, prot.Layout.Name
	var regions []any
	for _, r := range prot.Layout.Regions {
		regions = append(regions, map[string]any{"name": r.Name, "start": hex24(r.Start), "end": hex24(r.End), "class": string(r.Class)})
	}
	res["regions"] = regions
	return res, nil
}

// chipInfo identifies the target from its SWire chip id and flash id.
func (d *device) chipInfo() (map[string]any, error) {
	rev, id, err := d.prog.ReadChipID()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the target does not answer over SWire (%v): run attach first", err)
	}
	res := map[string]any{"target_chip_id": fmt.Sprintf("0x%04x", id), "target_revision": fmt.Sprintf("0x%02x", rev), "target_chip": sws.ChipName(id)}
	jedec, jerr := d.prog.JEDEC()
	if jerr != nil {
		jedec = 0
	}
	ident := target.Identify(id, rev, jedec)
	res["target_family"], res["target_variant"] = ident.Family, ident.Variant
	if f := target.Lookup(id); f != nil {
		res["target_variant_note"] = f.VariantNote
	}
	if ident.FlashBytes != 0 {
		res["flash_jedec"], res["flash_kib"] = fmt.Sprintf("0x%06x", jedec), float64(ident.FlashBytes>>10)
		if uid, err := d.prog.FlashUID(); err == nil {
			res["flash_uid"] = hex.EncodeToString(uid)
		}
	} else {
		res["flash_note"] = "flash id not plausible: halt the CPU to read the flash identity"
	}
	return res, nil
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
