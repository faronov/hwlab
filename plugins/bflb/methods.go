package bflbplugin

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
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/bflb/isp"
)

const (
	maxInlineRead = 64 << 10
	previewBytes  = 64
	maxHexData    = 4096
	maxFlashBytes = 32 << 20
)

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const optProps = `"baud":{"type":"number","description":"UART speed, default 115200 (ignored by the USB CDC port)"},"sync_bytes":{"type":"number","description":"handshake length, default 12 over USB CDC"},"timeout_ms":{"type":"number"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	T, F := pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	backup := `"backup":{"type":"string","description":"path of a FlashProgramming.backup .bin of this board (required)"}`
	return &pluginv1.PluginInfo{
		Id: "bflb", Name: "Bouffalo BL702 boot-ROM ISP (USB/UART)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(map[string]string{"validated": "partial"}),
		Methods: []*pluginv1.MethodSpec{
			spec(T, safe, "identify", "boot-ROM info (version, chip id, sign/encrypt requirements); unless load_helper=false also loads the eflash_loader helper into RAM (flash untouched) and reports the efuse MAC and flash JEDEC id/size",
				obj(optProps+`,"load_helper":{"type":"boolean"}`)),
			spec(F, safe, "info", "efuse MAC, flash JEDEC id and size, helper state; loads the helper into RAM if needed", obj(optProps)),
			spec(F, safe, "read", "read flash; args: address, length (max 64 KiB inline; with path, up to the whole flash to a new file)",
				`{"type":"object","required":["address","length"],"properties":{`+optProps+`,"address":{},"length":{"type":"number"},"path":{"type":"string"}}}`),
			spec(F, safe, "verify", "compare flash with a file or hex data by a SHA-256 computed on the chip; args: address, path or data",
				`{"type":"object","required":["address"],"properties":{`+optProps+`,"address":{},"path":{"type":"string"},"data":{"type":"string"}}}`),
			spec(F, safe, "backup", "read the whole flash twice, require identical SHA-256 that also equals the chip's own hash, store it 0600 with a manifest bound to the efuse MAC, JEDEC id and size; default dir $HWLAB_HOME/artifacts/bflb-flash. Never writes flash",
				obj(optProps+`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "erase the 4 KiB sectors covering address..address+length (address 4 KiB aligned). Requires backup=",
				`{"type":"object","required":["address","length","backup"],"properties":{`+optProps+`,"address":{},"length":{"type":"number"},`+backup+`}}`),
			spec(F, confirm, "program", "erase the covering sectors, write an image (path .bin or hex data) at a 4 KiB aligned address, run the helper's write check and compare the chip's SHA-256 of the range. For a BL702 firmware write the full image with its BFNP boot header at 0. Requires backup=",
				`{"type":"object","required":["address","backup"],"properties":{`+optProps+`,"address":{},"path":{"type":"string"},"data":{"type":"string"},`+backup+`}}`),
			spec(F, confirm, "restore", "rewrite every sector that differs from a backup of this board, then require the chip's SHA-256 of the whole flash to equal the backup. Restoring an old NV/security journal rolls back frame counters: do not do it on a board that was on a live network. path is an alias of backup",
				obj(optProps+`,"path":{"type":"string"},`+backup)),
			spec(F, confirm, "reset", "restart the chip through the helper (or the ROM). With BOOT released it boots the firmware in flash and leaves ISP mode; the USB ISP port disappears", obj(optProps)),
		},
	}
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s must be finite", key)
	}
	return f, true, nil
}

func optBool(a map[string]any, key string, def bool) (bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return def, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, fmt.Errorf("%s: want true or false, got %v", key, v)
	}
	return b, nil
}

// address reads a flash address given as a number or a "0x.." string.
func address(a map[string]any) (uint32, error) {
	v, ok := a["address"]
	if !ok || v == nil {
		return 0, errors.New("address is required")
	}
	var n uint64
	switch x := v.(type) {
	case float64:
		if x < 0 || x != math.Trunc(x) || x >= maxFlashBytes {
			return 0, fmt.Errorf("address %v: want an integer 0..0x%x", x, maxFlashBytes-1)
		}
		n = uint64(x)
	case string:
		var err error
		if n, err = strconv.ParseUint(strings.TrimSpace(x), 0, 32); err != nil || n >= maxFlashBytes {
			return 0, fmt.Errorf("address %q: want a number such as 0x10000", x)
		}
	default:
		return 0, fmt.Errorf("address: want a number or a 0x.. string, got %v", v)
	}
	return uint32(n), nil
}

func lengthArg(a map[string]any, max int) (int, error) {
	n, ok, err := optNumber(a, "length")
	if err != nil {
		return 0, err
	}
	if !ok || n < 1 || n != math.Trunc(n) || n > float64(max) {
		return 0, fmt.Errorf("length %v: want an integer 1..%d", n, max)
	}
	return int(n), nil
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

func image(a map[string]any, max int) ([]byte, error) {
	path, _ := a["path"].(string)
	_, hasData := a["data"]
	switch {
	case path != "" && hasData:
		return nil, errors.New("give either path or data, not both")
	case path != "":
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.Size() == 0 || st.Size() > int64(max) {
			return nil, fmt.Errorf("%s is %d bytes; want 1..%d (the flash size)", path, st.Size(), max)
		}
		return os.ReadFile(path)
	case hasData:
		return hexData(a, "data", min(max, maxHexData))
	}
	return nil, errors.New("path or data is required")
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hex32(v uint32) string { return fmt.Sprintf("0x%06x", v) }

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	switch method {
	case "TargetIdentify.identify", "FlashProgramming.info", "FlashProgramming.read", "FlashProgramming.verify", "FlashProgramming.backup",
		"FlashProgramming.erase", "FlashProgramming.program", "FlashProgramming.restore", "FlashProgramming.reset":
	default:
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
	d.cl.SetContext(ctx)
	defer d.cl.SetContext(context.Background())

	var res map[string]any
	switch method {
	case "TargetIdentify.identify":
		res, err = d.identify(args)
	case "FlashProgramming.info":
		res, err = d.flashInfo()
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
	if st := d.cl.Stats(); st.Retries > 0 {
		res["link_retries"] = float64(st.Retries)
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func wrap(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func (d *device) bootFields(res map[string]any) {
	res["helper_running"] = d.boot.HelperRunning()
	if d.rom != nil {
		res["rom_version"] = fmt.Sprintf("0x%08x", d.rom.ROMVersion)
		res["rom_requires_signed"] = d.rom.Sign != 0
		res["rom_requires_encrypted"] = d.rom.Encrypt != 0
		if len(d.rom.ChipID) > 0 {
			res["chip_id"] = hex.EncodeToString(d.rom.ChipID)
		}
	} else {
		res["rom_note"] = "the helper was already resident: the ROM's boot info is not available until the chip is reset"
	}
	if d.helperPath != "" {
		res["helper_loaded_now"] = true
		res["helper_image"] = d.helperPath
	}
}

// flashID is the identity of the connected board (helper running).
type flashID struct {
	mac   string
	jedec uint32
	size  int
}

func (d *device) flashIdentity() (flashID, error) {
	if err := d.ensureHelper(); err != nil {
		return flashID{}, err
	}
	mac, err := d.cl.MAC()
	if err != nil {
		return flashID{}, fmt.Errorf("read efuse MAC: %w", err)
	}
	jedec, size, err := d.cl.JEDEC()
	if err != nil {
		return flashID{}, err
	}
	m := hex.EncodeToString(mac)
	if m == "0000000000000000" || m == "ffffffffffffffff" {
		return flashID{}, status.Errorf(codes.FailedPrecondition, "efuse MAC reads %s: it cannot identify this board", m)
	}
	return flashID{mac: m, jedec: jedec, size: size}, nil
}

func (d *device) identify(a map[string]any) (map[string]any, error) {
	load, err := optBool(a, "load_helper", true)
	if err != nil {
		return nil, argErr(err)
	}
	res := map[string]any{"target_family": "BL702", "interface": "boot-ROM ISP"}
	if !load && !d.boot.HelperRunning() {
		d.bootFields(res)
		return res, nil
	}
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	d.bootFields(res)
	res["mac"], res["flash_jedec"], res["flash_bytes"] = id.mac, fmt.Sprintf("0x%06x", id.jedec), float64(id.size)
	return res, nil
}

func (d *device) flashInfo() (map[string]any, error) {
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"mac": id.mac, "flash_jedec": fmt.Sprintf("0x%06x", id.jedec), "flash_bytes": float64(id.size), "flash_kib": float64(id.size >> 10),
		"sector_bytes": float64(isp.Sector), "backup_required_for_modification": true, "chip_erase": "not offered",
	}
	d.bootFields(res)
	return res, nil
}

func (d *device) flashRead(a map[string]any) (map[string]any, error) {
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	addr, err := address(a)
	if err != nil {
		return nil, argErr(err)
	}
	path, _ := a["path"].(string)
	max := maxInlineRead
	if path != "" {
		max = id.size
	}
	n, err := lengthArg(a, max)
	if err != nil {
		return nil, argErr(err)
	}
	if int(addr)+n > id.size {
		return nil, argErr(fmt.Errorf("0x%06x+%d is past the end of the %d-byte flash", addr, n, id.size))
	}
	t0 := time.Now()
	b, err := d.cl.Read(addr, n)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(n), "sha256": shaHex(b), "read_seconds": time.Since(t0).Seconds()}
	if path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(b); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		res["path"] = path
		return res, nil
	}
	if n <= 4096 {
		res["data"] = hex.EncodeToString(b)
	} else {
		res["preview"] = hex.EncodeToString(b[:previewBytes])
	}
	return res, nil
}

func (d *device) flashVerify(a map[string]any) (map[string]any, error) {
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	addr, err := address(a)
	if err != nil {
		return nil, argErr(err)
	}
	img, err := image(a, id.size)
	if err != nil {
		return nil, argErr(err)
	}
	if int(addr)+len(img) > id.size {
		return nil, argErr(fmt.Errorf("0x%06x+%d is past the end of the %d-byte flash", addr, len(img), id.size))
	}
	sum, err := d.cl.SHA256(addr, len(img))
	if err != nil {
		return nil, err
	}
	want := shaHex(img)
	got := hex.EncodeToString(sum[:])
	return map[string]any{"address": hex32(addr), "length": float64(len(img)), "match": got == want, "expected_sha256": want, "flash_sha256": got, "method": "chip-side SHA-256"}, nil
}

// sectorRange is the erase range [start, end] covering addr..addr+n.
func sectorRange(addr uint32, n int) (uint32, uint32) {
	end := (addr + uint32(n) + isp.Sector - 1) &^ (isp.Sector - 1)
	return addr, end - 1
}

func (d *device) checkRange(id flashID, addr uint32, n int) error {
	if addr%isp.Sector != 0 {
		return fmt.Errorf("address 0x%06x is not 4 KiB aligned", addr)
	}
	if _, end := sectorRange(addr, n); int(end) >= id.size {
		return fmt.Errorf("0x%06x+%d is past the end of the %d-byte flash", addr, n, id.size)
	}
	return nil
}

func (d *device) flashErase(a map[string]any) (map[string]any, error) {
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	addr, err := address(a)
	if err != nil {
		return nil, argErr(err)
	}
	n, err := lengthArg(a, id.size)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.checkRange(id, addr, n); err != nil {
		return nil, argErr(err)
	}
	bk, err := d.backupGate(a, id)
	if err != nil {
		return nil, err
	}
	start, end := sectorRange(addr, n)
	t0 := time.Now()
	if err := d.cl.Erase(start, end); err != nil {
		return nil, err
	}
	return map[string]any{"erased_from": hex32(start), "erased_to": hex32(end + 1), "sectors": float64((end + 1 - start) / isp.Sector), "erase_seconds": time.Since(t0).Seconds(), "backup": bk}, nil
}

// write erases, writes, write-checks and hash-compares one range.
func (d *device) write(addr uint32, img []byte) (map[string]any, error) {
	start, end := sectorRange(addr, len(img))
	t0 := time.Now()
	if err := d.cl.Erase(start, end); err != nil {
		return nil, err
	}
	tErase := time.Since(t0)
	if err := d.cl.Write(addr, img); err != nil {
		return nil, fmt.Errorf("%w (sectors 0x%06x-0x%06x are erased or partially written)", err, start, end+1)
	}
	if err := d.cl.WriteCheck(); err != nil {
		return nil, fmt.Errorf("write check: %w", err)
	}
	tWrite := time.Since(t0) - tErase
	sum, err := d.cl.SHA256(addr, len(img))
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	want := shaHex(img)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, status.Errorf(codes.DataLoss, "after writing, the chip's SHA-256 of 0x%06x+%d is %s, want %s", addr, len(img), got, want)
	}
	return map[string]any{
		"address": hex32(addr), "bytes": float64(len(img)), "erased_from": hex32(start), "erased_to": hex32(end + 1),
		"sha256": want, "verified": true, "erase_seconds": tErase.Seconds(), "write_seconds": tWrite.Seconds(),
	}, nil
}

func (d *device) flashProgram(a map[string]any) (map[string]any, error) {
	id, err := d.flashIdentity()
	if err != nil {
		return nil, err
	}
	addr, err := address(a)
	if err != nil {
		return nil, argErr(err)
	}
	img, err := image(a, id.size)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.checkRange(id, addr, len(img)); err != nil {
		return nil, argErr(err)
	}
	bk, err := d.backupGate(a, id)
	if err != nil {
		return nil, err
	}
	res, err := d.write(addr, img)
	if err != nil {
		return nil, err
	}
	res["backup"] = bk
	if addr == 0 && (len(img) < 4 || string(img[:4]) != "BFNP") {
		res["warning"] = "the image at 0 does not start with a BFNP boot header: the ROM will not boot it"
	}
	res["next"] = "release BOOT, then FlashProgramming.reset (or replug) to boot the new firmware"
	return res, nil
}

// reset restarts the chip through the helper: the ROM has no reset command.
func (d *device) reset() (map[string]any, error) {
	if err := d.ensureHelper(); err != nil {
		return nil, err
	}
	if err := d.cl.Reset(); err != nil {
		return nil, err
	}
	d.boot = isp.BootInfo{}
	return map[string]any{"reset": true, "note": "with BOOT released the chip boots from flash and the ISP port disappears; with BOOT held it re-enters ISP"}, nil
}
