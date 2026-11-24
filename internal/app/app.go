// Package app holds the services shared by the CLI and the MCP server so both
// front ends behave identically: plugin/device discovery, capture runs,
// experiments, session queries, correlation, analysis and plotting.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/device"
	"github.com/faronov/hwlab/internal/plugin"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/status"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// App is the service layer.
type App struct {
	Cfg     config.Config
	Log     *slog.Logger
	Plugins *plugin.Manager
	Policy  policy.Policy

	baseCtx context.Context
	cancel  context.CancelFunc

	mu   sync.Mutex
	runs map[string]*Run
}

// New creates the services and discovers plugins.
func New(cfg config.Config, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		Cfg: cfg, Log: log, Policy: policy.RequireConfirmation{},
		Plugins: plugin.NewManager(plugin.Options{Paths: cfg.PluginPaths, CoreVersion: config.Version, Logger: log}),
		baseCtx: ctx, cancel: cancel, runs: map[string]*Run{},
	}
}

// Close finalises running captures and shuts plugins down.
func (a *App) Close(ctx context.Context) {
	a.mu.Lock()
	runs := make([]*Run, 0, len(a.runs))
	for _, r := range a.runs {
		runs = append(runs, r)
	}
	a.mu.Unlock()
	for _, r := range runs {
		_, _ = r.Finalize(ctx)
	}
	a.cancel()
	a.Plugins.Close(ctx)
}

// UseReplay registers the replay backend for a recorded session under the
// plugin id "replay", so experiments can use `plugin: replay`.
func (a *App) UseReplay(sessionRef string) error {
	dir, err := a.ResolveSession(sessionRef)
	if err != nil {
		return err
	}
	a.Plugins.RegisterBuiltin(replay.PluginID, func() (plugin.Backend, error) { return replay.New(dir) })
	return nil
}

// PluginView is the serialisable description of a plugin.
type PluginView struct {
	ID           string       `json:"id"`
	Name         string       `json:"name,omitempty"`
	Vendor       string       `json:"vendor,omitempty"`
	Version      string       `json:"version,omitempty"`
	Path         string       `json:"path,omitempty"`
	Builtin      bool         `json:"builtin,omitempty"`
	Capabilities []string     `json:"capabilities,omitempty"`
	Methods      []MethodView `json:"methods,omitempty"`
	Requirements []string     `json:"requirements,omitempty"`
	Error        string       `json:"error,omitempty"`
	// Support is the implementation/validation status from the core's support
	// matrix; nil when the plugin is not classified.
	Support *SupportView `json:"support,omitempty"`
}

// SupportView is the machine-readable validation status of a plugin.
type SupportView struct {
	status.Summary
	Backends []status.Backend `json:"backends"`
}

// MethodView describes a plugin method.
type MethodView struct {
	Capability  string `json:"capability"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Dangerous   bool   `json:"dangerous"`
	// RequiresConfirmation is true when the core refuses the call without
	// confirm=true (every non-SAFE method).
	RequiresConfirmation bool   `json:"requires_confirmation"`
	Danger               string `json:"danger"`
	// ArgsSchema is the plugin's JSON schema for the arguments (registry only).
	ArgsSchema json.RawMessage `json:"args_schema,omitempty"`
}

// ListPlugins starts every discovered plugin and reports its metadata.
func (a *App) ListPlugins(ctx context.Context) []PluginView { return a.listPlugins(ctx, false) }

func (a *App) listPlugins(ctx context.Context, schemas bool) []PluginView {
	var out []PluginView
	for _, e := range a.Plugins.List(ctx) {
		v := PluginView{ID: e.ID, Path: e.Path, Builtin: e.Builtin}
		if e.Err != nil {
			v.Error = e.Err.Error()
		}
		if i := e.Info; i != nil {
			v.Name, v.Vendor, v.Version = i.GetName(), i.GetVendor(), i.GetVersion()
			for _, c := range i.GetCapabilities() {
				v.Capabilities = append(v.Capabilities, c.GetId())
			}
			for _, m := range i.GetMethods() {
				mv := MethodView{Capability: m.GetCapability(), Name: m.GetName(),
					Description: m.GetDescription(), Dangerous: m.GetDanger() != pluginv1.Danger_DANGER_SAFE,
					RequiresConfirmation: m.GetDanger() != pluginv1.Danger_DANGER_SAFE, Danger: dangerName(m.GetDanger())}
				if schemas && m.GetArgsSchemaJson() != "" {
					mv.ArgsSchema = json.RawMessage(m.GetArgsSchemaJson())
				}
				v.Methods = append(v.Methods, mv)
			}
			for _, r := range i.GetRuntimeRequirements() {
				v.Requirements = append(v.Requirements, r.GetName())
			}
		}
		if ent, ok := status.Lookup(e.ID); ok {
			v.Support = &SupportView{Summary: ent.Summary(), Backends: ent.Backends}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DeviceError is a plugin that could not enumerate devices.
type DeviceError struct {
	Plugin string `json:"plugin"`
	Error  string `json:"error"`
}

// ListDevices aggregates devices from every plugin. A failing plugin is
// reported in the second result instead of failing the whole listing.
func (a *App) ListDevices(ctx context.Context) ([]device.Device, []DeviceError) {
	ids := a.Plugins.IDs()
	var (
		mu   sync.Mutex
		devs []device.Device
		errs []DeviceError
		wg   sync.WaitGroup
	)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			ds, err := a.pluginDevices(cctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, DeviceError{Plugin: id, Error: err.Error()})
				return
			}
			devs = append(devs, ds...)
		}()
	}
	wg.Wait()
	sort.Slice(devs, func(i, j int) bool { return devs[i].ID < devs[j].ID })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Plugin < errs[j].Plugin })
	return devs, errs
}

func (a *App) pluginDevices(ctx context.Context, id string) ([]device.Device, error) {
	b, err := a.Plugins.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	pd, err := b.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]device.Device, 0, len(pd))
	for _, d := range pd {
		out = append(out, device.FromProto(id, d))
	}
	return out, nil
}

// InvokeRequest calls a capability method on a device.
type InvokeRequest struct {
	Plugin     string         `json:"plugin"`
	Device     string         `json:"device"`
	Capability string         `json:"capability"`
	Method     string         `json:"method"`
	Args       map[string]any `json:"args,omitempty"`
	Confirm    bool           `json:"confirm,omitempty"`
	// ConfineFiles restricts file arguments to the artifacts directory (MCP).
	ConfineFiles bool `json:"-"`
}

// Invoke calls a declared plugin method. Undeclared methods are refused and
// dangerous ones require confirmation.
func (a *App) Invoke(ctx context.Context, req InvokeRequest) (map[string]any, error) {
	b, err := a.Plugins.Get(ctx, req.Plugin)
	if err != nil {
		return nil, err
	}
	var spec *pluginv1.MethodSpec
	for _, m := range b.Info().GetMethods() {
		if m.GetCapability() == req.Capability && m.GetName() == req.Method {
			spec = m
		}
	}
	if spec == nil {
		return nil, apperr.Newf(apperr.Unsupported, "plugin %q does not declare method %s.%s", req.Plugin, req.Capability, req.Method)
	}
	devs, err := a.pluginDevices(ctx, req.Plugin)
	if err != nil {
		return nil, err
	}
	dev, err := device.Select(devs, req.Plugin, req.Device)
	if err != nil {
		return nil, err
	}
	if spec.GetDanger() != pluginv1.Danger_DANGER_SAFE {
		act := policy.Action{Kind: policy.KindInvoke, Plugin: req.Plugin, Device: dev.LocalID, Capability: req.Capability, Method: req.Method}
		if err := a.Policy.Authorize(act, req.Confirm); err != nil {
			return nil, err
		}
	}
	if err := a.prepareArgs(spec, &req.Args, req.ConfineFiles); err != nil {
		return nil, err
	}
	pb := &pluginv1.InvokeRequest{DeviceId: dev.LocalID, Capability: req.Capability, Method: req.Method}
	if len(req.Args) > 0 {
		if pb.Args, err = toStruct(req.Args); err != nil {
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

var errNoSuchSession = errors.New("no such session")

func dangerName(d pluginv1.Danger) string {
	return strings.ToLower(strings.TrimPrefix(d.String(), "DANGER_"))
}

// TargetOpView is one generic target operation and the plugin method it calls.
type TargetOpView struct {
	Op         string `json:"op"`
	Capability string `json:"capability"`
	Method     string `json:"method"`
}

// Registry is the machine-readable catalogue of what the core can do:
// generic target operations, every plugin method with its danger level and
// argument schema, the support/validation status of each backend, and the
// error codes. The CLI and MCP render the same value.
type Registry struct {
	TargetOps  []TargetOpView    `json:"target_ops"`
	Plugins    []PluginView      `json:"plugins"`
	ErrorCodes map[string]string `json:"error_codes"`
}

// Registry builds the catalogue.
func (a *App) Registry(ctx context.Context) Registry {
	r := Registry{Plugins: a.listPlugins(ctx, true), ErrorCodes: ErrorCodes}
	for _, op := range TargetOpNames() {
		c := TargetOps[op]
		r.TargetOps = append(r.TargetOps, TargetOpView{Op: op, Capability: c[0], Method: c[1]})
	}
	return r
}
