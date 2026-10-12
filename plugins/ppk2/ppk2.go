// Package ppk2 is the hwlab plugin for the Nordic Power Profiler Kit II. The
// serial protocol lives in the protocol subpackage; this package turns it into
// the hwlab plugin service (discovery, lifecycle, capture, invoke).
package ppk2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usbserial"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ppk2/protocol"
)

// Nordic Semiconductor USB ids of the PPK2 (CDC-ACM).
const (
	VID = "1915"
	PID = "c00a"
)

const metadataTimeout = 2 * time.Second

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list usbserial.Lister
	open usbserial.Opener

	mu   sync.Mutex
	devs map[string]*device
}

// New returns a plugin using real serial ports.
func New() *Plugin { return NewWith(usbserial.List, usbserial.Open(115200)) }

// NewWith returns a plugin using the supplied port layer (for tests).
func NewWith(list usbserial.Lister, open usbserial.Opener) *Plugin {
	return &Plugin{list: list, open: open, devs: map[string]*device{}}
}

func caps(ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(pluginv1.CapPowerMeasurement, pluginv1.CapPowerSource, pluginv1.CapDigitalCapture,
		pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream)
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "ppk2", Name: "Nordic Power Profiler Kit II", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: deviceCaps(),
		Methods: []*pluginv1.MethodSpec{
			{Capability: pluginv1.CapPowerMeasurement, Name: "get_metadata", Description: "read calibration metadata", Danger: pluginv1.Danger_DANGER_SAFE},
			{Capability: pluginv1.CapPowerSource, Name: "set_voltage", Description: "set source-meter supply voltage (args: mv 800..5000)", Danger: pluginv1.Danger_DANGER_CONFIRM},
			{Capability: pluginv1.CapPowerSource, Name: "set_dut_power", Description: "switch the DUT supply on/off (args: on)", Danger: pluginv1.Danger_DANGER_CONFIRM},
		},
		// Driving a supply voltage into a DUT can destroy it.
		DangerousConfigKeys: []string{"supply_mv", "dut_power"},
	}
}

// ListDevices implements pluginsdk.Plugin. One device is reported per
// physical PPK2 (grouped by USB serial number); the serial ports belonging to
// it are listed in the "ports" property.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	infos, err := p.list()
	if err != nil {
		return nil, err
	}
	return devicesFrom(infos), nil
}

func devicesFrom(infos []usbserial.Info) []*pluginv1.Device {
	groups := map[string][]usbserial.Info{}
	for _, i := range usbserial.Match(infos, VID, PID) {
		id := i.SerialNumber
		if id == "" {
			id = i.Name
		}
		groups[id] = append(groups[id], i)
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pluginv1.Device, 0, len(ids))
	for _, id := range ids {
		var names []string
		for _, i := range groups[id] {
			names = append(names, i.Name)
		}
		out = append(out, &pluginv1.Device{
			Id: id, Name: "Power Profiler Kit II", Serial: groups[id][0].SerialNumber, Transport: "usb-serial",
			Capabilities: deviceCaps(),
			Properties:   map[string]string{"ports": strings.Join(names, ","), "vid": VID, "pid": PID},
		})
	}
	return out
}

func (p *Plugin) candidatePorts(id string, cfg map[string]any) ([]string, error) {
	if port, _ := cfg["port"].(string); port != "" {
		return []string{port}, nil
	}
	infos, err := p.list()
	if err != nil {
		return nil, err
	}
	for _, d := range devicesFrom(infos) {
		if d.GetId() == id {
			return strings.Split(d.GetProperties()["ports"], ","), nil
		}
	}
	return nil, fmt.Errorf("ppk2 device %q not found (is it plugged in?)", id)
}

// OpenDevice implements pluginsdk.Plugin. The PPK2 enumerates two CDC
// interfaces; each candidate port is probed with GET_META_DATA and the one
// that answers is used.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.devs[id]; ok {
		return fmt.Errorf("device %q already open", id)
	}
	dev, err := p.connect(id, cfg)
	if err != nil {
		return err
	}
	p.devs[id] = dev
	return nil
}

func (p *Plugin) connect(id string, cfg map[string]any) (*device, error) {
	ports, err := p.candidatePorts(id, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := parseOptions(cfg); err != nil {
		return nil, err
	}
	var errs []error
	for _, name := range ports {
		port, err := p.open(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		dev, err := newDevice(port, name, cfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, errors.Join(err, port.Close())))
			continue
		}
		return dev, nil
	}
	return nil, fmt.Errorf("ppk2 %q: no port answered: %w", id, errors.Join(errs...))
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	dev := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if dev == nil {
		return nil
	}
	return dev.close()
}

func (p *Plugin) device(id string) (*device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.devs[id]
	if d == nil {
		return nil, fmt.Errorf("device %q is not open", id)
	}
	if d.broken.Load() && !d.capturing.Load() {
		// The port failed (unplug, USB reset). Reopen it; the device
		// forgets nothing we need, the configuration is reapplied per capture.
		_ = d.port.Close()
		nd, err := p.connect(id, d.cfg)
		if err != nil {
			return nil, fmt.Errorf("ppk2 %q reconnect failed: %w", id, err)
		}
		p.devs[id] = nd
		return nd, nil
	}
	return d, nil
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	dev, err := p.device(req.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	c, st, err := dev.startCapture(ctx, req, sink)
	if err != nil && dev.broken.Load() {
		// The port died since it was opened (unplug/replug): reconnect once.
		if dev, rerr := p.device(req.DeviceID); rerr == nil {
			return dev.startCapture(ctx, req, sink)
		}
	}
	return c, st, err
}

// Invoke implements pluginsdk.Plugin. Methods work on an open device, or open
// the port transiently when the device is not open.
func (p *Plugin) Invoke(_ context.Context, req *pluginv1.InvokeRequest) (_ *pluginv1.InvokeResponse, retErr error) {
	args := req.GetArgs().AsMap()
	dev, err := p.device(req.GetDeviceId())
	if err != nil {
		tmp, cerr := p.connect(req.GetDeviceId(), nil)
		if cerr != nil {
			return nil, cerr
		}
		defer func() { retErr = errors.Join(retErr, tmp.close()) }()
		dev = tmp
	}
	res := map[string]any{}
	switch req.GetCapability() + "." + req.GetMethod() {
	case pluginv1.CapPowerMeasurement + ".get_metadata":
		for k, v := range dev.metadata.Raw {
			res[k] = v
		}
	case pluginv1.CapPowerSource + ".set_voltage":
		mv, ok := toInt(args["mv"])
		if !ok || mv < protocol.VddMinMV || mv > protocol.VddMaxMV {
			return nil, fmt.Errorf("set_voltage: mv must be in [%d,%d]", protocol.VddMinMV, protocol.VddMaxMV)
		}
		if err := dev.setVoltage(mv); err != nil {
			return nil, err
		}
		res["mv"] = mv
	case pluginv1.CapPowerSource + ".set_dut_power":
		on, ok := args["on"].(bool)
		if !ok {
			return nil, errors.New("set_dut_power: on must be a boolean")
		}
		if err := dev.setDUTPower(on); err != nil {
			return nil, err
		}
		res["on"] = on
	default:
		return nil, fmt.Errorf("unsupported method %s.%s", req.GetCapability(), req.GetMethod())
	}
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
