package ftdiplugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/jtag"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/ftdi/profiles"
)

const (
	opTimeout = 15 * time.Second
	// maxShiftBits bounds one raw IR/DR scan.
	maxShiftBits = 8192
)

func durationMs(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func isErr(err, target error) bool { return errors.Is(err, target) }

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const devProps = `"profile":{"type":"string","description":"adapter profile; default is chosen from the USB strings"},` +
	`"interface":{"type":"number","description":"FTDI channel carrying MPSSE, default from the profile (0 = A)"},` +
	`"clock_hz":{"type":"number","description":"requested TCK; ignored by firmware with a fixed clock"},` +
	`"detach_kernel_driver":{"type":"boolean","description":"detach ftdi_sio from the JTAG interface only, default true"},` +
	`"latency_ms":{"type":"number"},"timeout_ms":{"type":"number"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	T, S, I := pluginv1.CapJTAGTransport, pluginv1.CapJTAGScan, pluginv1.CapTargetIdentify
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	shiftArgs := `{"type":"object","required":["bits"],"properties":{` + devProps +
		`,"bits":{"type":"number","description":"number of bits, 1..8192"},` +
		`"data":{"type":"string","description":"hex, LSB-first byte order; default all ones"},` +
		`"capture":{"type":"boolean","description":"return the bits shifted out, default true"}}}`
	scanProps := devProps + `,"max_devices":{"type":"number","description":"bound on chain length, default 32"},` +
		`"measure_ir":{"type":"boolean","description":"also measure the total IR length and TAP count (loads BYPASS)"},` +
		`"ir_lengths":{"type":"array","items":{"type":"number"},"description":"expected IR length per TAP, nearest TDO first"}`
	return &pluginv1.PluginInfo{
		Id: "ftdi", Name: "FTDI MPSSE JTAG adapter (native)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: deviceCapsGeneric(),
		Methods: []*pluginv1.MethodSpec{
			spec(T, safe, "info", "adapter, profile, interface mapping and (when open) the MPSSE facts; claims nothing when the device is not open", obj(devProps)),
			spec(T, safe, "selftest", "MPSSE sync probe and loopback check; drives only TCK/TDI with the TAP idle", obj(devProps)),
			spec(T, safe, "tap_reset", "force the TAP chain to Test-Logic-Reset (five TCK cycles with TMS high); touches no target register", obj(devProps)),
			spec(T, confirm, "shift_ir", "shift bits through the instruction registers and return what came out; loads an instruction into every TAP", shiftArgs),
			spec(T, confirm, "shift_dr", "shift bits through the selected data registers and return what came out; may change target state", shiftArgs),
			spec(S, safe, "scan", "reset the TAPs and enumerate the chain from the IDCODE/BYPASS stream; repeatable, writes nothing to any target", obj(scanProps)),
			spec(I, safe, "identify", "list the IDCODEs of the chain with decoded version/part/manufacturer; JTAG identification only, no CPU or memory access", obj(scanProps)),
		},
	}
}

func deviceCapsGeneric() []*pluginv1.Capability {
	return deviceCaps(profiles.Generic)
}

func wrap(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, transportErr(err).Error())
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	switch method {
	case "JTAGTransport.info", "JTAGTransport.selftest", "JTAGTransport.tap_reset", "JTAGTransport.shift_ir", "JTAGTransport.shift_dr",
		"JTAGScan.scan", "TargetIdentify.identify":
	default:
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	if method == "JTAGTransport.info" {
		if res, ok, err := p.staticInfo(req.GetDeviceId(), args); err != nil {
			return nil, err
		} else if ok {
			return respond(res)
		}
	}
	// Per-call settings that are not device settings are method arguments.
	s, release, err := p.lease(ctx, req.GetDeviceId(), args)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "device is closed")
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	var res map[string]any
	switch method {
	case "JTAGTransport.info":
		res = s.facts()
	case "JTAGTransport.selftest":
		res, err = s.selftest(cctx)
	case "JTAGTransport.tap_reset":
		res, err = s.tapReset(cctx)
	case "JTAGTransport.shift_ir":
		res, err = s.shift(cctx, args, true)
	case "JTAGTransport.shift_dr":
		res, err = s.shift(cctx, args, false)
	case "JTAGScan.scan":
		res, err = s.scan(cctx, args)
	case "TargetIdentify.identify":
		res, err = s.identify(cctx, args)
	}
	if err != nil {
		return nil, wrap(err)
	}
	return respond(res)
}

func respond(res map[string]any) (*pluginv1.InvokeResponse, error) {
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

// staticInfo answers info for a device that is not open without claiming
// anything: it reads only enumeration data.
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
	o, err := parseOptions(defaultOptions(), args)
	if err != nil {
		return nil, false, status.Error(codes.InvalidArgument, err.Error())
	}
	prof, src, err := resolveProfile(e, o)
	if err != nil {
		return nil, false, status.Error(codes.InvalidArgument, err.Error())
	}
	res := profileFacts(e, prof, src)
	res["open"] = false
	res["note"] = "device not open: nothing was claimed; open it (or call selftest/scan) to learn the MPSSE facts"
	return res, true, nil
}

func profileFacts(e Entry, prof profiles.Profile, src string) map[string]any {
	d := e.Dev
	res := map[string]any{
		"vid": fmt.Sprintf("%04x", d.VID), "pid": fmt.Sprintf("%04x", d.PID),
		"manufacturer": d.Manufacturer, "product": d.Product, "serial": d.Serial,
		"profile": prof.Name, "profile_selection": src, "implementation": string(prof.Implementation),
		"jtag_interface": float64(prof.JTAGInterface), "jtag_channel": channelName(prof.JTAGInterface),
		"nominal_clock_hz": float64(prof.NominalClockHz), "validated": false,
		"layout": map[string]any{
			"tck": float64(prof.Layout.TCK), "tdi": float64(prof.Layout.TDI), "tdo": float64(prof.Layout.TDO), "tms": float64(prof.Layout.TMS),
			"nsrst_exposed": prof.Layout.NSRST >= 0, "ntrst_exposed": prof.Layout.NTRST >= 0,
		},
		"cpu_debug": false,
	}
	if prof.UARTInterface >= 0 {
		u := map[string]any{"interface": float64(prof.UARTInterface), "channel": channelName(prof.UARTInterface),
			"note": "not claimed by this plugin; use the uart plugin on the tty"}
		if ui, ok := d.Iface(prof.UARTInterface); ok {
			u["driver"] = ui.Driver
			if ui.TTY != "" {
				u["tty"] = "/dev/" + ui.TTY
			}
		}
		res["companion_uart"] = u
	}
	if e.Sim != nil {
		res["synthetic"] = true
	}
	if prof.Verified != "" {
		res["profile_verified"] = prof.Verified
	}
	return res
}

func (s *session) facts() map[string]any {
	res := profileFacts(s.entry, s.prof, s.profSrc)
	res["open"] = true
	var m map[string]any
	if b, err := json.Marshal(s.info); err == nil && json.Unmarshal(b, &m) == nil {
		res["adapter"] = m
	}
	res["jtag_interface"] = float64(s.iface)
	res["jtag_channel"] = channelName(s.iface)
	return res
}

func (s *session) selftest(ctx context.Context) (map[string]any, error) {
	res := map[string]any{}
	if err := s.eng.Sync(ctx); err != nil {
		res["sync"] = map[string]any{"ok": false, "error": err.Error()}
		return res, err
	}
	res["sync"] = map[string]any{"ok": true, "note": "bad-command probe 0xAA answered 0xFA 0xAA"}
	// Shifting with loopback on must not wander the TAP: park it in
	// Run-Test/Idle with TMS low first.
	if err := s.drv.GoTo(ctx, jtag.RunTestIdle); err != nil {
		return res, err
	}
	pattern := []byte{0xA5, 0x5A, 0x3C, 0xC3, 0x00, 0xFF, 0x81, 0x7E}
	lb, err := s.eng.LoopbackTest(ctx, pattern)
	if err != nil {
		return res, err
	}
	res["loopback"] = map[string]any{
		"supported": lb.Supported, "sent": hex.EncodeToString(lb.Sent), "received": hex.EncodeToString(lb.Received), "note": lb.Note,
	}
	res["clock_hz"] = float64(s.info.ClockHz)
	res["clock_fixed_by_firmware"] = s.info.ClockFixed
	if err := s.eng.Sync(ctx); err != nil {
		return res, fmt.Errorf("MPSSE lost sync after the loopback test: %w", err)
	}
	res["ok"] = true
	return res, nil
}

func (s *session) tapReset(ctx context.Context) (map[string]any, error) {
	if err := s.drv.Reset(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"state": s.drv.State().String(), "ok": true}, nil
}

func (s *session) shift(ctx context.Context, args map[string]any, ir bool) (map[string]any, error) {
	nf, ok, err := optNumber(args, "bits")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok || nf < 1 || nf > maxShiftBits || nf != math.Trunc(nf) {
		return nil, argErr(fmt.Errorf("bits: want an integer 1..%d", maxShiftBits))
	}
	n := int(nf)
	data := jtag.Ones(n)
	if v, ok := args["data"]; ok && v != nil {
		str, isStr := v.(string)
		if !isStr {
			return nil, argErr(fmt.Errorf("data: want a hex string"))
		}
		b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(str), "0x"))
		if err != nil {
			return nil, argErr(fmt.Errorf("data: %w", err))
		}
		if len(b) != (n+7)/8 {
			return nil, argErr(fmt.Errorf("data: %d bits need %d bytes, got %d", n, (n+7)/8, len(b)))
		}
		data = b
	}
	capture := true
	if v, ok := args["capture"]; ok {
		if b, isBool := v.(bool); isBool {
			capture = b
		} else {
			return nil, argErr(fmt.Errorf("capture: want true or false"))
		}
	}
	var out []byte
	if ir {
		out, err = s.drv.ShiftIR(ctx, data, n, capture)
	} else {
		out, err = s.drv.ShiftDR(ctx, data, n, capture)
	}
	if err != nil {
		return nil, err
	}
	res := map[string]any{"bits": float64(n), "state": s.drv.State().String(), "register": map[bool]string{true: "IR", false: "DR"}[ir]}
	if capture {
		res["out"] = hex.EncodeToString(out)
	}
	return res, nil
}

func (s *session) scanOpts(args map[string]any) (jtag.ScanOptions, error) {
	o := jtag.ScanOptions{MaxDevices: s.o.maxDevices}
	if v, ok, err := optNumber(args, "max_devices"); err != nil {
		return o, argErr(err)
	} else if ok {
		if v < 1 || v > 256 || v != math.Trunc(v) {
			return o, argErr(fmt.Errorf("max_devices %v: want 1..256", v))
		}
		o.MaxDevices = int(v)
	}
	if v, ok := args["measure_ir"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return o, argErr(fmt.Errorf("measure_ir: want true or false"))
		}
		o.MeasureIR = b
	}
	if v, ok := args["ir_lengths"]; ok && v != nil {
		arr, isArr := v.([]any)
		if !isArr {
			return o, argErr(fmt.Errorf("ir_lengths: want an array of numbers"))
		}
		for _, x := range arr {
			f, isNum := x.(float64)
			if !isNum || f < 1 || f > 64 || f != math.Trunc(f) {
				return o, argErr(fmt.Errorf("ir_lengths: every entry must be an integer 1..64"))
			}
			o.IRLengths = append(o.IRLengths, int(f))
		}
	}
	return o, nil
}

func chainMap(c jtag.Chain) (map[string]any, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m["devices"] == nil {
		m["devices"] = []any{}
	}
	return m, nil
}

func (s *session) scan(ctx context.Context, args map[string]any) (map[string]any, error) {
	opt, err := s.scanOpts(args)
	if err != nil {
		return nil, err
	}
	c, err := jtag.ScanChain(ctx, s.drv, opt)
	if err != nil {
		return nil, err
	}
	res, err := chainMap(c)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	res["count"] = float64(len(c.Devices))
	res["clock_hz"] = float64(s.info.ClockHz)
	res["profile"] = s.prof.Name
	res["target_verified"] = false
	if s.entry.Sim != nil {
		res["synthetic"] = true
	}
	return res, nil
}

func (s *session) identify(ctx context.Context, args map[string]any) (map[string]any, error) {
	opt, err := s.scanOpts(args)
	if err != nil {
		return nil, err
	}
	c, err := jtag.ScanChain(ctx, s.drv, opt)
	if err != nil {
		return nil, err
	}
	var targets []any
	for _, d := range c.Devices {
		t := map[string]any{"index": float64(d.Index)}
		if d.Bypass || d.IDCode == nil {
			t["bypass"] = true
		} else {
			id := d.IDCode
			t["idcode"] = fmt.Sprintf("0x%08x", id.Raw)
			t["version"] = float64(id.Version)
			t["part"] = fmt.Sprintf("0x%04x", id.Part)
			t["manufacturer"] = fmt.Sprintf("0x%03x", id.Manufacturer)
			if id.ManufacturerName != "" {
				t["manufacturer_name"] = id.ManufacturerName
			}
		}
		if d.IRLength > 0 {
			t["ir_length"] = float64(d.IRLength)
		}
		targets = append(targets, t)
	}
	res := map[string]any{
		"status": c.Status, "count": float64(len(c.Devices)), "targets": targets,
		"basis":           "JTAG IDCODE only; no CPU, memory or flash access",
		"target_verified": false,
	}
	if len(c.Notes) > 0 {
		var ns []any
		for _, n := range c.Notes {
			ns = append(ns, n)
		}
		res["notes"] = ns
	}
	if s.entry.Sim != nil {
		res["synthetic"] = true
	}
	return res, nil
}
