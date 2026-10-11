// Package nrf802154 is the hwlab plugin for the Nordic "nRF Sniffer for
// 802.15.4" firmware running on an nRF52840 DK or dongle. It captures raw
// IEEE 802.15.4 frames over the firmware's USB serial shell, writes a pcapng
// artifact (link type 283, 802.15.4 TAP, so channel/RSSI/LQI are visible in
// Wireshark) and streams packet events into the session timeline. tshark is an
// optional decoding layer; raw capture never needs it.
package nrf802154

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/decode"
	"github.com/faronov/hwlab/internal/usbserial"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

// Nordic Semiconductor's USB vendor id and the product id of the sniffer
// firmware. Other Nordic CDC devices are only offered as candidates when no
// confirmed sniffer is present. The PPK2 belongs to the ppk2 plugin.
const (
	nordicVID  = "1915"
	snifferPID = "154b"
	ppk2PID    = "c00a"
)

// Valid IEEE 802.15.4 2.4 GHz channels.
const (
	MinChannel = protocol.MinChannel
	MaxChannel = protocol.MaxChannel
)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list  usbserial.Lister
	open  usbserial.Opener
	dec   *decode.Tshark
	quiet time.Duration // command quiet period override (tests); 0 = default

	mu   sync.Mutex
	busy map[string]bool // serial port -> in use by a capture or scan
	cfg  map[string]map[string]any
}

// New returns the plugin with real serial enumeration.
func New() *Plugin { return NewWith(usbserial.List, usbserial.Open(115200), &decode.Tshark{}) }

// NewWith injects the port lister, opener and decoder (tests).
func NewWith(list usbserial.Lister, open usbserial.Opener, dec *decode.Tshark) *Plugin {
	return &Plugin{list: list, open: open, dec: dec, busy: map[string]bool{}, cfg: map[string]map[string]any{}}
}

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
		Id: "nrf802154", Name: "Nordic IEEE 802.15.4 sniffer", Vendor: "hwlab", Version: "0.2.0",
		Capabilities: caps(pluginv1.CapPacketCapture, pluginv1.CapTimedCapture, pluginv1.CapArtifactExport, pluginv1.CapProtocolDecode),
		Methods: []*pluginv1.MethodSpec{
			{Capability: pluginv1.CapPacketCapture, Name: "scan", Danger: pluginv1.Danger_DANGER_SAFE,
				Description:    "listen on each channel for dwell_ms and report packet counts and RSSI; args: channels (e.g. \"11-26\" or \"15,20\"), dwell_ms, min_packets",
				ArgsSchemaJson: `{"type":"object","properties":{"channels":{"type":"string"},"dwell_ms":{"type":"number"},"min_packets":{"type":"number"}}}`},
			{Capability: pluginv1.CapProtocolDecode, Name: "tshark_status", Danger: pluginv1.Danger_DANGER_SAFE,
				Description: "report whether tshark is available for decoding"},
			{Capability: pluginv1.CapProtocolDecode, Name: "decode_pcap", Danger: pluginv1.Danger_DANGER_SAFE,
				Description:    "decode a pcap/pcapng artifact with tshark; args: path, limit",
				ArgsSchemaJson: `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"limit":{"type":"number"}}}`},
		},
	}
}

func isSniffer(i usbserial.Info) bool {
	if !strings.EqualFold(i.VID, nordicVID) {
		return false
	}
	p := strings.ToLower(i.Product)
	return strings.EqualFold(i.PID, snifferPID) || strings.Contains(p, "802154") || strings.Contains(p, "802.15.4") || strings.Contains(p, "sniffer")
}

// ListDevices implements pluginsdk.Plugin. Confirmed sniffers (PID 154b or a
// sniffer product string) are listed; when there are none, other Nordic USB
// serial devices are listed as unconfirmed candidates.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	infos, err := p.list()
	if err != nil {
		return nil, err
	}
	var confirmed, candidates []usbserial.Info
	for _, i := range usbserial.Match(infos, nordicVID, "") {
		switch {
		case strings.EqualFold(i.PID, ppk2PID):
		case isSniffer(i):
			confirmed = append(confirmed, i)
		default:
			candidates = append(candidates, i)
		}
	}
	use, label := confirmed, "nRF 802.15.4 sniffer"
	if len(use) == 0 {
		use, label = candidates, "Nordic USB serial device (sniffer candidate, firmware not confirmed)"
	}
	var out []*pluginv1.Device
	for _, i := range use {
		id := i.SerialNumber
		if id == "" {
			id = filepath.Base(i.Name)
		}
		props := map[string]string{"port": i.Name, "vid": i.VID, "pid": i.PID, "product": i.Product, "confirmed": fmt.Sprint(isSniffer(i))}
		if !isSniffer(i) {
			props["candidate"] = "true"
		}
		out = append(out, &pluginv1.Device{
			Id: id, Name: label, Serial: i.SerialNumber, Transport: "usb-serial",
			Capabilities: caps(pluginv1.CapPacketCapture, pluginv1.CapTimedCapture, pluginv1.CapArtifactExport),
			Properties:   props,
		})
	}
	return out, nil
}

// resolvePort maps a device id (serial number, tty name) or an explicit
// "port" config value to a serial port path.
func (p *Plugin) resolvePort(id string, cfg map[string]any) (string, error) {
	if s, ok := cfg["port"].(string); ok && s != "" {
		return s, nil
	}
	infos, err := p.list()
	if err != nil {
		return "", err
	}
	for _, i := range usbserial.Match(infos, nordicVID, "") {
		if i.SerialNumber == id || filepath.Base(i.Name) == id || i.Name == id {
			return i.Name, nil
		}
	}
	return "", status.Errorf(codes.NotFound, "sniffer %q not found; is it plugged in? (hwlab devices)", id)
}

func (p *Plugin) acquire(port string) (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[port] {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is already in use by another capture or scan", port)
	}
	p.busy[port] = true
	return func() { p.mu.Lock(); delete(p.busy, port); p.mu.Unlock() }, nil
}

// ValidateChannel checks an IEEE 802.15.4 2.4 GHz channel number.
func ValidateChannel(v any) (int, error) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, fmt.Errorf("channel must be an integer in [%d,%d]", MinChannel, MaxChannel)
	}
	if ch := int(f); ch >= MinChannel && ch <= MaxChannel {
		return ch, nil
	}
	return 0, fmt.Errorf("channel %v out of range [%d,%d]", v, MinChannel, MaxChannel)
}

// OpenDevice implements pluginsdk.Plugin; it validates configuration only.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	if v, ok := cfg["channel"]; ok {
		if _, err := ValidateChannel(v); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg[id] = cfg
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cfg, id)
	return nil
}

func (p *Plugin) openSniffer(port string) (*protocol.Device, error) {
	d, err := protocol.Open(p.open, port)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"open %s: %v (is another program, e.g. ModemManager or Wireshark, holding the port? are you in the dialout group?)", port, err)
	}
	if p.quiet > 0 {
		d.Quiet = p.quiet
	}
	return d, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	var res map[string]any
	var err error
	switch req.GetCapability() + "." + req.GetMethod() {
	case pluginv1.CapProtocolDecode + ".tshark_status":
		res = map[string]any{}
		if path, aerr := p.dec.Available(); aerr != nil {
			res["available"] = false
			res["hint"] = aerr.Error()
		} else {
			res["available"] = true
			res["path"] = path
		}
	case pluginv1.CapProtocolDecode + ".decode_pcap":
		res, err = p.decodePcap(ctx, req.GetArgs().AsMap())
	case pluginv1.CapPacketCapture + ".scan":
		res, err = p.scan(ctx, req.GetDeviceId(), req.GetArgs().AsMap())
	default:
		return nil, fmt.Errorf("unsupported method %s.%s", req.GetCapability(), req.GetMethod())
	}
	if err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}
