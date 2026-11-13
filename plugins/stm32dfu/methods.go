package stm32dfuplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

const opTimeout = 30 * time.Second

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const devProps = `"timeout_ms":{"type":"number","description":"bound of one DFU command (download + status polling), default 3000"},` +
	`"allow_flash":{"type":"boolean","description":"DANGEROUS config: permit flash programming/restore (STM32WB5x user flash below SFSA only)"},` +
	`"allow_erase":{"type":"boolean","description":"DANGEROUS config: permit page erase"},` +
	`"allow_reset":{"type":"boolean","description":"DANGEROUS config: permit leaving DFU (the bootloader jumps to 0x08000000)"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	I, D, M, F := pluginv1.CapTargetIdentify, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	readArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps +
		`,"address":{"description":"number or string like \"0x20030030\""},"length":{"type":"number","description":"bytes, 1..4096 inline"},` +
		`"allow_peripheral":{"type":"boolean","description":"permit reads of peripherals (0x40000000-0x5FFFFFFF) and the system region (0xE0000000-); default false"},` +
		`"path":{"type":"string","description":"write the bytes to this new file (up to 16 MiB) instead of returning hex"},"overwrite":{"type":"boolean"}}}`
	flashReadArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps + `,"address":{},"length":{"type":"number"},"path":{"type":"string"},"overwrite":{"type":"boolean"}}}`
	flashImgArgs := `{"type":"object","required":["path","address"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{}}}`
	eraseArgs := `{"type":"object","required":["mode","address","length","confirm","backup"],"properties":{` + devProps + `,"mode":{"type":"string","enum":["region"]},"address":{},"length":{"type":"number"},"confirm":{"type":"string"},"backup":{"type":"string"}}}`
	progArgs := `{"type":"object","required":["path","address","confirm","backup"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{},"confirm":{"type":"string"},"backup":{"type":"string"}}}`
	restoreArgs := `{"type":"object","required":["path","confirm"],"properties":{` + devProps + `,"path":{"type":"string"},"confirm":{"type":"string"}}}`
	leaveArgs := `{"type":"object","required":["confirm"],"properties":{` + devProps + `,"confirm":{"type":"string"}}}`
	return &pluginv1.PluginInfo{
		Id: "stm32dfu", Name: "STM32 ROM USB DFU bootloader (DfuSe)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities:        deviceCaps(),
		DangerousConfigKeys: permKeys,
		Methods: []*pluginv1.MethodSpec{
			spec(D, safe, "info", "USB descriptors, bootloader version (bcdDevice), DFU functional descriptor and the alternate-setting memory map; claims nothing when the device is not open", obj(devProps)),
			spec(I, safe, "identify", "chip identity (DBGMCU_IDCODE when readable, else the memory map), flash size, UID64 and the STM32WB secure boundary (SFSA) read from the option bytes. Read only", obj(devProps)),
			spec(D, safe, "chip_info", "same as TargetIdentify.identify plus the DFU state and the DfuSe command list", obj(devProps)),
			spec(M, safe, "read", "DfuSe UPLOAD from any readable address (SRAM, option bytes, OTP, system memory, flash below the secure start); max 4096 bytes inline or up to 16 MiB to a new file with path (SHA-256 returned). Flash at or above SFSA*4K and the boot alias are refused", readArgs),
			spec(F, safe, "info", "STM32WB5x flash geometry, SFSA, user area, option bytes and protection decoded read-only; reports whether the boundary is established (otherwise the plugin stays read-only)", obj(devProps)),
			spec(F, safe, "read", "read the user flash below the secure start: max 4096 bytes inline, or up to 16 MiB to a new file with path (+overwrite); SHA-256 returned", flashReadArgs),
			spec(F, safe, "verify", "compare a file with the user flash at address without writing; any difference is an error", flashImgArgs),
			spec(F, confirm, "backup", "read the WHOLE user flash below the secure start twice, require identical SHA-256, store it (mode 0600) with a manifest in dir (default $HWLAB_HOME/artifacts/stm32dfu-flash); never modifies flash. The artifact stays local and must never be restored onto a joined Zigbee device", obj(devProps+`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "page-erase [address, address+length) on 4 KiB page boundaries below the secure start: mode=region, confirm=\"erase-flash-region\", allow_flash+allow_erase and backup=<verified full backup .bin> whose affected pages match the flash. Mass erase is never sent", eraseArgs),
			spec(F, confirm, "program", "program an image at address, changing only the covering pages (bytes outside the image are preserved), erasing a page only when a double word must change (needs allow_erase), read back and compared; needs allow_flash, confirm=\"program-flash\", backup=<verified full backup>. Option bytes are never written", progArgs),
			spec(F, confirm, "restore", "restore a verified full user-area backup (path) taken under the same SFSA: changes only differing pages, then compares the whole user-area SHA-256; needs allow_flash+allow_erase and confirm=\"restore-flash\". Never restore onto a device that has joined a Zigbee network", restoreArgs),
			spec(F, safe, "fus_info", "STM32WB: the secure boundary from the option bytes and the device information table in SRAM2a (reference table 0x20030000, FUS version word 0x20030030) decoded like the ST-LINK plugin. Read only", obj(devProps)),
			spec(F, confirm, "leave", "leave DFU: set the address pointer to 0x08000000, zero-length download, GETSTATUS; the bootloader jumps to the application and the device leaves USB. With BOOT0 still selected the next hardware reset returns to the bootloader. Needs allow_reset and confirm=\"leave-dfu\"", leaveArgs),
		},
	}
}

var methods = map[string]bool{}

func init() {
	for _, m := range (*Plugin)(nil).Info().Methods {
		methods[m.Capability+"."+m.Name] = true
	}
}

func opTimeoutFor(method string) time.Duration {
	if strings.HasPrefix(method, "FlashProgramming.") {
		return 20 * time.Minute
	}
	return opTimeout
}

func goneErr(format string, a ...any) error {
	return status.Errorf(codes.Unavailable, "DEVICE_GONE: "+format, a...)
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, usb.ErrDisconnected):
		return goneErr("%v", err)
	case errors.Is(err, dfu.ErrRefused):
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	if !methods[method] {
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	id := req.GetDeviceId()
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	if method == "DebugProbe.info" {
		if res, ok, err := p.staticInfo(id, args); err != nil {
			return nil, err
		} else if ok {
			return respond(res)
		}
	}
	if method == "MemoryAccess.read" {
		if _, err := parseRead(args); err != nil {
			return nil, err
		}
	}
	s, release, err := p.lease(ctx, id, args)
	if err != nil {
		return nil, wrap(err)
	}
	defer release()
	res, err := p.run(ctx, s, method, args)
	if err != nil {
		if errors.Is(err, usb.ErrDisconnected) {
			p.drop(id, s)
		}
		return nil, wrap(err)
	}
	if s.left {
		p.drop(id, s)
	}
	return respond(res)
}

func (p *Plugin) run(ctx context.Context, s *session, method string, args map[string]any) (res map[string]any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "device is closed")
	}
	if s.left {
		return nil, goneErr("the device left DFU")
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeoutFor(method))
	defer cancel()
	return s.dispatch(cctx, method, args)
}

func (s *session) dispatch(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	switch method {
	case "DebugProbe.info":
		return s.mark(s.dfuFacts()), nil
	case "TargetIdentify.identify":
		return s.identify(ctx, false)
	case "DebugProbe.chip_info":
		return s.identify(ctx, true)
	case "MemoryAccess.read":
		return s.readMem(ctx, args)
	case "FlashProgramming.info":
		return s.flashInfo(ctx)
	case "FlashProgramming.read":
		return s.flashRead(ctx, args)
	case "FlashProgramming.verify":
		return s.flashVerify(ctx, args)
	case "FlashProgramming.backup":
		return s.flashBackup(ctx, args)
	case "FlashProgramming.erase":
		return s.flashErase(ctx, args)
	case "FlashProgramming.program":
		return s.flashProgram(ctx, args)
	case "FlashProgramming.restore":
		return s.flashRestore(ctx, args)
	case "FlashProgramming.fus_info":
		return s.fusInfo(ctx)
	case "FlashProgramming.leave":
		return s.leave(ctx, args)
	}
	return nil, status.Errorf(codes.Unimplemented, "method %s", method)
}

func respond(res map[string]any) (*pluginv1.InvokeResponse, error) {
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

// mark adds the evidence labels every answer carries.
func (s *session) mark(res map[string]any) map[string]any {
	res["target_verified"] = s.entry.Sim == nil
	if s.entry.Sim != nil {
		res["synthetic"] = true
	}
	res["usb_serial"] = s.entry.Serial
	res["device"] = s.entry.ID
	return res
}

func usbFacts(e Entry) map[string]any {
	d := e.Dev
	res := map[string]any{
		"vid": fmt.Sprintf("%04x", d.VID), "pid": fmt.Sprintf("%04x", d.PID),
		"manufacturer": d.Manufacturer, "product": d.Product, "serial": e.Serial,
		"bootloader_version": bcd(d.BCD), "bcd_device": fmt.Sprintf("0x%04x", d.BCD),
		"target_protocol": "USB DFU 1.1 + ST DfuSe (STM32 system-memory bootloader, AN3156)",
		"memory_access":   "read only (UPLOAD); never at or above the STM32WB secure start",
		"flash":           "STM32WB5x user flash below SFSA: page erase/program (allow_flash/allow_erase, confirmation tokens, a verified backup)",
		"reset":           "leave DFU only (allow_reset, confirm=leave-dfu)",
	}
	var ifs []any
	for _, i := range d.Interfaces {
		ifs = append(ifs, map[string]any{"number": float64(i.Number), "class": fmt.Sprintf("0x%02x", i.Class), "driver": i.Driver, "name": i.Name})
	}
	res["interfaces"] = ifs
	if e.Sim != nil {
		res["synthetic"] = true
	}
	return res
}

// staticInfo answers info for a device that is not open without claiming
// anything.
func (p *Plugin) staticInfo(id string, args map[string]any) (map[string]any, bool, error) {
	p.mu.Lock()
	open := p.devs[id] != nil
	p.mu.Unlock()
	if open {
		return nil, false, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, false, err
	}
	over := map[string]any{}
	for _, k := range deviceKeys {
		if v, ok := args[k]; ok {
			over[k] = v
		}
	}
	if _, err := parseOptions(defaultOptions(), over); err != nil {
		return nil, false, status.Error(codes.InvalidArgument, err.Error())
	}
	res := usbFacts(e)
	res["open"] = false
	res["note"] = "device not open: nothing was claimed; call identify for the descriptors, memory map and secure boundary"
	return res, true, nil
}
