package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/device"
	"github.com/faronov/hwlab/internal/policy"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// TargetOps maps the generic target operations to the capability methods
// every debug-probe plugin (CC-Debugger, J-Link, CMSIS-DAP, Telink, ...)
// declares when it supports them.
var TargetOps = map[string][2]string{
	"info":         {pluginv1.CapDebugProbe, "info"},
	"identify":     {pluginv1.CapDebugProbe, "chip_info"},
	"attach":       {pluginv1.CapDebugProbe, "attach"},
	"halt":         {pluginv1.CapDebugProbe, "halt"},
	"resume":       {pluginv1.CapDebugProbe, "resume"},
	"reset":        {pluginv1.CapDebugProbe, "reset"},
	"registers":    {pluginv1.CapDebugProbe, "read_registers"},
	"memory-read":  {pluginv1.CapMemoryAccess, "read"},
	"memory-write": {pluginv1.CapMemoryAccess, "write"},
	"step":         {pluginv1.CapDebugProbe, "step"},
	"harts":        {pluginv1.CapDebugProbe, "harts"},
	"wait-halt":    {pluginv1.CapDebugProbe, "wait_halt"},
	"reconnect":    {pluginv1.CapDebugProbe, "reconnect"},
	// Hardware breakpoints/triggers (RISC-V debug targets).
	"breakpoint-set":   {pluginv1.CapDebugProbe, "set_breakpoint"},
	"breakpoint-clear": {pluginv1.CapDebugProbe, "clear_breakpoint"},
	"breakpoint-list":  {pluginv1.CapDebugProbe, "list_breakpoints"},
	// Data watchpoints (Cortex-M DWT) and probe clock.
	"watchpoint-set":   {pluginv1.CapDebugProbe, "set_watchpoint"},
	"watchpoint-clear": {pluginv1.CapDebugProbe, "clear_watchpoint"},
	"watchpoint-list":  {pluginv1.CapDebugProbe, "list_watchpoints"},
	"set-speed":        {pluginv1.CapDebugProbe, "set_speed"},
	"flash-info":       {pluginv1.CapFlashProgramming, "info"},
	"flash-read":       {pluginv1.CapFlashProgramming, "read"},
	"flash-program":    {pluginv1.CapFlashProgramming, "program"},
	"flash-verify":     {pluginv1.CapFlashProgramming, "verify"},
	"flash-erase":      {pluginv1.CapFlashProgramming, "erase"},
	"flash-backup":     {pluginv1.CapFlashProgramming, "backup"},
	"flash-restore":    {pluginv1.CapFlashProgramming, "restore"},
	// TI CC23xx (SACI boot ROM service interface, CCFG).
	"cc23xx-info":  {pluginv1.CapDebugProbe, "cc23xx_info"},
	"saci-status":  {pluginv1.CapDebugProbe, "saci_status"},
	"saci-enter":   {pluginv1.CapDebugProbe, "saci_enter"},
	"saci-exit":    {pluginv1.CapDebugProbe, "saci_exit"},
	"ccfg-info":    {pluginv1.CapFlashProgramming, "ccfg_info"},
	"ccfg-program": {pluginv1.CapFlashProgramming, "ccfg_program"},
	// JTAG probe transport (FTDI/MPSSE): enumerate and identify the TAP
	// chain. Neither touches target registers or memory.
	"jtag-scan":     {pluginv1.CapJTAGScan, "scan"},
	"jtag-identify": {pluginv1.CapTargetIdentify, "identify"},
}

// TargetCall is one capability call inside a target session.
type TargetCall struct {
	Capability string         `json:"capability"`
	Method     string         `json:"method"`
	Args       map[string]any `json:"args,omitempty"`
}

// TargetRequest runs one generic target operation inside an ephemeral device
// session: the device is opened with Config, Pre calls run first, then the
// operation, then Post calls (also after a failure, to restore the target),
// and the device is closed again.
type TargetRequest struct {
	Plugin  string         `json:"plugin,omitempty"`
	Device  string         `json:"device,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
	Op      string         `json:"op"`
	Args    map[string]any `json:"args,omitempty"`
	Pre     []TargetCall   `json:"pre,omitempty"`
	Post    []TargetCall   `json:"post,omitempty"`
	Confirm bool           `json:"confirm,omitempty"`
	// ConfineFiles restricts file arguments to the artifacts directory
	// (set by the MCP frontend, which must not read or write arbitrary host files).
	ConfineFiles bool `json:"-"`
}

// TargetStep is the outcome of one call.
type TargetStep struct {
	Call   string         `json:"call"`
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// TargetResult is the outcome of a target session.
type TargetResult struct {
	Plugin string         `json:"plugin"`
	Device string         `json:"device"`
	Pre    []TargetStep   `json:"pre,omitempty"`
	Result map[string]any `json:"result,omitempty"`
	Post   []TargetStep   `json:"post,omitempty"`
}

// TargetOpNames lists the supported generic operations.
func TargetOpNames() []string {
	out := make([]string, 0, len(TargetOps))
	for k := range TargetOps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pickTarget finds the debug-probe device when no plugin was named.
func (a *App) pickTarget(ctx context.Context, plug, dev, capName string) (device.Device, error) {
	if plug != "" {
		devs, err := a.pluginDevices(ctx, plug)
		if err != nil {
			return device.Device{}, err
		}
		return device.Select(devs, plug, dev)
	}
	all, _ := a.ListDevices(ctx)
	var cands []device.Device
	for _, d := range all {
		if d.Has(capName) && (dev == "" || d.ID == dev || d.LocalID == dev || d.Serial == dev || d.Name == dev) {
			cands = append(cands, d)
		}
	}
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		return device.Device{}, apperr.Newf(apperr.NotFound, "no device with capability %s found", capName)
	}
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	return device.Device{}, apperr.Newf(apperr.Ambiguous, "%d devices have %s (%s): name one with --plugin/--device", len(cands), capName, strings.Join(ids, ", "))
}

// Target runs a generic target operation. Declared methods only; dangerous
// calls and dangerous session configuration need Confirm.
func (a *App) Target(ctx context.Context, req TargetRequest) (*TargetResult, error) {
	op, ok := TargetOps[req.Op]
	if !ok {
		return nil, apperr.Newf(apperr.InvalidArgument, "unknown target operation %q (have: %s)", req.Op, strings.Join(TargetOpNames(), ", "))
	}
	d, err := a.pickTarget(ctx, req.Plugin, req.Device, op[0])
	if err != nil {
		return nil, err
	}
	b, err := a.Plugins.Get(ctx, d.PluginID)
	if err != nil {
		return nil, err
	}
	info := b.Info()
	declared := func(c TargetCall) (*pluginv1.MethodSpec, error) {
		for _, m := range info.GetMethods() {
			if m.GetCapability() == c.Capability && m.GetName() == c.Method {
				return m, nil
			}
		}
		return nil, apperr.Newf(apperr.Unsupported, "plugin %q does not support %s.%s", d.PluginID, c.Capability, c.Method)
	}
	main := TargetCall{Capability: op[0], Method: op[1], Args: req.Args}
	pre := append([]TargetCall{}, req.Pre...)
	post := append([]TargetCall{}, req.Post...)
	// Every call is authorized, then its arguments are validated, before the
	// device is opened: a refused or malformed request never touches hardware.
	prepare := func(c *TargetCall) error {
		spec, err := declared(*c)
		if err != nil {
			return err
		}
		if spec.GetDanger() != pluginv1.Danger_DANGER_SAFE {
			act := policy.Action{Kind: policy.KindInvoke, Plugin: d.PluginID, Device: d.LocalID, Capability: c.Capability, Method: c.Method}
			if err := a.Policy.Authorize(act, req.Confirm); err != nil {
				return err
			}
		}
		return a.prepareArgs(spec, &c.Args, req.ConfineFiles)
	}
	if err := prepare(&main); err != nil {
		return nil, err
	}
	for i := range pre {
		if err := prepare(&pre[i]); err != nil {
			return nil, err
		}
	}
	for i := range post {
		if err := prepare(&post[i]); err != nil {
			return nil, err
		}
	}
	var danger []string
	for _, k := range info.GetDangerousConfigKeys() {
		if _, ok := req.Config[k]; ok {
			danger = append(danger, k)
		}
	}
	if len(danger) > 0 {
		sort.Strings(danger)
		act := policy.Action{Kind: policy.KindConfig, Plugin: d.PluginID, Device: d.LocalID, ConfigKeys: danger}
		if err := a.Policy.Authorize(act, req.Confirm); err != nil {
			return nil, err
		}
	}
	if err := b.OpenDevice(ctx, d.LocalID, req.Config); err != nil {
		return nil, fmt.Errorf("open %s: %w", d.ID, err)
	}
	defer func() { _ = b.CloseDevice(context.WithoutCancel(ctx), d.LocalID) }()

	run := func(c TargetCall) (map[string]any, error) {
		pb := &pluginv1.InvokeRequest{DeviceId: d.LocalID, Capability: c.Capability, Method: c.Method}
		if len(c.Args) > 0 {
			if pb.Args, err = toStruct(c.Args); err != nil {
				return nil, err
			}
		}
		resp, err := b.Invoke(ctx, pb)
		if err != nil {
			return nil, err
		}
		if resp.GetResult() == nil {
			return map[string]any{}, nil
		}
		return resp.GetResult().AsMap(), nil
	}
	res := &TargetResult{Plugin: d.PluginID, Device: d.LocalID}
	var mainErr error
	for _, c := range pre {
		r, err := run(c)
		st := TargetStep{Call: c.Capability + "." + c.Method, Result: r}
		if err != nil {
			st.Error = err.Error()
			mainErr = err
		}
		res.Pre = append(res.Pre, st)
		if mainErr != nil {
			break
		}
	}
	if mainErr == nil {
		res.Result, mainErr = run(main)
	}
	// Post calls restore the target; they run even after a failure.
	for _, c := range post {
		r, err := run(c)
		st := TargetStep{Call: c.Capability + "." + c.Method, Result: r}
		if err != nil {
			st.Error = err.Error()
		}
		res.Post = append(res.Post, st)
	}
	if mainErr != nil {
		return res, mainErr
	}
	return res, nil
}
