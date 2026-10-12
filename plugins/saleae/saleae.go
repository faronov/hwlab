// Package saleae is the hwlab plugin for Saleae Logic analyzers, driven through
// the Logic 2 desktop application's automation gRPC API (default
// 127.0.0.1:10430, enabled in Logic 2's preferences or with --automation).
//
// Logic 2 captures into its own buffer and hands over finished data, so this
// plugin is capture-and-export oriented: it advertises TimedCapture and
// OfflineCapture, never LiveRawStream. A capture is started, waited for (or
// stopped), saved as a native .sal artifact, exported to CSV and converted to
// timeline events (digital edges, analog samples, decoded analyzer rows).
// Logic 2 and hwlab must share a filesystem because Logic 2 writes the files.
package saleae

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/saleae/internal/saleaepb"
)

// DefaultAddr is the Logic 2 automation endpoint.
const DefaultAddr = "127.0.0.1:10430"

// Environment overrides.
const (
	AddrEnv       = "HWLAB_SALEAE_ADDR"
	SimulationEnv = "HWLAB_SALEAE_SIMULATION" // "1": also list Logic 2 simulation devices
)

const rpcTimeout = 5 * time.Second

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	addr    string
	timeout time.Duration
	sim     bool

	mu   sync.Mutex
	busy map[string]bool
}

// New returns the plugin using $HWLAB_SALEAE_ADDR or DefaultAddr.
func New() *Plugin {
	addr := os.Getenv(AddrEnv)
	if addr == "" {
		addr = DefaultAddr
	}
	p := &Plugin{addr: addr, timeout: rpcTimeout, busy: map[string]bool{}}
	p.sim = os.Getenv(SimulationEnv) == "1"
	return p
}

// NewWithAddr targets a specific endpoint (tests).
func NewWithAddr(addr string) *Plugin {
	return &Plugin{addr: addr, timeout: 2 * time.Second, busy: map[string]bool{}}
}

// WithSimulation lists Logic 2 simulation devices too.
func (p *Plugin) WithSimulation() *Plugin { p.sim = true; return p }

func caps(ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(pluginv1.CapDigitalCapture, pluginv1.CapAnalogCapture, pluginv1.CapTimedCapture,
		pluginv1.CapOfflineCapture, pluginv1.CapArtifactExport)
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "saleae", Name: "Saleae Logic 2 automation", Vendor: "hwlab", Version: "0.2.0",
		Capabilities: deviceCaps(),
		RuntimeRequirements: []*pluginv1.RuntimeRequirement{{Name: "Saleae Logic 2", Kind: "service", Optional: false,
			Satisfied: p.reachable(),
			Description: "Install Logic 2 and enable the automation server (Preferences > Automation, or --automation); endpoint " +
				p.addr + ", override with " + AddrEnv + ". Logic 2 and hwlab must share a filesystem."}},
		Methods: []*pluginv1.MethodSpec{{Capability: pluginv1.CapDigitalCapture, Name: "app_info",
			Description: "report the Logic 2 application and automation API versions", Danger: pluginv1.Danger_DANGER_SAFE}},
	}
}

func (p *Plugin) reachable() bool {
	c, err := net.DialTimeout("tcp", p.addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	return c.Close() == nil
}

func (p *Plugin) dial(ctx context.Context) (*grpc.ClientConn, saleaepb.ManagerClient, error) {
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, saleaepb.NewManagerClient(conn), nil
}

func deviceName(t saleaepb.DeviceType) string {
	switch t {
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC:
		return "Saleae Logic"
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC_4:
		return "Saleae Logic 4"
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC_8:
		return "Saleae Logic 8"
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC_16:
		return "Saleae Logic 16"
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC_PRO_8:
		return "Saleae Logic Pro 8"
	case saleaepb.DeviceType_DEVICE_TYPE_LOGIC_PRO_16:
		return "Saleae Logic Pro 16"
	}
	return "Saleae device"
}

// ListDevices implements pluginsdk.Plugin. It asks Logic 2 for the attached
// analyzers; with Logic 2 not running (nothing listens on the endpoint) or no
// device attached it returns no devices rather than an error.
func (p *Plugin) ListDevices(ctx context.Context) (_ []*pluginv1.Device, retErr error) {
	conn, mgr, err := p.dial(ctx)
	if err != nil {
		return nil, nil
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// WaitForReady is off: a closed port must fail fast.
	reply, err := mgr.GetDevices(cctx, &saleaepb.GetDevicesRequest{IncludeSimulationDevices: p.sim})
	if err != nil {
		return nil, nil
	}
	var info *saleaepb.AppInfo
	if r, err := mgr.GetAppInfo(cctx, &saleaepb.GetAppInfoRequest{}); err == nil {
		info = r.GetAppInfo()
	}
	var out []*pluginv1.Device
	for _, d := range reply.GetDevices() {
		props := map[string]string{"endpoint": p.addr, "device_type": d.GetDeviceType().String(), "simulation": strconv.FormatBool(d.GetIsSimulation())}
		if info != nil {
			props["logic2_version"] = info.GetApplicationVersion()
		}
		out = append(out, &pluginv1.Device{Id: d.GetDeviceId(), Name: deviceName(d.GetDeviceType()), Serial: d.GetDeviceId(),
			Transport: "grpc", Capabilities: deviceCaps(), Properties: props})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out, nil
}

// OpenDevice implements pluginsdk.Plugin. It checks that Logic 2 is reachable
// and still lists the device.
func (p *Plugin) OpenDevice(ctx context.Context, id string, _ map[string]any) (retErr error) {
	conn, mgr, err := p.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	reply, err := mgr.GetDevices(cctx, &saleaepb.GetDevicesRequest{IncludeSimulationDevices: p.sim})
	if err != nil {
		return fmt.Errorf("logic 2 automation endpoint %s is not reachable: %w (start Logic 2 with the automation server enabled)", p.addr, err)
	}
	for _, d := range reply.GetDevices() {
		if d.GetDeviceId() == id {
			return nil
		}
	}
	return fmt.Errorf("logic 2 does not list device %q (is it connected and not in use by the UI?)", id)
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(context.Context, string) error { return nil }

func (p *Plugin) lock(id string) (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[id] {
		return nil, fmt.Errorf("device %q already has a capture running", id)
	}
	p.busy[id] = true
	return func() { p.mu.Lock(); delete(p.busy, id); p.mu.Unlock() }, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (_ *pluginv1.InvokeResponse, retErr error) {
	if req.GetCapability()+"."+req.GetMethod() != pluginv1.CapDigitalCapture+".app_info" {
		return nil, fmt.Errorf("unsupported method %s.%s", req.GetCapability(), req.GetMethod())
	}
	conn, mgr, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	r, err := mgr.GetAppInfo(cctx, &saleaepb.GetAppInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("logic 2 automation endpoint %s: %w", p.addr, err)
	}
	ai := r.GetAppInfo()
	v := ai.GetApiVersion()
	res, err := structpb.NewStruct(map[string]any{
		"application_version": ai.GetApplicationVersion(),
		"api_version":         fmt.Sprintf("%d.%d.%d", v.GetMajor(), v.GetMinor(), v.GetPatch()),
		"launch_pid":          float64(ai.GetLaunchPid()),
		"endpoint":            p.addr,
	})
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: res}, nil
}

var _ pluginsdk.Plugin = (*Plugin)(nil)
