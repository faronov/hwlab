package jlink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ownedtemp"
)

// FlashProgramming over J-Link Commander. Reads use savebin on a halted core
// (a running firmware writing its NVM would make two reads differ) and put
// the target back in the state it had. Every modifying call needs, like the
// other probe plugins: allow_flash (+ allow_erase / allow_chip_erase) in the
// device configuration (never from call args), a confirmation token, and a
// verified full backup whose bytes still match the affected sectors. Writes
// go through SEGGER's flash loader (loadbin) and are always read back.

// Confirmation tokens for destructive calls (the same as stlink, telink...).
const (
	tokProgram   = "program-flash"
	tokRestore   = "restore-flash"
	tokEraseReg  = "erase-flash-region"
	tokEraseChip = "erase-entire-chip"
)

const (
	maxFlashInline = 4096
	maxFlashFile   = 16 << 20
	backupFormat   = "hwlab-jlink-flash-backup/1"
)

// manifest describes a backup. It holds no firmware bytes.
type manifest struct {
	Format      string  `json:"format"`
	CreatedUTC  string  `json:"created_utc"`
	File        string  `json:"file"`
	SizeBytes   int     `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	SecondRead  string  `json:"second_read_sha256"`
	Device      string  `json:"device"`
	FlashBase   string  `json:"flash_base"`
	ReadSeconds float64 `json:"read_seconds"`
	Halted      bool    `json:"halted_during_read"`
	FlashJEDEC  string  `json:"flash_jedec_id,omitempty"` // PHY62xx SPI flash
	ProbeSerial string  `json:"probe_serial,omitempty"`   // local artifact only
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func hexAddr(v uint64) string { return fmt.Sprintf("0x%08X", v) }

func argErr(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, format, a...)
}

func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", argErr("%s: want a string", key)
	}
	return strings.TrimSpace(s), nil
}

func boolArg(args map[string]any, key string, def bool) (bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, argErr("%s: want true or false", key)
	}
	return b, nil
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

// perm reads a permission from the device configuration given to OpenDevice.
// Call args are merged over that configuration for everything else, so
// permissions must never be taken from them.
func (p *Plugin) perm(id, key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, _ := p.cfg[id][key].(bool)
	return v
}

func (p *Plugin) needFlash(id string, erase bool) error {
	if !p.perm(id, "allow_flash") {
		return denied("flash modification needs allow_flash=true in the device configuration")
	}
	if erase && !p.perm(id, "allow_erase") {
		return denied("erasing flash needs allow_erase=true in the device configuration")
	}
	return nil
}

func defaultBackupDir() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "jlink-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "jlink-flash")
}

// flashTimeout gives a long commander session time in proportion to the bytes
// it moves (about 1 s per 8 KiB on top of a minute; reads at 4 MHz SWD run at
// several hundred KiB/s, flash programming much slower).
func flashTimeout(ctx context.Context, bytes uint64) context.Context {
	return WithTimeout(ctx, time.Minute+time.Duration(bytes/8192)*time.Second)
}

// ---- geometry ----

// geom is the flash layout of the target, from SEGGER's device list unless
// the device configuration overrides it.
type geom struct {
	t            Target
	res          Resolution
	banks        []FlashBank // every bank SEGGER lists (plus a configured one)
	base, size   uint64
	sector       uint64 // 0 = unknown
	sizeSource   string
	sectorSource string
}

var (
	reEFR32S2 = regexp.MustCompile(`^(EFR32|MGM2|BGM2|FGM2)[A-Z]{0,2}2\d`)
	reEFR32S1 = regexp.MustCompile(`^(EFR32|MGM1|BGM1)[A-Z]{0,2}1\d?[A-Z]`)
)

// knownSector returns the erase granularity for families whose datasheets
// give one uniform page size. SEGGER's device list does not export it.
func knownSector(name string) (uint64, string) {
	n := strings.ToUpper(name)
	switch {
	case reEFR32S2.MatchString(n):
		return 8 << 10, "Silicon Labs Series 2 (xG2x): 8 KiB flash pages"
	case reEFR32S1.MatchString(n):
		return 2 << 10, "Silicon Labs Series 1 (xG1x): 2 KiB flash pages"
	case strings.HasPrefix(n, "NRF52"), strings.HasPrefix(n, "NRF53"), strings.HasPrefix(n, "NRF91"):
		return 4 << 10, "Nordic nRF52/nRF53/nRF91: 4 KiB flash pages"
	case strings.HasPrefix(n, "NRF51"):
		return 1 << 10, "Nordic nRF51: 1 KiB flash pages"
	}
	return 0, ""
}

// flashGeom resolves the target and its flash layout. For calls that modify
// flash (modify=true) the geometry overrides are taken from the device
// configuration only: a wrong sector size in call args could make SEGGER's
// loader erase bytes hwlab neither preserves nor checks against the backup.
func (p *Plugin) flashGeom(ctx context.Context, id string, args map[string]any, modify bool) (*geom, error) {
	t, err := target(args)
	if err != nil {
		return nil, argErr("%v", err)
	}
	if t.Device == "" {
		return nil, argErr(`no target device: pass "target" (a SEGGER device name such as EFR32MG21AXXXF1024 or NRF52840_XXAA) in the device config`)
	}
	t, r, err := p.resolveTarget(ctx, t)
	if err != nil {
		return nil, err
	}
	g := &geom{t: t, res: r, banks: append([]FlashBank(nil), r.Entry.Banks...), sizeSource: "SEGGER device list (ExpDevListXML)"}
	// The main flash is the first bank of at least 4 KiB: SEGGER lists small
	// configuration banks first for some parts (nRF5340: UICR at 0xFF8000).
	for _, b := range g.banks {
		if b.Size >= 4<<10 {
			g.base, g.size = b.Base, b.Size
			break
		}
	}
	if g.size == 0 && len(g.banks) > 0 {
		g.base, g.size = g.banks[0].Base, g.banks[0].Size
	}
	src := args
	if modify {
		p.mu.Lock()
		src = map[string]any{}
		for _, k := range []string{"flash_base", "flash_size", "flash_sector_bytes"} {
			if v, ok := p.cfg[id][k]; ok {
				src[k] = v
			}
		}
		p.mu.Unlock()
		for _, k := range []string{"flash_base", "flash_size", "flash_sector_bytes"} {
			if v, inArgs := args[k]; inArgs && !reflect.DeepEqual(v, src[k]) {
				return nil, argErr("%s: for erase/program/restore the flash geometry comes from the device configuration only", k)
			}
		}
	}
	_, hasBase := src["flash_base"]
	_, hasSize := src["flash_size"]
	if hasBase || hasSize {
		if !hasBase || !hasSize {
			return nil, argErr("flash_base and flash_size go together")
		}
		b, err1 := toAddr(src["flash_base"])
		s, err2 := toAddr(src["flash_size"])
		if err1 != nil || err2 != nil || s == 0 || s > 1<<30 {
			return nil, argErr("flash_base/flash_size: want numbers or strings like \"0x0\" and \"0x100000\"")
		}
		g.base, g.size, g.sizeSource = b, s, "device configuration (flash_base, flash_size)"
		g.banks = append([]FlashBank{{Base: b, Size: s}}, g.banks...)
	}
	if g.size == 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"UNSUPPORTED_BY_ATTACHED_HW: SEGGER's device list has no flash for %q (a generic core name?): pass the exact device name or flash_base/flash_size in the device configuration", t.Device)
	}
	g.sector, g.sectorSource = knownSector(t.Device)
	if v, ok := src["flash_sector_bytes"]; ok {
		n, err := toAddr(v)
		if err != nil || n == 0 || n&(n-1) != 0 || n > 1<<20 {
			return nil, argErr("flash_sector_bytes %v: want a power of two", v)
		}
		g.sector, g.sectorSource = n, "device configuration (flash_sector_bytes)"
	}
	if g.sector != 0 && (g.base%g.sector != 0 || g.size%g.sector != 0) {
		return nil, status.Errorf(codes.FailedPrecondition, "flash %s+0x%X is not a whole number of %d-byte sectors", hexAddr(g.base), g.size, g.sector)
	}
	return g, nil
}

// inBank reports whether [addr, addr+n) lies inside one flash bank.
func (g *geom) inBank(addr, n uint64) bool {
	for _, b := range g.banks {
		if addr >= b.Base && addr+n <= b.Base+b.Size && addr+n >= addr {
			return true
		}
	}
	return false
}

func (g *geom) inMain(addr, n uint64) bool {
	return addr >= g.base && addr+n <= g.base+g.size && addr+n >= addr
}

// needSectors refuses a modification when the erase granularity is unknown:
// SEGGER's loader would erase whole sectors that hwlab could not preserve or
// check against the backup.
func (g *geom) needSectors() error {
	if g.sector == 0 {
		return status.Errorf(codes.FailedPrecondition,
			"the flash sector size of %s is not known to hwlab: set flash_sector_bytes in the device configuration (from the datasheet) to modify its flash", g.t.Device)
	}
	return nil
}

// cover widens [addr, addr+n) to whole sectors.
func (g *geom) cover(addr, n uint64) (uint64, uint64) {
	lo := addr - (addr-g.base)%g.sector
	hi := addr + n
	if r := (hi - g.base) % g.sector; r != 0 {
		hi += g.sector - r
	}
	return lo, hi - lo
}

func (g *geom) mark(res map[string]any) map[string]any {
	res["target"] = g.t.Device
	if !strings.EqualFold(g.res.Input, g.t.Device) {
		res["target_requested"] = g.res.Input
		res["target_resolution"] = g.res.How
	}
	return res
}

// ---- commander sessions ----

// state reports whether the core is halted now, in its own short session.
func (p *Plugin) state(ctx context.Context, id string, t Target) (bool, error) {
	out, err := p.run(ctx, id, t, []string{"IsHalted"})
	if err != nil {
		return false, err
	}
	h, ok := firstHaltState(out)
	if !ok {
		return false, status.Errorf(codes.Internal, "J-Link: no CPU state in the IsHalted output: %v", tail(out, 3))
	}
	return h, nil
}

type span struct {
	addr, n uint64
}

// readSpans saves every span with savebin in one commander session. With
// halt the core is halted first and resumed afterwards when it was running.
func (p *Plugin) readSpans(ctx context.Context, id string, t Target, spans []span, halt bool) (data [][]byte, info map[string]any, err error) {
	wasHalted, err := p.state(ctx, id, t)
	if err != nil {
		return nil, nil, err
	}
	dir, err := ownedtemp.New("hwlab-jlink-read-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	var script []string
	resume := halt && !wasHalted
	if resume {
		script = append(script, "h", "IsHalted")
	}
	files := make([]string, len(spans))
	var total uint64
	for i, s := range spans {
		files[i] = dir.File(fmt.Sprintf("read%d.bin", i))
		script = append(script, fmt.Sprintf("savebin %q 0x%X 0x%X", files[i], s.addr, s.n))
		total += s.n
	}
	if resume {
		script = append(script, "g", "IsHalted")
	}
	out, err := p.run(flashTimeout(ctx, total), id, t, script)
	if err != nil {
		return nil, nil, err
	}
	if resume {
		if h, ok := firstHaltState(out); !ok || !h {
			return nil, nil, status.Errorf(codes.Unknown, "J-Link: %v before the flash read; nothing was read", cortexm.ErrNotHalted)
		}
	}
	if f := cmdFailure(out); f != "" {
		return nil, nil, status.Errorf(codes.Internal, "J-Link: flash read failed: %s", f)
	}
	for i, s := range spans {
		b, err := os.ReadFile(files[i])
		if err != nil || uint64(len(b)) != s.n {
			return nil, nil, status.Errorf(codes.Internal, "J-Link: savebin 0x%X+0x%X returned %d bytes: %v", s.addr, s.n, len(b), tail(out, 3))
		}
		data = append(data, b)
	}
	final, _, _ := haltState(out)
	info = map[string]any{"was_halted": wasHalted, "halted_during_read": halt || wasHalted, "resumed": resume && !final}
	if !halt && !wasHalted {
		info["note"] = "read with the core running (halt=false): a firmware writing its NVM can make reads inconsistent"
	}
	if resume && final {
		info["warning"] = "the core did not report running after the resume (g)"
	}
	return data, info, nil
}

// resetMode parses reset=run|halt|none. The legacy run=false means halt.
func resetMode(args map[string]any, def string) (string, error) {
	m, err := strArg(args, "reset")
	if err != nil {
		return "", err
	}
	if m == "" {
		m = def
		if run, ok := args["run"].(bool); ok {
			m = map[bool]string{true: "run", false: "halt"}[run]
		}
	}
	switch m {
	case "run", "halt", "none":
		return m, nil
	}
	return "", argErr("reset %q: want run, halt or none", m)
}

// afterScript ends a modifying session: reset and run, reset and stay halted
// (until the commander session ends), or leave the core as it was.
func afterScript(mode string, wasHalted bool) []string {
	switch mode {
	case "run":
		return []string{"r", "g", "IsHalted"}
	case "halt":
		return []string{"r", "IsHalted"}
	}
	if !wasHalted {
		return []string{"g", "IsHalted"}
	}
	return []string{"IsHalted"}
}

type write struct {
	addr uint64
	data []byte
}

// writeSpans programs every write with loadbin (SEGGER's flash loader erases
// and programs the touched sectors) or erases ranges, then reads back
// [rbAddr, rbAddr+rbLen) in the same halted session.
func (p *Plugin) writeSession(ctx context.Context, id string, t Target, wasHalted bool, writes []write, erases []span, rbAddr, rbLen uint64, mode string) (rb []byte, out string, err error) {
	dir, err := ownedtemp.New("hwlab-jlink-write-")
	if err != nil {
		return nil, "", err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	script := []string{"h", "IsHalted"}
	total := rbLen
	for _, e := range erases {
		script = append(script, fmt.Sprintf("erase 0x%X 0x%X", e.addr, e.addr+e.n-1))
		total += e.n
	}
	for i, w := range writes {
		f := dir.File(fmt.Sprintf("write%d.bin", i))
		if err := os.WriteFile(f, w.data, 0o600); err != nil {
			return nil, "", err
		}
		script = append(script, fmt.Sprintf("loadbin %q 0x%X", f, w.addr))
		total += 4 * uint64(len(w.data))
	}
	rbf := dir.File("readback.bin")
	script = append(script, fmt.Sprintf("savebin %q 0x%X 0x%X", rbf, rbAddr, rbLen))
	script = append(script, afterScript(mode, wasHalted)...)
	out, err = p.run(flashTimeout(ctx, total), id, t, script)
	if err != nil {
		return nil, out, err
	}
	if h, ok := firstHaltState(out); !ok || !h {
		return nil, out, status.Errorf(codes.Unknown, "J-Link: %v: refusing to continue; the commander output: %v", cortexm.ErrNotHalted, tail(out, 3))
	}
	if f := cmdFailure(out); f != "" {
		return nil, out, status.Errorf(codes.DataLoss, "J-Link: flash write failed: %s; the flash may be partly written: restore the backup", f)
	}
	rb, err = os.ReadFile(rbf)
	if err != nil || uint64(len(rb)) != rbLen {
		return nil, out, status.Errorf(codes.DataLoss, "J-Link: no read-back after the write (%d of %d bytes): %v", len(rb), rbLen, tail(out, 3))
	}
	return rb, out, nil
}

func finalState(res map[string]any, out, mode string) {
	res["reset"] = mode
	if h, _, ok := haltState(out); ok {
		res["halted_at_end_of_session"] = h
	}
	if mode == "halt" {
		res["note_reset"] = "the core was reset and halted; J-Link Commander lets it run again when its session ends"
	}
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
	if len(b) > len(a) {
		return len(a)
	}
	return -1
}

// ---- info / read / verify ----

func (p *Plugin) flashInfo(ctx context.Context, args map[string]any) (map[string]any, error) {
	g, err := p.flashGeom(ctx, "", args, false)
	if err != nil {
		return nil, err
	}
	banks := make([]any, len(g.banks))
	for i, b := range g.banks {
		banks[i] = map[string]any{"base": hexAddr(b.Base), "size": float64(b.Size)}
	}
	res := map[string]any{
		"supported": true, "device": g.t.Device, "core": g.res.Entry.Core,
		"flash_base": hexAddr(g.base), "flash_bytes": float64(g.size), "flash_kb": float64(g.size >> 10), "banks": banks,
		"flash_size_source": g.sizeSource,
		"basis":             "SEGGER's device list in the installed J-Link DLL; no probe or target access",
		"read_method":       "savebin on a halted core (resumed afterwards when it was running)",
		"write_method":      "SEGGER flash loader (loadbin/erase), always read back and compared",
	}
	if len(g.res.Entry.Aliases) > 0 {
		al := make([]any, len(g.res.Entry.Aliases))
		for i, a := range g.res.Entry.Aliases {
			al[i] = a
		}
		res["aliases"] = al
	}
	if g.res.Entry.RAMSize > 0 {
		res["work_ram_base"], res["work_ram_bytes"] = hexAddr(g.res.Entry.RAMBase), float64(g.res.Entry.RAMSize)
	}
	if g.sector > 0 {
		res["sector_bytes"], res["sector_source"] = float64(g.sector), g.sectorSource
	} else {
		res["sector_bytes_note"] = "unknown: erase/program/restore need flash_sector_bytes in the device configuration"
	}
	return g.mark(res), nil
}

func (p *Plugin) flashRead(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	g, err := p.flashGeom(ctx, id, args, false)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr("address: %v", err)
	}
	n, err := toInt(args["length"])
	if err != nil || n < 1 || n > maxFlashFile {
		return nil, argErr("length %v: want 1..%d bytes", args["length"], maxFlashFile)
	}
	if !g.inBank(addr, uint64(n)) {
		return nil, status.Errorf(codes.OutOfRange, "read [%s, +%d) is outside the flash banks of %s (main %s+0x%X); MemoryAccess.read reads other memory", hexAddr(addr), n, g.t.Device, hexAddr(g.base), g.size)
	}
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	if path == "" && n > maxFlashInline {
		return nil, argErr("length %d: at most %d bytes inline; pass path=<new file> for more", n, maxFlashInline)
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return nil, err
	}
	halt, err := boolArg(args, "halt", true)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	data, info, err := p.readSpans(ctx, id, g.t, []span{{addr, uint64(n)}}, halt)
	if err != nil {
		return nil, err
	}
	el := time.Since(t0)
	res := map[string]any{"address": hexAddr(addr), "length": float64(n), "sha256": sha(data[0]), "elapsed_ms": float64(el.Milliseconds()),
		"probe_serial": NormalizeSerial(id), "via": "J-Link Commander savebin"}
	for k, v := range info {
		res[k] = v
	}
	if path == "" {
		res["hex"] = hex.EncodeToString(data[0])
		return g.mark(res), nil
	}
	if err := ownedtemp.WriteFile(path, data[0], overwrite); err != nil {
		return nil, argErr("path %s: %v", path, err)
	}
	res["path"] = path
	return g.mark(res), nil
}

func readImageFile(path string) ([]byte, error) {
	if path == "" {
		return nil, argErr("path: the image file is required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, argErr("path: %v", err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxFlashFile {
		return nil, argErr("path %s: want a non-empty regular file up to %d bytes", path, maxFlashFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, argErr("path: %v", err)
	}
	return b, nil
}

func (p *Plugin) flashVerify(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	g, err := p.flashGeom(ctx, id, args, false)
	if err != nil {
		return nil, err
	}
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	img, err := readImage(path, args, g.base)
	if err != nil {
		return nil, err
	}
	spans := make([]span, len(img))
	for i, w := range img {
		if !g.inBank(w.addr, uint64(len(w.data))) {
			return nil, status.Errorf(codes.OutOfRange, "image %s: [%s, +%d) is outside the flash of %s", path, hexAddr(w.addr), len(w.data), g.t.Device)
		}
		spans[i] = span{w.addr, uint64(len(w.data))}
	}
	got, info, err := p.readSpans(ctx, id, g.t, spans, true)
	if err != nil {
		return nil, err
	}
	var n int
	for i, w := range img {
		if d := firstDiff(w.data, got[i]); d >= 0 {
			return nil, status.Errorf(codes.DataLoss, "verify FAILED: flash differs from %s first at %s (file 0x%02x, flash 0x%02x)", path, hexAddr(w.addr+uint64(d)), w.data[d], got[i][d])
		}
		n += len(w.data)
	}
	res := map[string]any{"verified": true, "path": path, "length": float64(n), "probe_serial": NormalizeSerial(id)}
	if len(img) == 1 {
		res["address"], res["sha256"] = hexAddr(img[0].addr), sha(img[0].data)
	}
	for k, v := range info {
		res[k] = v
	}
	return g.mark(res), nil
}

// ---- images ----

// readImage loads a .bin (at address, default def) or an Intel HEX file as
// address-sorted, non-overlapping runs.
func readImage(path string, args map[string]any, def uint64) ([]write, error) {
	b, err := readImageFile(path)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bin":
		addr := def
		if v, ok := args["address"]; ok && v != nil {
			if addr, err = toAddr(v); err != nil {
				return nil, argErr("address: %v", err)
			}
		}
		return []write{{addr, b}}, nil
	case ".hex", ".ihex":
		return parseIntelHex(b)
	}
	return nil, argErr("path %s: want a .bin (with address) or an Intel .hex file; convert other formats with objcopy", path)
}

// parseIntelHex reads data, EOF and extended segment/linear address records.
func parseIntelHex(b []byte) ([]write, error) {
	var recs []write
	var base uint64
	eof := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for line := 1; sc.Scan(); line++ {
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		if eof {
			return nil, argErr("hex line %d: data after the end-of-file record", line)
		}
		if t[0] != ':' {
			return nil, argErr("hex line %d: missing ':'", line)
		}
		r, err := hex.DecodeString(t[1:])
		if err != nil || len(r) < 5 || int(r[0]) != len(r)-5 {
			return nil, argErr("hex line %d: malformed record", line)
		}
		var sum byte
		for _, v := range r {
			sum += v
		}
		if sum != 0 {
			return nil, argErr("hex line %d: bad checksum", line)
		}
		data := r[4 : 4+int(r[0])]
		switch r[3] {
		case 0x00:
			recs = append(recs, write{base + uint64(r[1])<<8 + uint64(r[2]), append([]byte(nil), data...)})
		case 0x01:
			eof = true
		case 0x02, 0x04:
			if len(data) != 2 {
				return nil, argErr("hex line %d: bad address record", line)
			}
			v := uint64(data[0])<<8 | uint64(data[1])
			if r[3] == 0x02 {
				base = v << 4
			} else {
				base = v << 16
			}
		case 0x03, 0x05:
		default:
			return nil, argErr("hex line %d: unsupported record type 0x%02X", line, r[3])
		}
	}
	if !eof {
		return nil, argErr("hex file has no end-of-file record")
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].addr < recs[j].addr })
	var out []write
	total := 0
	for _, r := range recs {
		if len(r.data) == 0 {
			continue
		}
		total += len(r.data)
		if total > maxFlashFile {
			return nil, argErr("hex image is larger than %d bytes", maxFlashFile)
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			end := last.addr + uint64(len(last.data))
			if r.addr < end {
				return nil, argErr("hex records overlap at %s", hexAddr(r.addr))
			}
			if r.addr == end {
				last.data = append(last.data, r.data...)
				continue
			}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, argErr("hex image contains no data")
	}
	return out, nil
}

// ---- backup ----

func (p *Plugin) flashBackup(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	g, err := p.flashGeom(ctx, id, args, false)
	if err != nil {
		return nil, err
	}
	dir, err := strArg(args, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = defaultBackupDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	t0 := time.Now()
	data, info, err := p.readSpans(ctx, id, g.t, []span{{g.base, g.size}, {g.base, g.size}}, true)
	if err != nil {
		return nil, err
	}
	m := manifest{Format: backupFormat, SizeBytes: int(g.size), SHA256: sha(data[0]), SecondRead: sha(data[1]),
		Device: g.t.Device, FlashBase: hexAddr(g.base), ReadSeconds: time.Since(t0).Seconds(), Halted: true, ProbeSerial: NormalizeSerial(id)}
	if m.SHA256 != m.SecondRead {
		i := firstDiff(data[0], data[1])
		return nil, status.Errorf(codes.DataLoss, "the two full-flash reads differ (%s vs %s, first at %s): no backup was stored", m.SHA256, m.SecondRead, hexAddr(g.base+uint64(i)))
	}
	m.File = fmt.Sprintf("jlink-flash-%s-%08x-%x-%s.bin", safeName(g.t.Device), g.base, g.size, m.SHA256[:12])
	final := filepath.Join(dir, m.File)
	if err := writeBackup(final, data[0], m); err != nil {
		return nil, err
	}
	res := map[string]any{
		"path": final, "manifest": final + ".json", "size_bytes": float64(g.size), "flash_base": m.FlashBase,
		"sha256": m.SHA256, "second_read_sha256": m.SecondRead, "identical_reads": true, "read_seconds": m.ReadSeconds,
		"probe_serial": m.ProbeSerial,
		"note":         "main flash bank only; local artifact: it holds the complete firmware, never commit it",
	}
	for k, v := range info {
		res[k] = v
	}
	return g.mark(res), nil
}

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func safeName(s string) string { return reUnsafe.ReplaceAllString(s, "_") }

func writeBackup(path string, data []byte, m manifest) error {
	if old, err := os.ReadFile(path); err == nil && !bytes.Equal(old, data) {
		return status.Errorf(codes.AlreadyExists, "backup %s exists with other content: refusing to overwrite it", path)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	mb, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(path+".json", append(mb, '\n'), 0o600)
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
		return nil, nil, fmt.Errorf("manifest format %q is not %s", m.Format, backupFormat)
	}
	if sha(b) != m.SHA256 || len(b) != m.SizeBytes {
		return nil, nil, fmt.Errorf("backup %s does not match the size or SHA-256 in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead {
		return nil, nil, errors.New("the manifest does not record two identical reads")
	}
	return b, &m, nil
}

// backupFor loads the backup named by args[key] and checks it is of this
// device's main flash.
func backupFor(args map[string]any, key string, g *geom) (string, []byte, *manifest, error) {
	path, err := strArg(args, key)
	if err != nil {
		return "", nil, nil, err
	}
	if path == "" {
		return "", nil, nil, denied("refusing to modify flash without a verified full backup: take one with FlashProgramming.backup and pass %s=<path of the .bin>", key)
	}
	b, m, err := loadBackup(path)
	if err != nil {
		return path, nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	if !strings.EqualFold(m.Device, g.t.Device) || m.FlashBase != hexAddr(g.base) || uint64(len(b)) != g.size {
		return path, nil, nil, status.Errorf(codes.FailedPrecondition, "backup %s is of another flash (%s at %s, %d bytes) than the target (%s at %s, %d bytes)",
			path, m.Device, m.FlashBase, len(b), g.t.Device, hexAddr(g.base), g.size)
	}
	return path, b, m, nil
}

// gate requires that every affected sector still holds what the backup holds
// (or is erased, which is what an earlier erase covered by the backup leaves).
func gate(path string, b []byte, m *manifest, g *geom, lo uint64, cur []byte) (map[string]any, error) {
	for off := uint64(0); off < uint64(len(cur)); off += g.sector {
		end := min(off+g.sector, uint64(len(cur)))
		sec := cur[off:end]
		bo := lo + off - g.base
		if !bytes.Equal(sec, b[bo:bo+(end-off)]) && !isBlank(sec) {
			return nil, status.Errorf(codes.FailedPrecondition, "the backup %s no longer matches the sector at %s: the flash changed after the backup, take a new backup", path, hexAddr(lo+off))
		}
	}
	return map[string]any{"path": path, "sha256": m.SHA256, "affected_sectors_match_flash": true}, nil
}

// ---- program / erase / restore ----

// sectorRuns returns the runs of consecutive sectors in which want differs
// from cur, as writes of want.
func sectorRuns(lo, sector uint64, cur, want []byte) []write {
	var out []write
	for off := uint64(0); off < uint64(len(cur)); off += sector {
		end := off + sector
		if bytes.Equal(cur[off:end], want[off:end]) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].addr+uint64(len(out[n-1].data)) == lo+off {
			out[n-1].data = want[out[n-1].addr-lo : end]
			continue
		}
		out = append(out, write{lo + off, want[off:end]})
	}
	return out
}

func needsErase(cur, want []byte) bool {
	for i := range cur {
		if want[i]&^cur[i] != 0 {
			return true
		}
	}
	return false
}

func (p *Plugin) program(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	if fl, _ := args["flash_loader"].(string); fl != "" {
		if fl != FlashLoaderPHY62xx {
			return nil, argErr("unknown flash_loader %q (supported: %s)", fl, FlashLoaderPHY62xx)
		}
		if err := p.needFlash(id, false); err != nil {
			return nil, err
		}
		if err := tokenIs(args, "confirm", tokProgram, "programming flash"); err != nil {
			return nil, err
		}
		path, err := strArg(args, "path")
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(filepath.Ext(path), ".bin") {
			return nil, argErr("flash_loader %s takes a .bin file", fl)
		}
		if _, err := readImageFile(path); err != nil {
			return nil, err
		}
		t, err := p.needTarget(ctx, args)
		if err != nil {
			return nil, err
		}
		return p.programPHY62xx(ctx, id, t, path, args)
	}
	if err := p.needFlash(id, false); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokProgram, "programming flash"); err != nil {
		return nil, err
	}
	path, err := strArg(args, "path")
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".bin") && args["address"] == nil {
		return nil, argErr(".bin files need an address (e.g. \"0x0\")")
	}
	img, err := readImage(path, args, 0)
	if err != nil {
		return nil, err
	}
	mode, err := resetMode(args, "run")
	if err != nil {
		return nil, err
	}
	g, err := p.flashGeom(ctx, id, args, true)
	if err != nil {
		return nil, err
	}
	if err := g.needSectors(); err != nil {
		return nil, err
	}
	first, last := img[0], img[len(img)-1]
	end := last.addr + uint64(len(last.data))
	if !g.inMain(first.addr, end-first.addr) {
		return nil, status.Errorf(codes.OutOfRange, "image %s [%s, %s) is outside the main flash %s+0x%X of %s", path, hexAddr(first.addr), hexAddr(end), hexAddr(g.base), g.size, g.t.Device)
	}
	lo, n := g.cover(first.addr, end-first.addr)
	cur, _, err := p.readSpans(ctx, id, g.t, []span{{lo, n}}, true)
	if err != nil {
		return nil, err
	}
	want := append([]byte(nil), cur[0]...)
	var imgBytes []byte
	for _, w := range img {
		copy(want[w.addr-lo:], w.data)
		imgBytes = append(imgBytes, w.data...)
	}
	if needsErase(cur[0], want) && !p.perm(id, "allow_erase") {
		return nil, denied("this image needs sectors erased (a bit must go from 0 to 1): that needs allow_erase=true in the device configuration")
	}
	bpath, b, m, err := backupFor(args, "backup", g)
	if err != nil {
		return nil, err
	}
	bk, err := gate(bpath, b, m, g, lo, cur[0])
	if err != nil {
		return nil, err
	}
	runs := sectorRuns(lo, g.sector, cur[0], want)
	res := map[string]any{"programmed": path, "address": hexAddr(first.addr), "length": float64(len(imgBytes)), "sha256": sha(imgBytes),
		"sectors_covered": float64(n / g.sector), "backup": bk, "probe_serial": NormalizeSerial(id),
		"basis": "the covering sectors are read, the image is merged over them (bytes outside the image are preserved), only changed sectors are written with SEGGER's flash loader (loadbin), and the covering sectors are read back and compared"}
	if len(runs) == 0 {
		res["unchanged"], res["verified"] = true, true
		return g.mark(res), nil
	}
	wasHalted, err := p.state(ctx, id, g.t)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	rb, out, err := p.writeSession(ctx, id, g.t, wasHalted, runs, nil, lo, n, mode)
	if err != nil {
		return nil, err
	}
	if d := firstDiff(want, rb); d >= 0 {
		return nil, status.Errorf(codes.DataLoss, "verify FAILED at %s after programming (want 0x%02x, flash 0x%02x): restore the backup %s", hexAddr(lo+uint64(d)), want[d], rb[d], bpath)
	}
	var changed uint64
	for _, r := range runs {
		changed += uint64(len(r.data))
	}
	res["verified"], res["sectors_written"], res["elapsed_ms"] = true, float64(changed/g.sector), float64(time.Since(t0).Milliseconds())
	res["was_halted"] = wasHalted
	finalState(res, out, mode)
	return g.mark(res), nil
}

func (p *Plugin) flashErase(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	mode, err := strArg(args, "mode")
	if err != nil {
		return nil, err
	}
	if mode != "region" && mode != "chip" {
		return nil, argErr("mode %q: want region or chip (there is no implicit mode)", mode)
	}
	if err := p.needFlash(id, true); err != nil {
		return nil, err
	}
	if mode == "chip" {
		if !p.perm(id, "allow_chip_erase") {
			return nil, denied("erasing the whole flash needs allow_chip_erase=true in the device configuration")
		}
		if err := tokenIs(args, "confirm", tokEraseChip, "whole-flash erase"); err != nil {
			return nil, err
		}
	} else if err := tokenIs(args, "confirm", tokEraseReg, "erasing a flash region"); err != nil {
		return nil, err
	}
	rmode, err := resetMode(args, "none")
	if err != nil {
		return nil, err
	}
	g, err := p.flashGeom(ctx, id, args, true)
	if err != nil {
		return nil, err
	}
	if err := g.needSectors(); err != nil {
		return nil, err
	}
	lo, n := g.base, g.size
	if mode == "region" {
		addr, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr("address: %v", err)
		}
		l, err := toInt(args["length"])
		if err != nil || l < 1 {
			return nil, argErr("length %v: want at least 1 byte", args["length"])
		}
		if !g.inMain(addr, uint64(l)) {
			return nil, status.Errorf(codes.OutOfRange, "erase [%s, +%d) is outside the main flash %s+0x%X", hexAddr(addr), l, hexAddr(g.base), g.size)
		}
		if cl, cn := g.cover(addr, uint64(l)); cl != addr || cn != uint64(l) {
			return nil, argErr("[%s, +%d) does not start and end on %d-byte sector boundaries (the covering sectors are %s..%s)", hexAddr(addr), l, g.sector, hexAddr(cl), hexAddr(cl+cn))
		}
		lo, n = addr, uint64(l)
	}
	cur, _, err := p.readSpans(ctx, id, g.t, []span{{lo, n}}, true)
	if err != nil {
		return nil, err
	}
	bpath, b, m, err := backupFor(args, "backup", g)
	if err != nil {
		return nil, err
	}
	bk, err := gate(bpath, b, m, g, lo, cur[0])
	if err != nil {
		return nil, err
	}
	wasHalted, err := p.state(ctx, id, g.t)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	rb, out, err := p.writeSession(ctx, id, g.t, wasHalted, nil, []span{{lo, n}}, lo, n, rmode)
	if err != nil {
		return nil, err
	}
	if !isBlank(rb) {
		i := bytes.IndexFunc(rb, func(r rune) bool { return r != 0xFF })
		return nil, status.Errorf(codes.DataLoss, "erase FAILED: flash is not blank (first at about %s); restore the backup %s", hexAddr(lo+uint64(max(i, 0))), bpath)
	}
	res := map[string]any{"mode": mode, "address": hexAddr(lo), "length": float64(n), "sectors_erased": float64(n / g.sector), "verified_erased": true,
		"backup": bk, "was_halted": wasHalted, "elapsed_ms": float64(time.Since(t0).Milliseconds()), "probe_serial": NormalizeSerial(id),
		"note": "mode=chip erases the main flash bank only (other banks such as user data pages are left alone); every byte was read back as 0xFF"}
	finalState(res, out, rmode)
	return g.mark(res), nil
}

func (p *Plugin) flashRestore(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	if err := p.needFlash(id, true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokRestore, "restoring flash"); err != nil {
		return nil, err
	}
	mode, err := resetMode(args, "run")
	if err != nil {
		return nil, err
	}
	g, err := p.flashGeom(ctx, id, args, true)
	if err != nil {
		return nil, err
	}
	if err := g.needSectors(); err != nil {
		return nil, err
	}
	path, b, m, err := backupFor(args, "path", g)
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			return nil, argErr("path: the backup .bin is required")
		}
		return nil, err
	}
	cur, _, err := p.readSpans(ctx, id, g.t, []span{{g.base, g.size}}, true)
	if err != nil {
		return nil, err
	}
	runs := sectorRuns(g.base, g.sector, cur[0], b)
	res := map[string]any{"restored": true, "path": path, "sha256": m.SHA256, "probe_serial": NormalizeSerial(id)}
	if len(runs) == 0 {
		res["unchanged"], res["whole_flash_matches_backup"] = true, true
		return g.mark(res), nil
	}
	wasHalted, err := p.state(ctx, id, g.t)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	rb, out, err := p.writeSession(ctx, id, g.t, wasHalted, runs, nil, g.base, g.size, mode)
	if err != nil {
		return nil, err
	}
	if sha(rb) != m.SHA256 {
		return nil, status.Errorf(codes.DataLoss, "restore FAILED: whole-flash SHA-256 %s differs from the backup %s", sha(rb), m.SHA256)
	}
	var changed uint64
	for _, r := range runs {
		changed += uint64(len(r.data))
	}
	res["whole_flash_matches_backup"], res["sectors_written"] = true, float64(changed/g.sector)
	res["elapsed_ms"], res["was_halted"] = float64(time.Since(t0).Milliseconds()), wasHalted
	finalState(res, out, mode)
	return g.mark(res), nil
}
