package stm32dfuplugin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/ownedtemp"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

const (
	maxReadBytes = 4096
	maxReadFile  = 16 << 20

	periph1Lo, periph1Hi = 0x40000000, 0x5FFFFFFF
	periph2Lo            = 0xE0000000

	tokLeave = "leave-dfu"
	// The bootloader jumps to the vector table at the start of flash.
	leaveAddr = flashBase
)

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, format, a...)
}

func boolArg(args map[string]any, key string, def bool) (bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, argErr(fmt.Errorf("%s: want true or false", key))
	}
	return b, nil
}

func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", nil
	}
	str, isStr := v.(string)
	if !isStr {
		return "", argErr(fmt.Errorf("%s: want a string", key))
	}
	return str, nil
}

func intArg(args map[string]any, key string, def, lo, hi int) (int, error) {
	v, ok, err := optNumber(args, key)
	if err != nil {
		return 0, argErr(err)
	}
	if !ok {
		return def, nil
	}
	if v < float64(lo) || v > float64(hi) || v != math.Trunc(v) {
		return 0, argErr(fmt.Errorf("%s %v: want an integer %d..%d", key, v, lo, hi))
	}
	return int(v), nil
}

func toAddr(v any) (uint32, error) {
	var u uint64
	switch x := v.(type) {
	case float64:
		if x < 0 || x != math.Trunc(x) {
			return 0, fmt.Errorf("invalid address %v", x)
		}
		u = uint64(x)
	case string:
		var err error
		if u, err = strconv.ParseUint(strings.TrimSpace(x), 0, 64); err != nil {
			return 0, err
		}
	default:
		return 0, errors.New(`address must be a number or a string like "0x20030030"`)
	}
	if u > math.MaxUint32 {
		return 0, fmt.Errorf("address 0x%x does not fit the 32-bit address space", u)
	}
	return uint32(u), nil
}

func hexw(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

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

// ---- descriptors ----

func (s *session) dfuFacts() map[string]any {
	res := usbFacts(s.entry)
	res["open"] = true
	if s.serialDesc != "" {
		res["serial_descriptor"] = s.serialDesc
	}
	if s.serialNote != "" {
		res["serial_note"] = s.serialNote
	}
	if len(s.devDesc) >= 18 {
		v := binary.LittleEndian.Uint16(s.devDesc[12:])
		res["bootloader_version"], res["bcd_device"] = bcd(v), fmt.Sprintf("0x%04x", v)
	}
	if f := s.cfg.Functional; f != nil {
		res["dfu_functional"] = map[string]any{
			"transfer_size": float64(f.TransferSize), "detach_timeout_ms": float64(f.DetachTimeout),
			"dfu_version": fmt.Sprintf("0x%04x", f.DFUVersion), "attributes": fmt.Sprintf("0x%02x", f.Attributes),
			"can_download": f.Has(dfu.AttrCanDownload), "can_upload": f.Has(dfu.AttrCanUpload),
			"manifestation_tolerant": f.Has(dfu.AttrManifestationTolerant), "will_detach": f.Has(dfu.AttrWillDetach),
		}
	}
	var alts []any
	for _, a := range s.cfg.Alts {
		m := map[string]any{"interface": float64(a.Interface), "alt": float64(a.Alt), "name": a.Name}
		if a.ParseErr != "" {
			m["parse_error"] = a.ParseErr
		}
		if a.Memory != nil {
			m["region"] = a.Memory.Name
			var segs []any
			for _, sg := range a.Memory.Segments {
				segs = append(segs, map[string]any{"start": hexw(sg.Start), "end": hexw(sg.End()), "pages": float64(sg.Pages),
					"page_size": float64(sg.PageSize), "attributes": sg.AttrString()})
			}
			m["segments"] = segs
		}
		alts = append(alts, m)
	}
	res["alt_settings"] = alts
	res["dfu_interface"] = float64(s.iface)
	return res
}

func (s *session) identify(ctx context.Context, full bool) (map[string]any, error) {
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	res := t.facts()
	for k, v := range s.dfuFacts() {
		if _, ok := res[k]; !ok {
			res[k] = v
		}
	}
	if full {
		st, err := s.c.GetStatus(ctx)
		if err != nil {
			return nil, err
		}
		res["dfu_state"], res["dfu_status"] = st.State.String(), dfu.StatusName(st.Code)
		cmds, err := s.c.GetCommands(ctx)
		if err == nil {
			res["dfuse_commands"] = hex.EncodeToString(cmds)
		} else {
			res["dfuse_commands_error"] = err.Error()
			if e2 := s.c.EnsureIdle(ctx); e2 != nil {
				return nil, e2
			}
		}
	}
	return s.mark(res), nil
}

// ---- memory ----

type readReq struct {
	addr uint32
	n    int
	path string
}

// parseRead validates a read before any device is opened.
func parseRead(args map[string]any) (readReq, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return readReq{}, argErr(err)
	}
	nf, ok, err := optNumber(args, "length")
	if err != nil {
		return readReq{}, argErr(err)
	}
	path, err := strArg(args, "path")
	if err != nil {
		return readReq{}, err
	}
	limit := maxReadBytes
	if path != "" {
		limit = maxReadFile
	}
	if !ok || nf < 1 || nf > float64(limit) || nf != math.Trunc(nf) {
		if path == "" {
			return readReq{}, argErr(fmt.Errorf("length: want an integer 1..%d bytes inline (pass path=<new file> for up to %d bytes)", maxReadBytes, maxReadFile))
		}
		return readReq{}, argErr(fmt.Errorf("length: want an integer 1..%d bytes", limit))
	}
	n := int(nf)
	if uint64(addr)+uint64(n) > 1<<32 {
		return readReq{}, argErr(errors.New("the range wraps past the end of the 32-bit address space"))
	}
	allow, err := boolArg(args, "allow_peripheral", false)
	if err != nil {
		return readReq{}, err
	}
	if !allow {
		switch {
		case overlaps(addr, n, periph1Lo, periph1Hi):
			return readReq{}, argErr(fmt.Errorf("0x%08x+%d touches the peripheral region, where a read can have side effects: pass allow_peripheral=true to read it anyway", addr, n))
		case overlaps(addr, n, periph2Lo, math.MaxUint32):
			return readReq{}, argErr(fmt.Errorf("0x%08x+%d touches the system region (0xE0000000-): pass allow_peripheral=true to read it anyway", addr, n))
		}
	}
	return readReq{addr, n, path}, nil
}

// output returns data inline (hex) or writes it to a new file.
func output(res map[string]any, args map[string]any, path string, data []byte) error {
	res["sha256"] = sha(data)
	if path == "" {
		res["hex"] = hex.EncodeToString(data)
		return nil
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return err
	}
	if err := ownedtemp.WriteFile(path, data, overwrite); err != nil {
		return argErr(err)
	}
	res["path"] = path
	return nil
}

func (s *session) readMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseRead(args)
	if err != nil {
		return nil, err
	}
	if _, err := s.target(ctx); err != nil {
		return nil, err
	}
	t0 := time.Now()
	data, err := s.readAt(ctx, rq.addr, rq.n)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hexw(rq.addr), "length": float64(rq.n), "elapsed_ms": float64(time.Since(t0).Milliseconds()),
		"via": "DfuSe set-address + UPLOAD"}
	if a := s.altCovering(rq.addr, rq.n); a != nil {
		res["alt_setting"] = float64(a.Alt)
		res["region"] = a.Memory.Name
	}
	if err := output(res, args, rq.path, data); err != nil {
		return nil, err
	}
	return s.mark(res), nil
}

// ---- FUS ----

func (s *session) fusInfo(ctx context.Context) (map[string]any, error) {
	t, err := s.target(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"chip": t.chip, "secure_boundary_established": t.sfsaOK}
	if t.sfsaOK {
		res["sfsa"], res["secure_base"], res["user_flash_bytes"] = fmt.Sprintf("0x%02x", t.sfsa), hexw(t.secure), float64(t.userBytes)
	} else {
		res["read_only_reason"] = t.sfsaWhy
	}
	if t.ob != nil {
		res["flash_sfr"], res["flash_srrvr"], res["flash_ipccbr"] = hexw(t.obWord["sfr"]), hexw(t.obWord["srrvr"]), hexw(t.obWord["ipccbr"])
		if t.obWord["ipccbr"]&0x3FFF != 0 {
			res["note"] = "IPCCDBA moves the mailbox away from 0x20030000: the table below may be stale"
		}
	}
	ptrb, err := s.readAt(ctx, stm32.RefTable, 4)
	if err != nil {
		return nil, fmt.Errorf("reading the reference table pointer at 0x%08x: %w", uint32(stm32.RefTable), err)
	}
	ptr := binary.LittleEndian.Uint32(ptrb)
	res["reference_table"] = hexw(stm32.RefTable)
	res["device_info_table_ptr"] = hexw(ptr)
	vb, err := s.readAt(ctx, 0x20030030, 4)
	if err != nil {
		return nil, err
	}
	w := binary.LittleEndian.Uint32(vb)
	res["word_0x20030030"] = hexw(w)
	res["word_0x20030030_as_version"] = stm32.Version(w).String()
	if stm32.DeviceInfoPtrValid(ptr) {
		raw, err := s.readAt(ctx, ptr, stm32.DeviceInfoLen)
		if err != nil {
			return nil, err
		}
		d := stm32.DecodeDeviceInfo(ptr, raw)
		di := map[string]any{"layout": d.Layout, "table_ptr": hexw(d.TablePtr), "raw": hex.EncodeToString(d.Raw)}
		switch d.Layout {
		case "fus":
			di["fus_version"], di["fus_version_word"] = d.FUSVersion.String(), hexw(uint32(d.FUSVersion))
			di["stack_version"], di["stack_version_word"] = d.StackVersion.String(), hexw(uint32(d.StackVersion))
			di["fus_memory_size"], di["stack_memory_size"] = hexw(d.FUSMemSize), hexw(d.StackMemSize)
			di["state_flags"], di["safeboot_version"] = hexw(d.StateFlags), hexw(d.SafeBoot)
			di["uid64"], di["dev_id"] = fmt.Sprintf("0x%016x", d.UID64), fmt.Sprintf("0x%03x", d.DevID)
		case "wireless_stack":
			di["fus_version"], di["stack_version"] = d.FUSVersion.String(), d.StackVersion.String()
			di["safeboot_version"], di["info_stack"] = hexw(d.SafeBoot), hexw(d.InfoStack)
		}
		res["device_info"] = di
		if d.Layout == "fus" {
			res["fus_version"], res["stack_version"] = d.FUSVersion.String(), d.StackVersion.String()
		}
	} else {
		res["device_info"] = map[string]any{"layout": "none", "table_ptr": hexw(ptr)}
	}
	res["basis"] = "SRAM2a as the ROM bootloader left it: it starts CPU2's FUS, which writes the FUS-layout device information table (key 0xA94656B9) the reference table points at; ST documents the FUS version as the word at 0x20030030 (STM32_Programmer_CLI -r32 0x20030030). Read only"
	return s.mark(res), nil
}

// ---- leave ----

func (s *session) leave(ctx context.Context, args map[string]any) (map[string]any, error) {
	if !s.o.perm.reset {
		return nil, denied("leaving DFU needs allow_reset=true in the device configuration")
	}
	if err := tokenIs(args, "confirm", tokLeave, "leaving DFU (the bootloader jumps to the application)"); err != nil {
		return nil, err
	}
	alt := s.flashAltSetting()
	if alt == nil {
		return nil, status.Error(codes.FailedPrecondition, "no flash alternate setting at 0x08000000 to leave through")
	}
	if err := s.c.SelectAlt(ctx, alt.Alt); err != nil {
		return nil, err
	}
	if err := s.c.Leave(ctx, leaveAddr); err != nil {
		return nil, err
	}
	s.left = true
	return s.mark(map[string]any{
		"left_dfu": true, "jump_address": hexw(leaveAddr),
		"note": "the bootloader was told to leave DFU and jump to 0x08000000; the device leaves USB. With BOOT0 still selected (the dongle's switch/jumper) the next hardware reset or power cycle returns to the ROM bootloader",
	}), nil
}
