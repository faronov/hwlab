package stm32dfuplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

// Tokens shared with the ST-LINK plugin.
const (
	tokProgram  = "program-flash"
	tokRestore  = "restore-flash"
	tokEraseReg = "erase-flash-region"

	maxFlashInline = 4096
	maxFlashFile   = 16 << 20

	backupFormat = "hwlab-stm32-flash-backup/1"
	zigbeeWarn   = "never restore this backup onto a device that has joined a Zigbee network: it holds that device's network key, frame counters and identity and would clone them; restore only onto the same chip, unjoined, under the same SFSA"
)

// manifest describes a backup. It holds no firmware bytes. The common
// fields match the ST-LINK plugin, so either plugin accepts the other's
// STM32WB backups.
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
	ProbeSerial string  `json:"probe_serial,omitempty"`
	SecureBase  string  `json:"secure_base,omitempty"`
	UserBytes   int     `json:"user_bytes,omitempty"`
	// stm32dfu additions.
	SFSA              string `json:"sfsa,omitempty"`
	SFSASource        string `json:"sfsa_source,omitempty"`
	Transport         string `json:"transport,omitempty"`
	BootloaderVersion string `json:"bootloader_version,omitempty"`
	DevIDSource       string `json:"dev_id_source,omitempty"`
	Warning           string `json:"warning,omitempty"`
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

func (s *session) defaultDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "stm32dfu-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "stm32dfu-flash")
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
	if len(b) != len(a) {
		return len(a)
	}
	return -1
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

// flashTarget returns the identified target for flash calls. Reads need
// the boundary; modifications also need an STM32WB5x.
func (s *session) flashTarget(ctx context.Context, modify bool) (*target, error) {
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	if modify {
		if err := t.writable(); err != nil {
			return nil, err
		}
		if _, lo, hi, open := s.c.Window(); !open || lo != flashBase || hi != t.secure {
			return nil, status.Error(codes.Internal, "the DFU write window does not match the secure boundary: refusing")
		}
	} else if !t.sfsaOK {
		return nil, status.Errorf(codes.FailedPrecondition, "flash reads are refused while the STM32WB secure flash start is unknown (%s); MemoryAccess.read still reads SRAM and the option bytes", t.sfsaWhy)
	}
	return t, nil
}

func (t *target) contains(addr uint32, n int) bool {
	return n > 0 && addr >= flashBase && uint64(addr)+uint64(n) <= uint64(t.secure)
}

func rangeArgs(args map[string]any, t *target, what string) (uint32, int, error) {
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
	if what == "read" && n < dfu.MinXfer {
		return 0, 0, argErr(fmt.Errorf("read: length must be at least %d bytes (an AN3156 upload is 2..2048 bytes)", dfu.MinXfer))
	}
	if !t.contains(addr, n) {
		return 0, 0, status.Errorf(codes.OutOfRange, "%s: [0x%08x, +%d) is outside the user flash 0x%08x..0x%08x (the secure area from SFSA 0x%02x is never touched)", what, addr, n, uint32(flashBase), t.secure, t.sfsa)
	}
	return addr, n, nil
}

// readFlash reads user flash (refused at or above the secure start).
func (s *session) readFlash(ctx context.Context, addr uint32, n int) ([]byte, error) {
	b, err := s.readAt(ctx, addr, n)
	if err != nil {
		return nil, fmt.Errorf("reading flash 0x%08x+%d: %w", addr, n, err)
	}
	return b, nil
}

// ---- info / read / verify ----

func (s *session) flashInfo(ctx context.Context) (map[string]any, error) {
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	res := t.facts()
	res["supported"] = t.wb5x
	res["page_size"] = float64(pageSize)
	res["program_granularity_bytes"] = float64(8)
	res["program_method"] = "DfuSe: set address, page erase (0x41 + address) only where a double word must change, DNLOAD blocks of wTransferSize, read back"
	res["modification_allowed"] = t.writable() == nil
	if f := s.cfg.Functional; f != nil {
		res["transfer_size"] = float64(f.TransferSize)
	}
	if t.flashAlt != nil {
		res["flash_alt_setting"] = float64(t.flashAlt.Alt)
		res["flash_alt_name"] = t.flashAlt.Name
	}
	if _, lo, hi, open := s.c.Window(); open {
		res["write_window"] = fmt.Sprintf("[0x%08x, 0x%08x)", lo, hi)
	} else {
		res["write_window"] = "closed"
	}
	return s.mark(res), nil
}

func (s *session) flashRead(ctx context.Context, args map[string]any) (map[string]any, error) {
	t, err := s.flashTarget(ctx, false)
	if err != nil {
		return nil, err
	}
	addr, n, err := rangeArgs(args, t, "read")
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
	data, err := s.readFlash(ctx, addr, n)
	if err != nil {
		return nil, err
	}
	el := time.Since(t0)
	res := map[string]any{"address": hexw(addr), "length": float64(n), "elapsed_ms": float64(el.Milliseconds()), "via": "DfuSe UPLOAD on the flash alternate setting"}
	if el > 0 && n >= 1024 {
		res["throughput_kib_s"] = float64(n) / 1024 / el.Seconds()
	}
	if err := output(res, args, path, data); err != nil {
		return nil, err
	}
	return s.mark(res), nil
}

func (s *session) flashVerify(ctx context.Context, args map[string]any) (map[string]any, error) {
	t, err := s.flashTarget(ctx, false)
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
	if len(img) < dfu.MinXfer {
		return nil, argErr(fmt.Errorf("image %s: at least %d bytes (an AN3156 upload is 2..2048 bytes)", path, dfu.MinXfer))
	}
	if !t.contains(addr, len(img)) {
		return nil, status.Errorf(codes.OutOfRange, "image %s (%d bytes) at 0x%08x does not fit the user flash 0x%08x..0x%08x", path, len(img), addr, uint32(flashBase), t.secure)
	}
	got, err := s.readFlash(ctx, addr, len(img))
	if err != nil {
		return nil, err
	}
	if i := firstDiff(img, got); i >= 0 {
		return nil, status.Errorf(codes.DataLoss, "verify FAILED: flash differs from %s first at 0x%08x (file 0x%02x, flash 0x%02x); flash sha256 %s, file sha256 %s",
			path, addr+uint32(i), img[i], got[i], sha(got), sha(img))
	}
	return s.mark(map[string]any{"verified": true, "address": hexw(addr), "length": float64(len(img)), "sha256": sha(img)}), nil
}

// ---- backup ----

func (s *session) flashBackup(ctx context.Context, args map[string]any) (map[string]any, error) {
	t, err := s.flashTarget(ctx, false)
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
	size := t.userBytes
	t0 := time.Now()
	first, err := s.readFlash(ctx, flashBase, size)
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := s.readFlash(ctx, flashBase, size)
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	m := manifest{Format: backupFormat, SizeBytes: size, SHA256: sha(first), SecondRead: sha(second),
		Chip: t.chip, DevID: t.devID, RevID: t.revID, FlashKB: t.flashKB, ReadSeconds: time.Since(t0).Seconds(), ProbeSerial: s.entry.Serial,
		SecureBase: hexw(t.secure), UserBytes: size, SFSA: fmt.Sprintf("0x%02x", t.sfsa), SFSASource: t.boundarySource(),
		Transport: "usb-dfu (STM32 ROM bootloader)", BootloaderVersion: bcd(s.entry.Dev.BCD), DevIDSource: t.idSource, Warning: zigbeeWarn}
	if m.SHA256 != m.SecondRead {
		return nil, status.Errorf(codes.DataLoss, "the two user-flash reads differ (%s vs %s): no backup was stored", m.SHA256, m.SecondRead)
	}
	m.File = fmt.Sprintf("stm32-flash-%03x-%dK-user%dK-%s.bin", t.devID, t.flashKB, size>>10, m.SHA256[:12])
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
		"identical_reads": true, "read_seconds": m.ReadSeconds, "chip": t.chip, "sfsa": m.SFSA, "secure_base": m.SecureBase,
		"note": "local artifact: it holds the complete user firmware, never commit it. " + zigbeeWarn,
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
	if m.Format != backupFormat {
		return nil, nil, fmt.Errorf("manifest format %q: want %q", m.Format, backupFormat)
	}
	if sha(b) != m.SHA256 {
		return nil, nil, fmt.Errorf("backup %s does not match the SHA-256 in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead {
		return nil, nil, errors.New("the manifest does not record two identical reads")
	}
	return b, &m, nil
}

// checkLayout requires a backup of this chip taken under the current SFSA.
func checkLayout(path string, b []byte, m *manifest, t *target) error {
	if m.DevID != t.devID || len(b) != t.userBytes || m.UserBytes != t.userBytes || m.SecureBase != hexw(t.secure) {
		return status.Errorf(codes.FailedPrecondition, "backup %s is of another chip or secure boundary (%s dev 0x%03x, %d bytes, secure start %s) than the connected one (%s dev 0x%03x, %d user bytes, secure start 0x%08x, SFSA 0x%02x): FUS operations move the boundary, take a new backup",
			path, m.Chip, m.DevID, len(b), m.SecureBase, t.chip, t.devID, t.userBytes, t.secure, t.sfsa)
	}
	return nil
}

// backupGate requires a verified full backup of THIS chip, taken under the
// current SFSA, whose bytes in the affected pages equal the flash now (a
// completely erased page also passes: it holds nothing to lose).
func (s *session) backupGate(ctx context.Context, args map[string]any, t *target, pages []uint32) (map[string]any, error) {
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
	if err := checkLayout(path, b, m, t); err != nil {
		return nil, err
	}
	for _, pg := range pages {
		cur, err := s.readFlash(ctx, pg, pageSize)
		if err != nil {
			return nil, err
		}
		off := pg - flashBase
		if !bytes.Equal(cur, b[off:off+pageSize]) && !isBlank(cur) {
			return nil, status.Errorf(codes.FailedPrecondition, "the backup %s no longer matches page %d (0x%08x): the flash changed after the backup, take a new backup", path, off/pageSize, pg)
		}
	}
	return map[string]any{"path": path, "sha256": m.SHA256, "sfsa": m.SFSA, "affected_pages_match_flash": true}, nil
}

func pagesCovering(addr uint32, n int) []uint32 {
	var out []uint32
	for pg := addr &^ (pageSize - 1); uint64(pg) < uint64(addr)+uint64(n); pg += pageSize {
		out = append(out, pg)
	}
	return out
}

// ---- erase ----

func (s *session) flashErase(ctx context.Context, args map[string]any) (map[string]any, error) {
	mode, err := strArg(args, "mode")
	if err != nil {
		return nil, err
	}
	switch mode {
	case "region":
	case "chip":
		return nil, denied("mode chip: the DFU plugin never sends a mass erase (it would also reach the bootloader's own rules for the secure area); erase an explicit region below the secure start")
	default:
		return nil, argErr(fmt.Errorf("mode %q: want region (there is no implicit mode)", mode))
	}
	if err := s.needFlash(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokEraseReg, "erasing a flash region"); err != nil {
		return nil, err
	}
	t, err := s.flashTarget(ctx, true)
	if err != nil {
		return nil, err
	}
	addr, n, err := rangeArgs(args, t, "erase")
	if err != nil {
		return nil, err
	}
	if addr%pageSize != 0 || n%pageSize != 0 {
		return nil, argErr(fmt.Errorf("[0x%08x, +%d) does not start and end on 4 KiB page boundaries", addr, n))
	}
	pages := pagesCovering(addr, n)
	bk, err := s.backupGate(ctx, args, t, pages)
	if err != nil {
		return nil, err
	}
	erased := []any{}
	for _, pg := range pages {
		if err := s.erasePage(ctx, pg); err != nil {
			if len(erased) == 0 {
				return nil, err
			}
			return nil, status.Errorf(codes.DataLoss, "erase stopped at page 0x%08x after %d page(s): %v", pg, len(erased), err)
		}
		erased = append(erased, hexw(pg))
	}
	return s.mark(map[string]any{
		"mode": mode, "erased_pages": erased, "verified_erased": true, "backup": bk,
		"note": "every erased page was read back as 0xFF",
	}), nil
}

// erasePage erases one page through the flash alternate setting and reads
// it back.
func (s *session) erasePage(ctx context.Context, pg uint32) error {
	if err := s.c.SelectAlt(ctx, s.tgt.flashAlt.Alt); err != nil {
		return err
	}
	if err := s.c.ErasePage(ctx, pg); err != nil {
		return err
	}
	got, err := s.readFlash(ctx, pg, pageSize)
	if err != nil {
		return err
	}
	if !isBlank(got) {
		return fmt.Errorf("page 0x%08x is not blank after the erase", pg)
	}
	return nil
}

// ---- program / restore ----

type pagePlan struct {
	addr    uint32
	cur     []byte
	merged  []byte
	changed bool
	erase   bool
}

// needsErase applies the STM32WB rule: a 64-bit double word can be
// programmed only when it is erased (all ones).
func needsErase(cur, want []byte) bool {
	for i := 0; i+8 <= len(cur); i += 8 {
		if !bytes.Equal(cur[i:i+8], want[i:i+8]) && !isBlank(cur[i:i+8]) {
			return true
		}
	}
	return false
}

func (s *session) planProgram(ctx context.Context, addr uint32, img []byte) ([]pagePlan, error) {
	var out []pagePlan
	for _, pg := range pagesCovering(addr, len(img)) {
		cur, err := s.readFlash(ctx, pg, pageSize)
		if err != nil {
			return nil, err
		}
		merged := append([]byte(nil), cur...)
		lo, hi := max(addr, pg), min(addr+uint32(len(img)), pg+pageSize)
		copy(merged[lo-pg:hi-pg], img[lo-addr:hi-addr])
		out = append(out, pagePlan{addr: pg, cur: cur, merged: merged, changed: !bytes.Equal(cur, merged), erase: needsErase(cur, merged)})
	}
	return out, nil
}

// runs returns the contiguous double-word runs to download.
func runs(pl pagePlan) [][2]int {
	var out [][2]int
	start := -1
	for i := 0; i <= pageSize; i += 8 {
		want := i < pageSize && !isBlank(pl.merged[i:i+8])
		if want && !pl.erase {
			want = !bytes.Equal(pl.cur[i:i+8], pl.merged[i:i+8])
		}
		switch {
		case want && start < 0:
			start = i
		case !want && start >= 0:
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	return out
}

func (s *session) applyPlan(ctx context.Context, plan []pagePlan) (erased, programmed int, err error) {
	for _, pl := range plan {
		if !pl.changed {
			continue
		}
		if pl.erase {
			if err := s.erasePage(ctx, pl.addr); err != nil {
				return erased, programmed, err
			}
			erased++
		}
		if err := s.c.SelectAlt(ctx, s.tgt.flashAlt.Alt); err != nil {
			return erased, programmed, err
		}
		for _, r := range runs(pl) {
			if err := s.c.Write(ctx, pl.addr+uint32(r[0]), pl.merged[r[0]:r[1]]); err != nil {
				return erased, programmed, fmt.Errorf("page 0x%08x: %w", pl.addr, err)
			}
		}
		got, err := s.readFlash(ctx, pl.addr, pageSize)
		if err != nil {
			return erased, programmed, err
		}
		if i := firstDiff(pl.merged, got); i >= 0 {
			return erased, programmed, fmt.Errorf("page 0x%08x read back differs at 0x%08x (want 0x%02x, got 0x%02x)", pl.addr, pl.addr+uint32(i), pl.merged[i], got[i])
		}
		programmed++
	}
	return erased, programmed, nil
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
	t, err := s.flashTarget(ctx, true)
	if err != nil {
		return nil, err
	}
	if !t.contains(addr, len(img)) {
		return nil, status.Errorf(codes.OutOfRange, "image %s (%d bytes) at 0x%08x does not fit the user flash 0x%08x..0x%08x (the secure area from SFSA 0x%02x is never touched)", path, len(img), addr, uint32(flashBase), t.secure, t.sfsa)
	}
	plan, err := s.planProgram(ctx, addr, img)
	if err != nil {
		return nil, err
	}
	needErase := false
	pages := make([]uint32, len(plan))
	for i, pl := range plan {
		pages[i] = pl.addr
		needErase = needErase || (pl.changed && pl.erase)
	}
	if needErase && !s.o.perm.erase {
		return nil, denied("this image needs pages erased (a programmed double word must change): that needs allow_erase=true in the device configuration")
	}
	bk, err := s.backupGate(ctx, args, t, pages)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	erased, programmed, err := s.applyPlan(ctx, plan)
	if err != nil {
		return nil, status.Errorf(codes.DataLoss, "programming stopped after %d erase(s) and %d page(s): %v; the flash may be partly written: restore the backup", erased, programmed, err)
	}
	el := time.Since(t0)
	got, err := s.readFlash(ctx, addr, len(img))
	if err != nil {
		return nil, err
	}
	if i := firstDiff(img, got); i >= 0 {
		return nil, status.Errorf(codes.DataLoss, "final verify FAILED at 0x%08x (image 0x%02x, flash 0x%02x)", addr+uint32(i), img[i], got[i])
	}
	return s.mark(map[string]any{
		"address": hexw(addr), "length": float64(len(img)), "sha256": sha(img), "verified": true, "pages_touched": float64(len(plan)),
		"pages_erased": float64(erased), "pages_programmed": float64(programmed), "elapsed_ms": float64(el.Milliseconds()), "backup": bk,
		"basis": "per covering 4 KiB page: read, merge the image over it (bytes outside the image are preserved), erase only when a programmed double word must change, download, read back; then the whole image is compared again. Option bytes are never written",
		"note":  "the device stays in the bootloader; FlashProgramming.leave starts the application",
	}), nil
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
	b, m, err := loadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	t, err := s.flashTarget(ctx, true)
	if err != nil {
		return nil, err
	}
	if err := checkLayout(path, b, m, t); err != nil {
		return nil, err
	}
	plan, err := s.planProgram(ctx, flashBase, b)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	erased, programmed, err := s.applyPlan(ctx, plan)
	if err != nil {
		return nil, status.Errorf(codes.DataLoss, "restore stopped after %d erase(s) and %d page(s): %v", erased, programmed, err)
	}
	el := time.Since(t0)
	got, err := s.readFlash(ctx, flashBase, len(b))
	if err != nil {
		return nil, err
	}
	if sha(got) != m.SHA256 {
		return nil, status.Errorf(codes.DataLoss, "restore FAILED: user-flash SHA-256 %s differs from the backup %s", sha(got), m.SHA256)
	}
	return s.mark(map[string]any{
		"restored": true, "sha256": m.SHA256, "user_flash_matches_backup": true, "pages_erased": float64(erased),
		"pages_programmed": float64(programmed), "elapsed_ms": float64(el.Milliseconds()), "warning": zigbeeWarn,
	}), nil
}
