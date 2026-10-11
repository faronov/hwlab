package espusbjtag

import (
	"bytes"
	"context"
	"io"

	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/faronov/hwlab/internal/ownedtemp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// Confirmation tokens a caller must pass (args.confirm) for destructive calls.
const (
	tokProgram   = "program-flash"
	tokRestore   = "restore-flash"
	tokEraseReg  = "erase-flash-region"
	tokEraseApp  = "erase-app-partition"
	tokEraseChip = "erase-entire-chip"
	tokProtected = "modify-protected-region"
)

const (
	maxFlashInline = 4096
	maxFlashFile   = 64 << 20
)

// flashCtx is everything an ESP flash call works with while the hart is
// halted, the watchdogs are quiesced and the SPI controller is borrowed.
type flashCtx struct {
	dm     *riscv.DM
	f      *esp.Flash
	info   esp.Info
	sec    esp.Security
	ev     esp.Evidence
	parts  []esp.Partition
	tabErr error
}

// flashSession halts the hart if it is running, quiesces the watchdogs,
// borrows the SPI1 flash controller, runs fn, and restores everything. A
// read-only call resumes a hart it halted. A modifying call that failed
// leaves the hart halted so that nothing runs from half-written flash.
func (s *session) flashSession(ctx context.Context, args map[string]any, modify bool, fn func(*flashCtx) error) (err error) {
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return err
	}
	st, err := dm.Status(ctx)
	if err != nil {
		return err
	}
	halted := false
	switch st.State {
	case riscv.HartHalted:
	case riscv.HartRunning:
		if _, err := dm.Halt(ctx); err != nil {
			return err
		}
		halted = true
	default:
		return status.Errorf(codes.FailedPrecondition, "hart %d is %s: flash access needs a halted CPU", dm.Hart(), st.State)
	}
	fc := &flashCtx{dm: dm}
	var q esp.Quiesced
	quiesced := false
	defer func() {
		bg := context.WithoutCancel(ctx)
		if fc.f != nil {
			_ = fc.f.Close(bg)
		}
		if quiesced {
			if rerr := q.Restore(bg, dm); rerr != nil && err == nil {
				err = fmt.Errorf("flash call done but the watchdog state was not restored: %w", rerr)
			}
		}
		if halted && !modify {
			if _, rerr := dm.Resume(bg); rerr != nil && err == nil {
				err = fmt.Errorf("flash call done but the hart did not resume: %w", rerr)
			}
		}
	}()
	if fc.ev, err = esp.ReadEvidence(ctx, dm); err != nil {
		return err
	}
	if q, err = esp.Quiesce(ctx, dm); err != nil {
		return err
	}
	quiesced = true
	if fc.f, err = esp.OpenFlash(ctx, dm); err != nil {
		return err
	}
	if fc.info, err = fc.f.ReadInfo(ctx); err != nil {
		return err
	}
	if fc.sec, err = esp.ReadSecurity(ctx, dm); err != nil {
		return err
	}
	if modify {
		if err := fc.sec.CheckFlashWritable(); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	if fc.info.SizeBytes >= esp.PartitionTableOffset+0xC00 {
		var tb []byte
		if tb, err = fc.f.Read(ctx, esp.PartitionTableOffset, 0xC00); err != nil {
			return err
		}
		fc.parts, fc.tabErr = esp.ParsePartitionTable(tb)
	}
	return fn(fc)
}

func (fc *flashCtx) partitionByLabel(label string) (esp.Partition, bool) {
	for _, p := range fc.parts {
		if p.Label == label {
			return p, true
		}
	}
	return esp.Partition{}, false
}

func (fc *flashCtx) touched(addr uint32, n int) []esp.Partition {
	return esp.Overlap(fc.parts, addr, n)
}

// regionsTouched names every distinct flash region the range overlaps: the
// bootloader slot, the partition table and each partition.
func (fc *flashCtx) regionsTouched(addr uint32, n int) []string {
	var l []string
	for _, r := range esp.OverlapRanges(esp.ProtectedRanges(fc.parts), addr, n) {
		l = append(l, r.Name)
	}
	for _, p := range fc.touched(addr, n) {
		if !p.Protected() {
			l = append(l, "partition "+p.Label+" ("+p.Kind()+")")
		}
	}
	return l
}

func rangeNames(rs []esp.Range) string {
	var n []string
	for _, r := range rs {
		n = append(n, fmt.Sprintf("%s [0x%x, +0x%x)", r.Name, r.Addr, r.Size))
	}
	return strings.Join(n, "; ")
}

// ---- permission and confirmation gates ----

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

// protectedGate refuses a modification that overlaps protected ranges unless
// confirm_protected carries the token.
func (fc *flashCtx) protectedGate(args map[string]any, addr uint32, n int) ([]esp.Range, error) {
	hit := esp.OverlapRanges(esp.ProtectedRanges(fc.parts), addr, n)
	if len(hit) == 0 {
		return nil, nil
	}
	if got, _ := strArg(args, "confirm_protected"); got != tokProtected {
		return hit, denied("the range overlaps protected flash: %s. These hold the bootloader, the partition table or per-device data that cannot be regenerated; to modify them anyway pass confirm_protected=%q", rangeNames(hit), tokProtected)
	}
	return hit, nil
}

// backupGate requires a verified full-flash backup of THIS flash that matches
// the bytes about to be replaced.
func (fc *flashCtx) backupGate(ctx context.Context, args map[string]any, addr uint32, n int) (map[string]any, error) {
	path, err := strArg(args, "backup")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, denied("refusing to modify flash without a verified full backup: take one with FlashProgramming.backup and pass backup=<path of the .bin>")
	}
	b, m, err := esp.LoadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	switch {
	case m == nil:
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s has no manifest (%s.json): only backups written by FlashProgramming.backup are accepted", path, path)
	case m.SHA256 != m.SecondRead:
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: the manifest does not record two identical reads", path)
	case m.Flash.JEDEC != fc.info.JEDEC || len(b) != fc.info.SizeBytes:
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another flash (JEDEC %s, %d bytes) than the connected one (JEDEC %s, %d bytes)", path, m.Flash.JEDEC, len(b), fc.info.JEDEC, fc.info.SizeBytes)
	}
	start, span := esp.RoundSectors(addr, n)
	cur, err := fc.f.Read(ctx, start, span)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(cur, b[start:int(start)+span]) {
		return nil, status.Errorf(codes.FailedPrecondition, "the backup %s no longer matches the flash in [0x%x, +0x%x): the flash changed after the backup, take a new backup", path, start, span)
	}
	return map[string]any{"path": path, "sha256": m.SHA256, "region_matches_flash": true}, nil
}

// ---- helpers for images and ranges ----

func imageArgs(args map[string]any) (data []byte, path string, err error) {
	path, err = strArg(args, "path")
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
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", argErr(err)
	}
	return data, path, nil
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *session) defaultDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "esp-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "esp-flash")
}

func flashErr(err error) error {
	switch {
	case errors.Is(err, esp.ErrRange):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, esp.ErrAlign), errors.Is(err, esp.ErrImage):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, esp.ErrVerify), errors.Is(err, esp.ErrNotErased), errors.Is(err, esp.ErrBackupMismatch):
		return status.Error(codes.DataLoss, err.Error())
	}
	return err
}

// ---- info ----

func (s *session) flashInfo(ctx context.Context, args map[string]any) (map[string]any, error) {
	var res map[string]any
	err := s.flashSession(ctx, args, false, func(fc *flashCtx) error {
		info, err := toMap(fc.info)
		if err != nil {
			return err
		}
		sec, err := toMap(fc.sec)
		if err != nil {
			return err
		}
		res = map[string]any{
			"flash": info, "security": sec, "chip_evidence": map[string]any{
				"rom_magic": fmt.Sprintf("0x%08x", fc.ev.ROMMagic), "matches_esp32c6": fc.ev.MagicMatchesC6, "basis": fc.ev.Basis,
			},
			"flash_writable": fc.sec.CheckFlashWritable() == nil, "hart": float64(fc.dm.Hart()),
			"basis": "JEDEC id (0x9F), SFDP (0x5A) and the status register read through the SPI1 controller, plus eFuse security fields read-only. Nothing was written.",
		}
		var ps []any
		for _, p := range fc.parts {
			m, err := toMap(p)
			if err != nil {
				return err
			}
			m["kind"], m["protected"] = p.Kind(), p.Protected()
			ps = append(ps, m)
		}
		if ps == nil {
			ps = []any{}
		}
		res["partitions"] = ps
		if fc.tabErr != nil {
			res["partition_table_note"] = fc.tabErr.Error()
		}
		var pr []any
		for _, r := range esp.ProtectedRanges(fc.parts) {
			pr = append(pr, map[string]any{"name": r.Name, "address": fmt.Sprintf("0x%x", r.Addr), "size": float64(r.Size)})
		}
		res["protected_ranges"] = pr
		// Mapped-window cross-check: which app partition does the CPU's flash
		// window at 0x42000000 currently show?
		var maps []any
		for _, p := range fc.parts {
			if p.Type != 0x00 || p.Size < 256 {
				continue
			}
			phys, e1 := fc.f.Read(ctx, p.Offset, 256)
			win, e2 := fc.dm.ReadMem(ctx, 0x42000000, 256)
			if e1 == nil && e2 == nil {
				maps = append(maps, map[string]any{"partition": p.Label, "offset": fmt.Sprintf("0x%x", p.Offset), "window_0x42000000_equals": bytes.Equal(phys, win)})
			}
		}
		if maps != nil {
			res["mapped_window_check"] = maps
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	return s.mark(res, true), nil
}

// ---- read ----

func (s *session) flashRead(ctx context.Context, args map[string]any) (map[string]any, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	nf, ok, err := optNumber(args, "length")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok || nf != float64(int64(nf)) || nf < 0 || nf > maxFlashFile {
		return nil, argErr(fmt.Errorf("length: want an integer 1..%d", maxFlashFile))
	}
	n := int(nf)
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return nil, err
	}
	if path == "" && n > maxFlashInline {
		return nil, argErr(fmt.Errorf("length %d exceeds %d bytes inline: pass path=<new file> to write an artifact", n, maxFlashInline))
	}
	if path != "" && !overwrite {
		if _, serr := os.Stat(path); serr == nil {
			return nil, argErr(fmt.Errorf("path %s exists: pass overwrite=true to replace it", path))
		}
	}
	var res map[string]any
	err = s.flashSession(ctx, args, false, func(fc *flashCtx) error {
		start := time.Now()
		res = map[string]any{"address": fmt.Sprintf("0x%08x", addr), "length": float64(n), "flash_size": float64(fc.info.SizeBytes), "jedec_id": fc.info.JEDEC,
			"via": "physical SPI flash read (SPI1 user command 0x0B), not the CPU-mapped window"}
		if path == "" {
			b, err := fc.f.Read(ctx, addr, n)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			res["hex"], res["sha256"] = hex.EncodeToString(b), hex.EncodeToString(h[:])
		} else {
			if err := fc.f.CheckRange(addr, n); err != nil {
				return err
			}
			var h [32]byte
			err := ownedtemp.Publish(path, overwrite, func(w io.Writer) error {
				var err error
				h, err = fc.f.Dump(ctx, addr, n, w, nil)
				return err
			})
			if err != nil {
				return err
			}
			res["path"], res["sha256"] = path, hex.EncodeToString(h[:])
		}
		sec := time.Since(start).Seconds()
		res["elapsed_ms"] = float64(time.Since(start).Milliseconds())
		if sec > 0 {
			res["throughput_kb_s"] = float64(n) / 1024 / sec
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	return s.mark(res, true), nil
}

// ---- backup ----

func (s *session) flashBackup(ctx context.Context, args map[string]any) (map[string]any, error) {
	dir, err := strArg(args, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = s.defaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, argErr(err)
	}
	var res map[string]any
	err = s.flashSession(ctx, args, false, func(fc *flashCtx) error {
		m, path, err := fc.f.Backup(ctx, dir, s.entry.Dev.Serial, fc.info, nil)
		if err != nil {
			return err
		}
		mm, err := toMap(m)
		if err != nil {
			return err
		}
		res = map[string]any{
			"backup_path": path, "manifest_path": path + ".json", "size_bytes": float64(m.SizeBytes), "sha256": m.SHA256,
			"second_read_sha256": m.SecondRead, "reads_identical": m.SHA256 == m.SecondRead, "read_seconds": m.ReadSeconds,
			"jedec_id": m.Flash.JEDEC, "manifest": mm,
			"note": "the backup is a LOCAL artifact (mode 0600) and contains the device's full firmware and data; never commit or publish it",
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	return s.mark(res, true), nil
}

// ---- verify ----

func (s *session) flashVerify(ctx context.Context, args map[string]any) (map[string]any, error) {
	data, path, err := imageArgs(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(fmt.Errorf("%w (the load address of the image in flash)", err))
	}
	var res map[string]any
	err = s.flashSession(ctx, args, false, func(fc *flashCtx) error {
		cur, err := fc.f.Read(ctx, addr, len(data))
		if err != nil {
			return err
		}
		diff, first := 0, -1
		for i := range data {
			if cur[i] != data[i] {
				diff++
				if first < 0 {
					first = i
				}
			}
		}
		res = map[string]any{"path": path, "address": fmt.Sprintf("0x%08x", addr), "length": float64(len(data)), "image_sha256": sum(data),
			"flash_sha256": sum(cur), "match": diff == 0, "differing_bytes": float64(diff)}
		if first >= 0 {
			res["first_difference_at"] = fmt.Sprintf("0x%08x", addr+uint32(first))
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	if res["match"] != true {
		return nil, status.Errorf(codes.DataLoss, "flash differs from %s: %v byte(s) differ, first at %v", path, res["differing_bytes"], res["first_difference_at"])
	}
	return s.mark(res, true), nil
}

// ---- erase ----

func (s *session) flashErase(ctx context.Context, args map[string]any) (map[string]any, error) {
	mode, err := strArg(args, "mode")
	if err != nil {
		return nil, err
	}
	var tok string
	switch mode {
	case "region":
		tok = tokEraseReg
	case "app":
		tok = tokEraseApp
	case "chip":
		tok = tokEraseChip
	default:
		return nil, argErr(errors.New(`mode: must be "region", "app" or "chip" - there is no implicit erase`))
	}
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if mode == "chip" && !s.o.perm.chipErase {
		return nil, denied("erasing the entire chip needs allow_chip_erase=true in the device configuration (a stronger permission than allow_erase)")
	}
	if err := tokenIs(args, "confirm", tok, "this erase mode needs its confirmation token"); err != nil {
		return nil, err
	}
	var addr uint32
	var n int
	var label string
	if mode == "region" {
		a, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr(err)
		}
		l, err := intArg(args, "length", 0, 1, 1<<30)
		if err != nil {
			return nil, err
		}
		if l == 0 {
			return nil, argErr(errors.New("length: required, a multiple of 4096"))
		}
		addr, n = a, l
		if addr%esp.SectorSize != 0 || n%esp.SectorSize != 0 {
			return nil, argErr(fmt.Errorf("address 0x%x and length %d must be multiples of the 4096-byte sector", addr, n))
		}
	}
	if mode == "app" {
		if label, err = strArg(args, "partition"); err != nil || label == "" {
			return nil, argErr(errors.New("partition: the label of an app partition is required"))
		}
	}
	var res map[string]any
	err = s.flashSession(ctx, args, true, func(fc *flashCtx) error {
		switch mode {
		case "app":
			if fc.tabErr != nil || len(fc.parts) == 0 {
				return status.Errorf(codes.FailedPrecondition, "no valid partition table, cannot resolve %q: %v", label, fc.tabErr)
			}
			p, ok := fc.partitionByLabel(label)
			if !ok {
				return status.Errorf(codes.NotFound, "partition %q not found", label)
			}
			if p.Type != 0x00 {
				return status.Errorf(codes.InvalidArgument, "partition %q is %s, not an app partition: refusing", label, p.Kind())
			}
			addr, n = p.Offset, int(p.Size)
		case "chip":
			addr, n = 0, fc.info.SizeBytes
		}
		if err := fc.f.CheckRange(addr, n); err != nil {
			return err
		}
		hit, err := fc.protectedGate(args, addr, n)
		if err != nil {
			return err
		}
		bk, err := fc.backupGate(ctx, args, addr, n)
		if err != nil {
			return err
		}
		start := time.Now()
		if mode == "chip" {
			if err := fc.f.EraseChip(ctx); err != nil {
				return err
			}
			if err := fc.f.VerifyErased(ctx, 0, n); err != nil {
				return err
			}
		} else {
			if err := fc.f.Erase(ctx, addr, n, nil); err != nil {
				return err
			}
		}
		res = map[string]any{"mode": mode, "address": fmt.Sprintf("0x%08x", addr), "length": float64(n), "verified_erased": true,
			"backup": bk, "elapsed_ms": float64(time.Since(start).Milliseconds()), "protected_overlap": float64(len(hit)),
			"note": "the hart is left halted; nothing was resumed after an erase. Program an image or restore the backup before resetting"}
		if label != "" {
			res["partition"] = label
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	return s.mark(res, true), nil
}

// ---- program ----

func (s *session) flashProgram(ctx context.Context, args map[string]any) (map[string]any, error) {
	data, path, err := imageArgs(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(fmt.Errorf("%w (where in flash the image goes)", err))
	}
	if addr%2 != 0 {
		return nil, argErr(fmt.Errorf("address 0x%x is not 2-byte aligned", addr))
	}
	var im *esp.Image
	if raw, _ := args["raw"].(bool); data[0] != esp.ImageMagic && !raw {
		return nil, argErr(fmt.Errorf("%s is not an ESP image (no 0x%02X magic): pass raw=true to write it as unvalidated raw bytes", path, esp.ImageMagic))
	}
	if data[0] == esp.ImageMagic {
		parsed, err := esp.ParseImage(data)
		if err != nil {
			return nil, argErr(fmt.Errorf("%s is not a valid ESP image (truncated or corrupt images are refused before anything is erased): %w", path, err))
		}
		if parsed.ChipID != esp.ChipIDESP32C6 {
			return nil, argErr(fmt.Errorf("%s targets chip id 0x%04x, not the ESP32-C6 (0x%04x)", path, parsed.ChipID, esp.ChipIDESP32C6))
		}
		if parsed.SizeBytes > len(data) {
			return nil, argErr(fmt.Errorf("%s: image needs %d bytes but the file has %d", path, parsed.SizeBytes, len(data)))
		}
		im = &parsed
	}
	rst, err := strArg(args, "reset")
	if err != nil {
		return nil, err
	}
	if rst == "" {
		rst = "run"
	}
	if rst != "run" && rst != "halt" && rst != "none" {
		return nil, argErr(fmt.Errorf("reset %q: want run, halt or none", rst))
	}
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokProgram, "programming flash modifies the firmware"); err != nil {
		return nil, err
	}
	if rst != "none" && !s.o.perm.reset {
		return nil, denied("reset=%s needs allow_reset=true in the device configuration (or pass reset=none)", rst)
	}
	var res map[string]any
	err = s.flashSession(ctx, args, true, func(fc *flashCtx) error {
		if im != nil && !fc.ev.MagicMatchesC6 {
			return status.Errorf(codes.FailedPrecondition, "the image is for the ESP32-C6 but the connected chip did not show the ESP32-C6 ROM evidence (%s)", fc.ev.Basis)
		}
		if err := fc.f.CheckRange(addr, len(data)); err != nil {
			return err
		}
		if l := fc.regionsTouched(addr, len(data)); len(l) > 1 {
			return status.Errorf(codes.InvalidArgument, "the data spans %d flash regions (%s): program one region at a time", len(l), strings.Join(l, ", "))
		}
		hit, err := fc.protectedGate(args, addr, len(data))
		if err != nil {
			return err
		}
		bk, err := fc.backupGate(ctx, args, addr, len(data))
		if err != nil {
			return err
		}
		start := time.Now()
		rep, err := fc.f.WriteRegion(ctx, addr, data, nil)
		if err != nil {
			return err
		}
		prog := time.Since(start)
		got, err := fc.f.Read(ctx, addr, len(data))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, data) {
			return status.Errorf(codes.DataLoss, "final read-back differs from the image after programming (sha256 %s vs %s)", sum(got), sum(data))
		}
		rep2, _ := toMap(rep)
		res = map[string]any{
			"path": path, "address": fmt.Sprintf("0x%08x", addr), "length": float64(len(data)), "image_sha256": sum(data), "verified": true,
			"image_validated": im != nil, "backup": bk, "write_report": rep2, "protected_overlap": float64(len(hit)),
			"program_ms": float64(prog.Milliseconds()), "preserved": "bytes of edge sectors outside the image were read first and written back unchanged",
		}
		if im != nil {
			res["image"] = map[string]any{"segments": float64(len(im.Segments)), "entry": fmt.Sprintf("0x%08x", im.Entry), "chip_id": fmt.Sprintf("0x%04x", im.ChipID), "hash_appended": im.HashAppended}
		} else {
			res["note"] = "not an ESP image (no 0xE9 magic): written as raw bytes, structure not validated"
		}
		if prog > 0 {
			res["throughput_kb_s"] = float64(rep.BytesProgrammed) / 1024 / prog.Seconds()
		}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	res["reset"] = rst
	if rst != "none" {
		rr, err := s.reset(ctx, map[string]any{"kind": "system", "halt": rst == "halt", "hart": s.curHart()})
		if err != nil {
			res["reset_error"] = err.Error()
			return nil, fmt.Errorf("programmed and verified, but the reset failed (hart left halted): %w", err)
		}
		res["reset_result"] = rr
	}
	return s.mark(res, true), nil
}

// ---- restore ----

func (s *session) flashRestore(ctx context.Context, args map[string]any) (map[string]any, error) {
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, argErr(errors.New("path: the verified backup .bin is required"))
	}
	rst, err := strArg(args, "reset")
	if err != nil {
		return nil, err
	}
	if rst == "" {
		rst = "run"
	}
	if rst != "run" && rst != "halt" && rst != "none" {
		return nil, argErr(fmt.Errorf("reset %q: want run, halt or none", rst))
	}
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokRestore, "restoring a backup rewrites flash"); err != nil {
		return nil, err
	}
	if rst != "none" && !s.o.perm.reset {
		return nil, denied("reset=%s needs allow_reset=true in the device configuration (or pass reset=none)", rst)
	}
	b, m, err := esp.LoadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	if m == nil || m.SHA256 != m.SecondRead {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s is not a verified backup (manifest missing or without two identical reads)", path)
	}
	var res map[string]any
	err = s.flashSession(ctx, args, true, func(fc *flashCtx) error {
		if m.Flash.JEDEC != fc.info.JEDEC || len(b) != fc.info.SizeBytes {
			return status.Errorf(codes.FailedPrecondition, "backup is of another flash (JEDEC %s, %d bytes), connected is JEDEC %s, %d bytes", m.Flash.JEDEC, len(b), fc.info.JEDEC, fc.info.SizeBytes)
		}
		if m.USBSerial != "" && m.USBSerial != s.entry.Dev.Serial {
			if ok, _ := boolArg(args, "allow_other_device", false); !ok {
				return status.Errorf(codes.FailedPrecondition, "the backup was taken from another device (USB serial differs); pass allow_other_device=true to restore it here anyway")
			}
		}
		if _, err := fc.protectedGate(args, 0, len(b)); err != nil {
			return err
		}
		start := time.Now()
		rep, err := fc.f.WriteRegion(ctx, 0, b, nil)
		if err != nil {
			return err
		}
		took := time.Since(start)
		h := sha256.New()
		var got []byte
		if got, err = fc.f.Read(ctx, 0, len(b)); err != nil {
			return err
		}
		h.Write(got)
		if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
			return status.Errorf(codes.DataLoss, "after the restore the flash SHA-256 %x differs from the backup %s", h.Sum(nil), m.SHA256)
		}
		rep2, _ := toMap(rep)
		res = map[string]any{"path": path, "size_bytes": float64(len(b)), "sha256": m.SHA256, "flash_sha256_after": hex.EncodeToString(h.Sum(nil)), "verified": true,
			"write_report": rep2, "elapsed_ms": float64(took.Milliseconds())}
		return nil
	})
	if err != nil {
		return nil, flashErr(err)
	}
	res["reset"] = rst
	if rst != "none" {
		rr, err := s.reset(ctx, map[string]any{"kind": "system", "halt": rst == "halt", "hart": s.curHart()})
		if err != nil {
			return nil, fmt.Errorf("restored and verified, but the reset failed (hart left halted): %w", err)
		}
		res["reset_result"] = rr
	}
	return s.mark(res, true), nil
}
