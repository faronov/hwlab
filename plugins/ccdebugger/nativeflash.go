package ccdebugger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/ccdebugger/native"
	"github.com/faronov/hwlab/plugins/ccdebugger/native/flashplan"
	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// nativeFlashMethods are served by the native flash/identity layer. The
// destructive ones additionally need an explicit confirmation token argument
// and the matching allow_* permission of the open session.
var nativeFlashMethods = map[string]bool{
	pluginv1.CapDebugProbe + ".chip_info": true, pluginv1.CapDebugProbe + ".read_lock": true, pluginv1.CapDebugProbe + ".read_ieee": true, pluginv1.CapDebugProbe + ".read_mac": true,
	pluginv1.CapFlashProgramming + ".read": true, pluginv1.CapFlashProgramming + ".read_info_page": true, pluginv1.CapFlashProgramming + ".verify": true,
	pluginv1.CapFlashProgramming + ".erase": true, pluginv1.CapFlashProgramming + ".erase_pages": true, pluginv1.CapFlashProgramming + ".program": true,
	pluginv1.CapFlashProgramming + ".write_config": true,
}

func geometryMap(g target.Geometry) map[string]any {
	return map[string]any{
		"chip": g.Chip, "family": string(g.Family), "chip_id": hexU(uint64(g.ChipID)), "revision": hexU(uint64(g.Revision)),
		"flash_size": float64(g.FlashSize), "ram_size": float64(g.RAMSize), "page_size": float64(g.PageSize),
		"pages": float64(g.Pages), "banks": float64(g.Banks), "usb": g.USB, "programmable": g.Programmable,
		"lock_page": float64(g.LockPage()), "lock_address": hexU(uint64(g.LockAddr())),
		"secondary_ieee_address": hexU(uint64(g.SecondaryIEEEAddr())), "primary_ieee_xdata": hexU(uint64(g.PrimaryIEEE)),
	}
}

func lockMap(l target.Lock) map[string]any {
	pages := make([]any, len(l.LockedPages))
	for i, p := range l.LockedPages {
		pages[i] = float64(p)
	}
	return map[string]any{
		"raw": hex.EncodeToString(l.Raw[:]), "debug_locked": l.DebugLocked, "locked_pages": pages,
		"lock_page_implicitly_locked": l.LockPageLocked,
	}
}

// ieeeText renders a stored (LSB-first) address most-significant byte first.
func ieeeText(b []byte) string {
	parts := make([]string, len(b))
	for i := range b {
		parts[len(b)-1-i] = fmt.Sprintf("%02X", b[i])
	}
	return strings.Join(parts, ":")
}

func decodeIEEE(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.NewReplacer(":", "", " ", "", "-", "").Replace(s))
	if err != nil || len(b) != target.IEEESize {
		return nil, status.Errorf(codes.InvalidArgument, "an IEEE address is %d bytes of hex, most significant byte first (e.g. 00:12:4B:00:...)", target.IEEESize)
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

func newFile(path string, overwrite bool) (*os.File, string, error) {
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return nil, "", status.Errorf(codes.AlreadyExists, "%s exists; pass overwrite=true to replace it", path)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".hwlab-ccd-*")
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "create output next to %s: %v", path, err)
	}
	return f, f.Name(), nil
}

// publish atomically moves a finished temporary file into place.
func publish(f *os.File, tmp, path string, overwrite bool) (retErr error) {
	defer func() {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, status.Errorf(codes.Internal, "remove temporary output: %v", err))
		}
	}()
	if err := f.Close(); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", path, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return status.Errorf(codes.Internal, "write %s: %v", path, err)
	}
	move := func() error {
		if overwrite {
			return os.Rename(tmp, path)
		}
		return os.Link(tmp, path)
	}
	if err := move(); err != nil {
		if os.IsExist(err) {
			return status.Errorf(codes.AlreadyExists, "%s exists; pass overwrite=true to replace it", path)
		}
		return status.Errorf(codes.Internal, "write %s: %v", path, err)
	}
	return nil
}

func (p *Plugin) nativeChipInfo(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	ci, err := d.ReadChipInfo()
	if err != nil {
		return nil, mapErr(err)
	}
	res := geometryMap(ci.Geometry)
	res["adapter_target_id"] = hexU(uint64(ci.AdapterTargetID))
	res["chipinfo0"] = hexU(uint64(ci.CHIPINFO0))
	res["chipinfo1"] = hexU(uint64(ci.CHIPINFO1))
	res["identified_from"] = "hardware chip-information registers (a configured target name is not trusted)"
	if lk, err := d.ReadLock(); err == nil {
		res["lock"] = lockMap(lk)
	}
	return res, nil
}

func (p *Plugin) nativeReadLock(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	lk, err := d.ReadLock()
	if err != nil {
		return nil, mapErr(err)
	}
	return lockMap(lk), nil
}

func (p *Plugin) nativeReadIEEE(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	a, err := d.ReadIEEE()
	if err != nil {
		return nil, mapErr(err)
	}
	res := map[string]any{
		"primary": ieeeText(a.Primary), "primary_stored": hex.EncodeToString(a.Primary), "primary_present": a.PrimaryPresent,
		"primary_source": a.PrimarySource,
		"secondary":      ieeeText(a.Secondary), "secondary_stored": hex.EncodeToString(a.Secondary), "secondary_present": a.SecondaryPresent,
		"secondary_source": a.SecondarySource,
		"byte_order":       "primary/secondary are printed most-significant byte first; *_stored is the on-chip (LSB-first) order",
	}
	if !a.SecondaryPresent {
		res["secondary"] = ""
	}
	return res, nil
}

// nativeFlashRead streams physical flash (default: all of it) to a file.
func (p *Plugin) nativeFlashRead(id string, args map[string]any, infoPage bool) (map[string]any, error) {
	path, err := pathArg(args)
	if err != nil {
		return nil, err
	}
	ow, _ := args["overwrite"].(bool)
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	var n int
	if infoPage {
		page, err := d.ReadInfoPage()
		if err != nil {
			return nil, mapErr(err)
		}
		f, tmp, err := newFile(path, ow)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(page); err != nil {
			return nil, errors.Join(status.Errorf(codes.Internal, "write %s: %v", path, err), f.Close(), os.Remove(tmp))
		}
		h.Write(page)
		n = len(page)
		if err := publish(f, tmp, path, ow); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "bytes": float64(n), "sha256": hex.EncodeToString(h.Sum(nil)), "source": "information page, XDATA 0x7800"}, nil
	}
	g, ok := d.Geometry()
	if !ok {
		ci, err := d.ReadChipInfo()
		if err != nil {
			return nil, mapErr(err)
		}
		g = ci.Geometry
	}
	addr, length := 0, 0
	if _, ok := args["address"]; ok {
		if addr, err = intArg(args, "address", true); err != nil {
			return nil, err
		}
	}
	if _, ok := args["length"]; ok {
		if length, err = intArg(args, "length", true); err != nil {
			return nil, err
		}
	} else {
		length = g.FlashSize - addr
	}
	if length < 1 || addr+length > g.FlashSize {
		return nil, status.Errorf(codes.InvalidArgument, "range 0x%X+%d must lie inside the %d-byte flash", addr, length, g.FlashSize)
	}
	f, tmp, err := newFile(path, ow)
	if err != nil {
		return nil, err
	}
	n, err = d.ReadFlash(addr, length, io.MultiWriter(f, h), nil)
	if err != nil {
		return nil, errors.Join(mapErr(err), f.Close(), os.Remove(tmp))
	}
	if err := publish(f, tmp, path, ow); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "address": hexU(uint64(addr)), "bytes": float64(n), "sha256": hex.EncodeToString(h.Sum(nil)), "chip": g.Chip}, nil
}

// loadImage reads a .hex or raw .bin firmware file without exceeding the
// image bound.
func loadImage(args map[string]any) (_ flashplan.Image, _ string, retErr error) {
	path, err := pathArg(args)
	if err != nil {
		return flashplan.Image{}, "", err
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return flashplan.Image{}, "", status.Errorf(codes.InvalidArgument, "firmware file %s not found", path)
	}
	if fi.Size() > 4*flashplan.MaxImageBytes {
		return flashplan.Image{}, "", status.Errorf(codes.InvalidArgument, "%s is larger than any supported flash image", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return flashplan.Image{}, "", status.Errorf(codes.InvalidArgument, "%v", err)
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	var img flashplan.Image
	if strings.EqualFold(filepath.Ext(path), ".hex") {
		img, err = flashplan.ParseIntelHex(f)
	} else {
		base := 0
		if _, ok := args["base"]; ok {
			if base, err = intArg(args, "base", true); err != nil {
				return img, "", err
			}
		}
		var b []byte
		if b, err = io.ReadAll(io.LimitReader(f, flashplan.MaxImageBytes+1)); err == nil {
			img, err = flashplan.FromBinary(b, base)
		}
	}
	if err != nil {
		return img, "", status.Errorf(codes.InvalidArgument, "%s: %v", path, err)
	}
	return img, path, nil
}

func stringArg(args map[string]any, k string) string { s, _ := args[k].(string); return s }

func programMap(r native.ProgramResult, path string) map[string]any {
	er := make([]any, len(r.ErasedPages))
	for i, v := range r.ErasedPages {
		er[i] = float64(v)
	}
	return map[string]any{
		"ok": true, "path": path, "chip": r.Chip, "mode": string(r.Mode), "erased_pages": er,
		"blocks_programmed": float64(r.BlocksProgrammed), "bytes_programmed": float64(r.BytesProgrammed),
		"skipped_blank_blocks": float64(r.SkippedBlocks), "verify": string(r.Verify), "verified": r.Verified,
		"crc_checks": float64(r.CRCs), "elapsed_ms": float64(r.Elapsed.Milliseconds()),
		"reset_required": r.ResetRequired,
		"note":           "the target must be reset (reset_halt) before it runs again; programming clobbers scratch RAM and DMA state",
	}
}

// plan identifies the chip and lock state, then validates the image.
func (p *Plugin) plan(d *native.Debugger, args map[string]any, img flashplan.Image, chipErase bool) (flashplan.Plan, error) {
	mode, err := flashplan.ParseMode(stringArg(args, "mode"))
	if err != nil {
		return flashplan.Plan{}, status.Error(codes.InvalidArgument, err.Error())
	}
	ci, err := d.ReadChipInfo()
	if err != nil {
		return flashplan.Plan{}, mapErr(err)
	}
	lk, err := d.ReadLock()
	if err != nil {
		return flashplan.Plan{}, mapErr(err)
	}
	plan, err := flashplan.Build(ci.Geometry, &lk, img, flashplan.Options{Mode: mode, ChipErase: chipErase})
	if err != nil {
		return flashplan.Plan{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return plan, nil
}

func (p *Plugin) nativeProgram(id string, args map[string]any) (map[string]any, error) {
	img, path, err := loadImage(args)
	if err != nil {
		return nil, err
	}
	vm, err := native.ParseVerify(stringArg(args, "verify"))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	chip, _ := args["chip_erase"].(bool)
	plan, err := p.plan(d, args, img, chip)
	if err != nil {
		return nil, err
	}
	r, err := d.Program(plan, native.ProgramOptions{
		Verify: vm, Confirm: stringArg(args, "confirm"), ConfirmConfig: stringArg(args, "confirm_config"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return programMap(r, path), nil
}

func (p *Plugin) nativeVerify(id string, args map[string]any) (map[string]any, error) {
	img, path, err := loadImage(args)
	if err != nil {
		return nil, err
	}
	vm, err := native.ParseVerify(stringArg(args, "mode"))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if vm == native.VerifyNone {
		return nil, status.Error(codes.InvalidArgument, "mode none verifies nothing")
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	ci, err := d.ReadChipInfo()
	if err != nil {
		return nil, mapErr(err)
	}
	lk, err := d.ReadLock()
	if err != nil {
		return nil, mapErr(err)
	}
	plan, err := flashplan.Build(ci.Geometry, &lk, img, flashplan.Options{Mode: flashplan.FullChipImage})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	crcs, err := d.VerifyFlash(plan.Blocks, vm, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{
		"ok": true, "path": path, "chip": ci.Chip, "verify": string(vm), "blocks": float64(len(plan.Blocks)),
		"crc_checks": float64(crcs), "image_bytes": float64(plan.ImageBytes),
		"note": "all-0xFF blocks of the image are not compared",
	}, nil
}

func (p *Plugin) nativeChipErase(id string, args map[string]any) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	r, err := d.ChipErase(native.ChipEraseOptions{Confirm: stringArg(args, "confirm")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{
		"ok": true, "locked_before": r.LockedBefore, "polls": float64(r.PollCount), "elapsed_ms": float64(r.Elapsed.Milliseconds()),
		"status_after": statusMap(r.StatusAfter), "reattach_required": r.ReattachFirst,
		"note": "the whole flash, lock bits and secondary IEEE address were erased; the target was not resumed",
	}, nil
}

func (p *Plugin) nativeErasePages(id string, args map[string]any) (map[string]any, error) {
	var pages []int
	switch v := args["pages"].(type) {
	case []any:
		for _, x := range v {
			f, ok := x.(float64)
			if !ok || f != math.Trunc(f) || f < 0 || f > 1<<20 {
				return nil, status.Error(codes.InvalidArgument, "pages must be non-negative integers")
			}
			pages = append(pages, int(f))
		}
	default:
		first, err := intArg(args, "first_page", true)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "give pages (a list) or first_page and count")
		}
		count, err := intArg(args, "count", true)
		if err != nil || count < 1 || count > 1<<10 {
			return nil, status.Error(codes.InvalidArgument, "count must be 1..1024")
		}
		for i := 0; i < count; i++ {
			pages = append(pages, first+i)
		}
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	r, err := d.ErasePages(pages, native.ErasePagesOptions{Confirm: stringArg(args, "confirm")})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]any, len(r.Pages))
	for i, v := range r.Pages {
		out[i] = float64(v)
	}
	return map[string]any{"ok": true, "pages": out, "blank_checked": r.BlankCheck, "reset_required": true}, nil
}

// nativeWriteConfig rewrites the secondary IEEE address and/or lock bits
// after saving the complete lock-bit page to backup_path.
func (p *Plugin) nativeWriteConfig(id string, args map[string]any) (map[string]any, error) {
	var ch native.ConfigChange
	if s := stringArg(args, "secondary_ieee"); s != "" {
		b, err := decodeIEEE(s)
		if err != nil {
			return nil, err
		}
		ch.SecondaryIEEE = b
	}
	if s := stringArg(args, "lock"); s != "" {
		b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
		if err != nil || len(b) != target.LockSize {
			return nil, status.Errorf(codes.InvalidArgument, "lock must be %d bytes of hex", target.LockSize)
		}
		ch.Lock = b
	}
	if ch.SecondaryIEEE == nil && ch.Lock == nil {
		return nil, status.Error(codes.InvalidArgument, "give secondary_ieee and/or lock")
	}
	bpath := stringArg(args, "backup_path")
	if bpath == "" {
		return nil, status.Error(codes.InvalidArgument, "backup_path is required: the lock-bit page is saved there before anything is erased")
	}
	bpath, err := filepath.Abs(bpath)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	vm, err := native.ParseVerify(stringArg(args, "verify"))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	saved := ""
	r, err := d.WriteDeviceConfig(ch, native.ConfigOptions{
		Confirm: stringArg(args, "confirm"), ConfirmLock: stringArg(args, "confirm_lock"), Verify: vm,
		BackupBefore: func(page []byte) error {
			f, err := os.OpenFile(bpath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := f.Write(page); err != nil {
				return errors.Join(err, f.Close())
			}
			if err := f.Close(); err != nil {
				return err
			}
			saved = bpath
			return nil
		},
	})
	if err != nil {
		return nil, mapErr(err)
	}
	res := programMap(r, "")
	delete(res, "path")
	res["backup_path"] = saved
	return res, nil
}

func (p *Plugin) invokeNativeFlash(id, capName, method string, args map[string]any) (map[string]any, error) {
	switch capName + "." + method {
	case pluginv1.CapDebugProbe + ".chip_info":
		return p.nativeChipInfo(id)
	case pluginv1.CapDebugProbe + ".read_lock":
		return p.nativeReadLock(id)
	case pluginv1.CapDebugProbe + ".read_ieee", pluginv1.CapDebugProbe + ".read_mac":
		return p.nativeReadIEEE(id)
	case pluginv1.CapFlashProgramming + ".read":
		return p.nativeFlashRead(id, args, false)
	case pluginv1.CapFlashProgramming + ".read_info_page":
		return p.nativeFlashRead(id, args, true)
	case pluginv1.CapFlashProgramming + ".verify":
		return p.nativeVerify(id, args)
	case pluginv1.CapFlashProgramming + ".erase":
		return p.nativeChipErase(id, args)
	case pluginv1.CapFlashProgramming + ".erase_pages":
		return p.nativeErasePages(id, args)
	case pluginv1.CapFlashProgramming + ".program":
		return p.nativeProgram(id, args)
	case pluginv1.CapFlashProgramming + ".write_config":
		return p.nativeWriteConfig(id, args)
	}
	return nil, status.Errorf(codes.Unimplemented, "unsupported method %s.%s", capName, method)
}
