package bflbplugin

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/bflb/isp"
)

const backupFormat = "hwlab-bflb-flash-backup/1"

// backupManifest is stored next to every backup as <file>.json and ties the
// bytes to one board: efuse MAC, flash JEDEC id and size.
type backupManifest struct {
	Format      string  `json:"format"`
	CreatedUTC  string  `json:"created_utc"`
	File        string  `json:"file"`
	SizeBytes   int     `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	SecondRead  string  `json:"second_read_sha256"`
	ChipSHA256  string  `json:"chip_sha256"`
	MAC         string  `json:"efuse_mac"`
	FlashJEDEC  string  `json:"flash_jedec"`
	Port        string  `json:"port,omitempty"`
	ReadSeconds float64 `json:"read_seconds"`
}

func defaultBackupDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "bflb-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "bflb-flash")
}

// flashBackup reads the whole flash twice and has the chip hash it; all three
// must agree. It never modifies flash.
func (d *device) flashBackup(a map[string]any) (map[string]any, error) {
	dir, _ := a["dir"].(string)
	if dir == "" {
		dir = defaultBackupDir()
	}
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	first, err := d.cl.Read(0, id.size)
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := d.cl.Read(0, id.size)
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	chip, err := d.cl.SHA256(0, id.size)
	if err != nil {
		return nil, fmt.Errorf("chip hash: %w", err)
	}
	m := backupManifest{
		Format: backupFormat, SizeBytes: len(first), SHA256: shaHex(first), SecondRead: shaHex(second), ChipSHA256: hex.EncodeToString(chip[:]),
		MAC: id.mac, FlashJEDEC: fmt.Sprintf("0x%06x", id.jedec), Port: d.entry.Port, ReadSeconds: time.Since(t0).Seconds(),
	}
	if m.SHA256 != m.SecondRead || m.SHA256 != m.ChipSHA256 {
		return nil, status.Errorf(codes.DataLoss, "full-flash reads disagree (read 1 %s, read 2 %s, chip %s): no backup was stored", m.SHA256, m.SecondRead, m.ChipSHA256)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m.File = fmt.Sprintf("bflb-flash-%s-%s.bin", id.mac, m.SHA256[:12])
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
		"second_read_sha256": m.SecondRead, "chip_sha256": m.ChipSHA256, "identical_reads": true, "read_seconds": m.ReadSeconds,
		"mac": id.mac, "flash_jedec": m.FlashJEDEC, "already_present": reused,
		"note": "local artifact: it holds the complete firmware and board identity, never commit it",
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
	if m.SHA256 != m.SecondRead || m.SHA256 != m.ChipSHA256 {
		return nil, nil, errors.New("the manifest does not record agreeing reads")
	}
	return b, &m, nil
}

// backupGate requires a verified full backup of THIS board (same efuse MAC,
// JEDEC id and size), so the original state stays restorable.
func (d *device) backupGate(a map[string]any, id flashID) (map[string]any, error) {
	_, info, err := d.loadGate(a, id)
	return info, err
}

func (d *device) loadGate(a map[string]any, id flashID) ([]byte, map[string]any, error) {
	path, _ := a["backup"].(string)
	if path == "" {
		return nil, nil, status.Error(codes.PermissionDenied, "refusing to modify flash without a verified full backup of this board: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	jedec := fmt.Sprintf("0x%06x", id.jedec)
	if m.MAC != id.mac || m.FlashJEDEC != jedec || m.SizeBytes != id.size {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another board (MAC %s, jedec %s, %d bytes) than the connected one (MAC %s, jedec %s, %d bytes)",
			path, m.MAC, m.FlashJEDEC, m.SizeBytes, id.mac, jedec, id.size)
	}
	return b, map[string]any{"path": path, "sha256": m.SHA256, "mac": m.MAC, "board_matches": true}, nil
}

// flashRestore rewrites every sector that differs from the backup, then
// requires the chip's hash of the whole flash to equal the backup's.
func (d *device) flashRestore(a map[string]any) (map[string]any, error) {
	if _, ok := a["backup"]; !ok {
		if p, ok := a["path"].(string); ok {
			a["backup"] = p
		}
	}
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	b, bk, err := d.loadGate(a, id)
	if err != nil {
		return nil, err
	}
	cur, err := d.cl.Read(0, len(b))
	if err != nil {
		return nil, err
	}
	type run struct{ start, end uint32 }
	var runs []run
	differing := 0
	for s := uint32(0); s < uint32(len(b)); s += isp.Sector {
		e := s + isp.Sector
		if bytes.Equal(cur[s:e], b[s:e]) {
			continue
		}
		differing++
		if n := len(runs); n > 0 && runs[n-1].end == s {
			runs[n-1].end = e
		} else {
			runs = append(runs, run{s, e})
		}
	}
	restored := 0
	for _, r := range runs {
		if _, err := d.write(r.start, b[r.start:r.end]); err != nil {
			return nil, fmt.Errorf("restore 0x%06x-0x%06x: %w (%d sectors restored before; the flash may be partially restored)", r.start, r.end, err, restored)
		}
		restored += int((r.end - r.start) / isp.Sector)
	}
	sum, err := d.cl.SHA256(0, len(b))
	if err != nil {
		return nil, fmt.Errorf("final hash: %w", err)
	}
	got := hex.EncodeToString(sum[:])
	if got != bk["sha256"] {
		return nil, status.Errorf(codes.DataLoss, "after restore the chip's flash SHA-256 is %s, the backup's %s", got, bk["sha256"])
	}
	return map[string]any{"backup": bk, "sectors_differing": float64(differing), "sectors_restored": float64(restored), "flash_sha256": got, "matches_backup": true}, nil
}
