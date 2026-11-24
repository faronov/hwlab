package espromplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/esprom/rom"
)

// Confirmation tokens (args.confirm / args.confirm_protected), the same as
// the espusbjtag plugin's.
const (
	tokProgram   = "program-flash"
	tokRestore   = "restore-flash"
	tokEraseReg  = "erase-flash-region"
	tokEraseApp  = "erase-app-partition"
	tokEraseChip = "erase-entire-chip"
	tokProtected = "modify-protected-region"

	maxInline    = 4096
	maxImageFile = 64 << 20
)

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const optProps = `"baud":{"type":"number","description":"link rate after the stub runs (CHANGE_BAUDRATE): default 921600 on a USB-UART bridge, 460800 behind the S3 bridge, none on USB-Serial/JTAG; 0 stays at 115200"},` +
	`"timeout_ms":{"type":"number","description":"bound of one command, default 3000"},` +
	`"entry":{"type":"string","enum":["classic","usj","none"],"description":"how the chip is reset: classic (RTS->EN, DTR->strap: USB-UART bridges and the S3 bridge), usj (the chip's USB-Serial/JTAG port), none (already in download mode; lines untouched)"},` +
	`"stub":{"type":"boolean","description":"upload the flasher stub (default true; false: ROM only, identify and verify)"},` +
	`"allow_other_chip":{"type":"boolean","description":"try even though the tibsl plugin found a TI chip behind this port"},` +
	`"allow_flash":{"type":"boolean","description":"DANGEROUS config: permit flash program and restore"},` +
	`"allow_erase":{"type":"boolean","description":"DANGEROUS config: permit FlashProgramming.erase (with allow_flash)"},` +
	`"allow_chip_erase":{"type":"boolean","description":"DANGEROUS config: permit whole-chip erase"},` +
	`"allow_reset":{"type":"boolean","description":"DANGEROUS config: permit FlashProgramming.reset and reset=run after program/restore"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	T, F := pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + optProps + props + `}}` }
	req := func(r, props string) string {
		return `{"type":"object","required":[` + r + `],"properties":{` + optProps + props + `}}`
	}
	backup := `,"backup":{"type":"string","description":"a FlashProgramming.backup .bin of this chip (required)"}`
	prot := `,"confirm_protected":{"type":"string","enum":["` + tokProtected + `"],"description":"needed when the range touches the bootloader slot, the partition table or a non-app partition (nvs, phy_init, otadata, coredump, custom data), or anything without a valid partition table"}`
	reset := `,"reset":{"type":"string","enum":["run","none"],"description":"after success: run boots the application (needs allow_reset), none (default) stays in the loader"}`
	// The x-hwlab-dry-run mark lets the core run a dry run without
	// confirmation (ADR 0004): a dry run here only reads and plans.
	dry := `,"dry_run":{"type":"boolean","x-hwlab-dry-run":true,"description":"plan only (reads and chip MD5s, nothing is written): needs no confirmation, token, permission or backup"}`
	return &pluginv1.PluginInfo{
		Id: "esprom", Name: "Espressif serial bootloader (ROM download mode + flasher stub)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities:        deviceCaps(),
		DangerousConfigKeys: append([]string(nil), permKeys...),
		Methods: []*pluginv1.MethodSpec{
			spec(T, safe, "identify", "reset the chip into ROM download mode (per entry), SYNC, detect the chip (ROM word at 0x40001000, GET_SECURITY_INFO chip id), upload the flasher stub, switch the baud rate, attach the flash and report chip, revision, package, features, MAC (EUI-64), eFuse security state, flash JEDEC id and size, and the ROM boot message. Read only (eFuses and registers are only read); the chip stays in the loader until FlashProgramming.reset", obj("")),
			spec(F, safe, "info", "identify plus the partition table at 0x8000, protected ranges, application descriptors (project, version, IDF, date) of every app partition, the OTA selection (otadata) and what modification needs. Read only", obj("")),
			spec(F, safe, "read", "READ_FLASH through the stub (each 256 KiB checked by the stub's MD5): address (default 0), length (default to the end); max 4096 bytes inline, or any length to a new file with path",
				obj(`,"address":{},"length":{"type":"number"},"path":{"type":"string"}`)),
			spec(F, safe, "verify", "compare flash with a file (path) or hex data at address by SPI_FLASH_MD5 (the chip hashes, nothing is written); a mismatch reports the first differing 4 KiB sector",
				obj(`,"address":{},"path":{"type":"string"},"data":{"type":"string"}`)),
			spec(F, safe, "backup", "read the WHOLE flash twice (READ_FLASH, stub MD5 per 256 KiB), require identical SHA-256 and the chip's SPI_FLASH_MD5 of the whole flash to equal the host MD5, store it 0600 with a manifest bound to the MAC, JEDEC id and size, the decoded partition table and app descriptors; default dir $HWLAB_HOME/artifacts/esprom-flash. Never writes flash",
				obj(`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "erase flash: mode=region (address,length, 4 KiB aligned; confirm=\""+tokEraseReg+"\"), app (an app partition by label; \""+tokEraseApp+"\") or chip (\""+tokEraseChip+"\", needs allow_chip_erase). All need allow_flash+allow_erase and backup=; protected ranges need confirm_protected. The result is checked blank by SPI_FLASH_MD5",
				req(`"mode","confirm","backup"`, `,"mode":{"type":"string","enum":["region","app","chip"]},"address":{},"length":{"type":"number"},"partition":{"type":"string"},"confirm":{"type":"string","enum":["`+tokEraseReg+`","`+tokEraseApp+`","`+tokEraseChip+`"]}`+backup+prot)),
			spec(F, confirm, "program", "program an image (path, or hex data up to 4096 bytes) at address: ESP images (0xE9) are parsed first (truncated/corrupt refused; chip id must match; must lie inside one region: the bootloader slot or one partition); anything else needs raw=true. Only 4 KiB sectors whose SPI_FLASH_MD5 differs are erased and rewritten, edge bytes are preserved, each run and the image are verified by MD5. Needs allow_flash, confirm=\""+tokProgram+"\", backup= (confirm_protected for protected ranges); dry_run=true plans without writing or any gate",
				obj(`,"address":{},"path":{"type":"string"},"data":{"type":"string"},"raw":{"type":"boolean"},"confirm":{"type":"string","enum":["`+tokProgram+`"]}`+dry+backup+prot+reset)),
			spec(F, confirm, "restore", "rewrite every 4 KiB sector that differs from a verified backup of this chip (path or backup) and require the chip's MD5 of the restored range to equal the backup's: the whole flash, one partition of the backup's table (partition=label) or address+length (4 KiB aligned) - a region restore needs no fresh backup after a program. Needs allow_flash, confirm=\""+tokRestore+"\" and confirm_protected when a differing sector is protected in the backup's or the chip's layout. Restoring an old nvs rolls back keys, counters and network state: never restore onto a device that was used since",
				obj(`,"path":{"type":"string"},"partition":{"type":"string"},"address":{},"length":{"type":"number"},"confirm":{"type":"string","enum":["`+tokRestore+`"]}`+dry+backup+prot+reset)),
			spec(F, confirm, "reset", "drive the reset lines only (no ROM needed, works on a hung application): mode=run (default) resets the chip into its application (classic: RTS pulse with DTR released; usj: the USB-Serial/JTAG chip reset, TRM table 33.4-4), mode=bootloader resets it into ROM download mode and checks it answers SYNC. console_ms captures the application's console at 115200 after a run reset. Needs allow_reset",
				obj(`,"mode":{"type":"string","enum":["run","bootloader"]},"console_ms":{"type":"number","description":"0..10000"}`)),
		},
	}
}

var methods = map[string]bool{}

func init() {
	for _, m := range (*Plugin)(nil).Info().Methods {
		methods[m.Capability+"."+m.Name] = true
	}
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 0, 32)
		if err != nil {
			return 0, false, fmt.Errorf("%s %q: want a number", key, x)
		}
		f = float64(n)
	default:
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s must be finite", key)
	}
	return f, true, nil
}

func optBool(a map[string]any, key string) (bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return false, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		if b, err := strconv.ParseBool(x); err == nil {
			return b, nil
		}
	}
	return false, fmt.Errorf("%s: want true or false, got %v", key, v)
}

func u32Arg(a map[string]any, key string) (uint32, bool, error) {
	f, ok, err := optNumber(a, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	if f < 0 || f != math.Trunc(f) || f > math.MaxUint32 {
		return 0, false, fmt.Errorf("%s %v: want an integer 0..0xFFFFFFFF", key, f)
	}
	return uint32(f), true, nil
}

func strArg(a map[string]any, key string) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", argErr(fmt.Errorf("%s: want a string, got %v", key, v))
	}
	return s, nil
}

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, format, a...)
}

func tokenIs(a map[string]any, key, want, what string) error {
	if s, _ := a[key].(string); s != want {
		return denied("%s needs %s=%q", what, key, want)
	}
	return nil
}

func readImage(a map[string]any) ([]byte, string, error) {
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
		if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxImageFile {
			return nil, "", fmt.Errorf("%s is %d bytes; want a regular file of 1..%d bytes", path, st.Size(), maxImageFile)
		}
		b, err := os.ReadFile(path)
		return b, path, err
	case hasData:
		s, _ := a["data"].(string)
		s = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), "0x")
		b, err := hex.DecodeString(s)
		if err != nil || len(b) == 0 || len(b) > maxInline {
			return nil, "", fmt.Errorf("data: want 1..%d bytes of hex", maxInline)
		}
		return b, "", nil
	}
	return nil, "", errors.New("path or data is required")
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08X", v) }

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	if !methods[method] {
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, release, err := p.lease(req.GetDeviceId(), args)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil && method != "FlashProgramming.reset" {
		if err := d.reopen(ctx); err != nil {
			return nil, err
		}
	}
	var res map[string]any
	switch method {
	case "TargetIdentify.identify":
		res, err = d.withROM(ctx, func() (map[string]any, error) { return d.identify(), nil })
	case "FlashProgramming.info":
		res, err = d.withROM(ctx, d.flashInfo)
	case "FlashProgramming.read":
		res, err = d.flashRead(ctx, args)
	case "FlashProgramming.verify":
		res, err = d.flashVerify(ctx, args)
	case "FlashProgramming.backup":
		res, err = d.flashBackup(ctx, args)
	case "FlashProgramming.erase":
		res, err = d.flashErase(ctx, args)
	case "FlashProgramming.program":
		res, err = d.flashProgram(ctx, args)
	case "FlashProgramming.restore":
		res, err = d.flashRestore(ctx, args)
	case "FlashProgramming.reset":
		res, err = d.reset(ctx, args)
	}
	if err != nil {
		if errors.Is(err, rom.ErrTimeout) || errors.Is(err, rom.ErrDigest) || isLinkGone(err) {
			d.forget()
		}
		return nil, wrap(err)
	}
	if d.e.Sim != nil {
		res["synthetic"] = true
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

// withROM runs fn with the loader session up.
func (d *device) withROM(ctx context.Context, fn func() (map[string]any, error)) (map[string]any, error) {
	if err := d.ensureROM(ctx); err != nil {
		return nil, err
	}
	d.cl.SetContext(ctx)
	defer d.cl.SetContext(context.Background())
	return fn()
}

func wrap(err error) error {
	var se *rom.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, rom.ErrTimeout):
		// Not Unavailable: the core reports that code as a crashed plugin.
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, rom.ErrDigest):
		return status.Error(codes.DataLoss, err.Error())
	case errors.As(err, &se):
		return status.Error(codes.Aborted, err.Error())
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func (d *device) identify() map[string]any {
	res := d.chipFields()
	res["note"] = "the chip stays in the loader until FlashProgramming.reset (or the next reset)"
	return res
}

func (d *device) chipFields() map[string]any {
	c := d.det.Chip
	res := map[string]any{
		"chip": c.Name, "chip_basis": d.det.Basis, "rom_magic": hex32(d.det.Magic), "image_chip_id": fmt.Sprintf("0x%04X", c.ImageChipID),
		"transport": string(d.e.Kind), "entry": string(d.o.entry), "port": d.e.Port, "sync_attempts": float64(d.syncs),
		"loader": map[bool]string{true: "stub", false: "rom"}[d.cl.Stub()], "stub": d.stubNote, "baud": float64(d.linkBaud),
		"interface": "Espressif serial bootloader",
	}
	if d.o.entry.Wired() {
		res["lines_atomic"] = d.atomic
	}
	if d.longHold {
		res["strap_hold"] = "long (" + rom.StrapHoldLong.String() + ")"
	}
	if d.bootMsg != "" {
		res["rom_boot_message"] = d.bootMsg
	}
	if s := d.det.Security; s != nil && s.HasChipID {
		res["security_chip_id"], res["eco_version"] = float64(s.ChipID), float64(s.ECO)
	}
	if f := d.facts; f != nil {
		res["mac"], res["revision"], res["package"], res["features"] = f.MACString(), f.Revision(), f.Package, toAny(f.Features)
		res["security"], res["flash_encrypted"], res["secure_boot"], res["efuse_basis"] = f.Security, f.FlashEncrypted, f.SecureBoot, f.Basis
		if e := f.EUI64(); e != "" {
			res["eui64"] = e
		}
		if f.MACCRC != nil {
			res["mac_crc"] = fmt.Sprintf("0x%02X", *f.MACCRC)
		}
	} else if d.factsErr != nil {
		res["facts_error"] = d.factsErr.Error()
	}
	if d.jedec != 0 {
		res["flash_jedec"], res["flash_manufacturer"] = rom.JEDECString(d.jedec), rom.Manufacturer(d.jedec)
		res["flash_bytes"], res["flash_mib"] = float64(d.size), float64(d.size)/(1<<20)
	}
	if err := d.writable(); err != nil {
		res["writable"], res["read_only_reason"] = false, status.Convert(err).Message()
	} else {
		res["writable"] = d.cl.Stub()
		if !d.cl.Stub() {
			res["read_only_reason"] = d.stubNote
		}
	}
	return res
}

func (d *device) reset(ctx context.Context, a map[string]any) (map[string]any, error) {
	mode, err := strArg(a, "mode")
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = "run"
	}
	if mode != "run" && mode != "bootloader" {
		return nil, argErr(fmt.Errorf("mode %q: want run or bootloader", mode))
	}
	ms, ok, err := optNumber(a, "console_ms")
	if err != nil || (ok && (ms < 0 || ms > 10000)) {
		return nil, argErr(fmt.Errorf("console_ms: want 0..10000"))
	}
	if !d.o.perm.reset {
		return nil, denied("FlashProgramming.reset needs allow_reset=true in the device configuration")
	}
	if err := d.otherChipGuard(); err != nil {
		return nil, err
	}
	rec, err := d.p.journalStart(d, "reset", map[string]any{"mode": mode})
	if err != nil {
		return nil, err
	}
	res, err := d.resetTo(ctx, mode, int(ms))
	if jerr := d.p.journalEnd(rec, err, nil); jerr != nil && err == nil {
		res["journal_error"] = jerr.Error()
	}
	return res, err
}
