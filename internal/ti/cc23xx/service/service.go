// Package service is the probe-independent CC23xx service shared by the
// XDS110 and CMSIS-DAP plugins: identification, CCFG decoding, SACI control
// and the FlashProgramming contract (info, read, verify, backup, erase,
// program, restore, ccfg_program) on top of internal/ti/cc23xx.
//
// Every modifying call needs device-level permissions (set when the device is
// opened), a confirmation token and a verified full backup. SACI has no
// sector erase, so program, restore and ccfg_program are whole-chip
// operations that re-create the unchanged parts from the backup.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Confirmation tokens.
const (
	TokProgram   = "program-flash"
	TokRestore   = "restore-flash"
	TokEraseChip = "erase-entire-chip"
	TokCCFG      = "program-ccfg"
	// TokSecurity acknowledges a CCFG change that alters boot, debug or
	// programming permissions.
	TokSecurity = "accept-ccfg-security-change"
	TokSACI     = "enter-saci"
)

const (
	maxInline = 4096
	maxFile   = 16 << 20
	// FlashTimeout bounds one flash call (a full backup reads twice).
	FlashTimeout = 10 * time.Minute
)

// Perms are the device-level permissions. They are set when the device is
// opened; a single call cannot grant them.
type Perms struct {
	Flash, Erase, ChipErase, CCFGWrite bool
}

// Env is what one call needs.
type Env struct {
	S    *cc23xx.Session
	Perm Perms
	// Dir is the artifact directory for backups; empty selects
	// $HWLAB_HOME/artifacts/cc23xx-flash.
	Dir string
	// Serial is the probe serial, recorded in the local manifest only.
	Serial string
}

func (e *Env) dir() string {
	if e.Dir != "" {
		return e.Dir
	}
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "cc23xx-flash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "cc23xx-flash")
}

// Handles reports whether the service implements capability.method.
func Handles(capability, method string) bool {
	switch capability + "." + method {
	case "FlashProgramming.info", "FlashProgramming.read", "FlashProgramming.verify", "FlashProgramming.backup",
		"FlashProgramming.erase", "FlashProgramming.program", "FlashProgramming.restore", "FlashProgramming.ccfg_info",
		"FlashProgramming.ccfg_program", "DebugProbe.cc23xx_info", "DebugProbe.saci_status", "DebugProbe.saci_enter", "DebugProbe.saci_exit":
		return true
	}
	return false
}

// Specs returns the method specifications; devProps is the plugin's common
// per-call property list (clock_hz, ap, timeout_ms).
func Specs(devProps string) []*pluginv1.MethodSpec {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	F, D := pluginv1.CapFlashProgramming, pluginv1.CapDebugProbe
	sp := func(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
		return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
	}
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	req := func(required, props string) string {
		return `{"type":"object","required":[` + required + `],"properties":{` + props + `}}`
	}
	const resetProp = `"reset":{"type":"string","enum":["run","halt","none"],"description":"after success: run the application (default), stop at its reset handler, or reset into SACI and stay"}`
	const backupProp = `"backup":{"type":"string","description":"path of a verified full backup (.bin written by FlashProgramming.backup)"}`
	return []*pluginv1.MethodSpec{
		sp(D, safe, "cc23xx_info", "CC23xx identity from documented registers (device id, manufacturer, raw part id, flash size, banks), the decoded CCFG (no secrets), the boot state (BOOTSTA) and whether SACI or the AHB-AP is active; works while the device waits in SACI. No marketing name is derived", obj(devProps)),
		sp(D, safe, "saci_status", "boot state: BOOTSTA, whether SACI is active and the AHB-AP is open", obj(devProps)),
		sp(D, confirm, "saci_enter", "reset the device into SACI (the boot ROM service interface): via=nreset (probe pin, default when wired) or sysreset. Core access is closed until SACI is left. The application stops running; confirm=\"enter-saci\"", obj(devProps+`,"via":{"enum":["nreset","sysreset","saci"]},"confirm":{"type":"string"}`)),
		sp(D, confirm, "saci_exit", "leave SACI: mode=run (default), halt (stop in the boot ROM wait loop) or reset-halt (stop at the application reset handler). Refused with CCFG_INVALID after an erase until a CCFG is programmed and the device reset; a password protected target returns TARGET_SECURED", obj(devProps+`,"mode":{"enum":["run","halt","reset-halt"]}`)),
		sp(F, safe, "info", "flash identity and geometry from VIMS (MAIN size, 2 KiB sectors, banks, 16-byte program granule), the CCFG decode and the SACI permissions the CCFG grants; reads only", obj(devProps)),
		sp(F, safe, "ccfg_info", "decode the CCFG sector (CRC validity, permissions, debug authorization, application vector); no password, hash or user-record bytes are returned", obj(devProps)),
		sp(F, safe, "read", "read MAIN flash through the AHB-AP: max 4096 bytes inline, or up to 16 MiB to a new file with path (SHA-256 returned)",
			req(`"address","length"`, devProps+`,"address":{},"length":{"type":"number"},"path":{"type":"string"},"overwrite":{"type":"boolean"}`)),
		sp(F, safe, "verify", "compare an image file with flash at address through the AHB-AP; any difference is an error",
			req(`"path","address"`, devProps+`,"path":{"type":"string"},"address":{}`)),
		sp(F, confirm, "backup", "read the whole MAIN flash twice, require identical SHA-256, capture the CCFG, and store both (mode 0600) with a manifest in dir (default $HWLAB_HOME/artifacts/cc23xx-flash); never modifies the device. The artifact holds the complete firmware and the CCFG user record: keep it local",
			obj(devProps+`,"dir":{"type":"string"}`)),
		sp(F, confirm, "erase", "SACI chip erase (there is no sector erase): needs allow_flash+allow_erase+allow_chip_erase, confirm=\"erase-entire-chip\" and backup=<verified full backup>. Erases MAIN and the CCFG; the device then refuses to boot until a CCFG is programmed (restore does that). retain (default 0) is the SACI retain mask of sectors kept by the first erase",
			req(`"confirm","backup"`, devProps+`,"confirm":{"type":"string"},`+backupProp+`,"retain":{"type":"number","description":"sector retain mask, default 0 (nothing kept)"}`)),
		sp(F, confirm, "program", "program an image at address: the whole chip is erased and re-created from the backup with the image merged over it, programmed sector by sector, verified with SACI CRC32 and the same CCFG is programmed and verified; needs allow_flash+allow_erase+allow_chip_erase, confirm=\"program-flash\", backup=<verified full backup that equals the flash now, or a blank device>. The CCFG is never changed",
			req(`"path","address","confirm","backup"`, devProps+`,"path":{"type":"string"},"address":{},"confirm":{"type":"string"},`+backupProp+`,`+resetProp)),
		sp(F, confirm, "restore", "restore a verified full backup (MAIN + CCFG): chip erase, program, SACI verify of every sector and of the CCFG, reset; needs allow_flash+allow_erase+allow_chip_erase and confirm=\"restore-flash\". Works from an erased device waiting in SACI. A device that already matches the backup is left untouched",
			req(`"path","confirm"`, devProps+`,"path":{"type":"string"},"confirm":{"type":"string"},`+resetProp)),
		sp(F, confirm, "ccfg_program", "replace the CCFG with an image (2048 bytes with valid CRC32s): DANGEROUS. Needs allow_ccfg_write+allow_flash+allow_erase+allow_chip_erase, confirm=\"program-ccfg\", backup, and security_ack=\"accept-ccfg-security-change\" when the diff changes permissions, debug authorization, boot vectors or undocumented fields. Refuses an image that forbids chip erase. dry_run=true only returns the diff",
			req(`"path","backup"`, devProps+`,"path":{"type":"string"},"confirm":{"type":"string"},`+backupProp+`,"security_ack":{"type":"string"},"dry_run":{"type":"boolean"},`+resetProp)),
	}
}

// ---- errors ----

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, cc23xx.CodePermissionDenied+": "+format, a...)
}

func argErr(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}

// Err maps a CC23xx error to a gRPC status whose message starts with the
// stable error code.
func Err(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch cc23xx.CodeOf(err) {
	case "":
		return err
	case cc23xx.CodePermissionDenied, cc23xx.CodeTargetSecured:
		return status.Error(codes.PermissionDenied, err.Error())
	case cc23xx.CodeVerifyFailed:
		return status.Error(codes.DataLoss, err.Error())
	case cc23xx.CodeSACITimeout:
		return status.Error(codes.DeadlineExceeded, err.Error())
	case cc23xx.CodeSwdLost:
		return status.Error(codes.Unavailable, err.Error())
	case cc23xx.CodeUnsafeRequest:
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

// ---- args ----

func strArg(a map[string]any, k string) (string, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", argErr("%s: want a string", k)
	}
	return s, nil
}

func boolArg(a map[string]any, k string) (bool, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, argErr("%s: want true or false", k)
	}
	return b, nil
}

func u32Arg(a map[string]any, k string) (uint32, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return 0, argErr("%s is required", k)
	}
	switch x := v.(type) {
	case float64:
		if x < 0 || x > 0xFFFFFFFF || x != float64(uint32(x)) {
			return 0, argErr("%s %v: want an integer 0..4294967295", k, x)
		}
		return uint32(x), nil
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 0, 32)
		if err != nil {
			return 0, argErr("%s %q: want a 32-bit number such as 0x00001000", k, x)
		}
		return uint32(n), nil
	}
	return 0, argErr("%s: want a number or a 0x.. string", k)
}

func tokenIs(a map[string]any, key, want, why string) error {
	got, err := strArg(a, key)
	if err != nil {
		return err
	}
	if got != want {
		return denied("%s: pass %s=%q", why, key, want)
	}
	return nil
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func toAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func toMap(v any) map[string]any {
	m, _ := toAny(v).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// ---- dispatch ----

// Invoke runs one method.
func (e *Env) Invoke(ctx context.Context, capability, method string, args map[string]any) (map[string]any, error) {
	res, err := e.invoke(ctx, capability+"."+method, args)
	return res, Err(err)
}

func (e *Env) invoke(ctx context.Context, m string, a map[string]any) (map[string]any, error) {
	switch m {
	case "DebugProbe.cc23xx_info":
		return e.ccInfo(ctx)
	case "DebugProbe.saci_status":
		return e.saciStatus(ctx)
	case "DebugProbe.saci_enter":
		return e.saciEnter(ctx, a)
	case "DebugProbe.saci_exit":
		return e.saciExit(ctx, a)
	case "FlashProgramming.info":
		return e.flashInfo(ctx)
	case "FlashProgramming.ccfg_info":
		return e.ccfgInfo(ctx)
	case "FlashProgramming.read":
		return e.flashRead(ctx, a)
	case "FlashProgramming.verify":
		return e.flashVerify(ctx, a)
	case "FlashProgramming.backup":
		return e.flashBackup(ctx, a)
	case "FlashProgramming.erase":
		return e.flashErase(ctx, a)
	case "FlashProgramming.program":
		return e.flashProgram(ctx, a)
	case "FlashProgramming.restore":
		return e.flashRestore(ctx, a)
	case "FlashProgramming.ccfg_program":
		return e.ccfgProgram(ctx, a)
	}
	return nil, status.Errorf(codes.Unimplemented, "method %s", m)
}

// ---- identity, SACI ----

func (e *Env) saciStatus(ctx context.Context) (map[string]any, error) {
	st, err := e.S.ReadBootStatus(ctx)
	if err != nil {
		return nil, &cc23xx.Error{Code: cc23xx.CodeSwdLost, Msg: "reading the boot status", Err: err}
	}
	return toMap(st), nil
}

func (e *Env) ccInfo(ctx context.Context) (map[string]any, error) {
	id, err := e.S.Identify(ctx)
	if err != nil {
		return nil, err
	}
	res := toMap(id)
	res["note"] = "the family manual has no part-number table: no marketing name is derived from these registers"
	if id.SACI {
		res["note"] = "SACI is active: only the CFG-AP fields are available; leave SACI (saci_exit) to read the memory-mapped identity"
	}
	return res, nil
}

func (e *Env) saciEnter(ctx context.Context, a map[string]any) (map[string]any, error) {
	if err := tokenIs(a, "confirm", TokSACI, "resetting into SACI stops the application"); err != nil {
		return nil, err
	}
	via, err := strArg(a, "via")
	if err != nil {
		return nil, err
	}
	switch cc23xx.ResetVia(via) {
	case "":
		via = string(cc23xx.ResetPin)
		if e.S.PulseReset == nil {
			via = string(cc23xx.ResetSys)
		}
	case cc23xx.ResetPin, cc23xx.ResetSys, cc23xx.ResetSACI:
	default:
		return nil, argErr("via %q: want nreset, sysreset or saci", via)
	}
	t0 := time.Now()
	st, err := e.S.EnterSACI(ctx, cc23xx.ResetVia(via))
	if err != nil {
		return nil, err
	}
	res := toMap(st)
	res["via"], res["elapsed_ms"] = via, float64(time.Since(t0).Milliseconds())
	return res, nil
}

func (e *Env) saciExit(ctx context.Context, a map[string]any) (map[string]any, error) {
	mode, err := strArg(a, "mode")
	if err != nil {
		return nil, err
	}
	switch mode {
	case "":
		mode = string(cc23xx.ExitRun)
	case "run", "halt", "reset-halt":
	default:
		return nil, argErr("mode %q: want run, halt or reset-halt", mode)
	}
	r, err := e.S.LeaveSACI(ctx, cc23xx.ExitMode(mode))
	if err != nil {
		return nil, err
	}
	return toMap(r), nil
}

// ---- geometry ----

// ahbIdentity requires the AHB-AP: it returns the identity and geometry of a
// device that is not waiting in SACI.
func (e *Env) ahbIdentity(ctx context.Context) (*cc23xx.Identity, cc23xx.Geometry, error) {
	id, err := e.S.Identify(ctx)
	if err != nil {
		return nil, cc23xx.Geometry{}, err
	}
	if id.SACI {
		return id, cc23xx.Geometry{}, &cc23xx.Error{Code: cc23xx.CodeSACIActive, Msg: "the device waits in SACI and its flash is not memory mapped: leave SACI first (saci_exit), or use restore/program which work from SACI"}
	}
	if !id.Boot.AHB {
		return id, cc23xx.Geometry{}, &cc23xx.Error{Code: cc23xx.CodeAHBAPUnavailable, Msg: fmt.Sprintf("the AHB-AP is closed (BOOTSTA 0x%02x %s)", id.Boot.BootSta, id.Boot.State)}
	}
	g, err := id.Geometry()
	return id, g, err
}

func (e *Env) flashInfo(ctx context.Context) (map[string]any, error) {
	id, err := e.S.Identify(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"boot": toMap(id.Boot), "in_saci": id.SACI, "cfg_ap_device_id": hex32(id.CfgAPDeviceID)}
	if id.SACI || !id.Boot.AHB {
		res["note"] = "flash geometry is memory mapped: leave SACI (saci_exit) to read it; restore/program use the backup manifest while in SACI"
		return res, nil
	}
	g, err := id.Geometry()
	if err != nil {
		return nil, err
	}
	res["geometry"] = toMap(g)
	res["device_id"], res["part_id_raw"] = hex32(id.DeviceID), hex32(id.PartID)
	res["erase"] = "chip erase only (SACI has no sector erase); sector retain mask optional"
	res["program"] = "SACI PROG_MAIN_SECTOR, one 2 KiB sector at a time, 16-byte granule; verify by SACI CRC32"
	if id.CCFG != nil {
		res["ccfg"] = toMap(id.CCFG)
		res["saci_permissions"] = map[string]any{
			"chip_erase": id.CCFG.Permissions.AllowChipErase.String(), "flash_program": id.CCFG.Permissions.AllowFlashProgram.String(),
			"flash_verify": id.CCFG.Permissions.AllowFlashVerify.String(),
		}
	} else if id.CCFGErr != "" {
		res["ccfg_error"] = id.CCFGErr
	}
	return res, nil
}

func (e *Env) ccfgInfo(ctx context.Context) (map[string]any, error) {
	if _, _, err := e.ahbIdentity(ctx); err != nil {
		return nil, err
	}
	b, err := e.S.ReadCCFG(ctx)
	if err != nil {
		return nil, err
	}
	info, err := ccfg.Parse(b)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ccfg": toMap(info), "sha256": sha(b), "note": "no password, hash or user-record bytes are returned"}, nil
}

// ---- read / verify ----

func newFile(path string, overwrite bool, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (e *Env) flashRead(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, err := u32Arg(a, "address")
	if err != nil {
		return nil, err
	}
	n, err := u32Arg(a, "length")
	if err != nil {
		return nil, err
	}
	path, err := strArg(a, "path")
	if err != nil {
		return nil, err
	}
	overwrite, err := boolArg(a, "overwrite")
	if err != nil {
		return nil, err
	}
	if n == 0 || n > maxFile {
		return nil, argErr("length %d: want 1..%d", n, maxFile)
	}
	if path == "" && n > maxInline {
		return nil, argErr("length %d: more than %d bytes needs path (a new file)", n, maxInline)
	}
	_, g, err := e.ahbIdentity(ctx)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	b, err := e.S.ReadFlash(ctx, g, addr, int(n))
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(n), "sha256": sha(b), "elapsed_ms": float64(time.Since(t0).Milliseconds())}
	if path != "" {
		if err := newFile(path, overwrite, b); err != nil {
			return nil, argErr("path %s: %v", path, err)
		}
		res["path"] = path
		return res, nil
	}
	res["data"] = hex.EncodeToString(b)
	return res, nil
}

func readImage(a map[string]any) ([]byte, string, error) {
	path, err := strArg(a, "path")
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		return nil, "", argErr("path: the image file is required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", argErr("%v", err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxFile {
		return nil, "", argErr("path %s: want a non-empty regular file up to %d bytes", path, maxFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", argErr("%v", err)
	}
	return b, path, nil
}

func (e *Env) flashVerify(ctx context.Context, a map[string]any) (map[string]any, error) {
	img, path, err := readImage(a)
	if err != nil {
		return nil, err
	}
	addr, err := u32Arg(a, "address")
	if err != nil {
		return nil, err
	}
	_, g, err := e.ahbIdentity(ctx)
	if err != nil {
		return nil, err
	}
	got, err := e.S.ReadFlash(ctx, g, addr, len(img))
	if err != nil {
		return nil, err
	}
	for i := range img {
		if img[i] != got[i] {
			return nil, status.Errorf(codes.DataLoss, "%s: flash differs from %s at %s (image 0x%02x, flash 0x%02x)", cc23xx.CodeVerifyFailed, path, hex32(addr+uint32(i)), img[i], got[i])
		}
	}
	return map[string]any{"verified": true, "address": hex32(addr), "length": float64(len(img)), "sha256": sha(img)}, nil
}

// ---- backup ----

// Manifest describes a backup. It holds no firmware bytes.
type Manifest struct {
	Format        string  `json:"format"`
	CreatedUTC    string  `json:"created_utc"`
	File          string  `json:"file"`
	CCFGFile      string  `json:"ccfg_file"`
	SizeBytes     int     `json:"size_bytes"`
	SHA256        string  `json:"sha256"`
	SecondRead    string  `json:"second_read_sha256"`
	CCFGSHA256    string  `json:"ccfg_sha256"`
	CfgAPDeviceID uint32  `json:"cfg_ap_device_id"`
	DeviceID      uint32  `json:"device_id"`
	PartIDRaw     uint32  `json:"part_id_raw"`
	FlashBytes    uint32  `json:"flash_bytes"`
	Banks         int     `json:"banks"`
	ReadSeconds   float64 `json:"read_seconds"`
	ProbeSerial   string  `json:"probe_serial,omitempty"` // local artifact only
}

const manifestFormat = "hwlab-cc23xx-flash-backup/1"

// Backup is a loaded, checked backup.
type Backup struct {
	Main []byte
	CCFG []byte
	M    Manifest
	Path string
}

// LoadBackup reads a backup and checks its files against the manifest.
func LoadBackup(path string) (*Backup, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mb, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, fmt.Errorf("no manifest %s.json: only backups written by FlashProgramming.backup are accepted", path)
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Format != manifestFormat {
		return nil, fmt.Errorf("manifest format %q is not %s", m.Format, manifestFormat)
	}
	if sha(b) != m.SHA256 || len(b) != m.SizeBytes || uint32(len(b)) != m.FlashBytes {
		return nil, fmt.Errorf("backup %s does not match the SHA-256 or size in its manifest: refusing to use it", path)
	}
	if m.SHA256 != m.SecondRead {
		return nil, errors.New("the manifest does not record two identical reads")
	}
	cb, err := os.ReadFile(path + ".ccfg")
	if err != nil {
		return nil, fmt.Errorf("no CCFG capture %s.ccfg: %w", path, err)
	}
	if sha(cb) != m.CCFGSHA256 {
		return nil, fmt.Errorf("the CCFG capture %s.ccfg does not match the manifest", path)
	}
	if len(b)%cc23xx.SectorSize != 0 || len(cb) != ccfg.Size {
		return nil, errors.New("backup sizes are not whole sectors / a CCFG sector")
	}
	return &Backup{Main: b, CCFG: cb, M: m, Path: path}, nil
}

func (e *Env) flashBackup(ctx context.Context, a map[string]any) (map[string]any, error) {
	dir, err := strArg(a, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = e.dir()
	}
	id, g, err := e.ahbIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if id.CCFG == nil || !id.CCFG.Valid {
		return nil, &cc23xx.Error{Code: cc23xx.CodeCCFGInvalid, Msg: "the CCFG is not valid, so a restore could not program it back: no backup was taken"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	t0 := time.Now()
	first, err := e.S.ReadFlash(ctx, g, g.MainBase, int(g.MainBytes))
	if err != nil {
		return nil, fmt.Errorf("first read: %w", err)
	}
	second, err := e.S.ReadFlash(ctx, g, g.MainBase, int(g.MainBytes))
	if err != nil {
		return nil, fmt.Errorf("second read: %w", err)
	}
	c1, err := e.S.ReadCCFG(ctx)
	if err != nil {
		return nil, err
	}
	c2, err := e.S.ReadCCFG(ctx)
	if err != nil {
		return nil, err
	}
	m := Manifest{Format: manifestFormat, SizeBytes: len(first), SHA256: sha(first), SecondRead: sha(second), CCFGSHA256: sha(c1),
		CfgAPDeviceID: id.CfgAPDeviceID, DeviceID: id.DeviceID, PartIDRaw: id.PartID, FlashBytes: g.MainBytes, Banks: g.Banks,
		ReadSeconds: time.Since(t0).Seconds(), ProbeSerial: e.Serial}
	if m.SHA256 != m.SecondRead || m.CCFGSHA256 != sha(c2) {
		return nil, status.Errorf(codes.DataLoss, "the two full-flash reads differ (%s vs %s): no backup was stored", m.SHA256, m.SecondRead)
	}
	m.File = fmt.Sprintf("cc23xx-flash-%04x-%dK-%s.bin", id.DeviceIDField, g.MainBytes/1024, m.SHA256[:12])
	m.CCFGFile = m.File + ".ccfg"
	final := filepath.Join(dir, m.File)
	if err := os.WriteFile(final, first, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(final+".ccfg", c1, 0o600); err != nil {
		return nil, err
	}
	m.CreatedUTC = time.Now().UTC().Format(time.RFC3339)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(final+".json", append(mb, '\n'), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{
		"path": final, "ccfg_path": final + ".ccfg", "manifest": final + ".json", "size_bytes": float64(len(first)),
		"sha256": m.SHA256, "second_read_sha256": m.SecondRead, "ccfg_sha256": m.CCFGSHA256, "identical_reads": true,
		"read_seconds": m.ReadSeconds,
		"note":         "local artifact: it holds the complete firmware and the CCFG (user record, password hash); never commit or share it",
	}, nil
}

// ---- gates ----

func (e *Env) needFlash() error {
	if !e.Perm.Flash {
		return denied("flash modification needs allow_flash=true in the device configuration")
	}
	if !e.Perm.Erase {
		return denied("SACI has no sector erase: every flash change erases the chip and needs allow_erase=true in the device configuration")
	}
	if !e.Perm.ChipErase {
		return denied("every flash change on a CC23xx is a chip erase and needs allow_chip_erase=true in the device configuration")
	}
	return nil
}

func loadGate(a map[string]any, key string) (*Backup, error) {
	path, err := strArg(a, key)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, denied("refusing to modify flash without a verified full backup: take one with FlashProgramming.backup and pass %s=<path of the .bin>", key)
	}
	b, err := LoadBackup(path)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s: %v", path, err)
	}
	if info, perr := ccfg.Parse(b.CCFG); perr != nil || !info.Valid {
		return nil, status.Errorf(codes.FailedPrecondition, "backup %s holds an invalid CCFG and cannot be restored", path)
	}
	return b, nil
}

// geometryFor returns the geometry to use with a backup. With the AHB-AP open
// it is read from the device and must equal the backup; in SACI it is derived
// from the backup after the CFG-AP device id matched.
func (e *Env) geometryFor(ctx context.Context, b *Backup) (cc23xx.Geometry, error) {
	id, err := e.S.Identify(ctx)
	if err != nil {
		return cc23xx.Geometry{}, err
	}
	if id.CfgAPDeviceID != b.M.CfgAPDeviceID {
		return cc23xx.Geometry{}, status.Errorf(codes.FailedPrecondition, "backup %s is of another device (CFG-AP device id %s, connected %s): refusing", b.Path, hex32(b.M.CfgAPDeviceID), hex32(id.CfgAPDeviceID))
	}
	if id.Boot.AHB && !id.SACI {
		g, err := id.Geometry()
		if err != nil {
			return g, err
		}
		if g.MainBytes != b.M.FlashBytes {
			return g, status.Errorf(codes.FailedPrecondition, "backup %s is %d bytes, the device has %d bytes of flash", b.Path, b.M.FlashBytes, g.MainBytes)
		}
		return g, nil
	}
	return cc23xx.Geometry{MainBase: cc23xx.FlashBase, MainBytes: b.M.FlashBytes, SectorSize: cc23xx.SectorSize,
		Sectors: int(b.M.FlashBytes) / cc23xx.SectorSize, Banks: b.M.Banks, CCFGBase: ccfg.BaseAddr, CCFGBytes: ccfg.Size, Program: 16}, nil
}

// matchBackup tells (inside SACI) whether the device holds the backup, is
// completely blank, or something else.
func matchBackup(ctx context.Context, tx *cc23xx.FlashTxn, g cc23xx.Geometry, b *Backup) (string, error) {
	plan, err := cc23xx.PlanImage(g, g.MainBase, b.Main)
	if err != nil {
		return "", err
	}
	err = tx.Verify(ctx, plan)
	if err == nil {
		if err = tx.VerifyCCFG(ctx, b.CCFG); err == nil {
			return "backup", nil
		}
	}
	if cc23xx.CodeOf(err) != cc23xx.CodeVerifyFailed {
		return "", err
	}
	if tx.Blank(ctx, 0, g.Sectors) == nil && tx.BlankCCFG(ctx) == nil {
		return "blank", nil
	}
	return "other", nil
}

func resetMode(a map[string]any) (cc23xx.ExitMode, error) {
	s, err := strArg(a, "reset")
	if err != nil {
		return "", err
	}
	switch s {
	case "", "run":
		return cc23xx.ExitRun, nil
	case "halt":
		return cc23xx.ExitResetHalt, nil
	case "none":
		return "", nil
	}
	return "", argErr("reset %q: want run, halt or none", s)
}

func partial(stage, backup string, err error) error {
	code := cc23xx.CodeOf(err)
	if code == "" {
		code = cc23xx.CodeFlashFailed
	}
	return status.Errorf(codes.DataLoss, "%s: %s failed: %v. The device is in SACI with erased or partly programmed flash and will not boot; restore it with FlashProgramming.restore path=%s", code, stage, err, backup)
}

// reflash erases the chip and writes main and the CCFG, verified in the same
// SACI session (verify commands stop working after the first reset).
func reflash(ctx context.Context, tx *cc23xx.FlashTxn, g cc23xx.Geometry, main, ccfgImg []byte, backupPath string, res map[string]any) error {
	plan, err := cc23xx.PlanImage(g, g.MainBase, main)
	if err != nil {
		return err
	}
	t0 := time.Now()
	if err := tx.ChipErase(ctx, 0); err != nil {
		return partial("chip erase", backupPath, err)
	}
	res["erase_ms"] = float64(time.Since(t0).Milliseconds())
	t1 := time.Now()
	if err := tx.Program(ctx, plan); err != nil {
		return partial("MAIN programming", backupPath, err)
	}
	res["program_ms"], res["program_bytes"] = float64(time.Since(t1).Milliseconds()), float64(plan.ProgramBytes)
	res["sectors"], res["blank_sectors"] = float64(len(plan.Sectors)), float64(plan.BlankSectors)
	t2 := time.Now()
	if err := tx.Verify(ctx, plan); err != nil {
		return partial("MAIN verify", backupPath, err)
	}
	res["verify_ms"] = float64(time.Since(t2).Milliseconds())
	if err := tx.ProgramCCFG(ctx, ccfgImg); err != nil {
		return partial("CCFG programming", backupPath, err)
	}
	if err := tx.VerifyCCFG(ctx, ccfgImg); err != nil {
		return partial("CCFG verify", backupPath, err)
	}
	res["main_verified"], res["ccfg_verified"] = true, true
	return nil
}

func finish(ctx context.Context, tx *cc23xx.FlashTxn, mode cc23xx.ExitMode, backupPath string, res map[string]any) error {
	r, err := tx.Finish(ctx, mode)
	res["reset"] = toMap(r)
	if err != nil {
		return status.Errorf(codes.DataLoss, "flash written and verified but the device did not start: %v (the flash is intact; restore %s if the device stays unusable)", err, backupPath)
	}
	return nil
}

// ---- erase ----

func (e *Env) flashErase(ctx context.Context, a map[string]any) (map[string]any, error) {
	if err := e.needFlash(); err != nil {
		return nil, err
	}
	if err := tokenIs(a, "confirm", TokEraseChip, "erasing the chip"); err != nil {
		return nil, err
	}
	retain := 0.0
	if v, ok := a["retain"]; ok && v != nil {
		f, isNum := v.(float64)
		if !isNum || f < 0 || f > 0xFFFF || f != float64(uint16(f)) {
			return nil, argErr("retain %v: want an integer 0..65535", v)
		}
		retain = f
	}
	b, err := loadGate(a, "backup")
	if err != nil {
		return nil, err
	}
	g, err := e.geometryFor(ctx, b)
	if err != nil {
		return nil, err
	}
	tx, err := e.S.BeginFlash(ctx)
	if err != nil {
		return nil, err
	}
	state, err := matchBackup(ctx, tx, g, b)
	if err != nil {
		return nil, err
	}
	switch state {
	case "other":
		return nil, status.Errorf(codes.FailedPrecondition, "the backup %s does not match the flash: it changed after the backup, take a new one. The device was not erased and waits in SACI", b.Path)
	case "blank":
		return map[string]any{"erased": true, "already_blank": true, "state": "saci", "backup": b.Path}, nil
	}
	t0 := time.Now()
	if err := tx.ChipErase(ctx, uint16(retain)); err != nil {
		return nil, partial("chip erase", b.Path, err)
	}
	return map[string]any{
		"erased": true, "elapsed_ms": float64(time.Since(t0).Milliseconds()), "state": "saci", "retain_mask": retain,
		"retain_policy": "SACI chip erase invalidates the CCFG, erases the MAIN sectors not in the retain mask, then erases the CCFG; a second chip erase after a reset also erases retained sectors",
		"backup":        b.Path, "backup_sha256": b.M.SHA256,
		"note": "the device waits in SACI with an invalid CCFG and will not boot until a CCFG is programmed and the device reset; FlashProgramming.restore or program does that",
	}, nil
}

// ---- program ----

func (e *Env) flashProgram(ctx context.Context, a map[string]any) (map[string]any, error) {
	if err := e.needFlash(); err != nil {
		return nil, err
	}
	if err := tokenIs(a, "confirm", TokProgram, "programming flash"); err != nil {
		return nil, err
	}
	img, path, err := readImage(a)
	if err != nil {
		return nil, err
	}
	addr, err := u32Arg(a, "address")
	if err != nil {
		return nil, err
	}
	mode, err := resetMode(a)
	if err != nil {
		return nil, err
	}
	b, err := loadGate(a, "backup")
	if err != nil {
		return nil, err
	}
	g, err := e.geometryFor(ctx, b)
	if err != nil {
		return nil, err
	}
	if uint64(addr) < uint64(g.MainBase) || uint64(addr)+uint64(len(img)) > uint64(g.MainBase)+uint64(g.MainBytes) {
		return nil, status.Errorf(codes.OutOfRange, "image %s (%d bytes) at %s does not fit MAIN flash %s..%s; the CCFG is never written by program", path, len(img), hex32(addr), hex32(g.MainBase), hex32(g.MainBase+g.MainBytes))
	}
	tx, err := e.S.BeginFlash(ctx)
	if err != nil {
		return nil, err
	}
	state, err := matchBackup(ctx, tx, g, b)
	if err != nil {
		return nil, err
	}
	if state == "other" {
		return nil, status.Errorf(codes.FailedPrecondition, "the backup %s does not match the flash and the flash is not blank: take a new backup. Nothing was erased", b.Path)
	}
	base := make([]byte, g.MainBytes)
	for i := range base {
		base[i] = 0xFF
	}
	if state == "backup" {
		copy(base, b.Main)
	}
	copy(base[addr-g.MainBase:], img)
	res := map[string]any{"address": hex32(addr), "length": float64(len(img)), "image_sha256": sha(img), "merged_over": state, "backup": b.Path}
	t0 := time.Now()
	if err := reflash(ctx, tx, g, base, b.CCFG, b.Path, res); err != nil {
		return nil, err
	}
	if err := finish(ctx, tx, mode, b.Path, res); err != nil {
		return nil, err
	}
	res["elapsed_ms"] = float64(time.Since(t0).Milliseconds())
	res["ccfg_changed"] = false
	res["basis"] = "SACI has no sector erase: the chip was erased and MAIN re-created from the backup (or blank) with the image merged over it; every sector CRC32 and the CCFG identity were verified in the same SACI session"
	return res, nil
}

// ---- restore ----

func (e *Env) flashRestore(ctx context.Context, a map[string]any) (map[string]any, error) {
	if err := e.needFlash(); err != nil {
		return nil, err
	}
	if err := tokenIs(a, "confirm", TokRestore, "restoring flash"); err != nil {
		return nil, err
	}
	mode, err := resetMode(a)
	if err != nil {
		return nil, err
	}
	b, err := loadGate(a, "path")
	if err != nil {
		return nil, err
	}
	g, err := e.geometryFor(ctx, b)
	if err != nil {
		return nil, err
	}
	tx, err := e.S.BeginFlash(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"sha256": b.M.SHA256, "backup": b.Path}
	t0 := time.Now()
	state, err := matchBackup(ctx, tx, g, b)
	if err != nil {
		return nil, err
	}
	res["restored"], res["whole_flash_matches_backup"], res["already_matched"] = true, true, state == "backup"
	if state != "backup" {
		if err := reflash(ctx, tx, g, b.Main, b.CCFG, b.Path, res); err != nil {
			return nil, err
		}
	}
	if err := finish(ctx, tx, mode, b.Path, res); err != nil {
		return nil, err
	}
	res["elapsed_ms"] = float64(time.Since(t0).Milliseconds())
	return res, nil
}

// ---- ccfg_program ----

func (e *Env) ccfgProgram(ctx context.Context, a map[string]any) (map[string]any, error) {
	dry, err := boolArg(a, "dry_run")
	if err != nil {
		return nil, err
	}
	img, path, err := readImage(a)
	if err != nil {
		return nil, err
	}
	if len(img) != ccfg.Size {
		return nil, argErr("path %s: a CCFG image is exactly %d bytes, got %d", path, ccfg.Size, len(img))
	}
	info, err := ccfg.Parse(img)
	if err != nil {
		return nil, argErr("%v", err)
	}
	if !info.Valid {
		return nil, &cc23xx.Error{Code: cc23xx.CodeCCFGInvalid, Msg: "the image's CRC32 fields are not valid; hwlab does not repair or synthesise a CCFG"}
	}
	b, err := loadGate(a, "backup")
	if err != nil {
		return nil, err
	}
	changes, err := ccfg.Diff(b.CCFG, img)
	if err != nil {
		return nil, err
	}
	sec := ccfg.Security(changes)
	res := map[string]any{"changes": toAny(changes), "security_relevant": sec, "new_ccfg": toMap(info)}
	if dry {
		res["dry_run"] = true
		return res, nil
	}
	if !e.Perm.CCFGWrite {
		return nil, denied("writing the CCFG needs allow_ccfg_write=true in the device configuration")
	}
	if err := e.needFlash(); err != nil {
		return nil, err
	}
	if err := tokenIs(a, "confirm", TokCCFG, "programming the CCFG"); err != nil {
		return nil, err
	}
	if sec {
		if err := tokenIs(a, "security_ack", TokSecurity, "this CCFG changes security-relevant fields (see changes)"); err != nil {
			return nil, err
		}
	}
	if !info.Permissions.AllowChipErase.Allowed() || !info.Permissions.AllowFlashProgram.Allowed() {
		return nil, denied("refusing a CCFG that forbids chip erase or flash programming: the device could never be recovered by hwlab")
	}
	if len(changes) == 0 {
		res["note"] = "the image equals the CCFG of the backup: nothing to change"
		return res, nil
	}
	g, err := e.geometryFor(ctx, b)
	if err != nil {
		return nil, err
	}
	mode, err := resetMode(a)
	if err != nil {
		return nil, err
	}
	tx, err := e.S.BeginFlash(ctx)
	if err != nil {
		return nil, err
	}
	state, err := matchBackup(ctx, tx, g, b)
	if err != nil {
		return nil, err
	}
	if state != "backup" {
		return nil, status.Errorf(codes.FailedPrecondition, "the flash does not match the backup %s (%s): take a new backup. Nothing was erased", b.Path, state)
	}
	if err := reflash(ctx, tx, g, b.Main, img, b.Path, res); err != nil {
		return nil, err
	}
	if err := finish(ctx, tx, mode, b.Path, res); err != nil {
		return nil, err
	}
	res["ccfg_changed"], res["new_ccfg_sha256"] = true, sha(img)
	return res, nil
}
