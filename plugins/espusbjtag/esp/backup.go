package esp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

var ErrBackupMismatch = errors.New("the two full-flash reads differ")

// Manifest describes a backup. It deliberately holds no firmware bytes.
type Manifest struct {
	Format      string      `json:"format"`
	CreatedUTC  string      `json:"created_utc"`
	File        string      `json:"file"`
	SizeBytes   int         `json:"size_bytes"`
	SHA256      string      `json:"sha256"`
	SecondRead  string      `json:"second_read_sha256"`
	Flash       Info        `json:"flash"`
	Bootloader  *Image      `json:"bootloader_image,omitempty"`
	Partitions  []Partition `json:"partitions,omitempty"`
	TableNote   string      `json:"partition_table_note,omitempty"`
	ReadSeconds float64     `json:"read_seconds"`
	USBSerial   string      `json:"usb_serial,omitempty"` // local artifact only
}

// Backup reads the whole flash twice. The first pass is written to dir; the
// second is only hashed. The files are kept only if both SHA-256 values are
// identical; otherwise the partial file is renamed *.mismatch and an error is
// returned. It never modifies the flash.
func (f *Flash) Backup(ctx context.Context, dir, usbSerial string, info Info, prog Progress) (Manifest, string, error) {
	m := Manifest{Format: "hwlab-esp-flash-backup/1", Flash: info, SizeBytes: info.SizeBytes, USBSerial: usbSerial}
	if f.size != info.SizeBytes || info.SizeBytes <= 0 {
		return m, "", fmt.Errorf("flash size not established")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return m, "", err
	}
	tmp, err := os.CreateTemp(dir, "flash-backup-*.partial")
	if err != nil {
		return m, "", err
	}
	defer func() { _ = tmp.Close() }()
	t0 := time.Now()
	h1, err := f.Dump(ctx, 0, info.SizeBytes, tmp, prog)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return m, "", fmt.Errorf("first read: %w", err)
	}
	h2, err := f.Dump(ctx, 0, info.SizeBytes, io.Discard, nil)
	m.ReadSeconds = time.Since(t0).Seconds()
	if err != nil {
		_ = os.Remove(tmp.Name())
		return m, "", fmt.Errorf("second read: %w", err)
	}
	m.SHA256, m.SecondRead = hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:])
	if h1 != h2 {
		bad := tmp.Name() + ".mismatch"
		_ = os.Rename(tmp.Name(), bad)
		return m, bad, fmt.Errorf("%w: %s vs %s (kept %s for inspection; do NOT use it)", ErrBackupMismatch, m.SHA256, m.SecondRead, bad)
	}
	if err := tmp.Sync(); err != nil {
		return m, "", err
	}
	m.File = fmt.Sprintf("esp-flash-%s-%dK-%s.bin", info.JEDEC, info.SizeBytes/1024, m.SHA256[:12])
	final := filepath.Join(dir, m.File)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return m, "", err
	}
	_ = os.Chmod(final, 0o600)
	if b, err := os.ReadFile(final); err == nil {
		m.fillLayout(b)
	}
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
		return m, final, err
	}
	return m, final, nil
}

func (m *Manifest) fillLayout(b []byte) {
	if len(b) > 0x1000 {
		if im, err := ParseImage(b[0:]); err == nil {
			m.Bootloader = &im
		}
	}
	if len(b) >= PartitionTableOffset+0xC00 {
		ps, err := ParsePartitionTable(b[PartitionTableOffset : PartitionTableOffset+0xC00])
		if err != nil {
			m.TableNote = err.Error()
		} else {
			m.Partitions = ps
		}
	}
}

// LoadBackup reads a backup file and checks it against its manifest hash when
// the manifest exists.
func LoadBackup(path string) ([]byte, *Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var m *Manifest
	if mb, err := os.ReadFile(path + ".json"); err == nil {
		var mm Manifest
		if err := json.Unmarshal(mb, &mm); err != nil {
			return nil, nil, fmt.Errorf("manifest: %w", err)
		}
		s := sha256.Sum256(b)
		if hex.EncodeToString(s[:]) != mm.SHA256 {
			return nil, nil, fmt.Errorf("backup %s does not match the SHA-256 in its manifest: refusing to use it", path)
		}
		m = &mm
	}
	return b, m, nil
}
