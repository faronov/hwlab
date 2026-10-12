// Package fake is the reference hwlab plugin. It simulates a coherent lab:
// a power meter, a logic analyzer, an 802.15.4 sniffer and a debug probe
// that all observe the same periodic "radio TX" scenario, so timeline,
// correlation, plot and replay can be tested without hardware.
package fake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Device ids exposed by the plugin.
const (
	DevPower = "power0"
	DevLogic = "logic0"
	DevRadio = "radio0"
	DevDebug = "debug0"
	// DevControl is command/response only: no capture capability.
	DevControl = "ctl0"
)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	mu      sync.Mutex
	open    map[string]map[string]any
	voltage float64
}

// New returns the fake plugin.
func New() *Plugin { return &Plugin{open: map[string]map[string]any{}, voltage: 3.3} }

func caps(ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "fake", Name: "hwlab reference fake plugin", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(pluginv1.CapPowerMeasurement, pluginv1.CapVoltageMeasurement, pluginv1.CapPowerSource,
			pluginv1.CapDigitalCapture, pluginv1.CapPacketCapture, pluginv1.CapDebugProbe, pluginv1.CapRTT,
			pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream, pluginv1.CapArtifactExport),
		Methods: []*pluginv1.MethodSpec{
			{Capability: pluginv1.CapPowerSource, Name: "set_voltage", Description: "set fake supply voltage (args: volts)", Danger: pluginv1.Danger_DANGER_CONFIRM},
			{Capability: pluginv1.CapPowerSource, Name: "get_voltage", Description: "read fake supply voltage", Danger: pluginv1.Danger_DANGER_SAFE},
			{Capability: pluginv1.CapDebugProbe, Name: "echo", Description: "echo args back", Danger: pluginv1.Danger_DANGER_SAFE},
		},
		DangerousConfigKeys: []string{"supply_mv"},
	}
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	mk := func(id, name string, c ...string) *pluginv1.Device {
		return &pluginv1.Device{Id: id, Name: name, Serial: "FAKE-" + id, Transport: "virtual",
			Capabilities: caps(append(c, pluginv1.CapTimedCapture)...), Properties: map[string]string{"simulated": "true"}}
	}
	return []*pluginv1.Device{
		mk(DevPower, "Fake power meter", pluginv1.CapPowerMeasurement, pluginv1.CapVoltageMeasurement, pluginv1.CapPowerSource, pluginv1.CapLiveRawStream),
		mk(DevLogic, "Fake logic analyzer", pluginv1.CapDigitalCapture),
		mk(DevRadio, "Fake 802.15.4 sniffer", pluginv1.CapPacketCapture, pluginv1.CapArtifactExport),
		mk(DevDebug, "Fake debug probe", pluginv1.CapDebugProbe, pluginv1.CapRTT),
		{Id: DevControl, Name: "Fake control probe", Serial: "FAKE-" + DevControl, Transport: "virtual",
			Capabilities: caps(pluginv1.CapDebugProbe, pluginv1.CapPowerSource), Properties: map[string]string{"simulated": "true"}},
	}, nil
}

func validDevice(id string) bool {
	switch id {
	case DevPower, DevLogic, DevRadio, DevDebug, DevControl:
		return true
	}
	return false
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	if !validDevice(id) {
		return fmt.Errorf("unknown device %q", id)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open[id] = cfg
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.open, id)
	return nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(_ context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	args := req.GetArgs().AsMap()
	res := map[string]any{}
	switch req.GetCapability() + "." + req.GetMethod() {
	case pluginv1.CapPowerSource + ".set_voltage":
		v, ok := args["volts"].(float64)
		if !ok || v < 0 || v > 5 {
			return nil, fmt.Errorf("set_voltage: volts must be a number in [0,5]")
		}
		p.mu.Lock()
		p.voltage = v
		p.mu.Unlock()
		res["volts"] = v
	case pluginv1.CapPowerSource + ".get_voltage":
		p.mu.Lock()
		res["volts"] = p.voltage
		p.mu.Unlock()
	case pluginv1.CapDebugProbe + ".echo":
		res = args
	default:
		return nil, fmt.Errorf("unsupported method %s.%s", req.GetCapability(), req.GetMethod())
	}
	return newInvokeResponse(res)
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	p.mu.Lock()
	openCfg, opened := p.open[req.DeviceID]
	p.mu.Unlock()
	if req.DeviceID == DevControl {
		return nil, nil, fmt.Errorf("device %q cannot capture", req.DeviceID)
	}
	if !opened {
		return nil, nil, fmt.Errorf("device %q is not open", req.DeviceID)
	}
	cfg := parseConfig(openCfg, req.Config)
	cfg.supplyV = p.currentVoltage(cfg)
	c := &capture{req: req, cfg: cfg, sink: sink}
	var streams []*pluginv1.StreamInfo

	switch req.DeviceID {
	case DevPower:
		streams = []*pluginv1.StreamInfo{{StreamId: "current", Kind: "power", Attributes: map[string]string{
			"sample_period_ns": fmt.Sprint(cfg.samplePeriod.Nanoseconds())}}}
	case DevLogic:
		streams = []*pluginv1.StreamInfo{{StreamId: "digital", Kind: "digital"}}
	case DevRadio:
		streams = []*pluginv1.StreamInfo{{StreamId: "packets", Kind: "packet"}}
		if err := c.openPCAP(); err != nil {
			return nil, nil, err
		}
	case DevDebug:
		streams = []*pluginv1.StreamInfo{{StreamId: "rtt", Kind: "log"}}
	}
	c.wg.Add(1)
	go c.run(ctx)
	return c, &pluginsdk.CaptureStart{Streams: streams}, nil
}

func (p *Plugin) currentVoltage(cfg config) float64 {
	if cfg.supplyMV > 0 {
		return cfg.supplyMV / 1000
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.voltage
}

type capture struct {
	req  pluginsdk.CaptureRequest
	cfg  config
	sink *pluginsdk.Sink
	wg   sync.WaitGroup

	events  uint64
	samples uint64
	err     error

	pcapFile *os.File
	pcapW    *pcap.Writer
	pcapPath string
}

func (c *capture) openPCAP() error {
	if c.req.ArtifactDir == "" {
		return nil
	}
	c.pcapPath = filepath.Join(c.req.ArtifactDir, "radio0.pcap")
	f, err := os.Create(c.pcapPath)
	if err != nil {
		return fmt.Errorf("create pcap: %w", err)
	}
	w, err := pcap.NewWriter(f, pcap.LinkIEEE802154WithFCS)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	c.pcapFile, c.pcapW = f, w
	return nil
}

func (c *capture) Wait() error { c.wg.Wait(); return c.err }

func (c *capture) Result() *pluginv1.StopCaptureResponse {
	res := &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{Events: c.events, Samples: c.samples}}
	if c.pcapFile != nil {
		if err := c.pcapFile.Close(); err == nil {
			res.Artifacts = append(res.Artifacts, &pluginv1.ArtifactRef{Name: "radio0.pcap", Kind: "pcap",
				MimeType: "application/vnd.tcpdump.pcap", Path: c.pcapPath,
				Metadata: map[string]string{"link_type": "195", "timestamps": "session-relative-ns"}})
		}
	}
	return res
}

func (c *capture) run(ctx context.Context) {
	defer c.wg.Done()
	g := newGenerator(c.cfg, c.req.DeviceID)
	emit := func(ectx context.Context, from, to time.Duration) bool {
		evs := g.window(c, from, to)
		if len(evs) == 0 {
			return true
		}
		return c.sink.Emit(ectx, evs...) == nil
	}
	limit := c.req.Duration
	if !c.cfg.realtime {
		// Simulated time: generate as fast as the consumer drains.
		if limit <= 0 {
			limit = time.Second
		}
		for from := time.Duration(0); from < limit; from += chunk {
			if !emit(ctx, from, min(from+chunk, limit)) {
				return
			}
		}
		return
	}
	tick := time.NewTicker(chunk)
	defer tick.Stop()
	start := time.Now()
	var last time.Duration
	for {
		select {
		case <-ctx.Done():
			// Flush the trailing window so a stop does not lose events.
			now := time.Since(start)
			if limit > 0 && now > limit {
				now = limit
			}
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			emit(fctx, last, now)
			cancel()
			return
		case <-tick.C:
			now := time.Since(start)
			if limit > 0 && now > limit {
				now = limit
			}
			if !emit(ctx, last, now) {
				return
			}
			last = now
			if limit > 0 && now >= limit {
				return
			}
		}
	}
}
