package tibslplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/tibsl/bsl"
)

// Confirmation tokens (args.confirm / args.confirm_config / args.ccfg_override).
const (
	tokErase     = "erase-flash-region"
	tokBankErase = "bank-erase"
	tokProgram   = "program-flash"
	tokRestore   = "restore-flash"
	tokCCFG      = "modify-ccfg"
	tokLockout   = "allow-bsl-lockout"

	maxInline    = 4096
	maxMemFile   = 1 << 20
	maxFlashFile = 2 << 20
)

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const optProps = `"baud":{"type":"number","description":"UART speed for the ROM auto-baud, default 460800 (or the port's @baud)"},` +
	`"timeout_ms":{"type":"number","description":"bound of one ACK/reply, default 2000"},` +
	`"bsl_entry":{"type":"string","enum":["manual","dtr-rts","rts-dtr"],"description":"how the chip enters the bootloader: dtr-rts (default; RTS->RESET, DTR->backdoor pin, Sonoff ZBDongle-P), rts-dtr (swapped) or manual (BSL button; lines untouched)"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	T, F, M := pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming, pluginv1.CapMemoryAccess
	obj := func(props string) string { return `{"type":"object","properties":{` + optProps + props + `}}` }
	req := func(r, props string) string {
		return `{"type":"object","required":[` + r + `],"properties":{` + optProps + props + `}}`
	}
	backup := `,"backup":{"type":"string","description":"a FlashProgramming.backup .bin of this chip (required)"}`
	ccfg := `,"confirm_config":{"type":"string","enum":["` + tokCCFG + `"],"description":"needed when the CCFG/CCA sector (last flash page) changes"}` +
		`,"ccfg_override":{"type":"string","enum":["` + tokLockout + `"],"description":"DANGEROUS: accept a CCFG/CCA that disables the bootloader/backdoor or locks debug; recovery then needs JTAG"}`
	return &pluginv1.PluginInfo{
		Id: "tibsl", Name: "TI CC13xx/CC26xx/CC2538 ROM serial bootloader (UART)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(map[string]string{"validated": "partial"}),
		Methods: []*pluginv1.MethodSpec{
			spec(T, safe, "identify", "enter the ROM bootloader (per bsl_entry), synchronize and identify: GET_CHIP_ID, FCFG1 ICEPICK_DEVICE_ID (wafer id, PG), USER_ID (package, PA, protocols), FLASH_SIZE, factory IEEE (FCFG1 MAC_15_4) and the CCFG override, bootloader backdoor. Read only; the chip stays in the bootloader until FlashProgramming.reset", obj("")),
			spec(F, safe, "info", "identity plus flash geometry, CCFG/CCA location and decode, erase/protection state and what modification needs. Read only", obj("")),
			spec(F, safe, "ccfg_info", "decode the CCFG (CC26xx) or CCA (CC2538): bootloader/backdoor, image valid, TAP/DAP enables, TI FA, erase enables, write protection, IEEE override. Read only", obj("")),
			spec(M, safe, "read", "MEMORY_READ: flash, SRAM, FCFG1 and the identity registers (others need allow_peripheral=true); max 4096 bytes inline or up to 1 MiB to a new file with path",
				req(`"address","length"`, `,"address":{},"length":{"type":"number"},"path":{"type":"string"},"allow_peripheral":{"type":"boolean"}`)),
			spec(F, safe, "read", "read flash with MEMORY_READ (chunked) and check the ROM's CRC32 of the range against the bytes read; max 4096 bytes inline, or up to the whole flash to a new file with path. address defaults to the flash start, length to the whole flash",
				obj(`,"address":{},"length":{"type":"number"},"path":{"type":"string"}`)),
			spec(F, safe, "verify", "compare flash with a file (path) or hex data at address by the ROM's CRC32; never writes",
				obj(`,"address":{},"path":{"type":"string"},"data":{"type":"string"}`)),
			spec(F, safe, "backup", "read the whole flash twice, require identical SHA-256 and the ROM's CRC32 to match, store it 0600 with a manifest bound to the factory IEEE, ICEPICK id, USER_ID and flash size; default dir $HWLAB_HOME/artifacts/tibsl-flash. Never writes flash",
				obj(`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "mode=region (default): erase the sectors covering [address, address+length), address sector-aligned, confirm=\""+tokErase+"\"; mode=bank: erase the whole flash including the CCFG (BANK_ERASE; CC2538: ERASE of the whole range), confirm=\""+tokBankErase+"\". Touching the CCFG/CCA sector also needs confirm_config. Requires backup=",
				req(`"confirm","backup"`, `,"mode":{"type":"string","enum":["region","bank"]},"address":{},"length":{"type":"number"},"confirm":{"type":"string","enum":["`+tokErase+`","`+tokBankErase+`"]}`+backup+ccfg)),
			spec(F, confirm, "program", "program an image (path .bin or hex data) at address (default flash start): only sectors whose content changes are erased and rewritten, bytes outside the image are preserved, each sector and the whole range are checked by the ROM's CRC32. The CCFG/CCA sector is written last and only with confirm_config; the resulting CCFG is checked for lockout. dry_run=true plans without writing. reset_after=true starts the application. Requires confirm=\""+tokProgram+"\" and backup=",
				obj(`,"address":{},"path":{"type":"string"},"data":{"type":"string"},"confirm":{"type":"string","enum":["`+tokProgram+`"]},"dry_run":{"type":"boolean"},"reset_after":{"type":"boolean"}`+backup+ccfg)),
			spec(F, confirm, "restore", "rewrite every sector that differs from a backup of this chip (path or backup), CCFG sector last, then require the ROM's CRC32 of the whole flash to equal the backup's. Restoring an old Zigbee NV area rolls back frame counters: never restore onto a device that joined a network since. Requires confirm=\""+tokRestore+"\"",
				obj(`,"path":{"type":"string"},"confirm":{"type":"string","enum":["`+tokRestore+`"]},"dry_run":{"type":"boolean"},"reset_after":{"type":"boolean"}`+backup+ccfg)),
			spec(F, confirm, "reset", "leave the bootloader: release DTR/RTS (RESET and backdoor pin) and send COMMAND_RESET; the chip boots its flash image (a manual board: release the BSL button first)", obj("")),
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

// u32Arg reads an address-like argument (number or "0x.." string).
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

func tokenIs(a map[string]any, key, want, what string) error {
	if s, _ := a[key].(string); s != want {
		return status.Errorf(codes.PermissionDenied, "%s needs %s=%q", what, key, want)
	}
	return nil
}

func hexData(a map[string]any, key string, max int) ([]byte, error) {
	s, _ := a[key].(string)
	s = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), "0x")
	data, err := hex.DecodeString(s)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("%s: want a non-empty hex string", key)
	}
	if len(data) > max {
		return nil, fmt.Errorf("%s is %d bytes; at most %d", key, len(data), max)
	}
	return data, nil
}

func readImage(a map[string]any, max int) ([]byte, string, error) {
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
		if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > int64(max) {
			return nil, "", fmt.Errorf("%s is %d bytes; want a regular file of 1..%d bytes (the flash size)", path, st.Size(), max)
		}
		b, err := os.ReadFile(path)
		return b, path, err
	case hasData:
		b, err := hexData(a, "data", min(max, maxInline))
		return b, "", err
	}
	return nil, "", errors.New("path or data is required")
}

func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08X", v) }

func crcHex(v uint32) string { return fmt.Sprintf("0x%08X", v) }

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
	d, release, err := p.lease(ctx, req.GetDeviceId(), args)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cl.SetContext(ctx)
	defer d.cl.SetContext(context.Background())

	var res map[string]any
	switch method {
	case "TargetIdentify.identify":
		res, err = d.identify()
	case "FlashProgramming.info":
		res, err = d.flashInfo()
	case "FlashProgramming.ccfg_info":
		res, err = d.ccfgInfo()
	case "MemoryAccess.read":
		res, err = d.memRead(args)
	case "FlashProgramming.read":
		res, err = d.flashRead(args)
	case "FlashProgramming.verify":
		res, err = d.flashVerify(args)
	case "FlashProgramming.backup":
		res, err = d.flashBackup(args)
	case "FlashProgramming.erase":
		res, err = d.flashErase(args)
	case "FlashProgramming.program":
		res, err = d.flashProgram(args)
	case "FlashProgramming.restore":
		res, err = d.flashRestore(args)
	case "FlashProgramming.reset":
		res, err = d.reset()
	}
	if err != nil {
		return nil, wrap(err)
	}
	if d.entry.Sim != nil {
		res["synthetic"] = true
	}
	if st := d.cl.Stats(); st.JunkBytes > 0 {
		res["link_junk_bytes"] = float64(st.JunkBytes)
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func wrap(err error) error {
	var se *bsl.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, bsl.ErrTimeout):
		return status.Error(codes.Unavailable, err.Error())
	case errors.As(err, &se) && se.Status == bsl.StatusInvalidAdr:
		return status.Error(codes.OutOfRange, err.Error())
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func (d *device) chipFields(res map[string]any) {
	c := d.chip
	res["family"] = c.Family.String()
	res["chip"] = c.Name
	res["series"] = c.Series
	res["chip_id"] = hex32(c.ChipID)
	res["bsl_entry"] = string(d.o.entry)
	res["baud"] = float64(d.o.baud)
	if d.o.entry.Wired() {
		res["lines_atomic"] = d.atomic
	}
	if d.resynced {
		res["resynced"] = true
	}
	if c.Family == bsl.FamilyCC26xx && c.ICEPick != 0 {
		res["icepick_device_id"] = hex32(c.ICEPick)
		res["wafer_id"] = fmt.Sprintf("0x%04X", c.WaferID)
		res["pg_rev"] = float64(c.PGRev)
		res["user_id"] = hex32(c.UserID)
		res["package"] = c.Package
		res["pa"] = c.PA
		res["protocols"] = toAny(c.Protocols)
	}
	if c.FlashBytes > 0 {
		res["flash_base"] = hex32(c.FlashBase)
		res["flash_bytes"] = float64(c.FlashBytes)
		res["flash_kib"] = float64(c.FlashBytes >> 10)
		res["sector_bytes"] = float64(c.SectorBytes)
		res["ieee"] = c.IEEEString()
	}
	res["writable"] = c.Writable
	if c.ReadOnlyWhy != "" {
		res["read_only_reason"] = c.ReadOnlyWhy
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// config reads the current CCFG/CCA.
func (d *device) config() ([]byte, error) {
	if d.chip.ConfigLen == 0 {
		return nil, status.Error(codes.FailedPrecondition, "the CCFG location of this chip is unknown")
	}
	b, err := d.cl.Read(d.chip.ConfigAddr, d.chip.ConfigLen, nil)
	if err != nil {
		return nil, fmt.Errorf("read CCFG: %w", err)
	}
	return b, nil
}

func (d *device) identify() (map[string]any, error) {
	res := map[string]any{"interface": "TI ROM serial bootloader"}
	d.chipFields(res)
	if d.chip.ConfigLen > 0 {
		cfg, err := d.config()
		if err != nil {
			return nil, err
		}
		dec := bsl.DecodeConfig(d.chip.Family, cfg)
		res["bootloader"] = dec["bootloader"]
		if o, ok := dec["ieee_override"]; ok {
			res["ieee_ccfg_override"] = o
		}
	}
	res["note"] = "the chip stays in the bootloader until FlashProgramming.reset"
	return res, nil
}

func (d *device) flashInfo() (map[string]any, error) {
	res, err := d.identify()
	if err != nil {
		return nil, err
	}
	delete(res, "note")
	if d.chip.ConfigLen > 0 {
		res["config_address"] = hex32(d.chip.ConfigAddr)
		res["config_bytes"] = float64(d.chip.ConfigLen)
		res["config_sector"] = hex32(d.chip.ConfigSector())
	}
	res["backup_required_for_modification"] = true
	res["tokens"] = map[string]any{"erase": tokErase, "bank_erase": tokBankErase, "program": tokProgram, "restore": tokRestore,
		"config_sector": "confirm_config=" + tokCCFG, "lockout_override": "ccfg_override=" + tokLockout}
	return res, nil
}

func (d *device) ccfgInfo() (map[string]any, error) {
	cfg, err := d.config()
	if err != nil {
		return nil, err
	}
	res := bsl.DecodeConfig(d.chip.Family, cfg)
	res["address"] = hex32(d.chip.ConfigAddr)
	res["bytes"] = float64(len(cfg))
	res["chip"] = d.chip.Name
	res["raw_hex"] = hex.EncodeToString(cfg)
	return res, nil
}

type window struct {
	lo, hi uint32
	name   string
}

func (d *device) windows() []window {
	c := d.chip
	w := []window{{c.FlashBase, c.FlashBase + uint32(c.FlashBytes), "flash"}}
	if c.Family == bsl.FamilyCC2538 {
		return append(w, window{0x20000000, 0x20008000, "SRAM"}, window{0x00280000, 0x00280800, "info page"}, window{bsl.CC2538DieCfg0, bsl.CC2538DieCfg0 + 12, "DIECFG"})
	}
	return append(w, window{0x20000000, 0x20000000 + 80<<10, "SRAM"}, window{bsl.FCFG1Base, bsl.FCFG1Base + bsl.FCFG1Size, "FCFG1"},
		window{bsl.RegFlashSize, bsl.RegFlashSize + 4, "FLASH_SIZE"})
}

func (d *device) memRead(a map[string]any) (map[string]any, error) {
	addr, ok, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		return nil, argErr(errors.New("address is required"))
	}
	path, _ := a["path"].(string)
	max := maxInline
	if path != "" {
		max = maxMemFile
	}
	nf, ok, err := optNumber(a, "length")
	if err != nil || !ok || nf < 1 || nf > float64(max) || nf != math.Trunc(nf) {
		return nil, argErr(fmt.Errorf("length: want an integer 1..%d", max))
	}
	n := int(nf)
	if uint64(addr)+uint64(n) > math.MaxUint32+1 {
		return nil, argErr(errors.New("address+length wraps"))
	}
	periph, err := optBool(a, "allow_peripheral")
	if err != nil {
		return nil, argErr(err)
	}
	region := ""
	for _, w := range d.windows() {
		if addr >= w.lo && uint64(addr)+uint64(n) <= uint64(w.hi) {
			region = w.name
		}
	}
	if region == "" && !periph {
		return nil, status.Errorf(codes.PermissionDenied, "0x%08X+%d is outside flash, SRAM, FCFG1 and the identity registers: reading peripherals can have side effects, pass allow_peripheral=true", addr, n)
	}
	b, err := d.cl.Read(addr, n, nil)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(n), "sha256": shaHex(b)}
	if region != "" {
		res["region"] = region
	}
	if path != "" {
		if err := writeNew(path, b); err != nil {
			return nil, err
		}
		res["path"] = path
	} else {
		res["data"] = hex.EncodeToString(b)
	}
	return res, nil
}

// flashRange validates [addr, addr+n) against the flash.
func (d *device) flashRange(addr uint32, n int) error {
	c := d.chip
	if c.FlashBytes == 0 {
		return status.Error(codes.FailedPrecondition, "the flash size of this chip is unknown: "+c.ReadOnlyWhy)
	}
	if addr < c.FlashBase || uint64(addr)+uint64(n) > uint64(c.FlashBase)+uint64(c.FlashBytes) || n < 1 {
		return argErr(fmt.Errorf("0x%08X+%d is outside the flash 0x%08X..0x%08X", addr, n, c.FlashBase, c.FlashBase+uint32(c.FlashBytes)))
	}
	return nil
}

// readChecked reads flash and checks it against the ROM's CRC32.
func (d *device) readChecked(addr uint32, n int) ([]byte, uint32, error) {
	b, err := d.cl.Read(addr, n, nil)
	if err != nil {
		return nil, 0, err
	}
	chip, err := d.cl.CRC32(addr, n)
	if err != nil {
		return nil, 0, err
	}
	if host := crc32.ChecksumIEEE(b); host != chip {
		return nil, 0, status.Errorf(codes.DataLoss, "read 0x%08X+%d: CRC32 of the bytes read is %s, the ROM's %s", addr, n, crcHex(host), crcHex(chip))
	}
	return b, chip, nil
}

func (d *device) flashRead(a map[string]any) (map[string]any, error) {
	c := d.chip
	addr, ok, err := u32Arg(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		addr = c.FlashBase
	}
	path, _ := a["path"].(string)
	nf, ok, err := optNumber(a, "length")
	if err != nil {
		return nil, argErr(err)
	}
	n := c.FlashBytes - int(addr-c.FlashBase)
	if ok {
		if nf < 1 || nf != math.Trunc(nf) || nf > float64(c.FlashBytes) {
			return nil, argErr(fmt.Errorf("length %v: want an integer 1..%d", nf, c.FlashBytes))
		}
		n = int(nf)
	}
	if err := d.flashRange(addr, n); err != nil {
		return nil, err
	}
	if path == "" && n > maxInline {
		return nil, argErr(fmt.Errorf("length %d: at most %d bytes inline; pass path to read more", n, maxInline))
	}
	t0 := time.Now()
	b, crc, err := d.readChecked(addr, n)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(n), "sha256": shaHex(b), "crc32": crcHex(crc), "crc_verified": true, "read_seconds": time.Since(t0).Seconds()}
	if path != "" {
		if err := writeNew(path, b); err != nil {
			return nil, err
		}
		res["path"] = path
		return res, nil
	}
	res["data"] = hex.EncodeToString(b)
	return res, nil
}

func (d *device) flashVerify(a map[string]any) (map[string]any, error) {
	c := d.chip
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
	chip, err := d.cl.CRC32(addr, len(img))
	if err != nil {
		return nil, err
	}
	want := crc32.ChecksumIEEE(img)
	res := map[string]any{"address": hex32(addr), "length": float64(len(img)), "match": chip == want, "expected_crc32": crcHex(want), "flash_crc32": crcHex(chip),
		"image_sha256": shaHex(img), "method": "ROM CRC32"}
	if path != "" {
		res["path"] = path
	}
	return res, nil
}

// reset releases the lines and restarts the chip into its flash image.
func (d *device) reset() (map[string]any, error) {
	if err := bsl.Release(d.conn.Lines, d.o.entry); err != nil {
		return nil, err
	}
	if err := d.cl.Reset(); err != nil {
		return nil, err
	}
	d.left = true
	note := "the chip restarted from flash; with a valid image and the backdoor pin released the application runs"
	if !d.o.entry.Wired() {
		note += " (release the BSL button)"
	}
	return map[string]any{"reset": true, "note": note}, nil
}
