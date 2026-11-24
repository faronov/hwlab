package espromplugin

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the loader's digests are MD5
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/ownedtemp"
	"github.com/faronov/hwlab/plugins/esprom/rom"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

const (
	backupFormat = "hwlab-esprom-flash-backup/1"
	sector       = rom.SectorSize
	// readSegment is one READ_FLASH command: each is checked by the stub's
	// MD5 and retried on its own.
	readSegment  = 256 << 10
	readAttempts = 3
	nvsWarn      = "the backup holds the device's nvs (Wi-Fi/Thread/Zigbee credentials, keys, counters) and identity: it is a LOCAL artifact (0600), never commit or share it, and restore it only onto the same chip"
)

func artifactDir(sub string) string { return filepath.Join(hwlabHome(), "artifacts", sub) }

// readFlash streams [addr, addr+n) to w in segments, each verified by the
// stub's MD5 and retried (abort + resync) up to readAttempts times. A
// segment reaches w only once it is complete and verified.
func (d *device) readFlash(addr uint32, n int, w io.Writer) ([32]byte, [16]byte, error) {
	var sha [32]byte
	var sum [16]byte
	if err := d.needStub(); err != nil {
		return sha, sum, err
	}
	if uint64(addr)+uint64(n) > uint64(d.size) || n < 0 {
		return sha, sum, argErr(fmt.Errorf("0x%08X+%d is outside the %d-byte flash", addr, n, d.size))
	}
	hs, hm := sha256.New(), md5.New() //nolint:gosec
	var seg bytes.Buffer
	for off := 0; off < n; off += readSegment {
		k := min(readSegment, n-off)
		var err error
		for try := 0; try < readAttempts; try++ {
			seg.Reset()
			if _, err = d.cl.ReadFlash(addr+uint32(off), k, &seg); err == nil {
				break
			}
			var se *rom.StatusError
			if errors.As(err, &se) {
				return sha, sum, err
			}
			if aerr := d.cl.AbortRead(); aerr != nil {
				return sha, sum, fmt.Errorf("%w; resync after the failed read: %w", err, aerr)
			}
		}
		if err != nil {
			return sha, sum, fmt.Errorf("after %d attempts: %w", readAttempts, err)
		}
		hs.Write(seg.Bytes())
		hm.Write(seg.Bytes())
		if _, err := w.Write(seg.Bytes()); err != nil {
			return sha, sum, err
		}
	}
	copy(sha[:], hs.Sum(nil))
	copy(sum[:], hm.Sum(nil))
	return sha, sum, nil
}

func (d *device) readBytes(addr uint32, n int) ([]byte, error) {
	var b bytes.Buffer
	_, _, err := d.readFlash(addr, n, &b)
	return b.Bytes(), err
}

func (d *device) flashRange(addr uint32, n int) error {
	if d.size == 0 {
		return status.Errorf(codes.FailedPrecondition, "the flash size is unknown (JEDEC %s)", rom.JEDECString(d.jedec))
	}
	if n < 1 || uint64(addr)+uint64(n) > uint64(d.size) {
		return argErr(fmt.Errorf("0x%08X+%d is outside the flash 0x0..0x%X", addr, n, d.size))
	}
	return nil
}

// ---- layout ----

// region is a named flash range; protected ones need confirm_protected.
type region struct {
	Name      string `json:"name"`
	Addr      uint32 `json:"address"`
	Size      uint32 `json:"size"`
	Protected bool   `json:"protected"`
	App       bool   `json:"app,omitempty"`
}

// table reads and parses the partition table at 0x8000 once per session.
func (d *device) table() ([]esp.Partition, error) {
	if d.ptRead {
		return d.parts, d.ptErr
	}
	if d.size < esp.PartitionTableOffset+0xC00 {
		d.ptRead, d.ptErr = true, errors.New("flash smaller than the partition table offset")
		return nil, d.ptErr
	}
	b, err := d.readBytes(esp.PartitionTableOffset, 0xC00)
	if err != nil {
		return nil, err
	}
	d.parts, d.ptErr = esp.ParsePartitionTable(b)
	d.ptRead = true
	return d.parts, d.ptErr
}

// regions maps the flash: the bootloader slot and the partition table
// sector are protected, so is every partition that is not an application
// (nvs, phy_init, otadata, coredump, nvs_keys, custom data). Without a valid
// table the whole flash is one protected region.
func regions(size int, parts []esp.Partition, ptErr error) []region {
	if ptErr != nil || len(parts) == 0 {
		return []region{{Name: "whole flash (no valid partition table at 0x8000: layout unknown)", Addr: 0, Size: uint32(size), Protected: true}}
	}
	rs := []region{
		{Name: "bootloader slot", Addr: 0, Size: esp.PartitionTableOffset, Protected: true},
		{Name: "partition table", Addr: esp.PartitionTableOffset, Size: sector, Protected: true},
	}
	for _, p := range parts {
		app := p.Type == 0x00
		rs = append(rs, region{Name: "partition " + p.Label + " (" + p.Kind() + ")", Addr: p.Offset, Size: p.Size, Protected: !app, App: app})
	}
	return rs
}

func overlapping(rs []region, addr uint32, n int) []region {
	var out []region
	end := uint64(addr) + uint64(n)
	for _, r := range rs {
		if uint64(r.Addr) < end && uint64(addr) < uint64(r.Addr)+uint64(r.Size) {
			out = append(out, r)
		}
	}
	return out
}

func regionNames(rs []region) string {
	var l []string
	for _, r := range rs {
		l = append(l, fmt.Sprintf("%s [0x%X, +0x%X)", r.Name, r.Addr, r.Size))
	}
	return strings.Join(l, "; ")
}

func (d *device) layout() []region {
	parts, err := d.table()
	return regions(d.size, parts, err)
}

// protectedGate refuses touching protected ranges without confirm_protected.
func protectedGate(a map[string]any, hit []region) error {
	var prot []region
	for _, r := range hit {
		if r.Protected {
			prot = append(prot, r)
		}
	}
	if len(prot) == 0 {
		return nil
	}
	if s, _ := a["confirm_protected"].(string); s != tokProtected {
		return denied("the change touches protected flash: %s. These hold the bootloader, the partition table or per-device data (keys, credentials, calibration, network state) that reflashing cannot regenerate; to modify them anyway pass confirm_protected=%q", regionNames(prot), tokProtected)
	}
	return nil
}

// ---- application descriptors and OTA selection ----

// appDesc decodes the esp_app_desc_t that follows the image header and the
// first segment header (ESP-IDF esp_app_format.h, magic 0xABCD5432).
func appDesc(b []byte) map[string]any {
	if len(b) < 32+176 || b[0] != esp.ImageMagic {
		return nil
	}
	res := map[string]any{"image_chip_id": fmt.Sprintf("0x%04X", binary.LittleEndian.Uint16(b[12:])), "segments": float64(b[1]),
		"entry": hex32(binary.LittleEndian.Uint32(b[4:]))}
	desc := b[32:]
	if binary.LittleEndian.Uint32(desc) != 0xABCD5432 {
		res["app_desc"] = "absent (no esp_app_desc_t magic)"
		return res
	}
	str := func(lo, hi int) string { return strings.TrimRight(string(bytes.TrimRight(desc[lo:hi], "\x00")), " ") }
	res["secure_version"] = float64(binary.LittleEndian.Uint32(desc[4:]))
	res["version"], res["project_name"], res["time"], res["date"], res["idf_ver"] = str(16, 48), str(48, 80), str(80, 96), str(96, 112), str(112, 144)
	res["app_elf_sha256"] = hex.EncodeToString(desc[144:176])
	return res
}

// otaCRC is the ROM's crc32_le(UINT32_MAX, ota_seq, 4).
func otaCRC(seq []byte) uint32 { return crc32.Update(0xFFFFFFFF, crc32.IEEETable, seq) }

var otaStates = map[uint32]string{0: "NEW", 1: "PENDING_VERIFY", 2: "VALID", 3: "INVALID", 4: "ABORTED", 0xFFFFFFFF: "UNDEFINED"}

// otaSelect decodes the two esp_ota_select_entry_t of an otadata partition
// (ota_seq, seq_label[20], ota_state, crc = ROM crc32_le(UINT32_MAX, ota_seq))
// and names the app slot the bootloader would choose.
func otaSelect(b []byte, parts []esp.Partition) map[string]any {
	var otas []esp.Partition
	for _, p := range parts {
		if p.Type == 0x00 && p.Subtype >= 0x10 && p.Subtype <= 0x1F {
			otas = append(otas, p)
		}
	}
	sort.Slice(otas, func(i, j int) bool { return otas[i].Subtype < otas[j].Subtype })
	var entries []any
	best := uint32(0)
	for i := 0; i < 2 && (i+1)*0x1000 <= len(b); i++ {
		e := b[i*0x1000:]
		seq, state, crc := binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[24:]), binary.LittleEndian.Uint32(e[28:])
		ok := seq != 0xFFFFFFFF && crc == otaCRC(e[:4])
		st := otaStates[state]
		if st == "" {
			st = fmt.Sprintf("0x%08X", state)
		}
		entries = append(entries, map[string]any{"sector": float64(i), "ota_seq": float64(seq), "ota_state": st, "crc_ok": ok})
		if ok && seq > best && state != 3 && state != 4 {
			best = seq
		}
	}
	res := map[string]any{"entries": entries}
	switch {
	case best == 0:
		res["boot_partition"] = "factory (or the first app partition): no valid OTA selection"
	case len(otas) == 0:
		res["boot_partition"] = "unknown: no ota_N partitions"
	default:
		res["boot_partition"] = otas[(best-1)%uint32(len(otas))].Label
	}
	return res
}

// describeFlash decodes partitions, app descriptors and OTA selection from
// bytes read at the given offsets (read returns flash [addr, addr+n)).
func describeFlash(read func(addr uint32, n int) ([]byte, error), parts []esp.Partition, bootOff uint32) (map[string]any, error) {
	res := map[string]any{}
	if b, err := read(bootOff, 32+176); err == nil {
		if bd := appDesc(b); bd != nil {
			delete(bd, "app_desc")
			res["bootloader"] = map[string]any{"offset": hex32(bootOff), "image_chip_id": bd["image_chip_id"], "segments": bd["segments"]}
		} else {
			res["bootloader"] = map[string]any{"offset": hex32(bootOff), "valid": false}
		}
	} else {
		return nil, err
	}
	var apps []any
	for _, p := range parts {
		switch {
		case p.Type == 0x00:
			b, err := read(p.Offset, 32+176)
			if err != nil {
				return nil, err
			}
			a := appDesc(b)
			if a == nil {
				a = map[string]any{"image": "none (no 0xE9 image header)"}
			}
			a["partition"], a["offset"] = p.Label, hex32(p.Offset)
			apps = append(apps, a)
		case p.Type == 0x01 && p.Subtype == 0x00:
			b, err := read(p.Offset, min(int(p.Size), 0x2000))
			if err != nil {
				return nil, err
			}
			res["otadata"] = otaSelect(b, parts)
		}
	}
	if apps == nil {
		apps = []any{}
	}
	res["apps"] = apps
	return res, nil
}

func partitionsAny(parts []esp.Partition, rs []region) []any {
	out := []any{}
	for _, p := range parts {
		prot := true
		for _, r := range rs {
			if r.Addr == p.Offset && strings.HasPrefix(r.Name, "partition "+p.Label+" ") {
				prot = r.Protected
			}
		}
		out = append(out, map[string]any{"label": p.Label, "kind": p.Kind(), "type": float64(p.Type), "subtype": float64(p.Subtype),
			"offset": hex32(p.Offset), "size": float64(p.Size), "encrypted": p.Encrypted, "protected": prot})
	}
	return out
}

func regionsAny(rs []region) []any {
	out := []any{}
	for _, r := range rs {
		if r.Protected {
			out = append(out, map[string]any{"name": r.Name, "address": hex32(r.Addr), "size": float64(r.Size)})
		}
	}
	return out
}

func (d *device) flashInfo() (map[string]any, error) {
	res := d.chipFields()
	if err := d.needStub(); err != nil {
		res["flash_layout"] = status.Convert(err).Message()
		return res, nil
	}
	parts, ptErr := d.table()
	if ptErr != nil && !errors.Is(ptErr, esp.ErrPartitionTable) {
		return nil, ptErr
	}
	rs := regions(d.size, parts, ptErr)
	res["partitions"], res["protected_ranges"] = partitionsAny(parts, rs), regionsAny(rs)
	if ptErr != nil {
		res["partition_table_note"] = ptErr.Error()
	} else {
		desc, err := describeFlash(d.readBytes, parts, d.det.Chip.BootloaderOffset)
		if err != nil {
			return nil, err
		}
		for k, v := range desc {
			res[k] = v
		}
	}
	res["backup_required_for_modification"] = true
	res["tokens"] = map[string]any{"program": tokProgram, "restore": tokRestore, "erase_region": tokEraseReg, "erase_app": tokEraseApp,
		"erase_chip": tokEraseChip, "protected": "confirm_protected=" + tokProtected}
	res["permissions"] = "allow_flash (program/restore), allow_erase (erase/restore), allow_chip_erase (mode=chip), allow_reset (reset, reset=run)"
	return res, nil
}

// ---- read / verify ----

func (d *device) flashRead(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, _, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	nf, hasLen, err := optNumber(a, "length")
	if err != nil {
		return nil, argErr(err)
	}
	path, err := strArg(a, "path")
	if err != nil {
		return nil, err
	}
	if hasLen && (nf < 1 || nf != float64(int(nf))) {
		return nil, argErr(fmt.Errorf("length %v: want a positive integer", nf))
	}
	if path == "" && (!hasLen || nf > maxInline) {
		return nil, argErr(fmt.Errorf("at most %d bytes inline: give length, or path to write a file", maxInline))
	}
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return nil, argErr(fmt.Errorf("path %s exists: refusing to overwrite it", path))
		}
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		n := d.size - int(addr)
		if hasLen {
			n = int(nf)
		}
		if err := d.flashRange(addr, n); err != nil {
			return nil, err
		}
		t0 := time.Now()
		res := map[string]any{"address": hex32(addr), "length": float64(n), "verified_by": "stub MD5 per 256 KiB READ_FLASH"}
		if path == "" {
			b, err := d.readBytes(addr, n)
			if err != nil {
				return nil, err
			}
			res["data"], res["sha256"] = hex.EncodeToString(b), shaHex(b)
			return res, nil
		}
		var sha [32]byte
		err := ownedtemp.Publish(path, false, func(w io.Writer) error {
			var err error
			sha, _, err = d.readFlash(addr, n, w)
			return err
		})
		if err != nil {
			return nil, err
		}
		_ = os.Chmod(path, 0o600)
		res["path"], res["sha256"] = path, hex.EncodeToString(sha[:])
		res["seconds"] = time.Since(t0).Seconds()
		return res, nil
	})
}

func md5Of(b []byte) [16]byte { return md5.Sum(b) } //nolint:gosec

func (d *device) flashVerify(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, _, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	img, path, err := readImage(a)
	if err != nil {
		return nil, argErr(err)
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		if err := d.flashRange(addr, len(img)); err != nil {
			return nil, err
		}
		got, err := d.cl.FlashMD5(addr, len(img))
		if err != nil {
			return nil, err
		}
		want := md5Of(img)
		res := map[string]any{"address": hex32(addr), "length": float64(len(img)), "match": got == want, "image_md5": hex.EncodeToString(want[:]),
			"flash_md5": hex.EncodeToString(got[:]), "image_sha256": shaHex(img), "method": "SPI_FLASH_MD5 (computed by the chip)"}
		if path != "" {
			res["path"] = path
		}
		if got != want {
			for off := 0; off < len(img); off += sector {
				k := min(sector, len(img)-off)
				s, err := d.cl.FlashMD5(addr+uint32(off), k)
				if err != nil {
					return nil, err
				}
				if s != md5Of(img[off:off+k]) {
					res["first_differing_sector"] = hex32((addr + uint32(off)) / sector * sector)
					break
				}
			}
		}
		return res, nil
	})
}

// ---- backup ----

type backupManifest struct {
	Format         string         `json:"format"`
	CreatedUTC     string         `json:"created_utc"`
	File           string         `json:"file"`
	SizeBytes      int            `json:"size_bytes"`
	SHA256         string         `json:"sha256"`
	SecondRead     string         `json:"second_read_sha256"`
	MD5            string         `json:"md5"`
	ChipMD5        string         `json:"chip_md5"`
	Chip           string         `json:"chip"`
	Revision       string         `json:"revision,omitempty"`
	Package        string         `json:"package,omitempty"`
	MAC            string         `json:"mac"`
	EUI64          string         `json:"eui64,omitempty"`
	JEDEC          string         `json:"flash_jedec"`
	BootloaderAt   string         `json:"bootloader_offset"`
	Partitions     []any          `json:"partitions,omitempty"`
	PartitionsNote string         `json:"partition_table_note,omitempty"`
	Layout         map[string]any `json:"layout,omitempty"`
	DeviceID       string         `json:"device_id"`
	Transport      string         `json:"transport"`
	Port           string         `json:"port,omitempty"`
	USBSerial      string         `json:"usb_serial,omitempty"`
	Baud           int            `json:"baud"`
	Stub           string         `json:"stub"`
	ReadSeconds    float64        `json:"read_seconds"`
	Warning        string         `json:"warning"`
}

// flashBackup reads the whole flash twice, compares both SHA-256 and the
// chip's MD5 of the whole flash, and keeps the first read only if all agree.
func (d *device) flashBackup(ctx context.Context, a map[string]any) (map[string]any, error) {
	dir, err := strArg(a, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = artifactDir("esprom-flash")
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		if err := d.needStub(); err != nil {
			return nil, err
		}
		if d.facts == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "the chip's MAC could not be read (%v): a backup must be bound to it", d.factsErr)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(dir, "esprom-backup-*.partial")
		if err != nil {
			return nil, err
		}
		defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
		t0 := time.Now()
		sha1, md51, err := d.readFlash(0, d.size, tmp)
		if err != nil {
			return nil, fmt.Errorf("first read: %w", err)
		}
		sha2, _, err := d.readFlash(0, d.size, io.Discard)
		if err != nil {
			return nil, fmt.Errorf("second read: %w", err)
		}
		chip, err := d.cl.FlashMD5(0, d.size)
		if err != nil {
			return nil, fmt.Errorf("SPI_FLASH_MD5 of the whole flash: %w", err)
		}
		m := d.manifest()
		m.SHA256, m.SecondRead = hex.EncodeToString(sha1[:]), hex.EncodeToString(sha2[:])
		m.MD5, m.ChipMD5 = hex.EncodeToString(md51[:]), hex.EncodeToString(chip[:])
		m.ReadSeconds = time.Since(t0).Seconds()
		if sha1 != sha2 || md51 != chip {
			return nil, status.Errorf(codes.DataLoss, "full-flash reads disagree (read 1 %s, read 2 %s, MD5 %s, chip MD5 %s): no backup was stored", m.SHA256, m.SecondRead, m.MD5, m.ChipMD5)
		}
		if err := tmp.Sync(); err != nil {
			return nil, err
		}
		mac := strings.ReplaceAll(m.MAC, ":", "")
		m.File = fmt.Sprintf("esprom-flash-%s-%s-%dK-%s.bin", strings.ToLower(strings.ReplaceAll(m.Chip, "-", "")), mac, d.size>>10, m.SHA256[:12])
		final := filepath.Join(dir, m.File)
		b, err := os.ReadFile(tmp.Name())
		if err != nil {
			return nil, err
		}
		m.Partitions, m.PartitionsNote, m.Layout = describeBackup(b, d.det.Chip.BootloaderOffset)
		reused := false
		if old, err := os.ReadFile(final); err == nil {
			if shaHex(old) != m.SHA256 {
				return nil, status.Errorf(codes.AlreadyExists, "%s exists with different content: refusing to overwrite it", final)
			}
			reused = true
		} else {
			if err := tmp.Close(); err != nil {
				return nil, err
			}
			if err := os.Rename(tmp.Name(), final); err != nil {
				return nil, err
			}
			_ = os.Chmod(final, 0o600)
			m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
			mb, _ := json.MarshalIndent(m, "", "  ")
			if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
				return nil, err
			}
		}
		res := map[string]any{
			"path": final, "manifest": final + ".json", "size_bytes": float64(d.size), "sha256": m.SHA256, "second_read_sha256": m.SecondRead,
			"md5": m.MD5, "chip_md5": m.ChipMD5, "identical_reads": true, "read_seconds": m.ReadSeconds, "chip": m.Chip, "mac": m.MAC,
			"flash_jedec": m.JEDEC, "baud": float64(d.linkBaud), "already_present": reused, "note": nvsWarn,
		}
		if m.Layout != nil {
			res["layout"] = m.Layout
		}
		if m.PartitionsNote != "" {
			res["partition_table_note"] = m.PartitionsNote
		}
		return res, nil
	})
}

func (d *device) manifest() backupManifest {
	m := backupManifest{Format: backupFormat, SizeBytes: d.size, Chip: d.det.Chip.Name, JEDEC: rom.JEDECString(d.jedec),
		BootloaderAt: hex32(d.det.Chip.BootloaderOffset), DeviceID: d.id, Transport: string(d.e.Kind), Port: d.e.Port, USBSerial: d.e.Serial,
		Baud: d.linkBaud, Stub: d.stubNote, Warning: nvsWarn}
	if f := d.facts; f != nil {
		m.MAC, m.Revision, m.Package, m.EUI64 = f.MACString(), f.Revision(), f.Package, f.EUI64()
	}
	return m
}

// describeBackup decodes the layout from backup bytes.
func describeBackup(b []byte, bootOff uint32) ([]any, string, map[string]any) {
	if len(b) < esp.PartitionTableOffset+0xC00 {
		return nil, "flash smaller than the partition table offset", nil
	}
	parts, err := esp.ParsePartitionTable(b[esp.PartitionTableOffset : esp.PartitionTableOffset+0xC00])
	if err != nil {
		return nil, err.Error(), nil
	}
	read := func(addr uint32, n int) ([]byte, error) {
		if int(addr)+n > len(b) {
			return nil, fmt.Errorf("0x%X+%d outside the backup", addr, n)
		}
		return b[addr : int(addr)+n], nil
	}
	lay, err := describeFlash(read, parts, bootOff)
	if err != nil {
		return partitionsAny(parts, regions(len(b), parts, nil)), err.Error(), nil
	}
	return partitionsAny(parts, regions(len(b), parts, nil)), "", lay
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
	sum := md5Of(b)
	if shaHex(b) != m.SHA256 || len(b) != m.SizeBytes || hex.EncodeToString(sum[:]) != m.MD5 {
		return nil, nil, fmt.Errorf("backup %s does not match the SHA-256/MD5/size in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead || m.MD5 != m.ChipMD5 {
		return nil, nil, errors.New("the manifest does not record two identical reads and an equal chip MD5")
	}
	return b, &m, nil
}

// backupGate requires a verified full backup of THIS chip whose bytes in
// [addr, addr+n) still equal the flash (checked by the chip's MD5).
func (d *device) backupGate(a map[string]any, addr uint32, n int) ([]byte, map[string]any, error) {
	path, _ := a["backup"].(string)
	if path == "" {
		return nil, nil, denied("refusing to modify flash without a verified full backup of this chip: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	id := d.manifest()
	if m.Chip != id.Chip || m.MAC != id.MAC || m.JEDEC != id.JEDEC || m.SizeBytes != d.size {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another chip (%s MAC %s, flash %s, %d bytes) than the connected one (%s MAC %s, flash %s, %d bytes)",
			path, m.Chip, m.MAC, m.JEDEC, m.SizeBytes, id.Chip, id.MAC, id.JEDEC, d.size)
	}
	res := map[string]any{"path": path, "sha256": m.SHA256, "mac": m.MAC, "chip_matches": true}
	if n > 0 {
		start, end := addr/sector*sector, (uint64(addr)+uint64(n)+sector-1)/sector*sector
		end = min(end, uint64(d.size))
		got, err := d.cl.FlashMD5(start, int(end-uint64(start)))
		if err != nil {
			return nil, nil, err
		}
		if got != md5Of(b[start:end]) {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "the backup %s no longer matches the flash in [0x%X, 0x%X): the flash changed after the backup, take a new one", path, start, end)
		}
		res["region_matches_flash"] = true
	}
	return b, res, nil
}

// ---- write planning ----

type run struct {
	addr uint32
	data []byte
}

type writePlan struct {
	start     uint32
	want      []byte
	runs      []run
	changed   int
	unchanged int
}

const planBlock = 0x10000

// plan works out the 4 KiB sectors whose content must change for img to
// be at addr: edge sectors are read and merged so their other bytes stay,
// then the chip's MD5 of the span, of 64 KiB blocks and of sectors is
// compared with the wanted bytes. Contiguous changed sectors form runs.
func (d *device) plan(addr uint32, img []byte) (*writePlan, error) {
	start := addr / sector * sector
	end := (uint64(addr) + uint64(len(img)) + sector - 1) / sector * sector
	want := make([]byte, end-uint64(start))
	imgEnd := uint64(addr) + uint64(len(img))
	if addr != start {
		b, err := d.readBytes(start, sector)
		if err != nil {
			return nil, err
		}
		copy(want, b)
	}
	// The last sector is partly covered; it is the first one when the span
	// is a single sector, which is already read when addr is unaligned.
	if last := uint32(end) - sector; imgEnd != end && (last != start || addr == start) {
		b, err := d.readBytes(last, sector)
		if err != nil {
			return nil, err
		}
		copy(want[last-start:], b)
	}
	copy(want[addr-start:], img)
	p := &writePlan{start: start, want: want}
	total, err := d.cl.FlashMD5(start, len(want))
	if err != nil {
		return nil, err
	}
	if total == md5Of(want) {
		p.unchanged = len(want) / sector
		return p, nil
	}
	var changed []uint32
	for off := 0; off < len(want); off += planBlock {
		k := min(planBlock, len(want)-off)
		s, err := d.cl.FlashMD5(start+uint32(off), k)
		if err != nil {
			return nil, err
		}
		if s == md5Of(want[off:off+k]) {
			p.unchanged += k / sector
			continue
		}
		for so := off; so < off+k; so += sector {
			s, err := d.cl.FlashMD5(start+uint32(so), sector)
			if err != nil {
				return nil, err
			}
			if s == md5Of(want[so:so+sector]) {
				p.unchanged++
			} else {
				changed = append(changed, start+uint32(so))
			}
		}
	}
	p.changed = len(changed)
	for i := 0; i < len(changed); {
		j := i
		for j+1 < len(changed) && changed[j+1] == changed[j]+sector && (j+1-i)*sector < 1<<20 {
			j++
		}
		a := changed[i]
		p.runs = append(p.runs, run{addr: a, data: want[a-start : changed[j]+sector-start]})
		i = j + 1
	}
	return p, nil
}

func (p *writePlan) sectorsTouched(rs []region) []region {
	var hit []region
	seen := map[string]bool{}
	for _, r := range p.runs {
		for _, x := range overlapping(rs, r.addr, len(r.data)) {
			if !seen[x.Name] {
				seen[x.Name] = true
				hit = append(hit, x)
			}
		}
	}
	return hit
}

func (p *writePlan) report(res map[string]any) {
	var l []any
	for _, r := range p.runs {
		l = append(l, fmt.Sprintf("0x%X+0x%X", r.addr, len(r.data)))
	}
	if l == nil {
		l = []any{}
	}
	res["sectors_changed"], res["sectors_unchanged"], res["runs"] = float64(p.changed), float64(p.unchanged), l
}

// writeAttempts bounds the restarts of one run after a lost or corrupted
// FLASH_DATA (rom.Client.WriteFlash).
const writeAttempts = 4

// execute writes each run through the stub and checks it by the chip's MD5
// (whose answer also carries a deferred write error of the run's last
// block). It returns how many times a download restarted at a block.
func (d *device) execute(p *writePlan) (int, error) {
	total := 0
	for i, r := range p.runs {
		n, err := d.cl.WriteFlash(r.addr, r.data, writeAttempts)
		total += n
		if err != nil {
			return total, fmt.Errorf("run %d of %d at 0x%X: %w (that range may be erased or partly written)", i+1, len(p.runs), r.addr, err)
		}
		got, err := d.cl.FlashMD5(r.addr, len(r.data))
		if err != nil {
			return total, fmt.Errorf("run %d of %d at 0x%X: %w (that range may be erased or partly written)", i+1, len(p.runs), r.addr, err)
		}
		if got != md5Of(r.data) {
			return total, status.Errorf(codes.DataLoss, "run 0x%X+0x%X: chip MD5 %x after writing, want %x", r.addr, len(r.data), got, md5Of(r.data))
		}
	}
	return total, nil
}

// ---- journal ----

type journalRec struct{ rec map[string]any }

func (p *Plugin) journalPath() string {
	return filepath.Join(artifactDir("esprom-flash"), "journal.jsonl")
}

func (p *Plugin) appendJournal(rec map[string]any) error {
	p.jmu.Lock()
	defer p.jmu.Unlock()
	path := p.journalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// journalStart records a modifying call before it runs; the call is refused
// when that is not possible.
func (p *Plugin) journalStart(d *device, op string, details map[string]any) (*journalRec, error) {
	rec := map[string]any{"utc": time.Now().UTC().Format(time.RFC3339Nano), "phase": "start", "op": op, "device": d.id, "port": d.e.Port,
		"transport": string(d.e.Kind)}
	if d.det != nil {
		rec["chip"] = d.det.Chip.Name
	}
	if d.facts != nil {
		rec["mac"] = d.facts.MACString()
	}
	for k, v := range details {
		rec[k] = v
	}
	if err := p.appendJournal(rec); err != nil {
		return nil, status.Errorf(codes.Internal, "%s refused: cannot write the journal %s: %v", op, p.journalPath(), err)
	}
	return &journalRec{rec: rec}, nil
}

func (p *Plugin) journalEnd(j *journalRec, opErr error, extra map[string]any) error {
	rec := map[string]any{}
	for k, v := range j.rec {
		rec[k] = v
	}
	for k, v := range extra {
		rec[k] = v
	}
	rec["utc"], rec["phase"] = time.Now().UTC().Format(time.RFC3339Nano), "done"
	if opErr != nil {
		rec["phase"], rec["error"] = "failed", opErr.Error()
	}
	return p.appendJournal(rec)
}

// ---- program ----

func (d *device) needFlash(erase bool) error {
	if !d.o.perm.flash {
		return denied("flash modification needs allow_flash=true in the device configuration")
	}
	if erase && !d.o.perm.erase {
		return denied("this needs allow_erase=true in the device configuration")
	}
	return nil
}

func (d *device) resetArg(a map[string]any) (bool, error) {
	r, err := strArg(a, "reset")
	if err != nil {
		return false, err
	}
	switch r {
	case "", "none":
		return false, nil
	case "run":
		if !d.o.perm.reset {
			return false, denied("reset=run needs allow_reset=true in the device configuration (or reset=none)")
		}
		return true, nil
	}
	return false, argErr(fmt.Errorf("reset %q: want run or none", r))
}

func (d *device) flashProgram(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, ok, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		return nil, argErr(errors.New("address is required (where in flash the image goes)"))
	}
	img, path, err := readImage(a)
	if err != nil {
		return nil, argErr(err)
	}
	dry, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	raw, err := optBool(a, "raw")
	if err != nil {
		return nil, argErr(err)
	}
	var im *esp.Image
	if img[0] == esp.ImageMagic && !raw {
		parsed, err := esp.ParseImage(img)
		if err != nil {
			return nil, argErr(fmt.Errorf("not a valid ESP image (truncated or corrupt images are refused before anything is erased; raw=true writes it unchecked): %w", err))
		}
		im = &parsed
	} else if !raw {
		return nil, argErr(fmt.Errorf("not an ESP image (no 0x%02X magic): pass raw=true to write it as raw bytes", esp.ImageMagic))
	}
	resetAfter := false
	if !dry {
		if err := d.needFlash(false); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokProgram, "programming flash"); err != nil {
			return nil, err
		}
		if resetAfter, err = d.resetArg(a); err != nil {
			return nil, err
		}
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		if err := d.needStub(); err != nil {
			return nil, err
		}
		if err := d.flashRange(addr, len(img)); err != nil {
			return nil, err
		}
		if err := d.writable(); err != nil {
			return nil, err
		}
		if im != nil && im.ChipID != d.det.Chip.ImageChipID {
			return nil, status.Errorf(codes.FailedPrecondition, "the image is for chip id 0x%04X but the connected chip is the %s (0x%04X)", im.ChipID, d.det.Chip.Name, d.det.Chip.ImageChipID)
		}
		rs := d.layout()
		hit := overlapping(rs, addr, len(img))
		if im != nil && len(hit) > 1 {
			return nil, argErr(fmt.Errorf("an ESP image must lie inside one region; this one spans %s (raw=true writes a merged image unchecked)", regionNames(hit)))
		}
		res := map[string]any{"address": hex32(addr), "bytes": float64(len(img)), "image_sha256": shaHex(img), "regions": regionNames(hit),
			"image_validated": im != nil}
		if path != "" {
			res["path"] = path
		}
		if im != nil {
			res["image"] = map[string]any{"chip_id": fmt.Sprintf("0x%04X", im.ChipID), "segments": float64(len(im.Segments)), "entry": hex32(im.Entry), "hash_appended": im.HashAppended}
		}
		if dry {
			p, err := d.plan(addr, img)
			if err != nil {
				return nil, err
			}
			p.report(res)
			res["dry_run"] = true
			need := "allow_flash, confirm=" + tokProgram + ", backup=<FlashProgramming.backup .bin>"
			if protectedGate(map[string]any{}, hit) != nil {
				need += ", confirm_protected=" + tokProtected
			}
			res["would_need"] = need
			return res, nil
		}
		if err := protectedGate(a, hit); err != nil {
			return nil, err
		}
		_, bk, err := d.backupGate(a, addr, len(img))
		if err != nil {
			return nil, err
		}
		t0 := time.Now()
		p, err := d.plan(addr, img)
		if err != nil {
			return nil, err
		}
		p.report(res)
		j, err := d.p.journalStart(d, "program", map[string]any{"address": hex32(addr), "bytes": len(img), "image_sha256": res["image_sha256"],
			"backup": bk["path"], "runs": res["runs"]})
		if err != nil {
			return nil, err
		}
		retries, err := d.execute(p)
		if err == nil {
			var got [16]byte
			if got, err = d.cl.FlashMD5(addr, len(img)); err == nil && got != md5Of(img) {
				err = status.Errorf(codes.DataLoss, "after programming the chip MD5 of 0x%X+%d is %x, want %x", addr, len(img), got, md5Of(img))
			}
		}
		res["write_retries"] = float64(retries)
		if jerr := d.p.journalEnd(j, err, map[string]any{"write_retries": retries}); jerr != nil && err == nil {
			res["journal_error"] = jerr.Error()
		}
		if err != nil {
			return nil, err
		}
		res["verified"], res["backup"], res["seconds"] = true, bk, time.Since(t0).Seconds()
		return d.after(ctx, res, resetAfter)
	})
}

func (d *device) after(ctx context.Context, res map[string]any, resetAfter bool) (map[string]any, error) {
	if !resetAfter {
		res["next"] = "the chip stays in the loader: FlashProgramming.reset (mode=run) starts the application"
		return res, nil
	}
	r, err := d.resetTo(ctx, "run", 0)
	if err != nil {
		return nil, fmt.Errorf("written and verified, but the reset failed: %w", err)
	}
	res["reset"] = r
	return res, nil
}

// ---- erase ----

func (d *device) flashErase(ctx context.Context, a map[string]any) (map[string]any, error) {
	mode, err := strArg(a, "mode")
	if err != nil {
		return nil, err
	}
	tok := map[string]string{"region": tokEraseReg, "app": tokEraseApp, "chip": tokEraseChip}[mode]
	if tok == "" {
		return nil, argErr(errors.New(`mode: must be "region", "app" or "chip" (there is no implicit erase)`))
	}
	if err := d.needFlash(true); err != nil {
		return nil, err
	}
	if mode == "chip" && !d.o.perm.chipErase {
		return nil, denied("erasing the whole chip needs allow_chip_erase=true in the device configuration")
	}
	if err := tokenIs(a, "confirm", tok, "erase mode "+mode); err != nil {
		return nil, err
	}
	var addr uint32
	var n int
	label := ""
	switch mode {
	case "region":
		var ok bool
		if addr, ok, err = u32Arg(a, "address"); err != nil || !ok {
			return nil, argErr(errors.New("mode=region needs address and length"))
		}
		nf, ok, err := optNumber(a, "length")
		if err != nil || !ok || nf < 1 || nf != float64(int(nf)) {
			return nil, argErr(errors.New("length: a positive multiple of 4096"))
		}
		n = int(nf)
		if addr%sector != 0 || n%sector != 0 {
			return nil, argErr(fmt.Errorf("address 0x%X and length %d must be multiples of the 4096-byte sector", addr, n))
		}
	case "app":
		if label, err = strArg(a, "partition"); err != nil || label == "" {
			return nil, argErr(errors.New("partition: the label of an app partition is required"))
		}
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		if err := d.needStub(); err != nil {
			return nil, err
		}
		if err := d.writable(); err != nil {
			return nil, err
		}
		switch mode {
		case "app":
			parts, err := d.table()
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "no valid partition table, cannot resolve %q: %v", label, err)
			}
			found := false
			for _, p := range parts {
				if p.Label == label {
					if p.Type != 0x00 {
						return nil, argErr(fmt.Errorf("partition %q is %s, not an app partition", label, p.Kind()))
					}
					addr, n, found = p.Offset, int(p.Size), true
				}
			}
			if !found {
				return nil, status.Errorf(codes.NotFound, "partition %q not found", label)
			}
		case "chip":
			addr, n = 0, d.size
		}
		if err := d.flashRange(addr, n); err != nil {
			return nil, err
		}
		hit := overlapping(d.layout(), addr, n)
		if err := protectedGate(a, hit); err != nil {
			return nil, err
		}
		_, bk, err := d.backupGate(a, addr, n)
		if err != nil {
			return nil, err
		}
		j, err := d.p.journalStart(d, "erase", map[string]any{"mode": mode, "address": hex32(addr), "bytes": n, "backup": bk["path"]})
		if err != nil {
			return nil, err
		}
		t0 := time.Now()
		if mode == "chip" {
			err = d.cl.EraseFlash()
		} else {
			err = d.cl.EraseRegion(addr, n)
		}
		if err == nil {
			var got [16]byte
			if got, err = d.cl.FlashMD5(addr, n); err == nil && got != md5Of(bytes.Repeat([]byte{0xFF}, n)) {
				err = status.Errorf(codes.DataLoss, "0x%X+%d is not blank after erasing", addr, n)
			}
		}
		if jerr := d.p.journalEnd(j, err, nil); jerr != nil && err == nil {
			err = jerr
		}
		if err != nil {
			return nil, err
		}
		res := map[string]any{"mode": mode, "address": hex32(addr), "bytes": float64(n), "blank_verified": true, "backup": bk,
			"regions": regionNames(hit), "seconds": time.Since(t0).Seconds(),
			"note": "the chip stays in the loader; program an image or restore the backup before resetting"}
		if label != "" {
			res["partition"] = label
		}
		return res, nil
	})
}

// ---- restore ----

// restoreRange resolves what a restore covers: the whole flash, a partition
// of the backup's table (partition=label) or address+length (4 KiB aligned).
func restoreRange(a map[string]any, b []byte, parts []esp.Partition, ptErr error) (uint32, int, string, error) {
	label, err := strArg(a, "partition")
	if err != nil {
		return 0, 0, "", err
	}
	addr, hasAddr, err := u32Arg(a, "address")
	if err != nil {
		return 0, 0, "", argErr(err)
	}
	nf, hasLen, err := optNumber(a, "length")
	if err != nil {
		return 0, 0, "", argErr(err)
	}
	switch {
	case label != "" && (hasAddr || hasLen):
		return 0, 0, "", argErr(errors.New("give either partition or address+length, not both"))
	case label != "":
		if ptErr != nil {
			return 0, 0, "", status.Errorf(codes.FailedPrecondition, "the backup has no valid partition table, cannot resolve %q: %v", label, ptErr)
		}
		for _, p := range parts {
			if p.Label == label {
				return p.Offset, int(p.Size), "partition " + label + " (" + p.Kind() + ")", nil
			}
		}
		return 0, 0, "", status.Errorf(codes.NotFound, "partition %q is not in the backup's partition table", label)
	case hasAddr != hasLen:
		return 0, 0, "", argErr(errors.New("a region restore needs both address and length"))
	case hasAddr:
		n := int(nf)
		if nf < 1 || nf != float64(n) || addr%sector != 0 || n%sector != 0 {
			return 0, 0, "", argErr(fmt.Errorf("address 0x%X and length %v must be multiples of the 4096-byte sector", addr, nf))
		}
		if uint64(addr)+uint64(n) > uint64(len(b)) {
			return 0, 0, "", argErr(fmt.Errorf("0x%X+%d is outside the %d-byte backup", addr, n, len(b)))
		}
		return addr, n, fmt.Sprintf("region [0x%X, +0x%X)", addr, n), nil
	}
	return 0, len(b), "whole flash", nil
}

func (d *device) flashRestore(ctx context.Context, a map[string]any) (map[string]any, error) {
	if _, ok := a["backup"]; !ok {
		if p, ok := a["path"].(string); ok {
			a["backup"] = p
		}
	}
	dry, err := optBool(a, "dry_run")
	if err != nil {
		return nil, argErr(err)
	}
	resetAfter := false
	if !dry {
		if err := d.needFlash(false); err != nil {
			return nil, err
		}
		if err := tokenIs(a, "confirm", tokRestore, "restoring flash"); err != nil {
			return nil, err
		}
		if resetAfter, err = d.resetArg(a); err != nil {
			return nil, err
		}
	}
	return d.withROM(ctx, func() (map[string]any, error) {
		if err := d.needStub(); err != nil {
			return nil, err
		}
		if err := d.writable(); err != nil {
			return nil, err
		}
		b, bk, err := d.backupGate(a, 0, 0)
		if err != nil {
			return nil, err
		}
		// The layout that matters is the one being restored, and the one
		// on the chip now: a sector protected in either needs the token.
		parts, ptErr := esp.ParsePartitionTable(b[esp.PartitionTableOffset : esp.PartitionTableOffset+0xC00])
		addr, n, what, err := restoreRange(a, b, parts, ptErr)
		if err != nil {
			return nil, err
		}
		want := b[addr : int(addr)+n]
		p, err := d.plan(addr, want)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"backup": bk, "restores": what, "address": hex32(addr), "bytes": float64(n), "warning": nvsWarn}
		p.report(res)
		hit := p.sectorsTouched(append(regions(len(b), parts, ptErr), d.layout()...))
		res["regions_changed"] = regionNames(hit)
		if dry {
			res["dry_run"] = true
			need := "allow_flash, confirm=" + tokRestore
			if protectedGate(map[string]any{}, hit) != nil {
				need += ", confirm_protected=" + tokProtected
			}
			res["would_need"] = need
			return res, nil
		}
		if err := protectedGate(a, hit); err != nil {
			return nil, err
		}
		j, err := d.p.journalStart(d, "restore", map[string]any{"backup": bk["path"], "restores": what, "runs": res["runs"]})
		if err != nil {
			return nil, err
		}
		t0 := time.Now()
		retries, err := d.execute(p)
		if err == nil {
			var got [16]byte
			if got, err = d.cl.FlashMD5(addr, n); err == nil && got != md5Of(want) {
				err = status.Errorf(codes.DataLoss, "after the restore the chip MD5 of %s is %x, the backup's %x", what, got, md5Of(want))
			}
		}
		res["write_retries"] = float64(retries)
		if jerr := d.p.journalEnd(j, err, map[string]any{"write_retries": retries}); jerr != nil && err == nil {
			res["journal_error"] = jerr.Error()
		}
		if err != nil {
			return nil, fmt.Errorf("restore: %w (the flash may be partially restored)", err)
		}
		res["matches_backup"], res["seconds"] = true, time.Since(t0).Seconds()
		return d.after(ctx, res, resetAfter)
	})
}
