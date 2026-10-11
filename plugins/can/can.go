package canplugin

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/can"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

const (
	maxRecvFrames = 256
	defaultRecv   = 16
)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener
	hub  *Hub // set when the in-process virtual bus is enabled

	mu   sync.Mutex
	devs map[string]*device
}

// device is one open interface.
type device struct {
	id   string
	info IfaceInfo

	mu   sync.Mutex // guards bus, opts, cap and the counters
	bus  Bus
	opts Options
	cap  *capture
	rx   uint64
	tx   uint64

	rmu sync.Mutex // serialises Invoke reads
	wmu sync.Mutex // serialises writes
}

func (d *device) endCapture(c *capture) {
	d.mu.Lock()
	if d.cap == c {
		d.cap = nil
	}
	d.mu.Unlock()
}

// New returns the plugin serving the SocketCAN interfaces of the host and, with
// HWLAB_CAN_VIRTUAL=1, the in-process virtual bus.
func New() *Plugin {
	hub := NewHub()
	virtual := os.Getenv(VirtualEnv) == "1"
	list := func() ([]IfaceInfo, error) {
		infos, err := ListSocketCAN()
		if virtual {
			infos = append(infos, hub.Info())
		}
		return infos, err
	}
	open := func(name string, o Options) (Bus, error) {
		if virtual && name == VirtualName {
			return hub.Open(name, o)
		}
		return OpenSocketCAN(name, o)
	}
	return NewWith(list, open).withHub(hub, virtual)
}

func (p *Plugin) withHub(h *Hub, enabled bool) *Plugin {
	if enabled {
		p.hub = h
	}
	return p
}

// NewWith injects interface enumeration and opening (tests, other backends).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func method(danger pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapCANTransport, Name: name, Description: desc, Danger: danger, ArgsSchemaJson: schema}
}

const optionProps = `"fd":{"enum":[true,false,"auto"]},"own":{"type":"boolean"},"error_frames":{"type":"boolean"},` +
	`"filters":{"description":"candump notation id:mask, id~mask (inverted) or a bare id; a list or comma-separated","anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]}`

func deviceCaps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapCANTransport, pluginv1.CapPacketCapture, pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream, pluginv1.CapArtifactExport}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "can", Name: "CAN / CAN-FD bus", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: deviceCaps(),
		Methods: []*pluginv1.MethodSpec{
			method(pluginv1.Danger_DANGER_SAFE, "status", "interface state, options and whether a capture is running", `{"type":"object"}`),
			method(pluginv1.Danger_DANGER_SAFE, "recv",
				"listen for up to timeout_ms and return at most max_frames (256) decoded frames; not while a capture runs",
				`{"type":"object","properties":{`+optionProps+`,"max_frames":{"type":"number"},"timeout_ms":{"type":"number"}}}`),
			method(pluginv1.Danger_DANGER_CONFIRM, "send",
				"transmit ONE frame onto the bus (can disturb or actuate every node on it); id in hex, data as hex, optional extended, fd, brs, esi, rtr and dlc",
				`{"type":"object","required":["id"],"properties":{`+optionProps+`,"id":{"anyOf":[{"type":"string"},{"type":"number"}]},"extended":{"type":"boolean"},`+
					`"fd":{"type":"boolean"},"brs":{"type":"boolean"},"esi":{"type":"boolean"},"rtr":{"type":"boolean"},"dlc":{"type":"number"},"data":{"type":"string"}}}`),
			method(pluginv1.Danger_DANGER_SAFE, "inject",
				"virtual bus only (HWLAB_CAN_VIRTUAL=1): deliver a frame as if another node had sent it, for demos and fixtures; same arguments as send",
				`{"type":"object","required":["id"]}`),
		},
	}
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	infos, err := p.list()
	if err != nil {
		return nil, err
	}
	out := make([]*pluginv1.Device, 0, len(infos))
	for _, i := range infos {
		transport := "can"
		if i.Virtual {
			transport = "virtual"
		}
		out = append(out, &pluginv1.Device{Id: i.Name, Name: i.Name, Transport: transport, Capabilities: deviceCaps(), Properties: map[string]string{
			"backend": i.Backend, "fd": strconv.FormatBool(i.FD), "up": strconv.FormatBool(i.Up), "mtu": strconv.Itoa(i.MTU),
			"virtual": strconv.FormatBool(i.Virtual),
		}})
	}
	return out, nil
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func (p *Plugin) find(id string) (IfaceInfo, error) {
	infos, err := p.list()
	if err != nil {
		return IfaceInfo{}, status.Error(codes.Unavailable, err.Error())
	}
	for _, i := range infos {
		if i.Name == id {
			return i, nil
		}
	}
	return IfaceInfo{}, status.Errorf(codes.NotFound, "CAN interface %q not found (try `hwlab devices`; a vcan needs `ip link add dev %s type vcan`)", id, id)
}

// parseOptions reads fd, own, error_frames and filters over base. fd "auto"
// means "when the interface is FD capable".
func parseOptions(base Options, info IfaceInfo, cfg map[string]any) (Options, error) {
	o := base
	if v, ok := cfg["fd"]; ok {
		switch t := v.(type) {
		case bool:
			o.FD = t
		case string:
			if t != "auto" {
				return o, fmt.Errorf("fd %q: want true, false or auto", t)
			}
			o.FD = info.FD
		default:
			return o, fmt.Errorf("fd %v: want true, false or auto", v)
		}
		if o.FD && !info.FD {
			return o, fmt.Errorf("interface %s is not CAN FD capable (MTU %d): use fd: false", info.Name, info.MTU)
		}
	}
	for _, k := range []string{"own", "error_frames"} {
		v, ok := cfg[k]
		if !ok {
			continue
		}
		b, isBool := v.(bool)
		if !isBool {
			return o, fmt.Errorf("%s %v: want true or false", k, v)
		}
		if k == "own" {
			o.Own = b
		} else {
			o.ErrorFrames = b
		}
	}
	if v, ok := cfg["filters"]; ok {
		fs, err := parseFilters(v)
		if err != nil {
			return o, err
		}
		o.Filters = fs
	}
	return o, nil
}

func defaultOptions(info IfaceInfo) Options { return Options{FD: info.FD, Own: true} }

func parseFilters(v any) ([]can.Filter, error) {
	var parts []string
	switch t := v.(type) {
	case string:
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("filters: %v is not a string", e)
			}
			parts = append(parts, s)
		}
	case nil:
	default:
		return nil, fmt.Errorf("filters %v: want a string or a list of strings", v)
	}
	out := make([]can.Filter, 0, len(parts))
	for _, s := range parts {
		f, err := can.ParseFilter(s)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	info, err := p.find(id)
	if err != nil {
		return err
	}
	opts, err := parseOptions(defaultOptions(info), info, cfg)
	if err != nil {
		return argErr(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.devs[id]; old != nil {
		old.mu.Lock()
		running := old.cap != nil
		old.mu.Unlock()
		if running {
			return status.Errorf(codes.FailedPrecondition, "%s has a running capture", id)
		}
		_ = old.bus.Close()
		delete(p.devs, id)
	}
	bus, err := p.open(id, opts)
	if err != nil {
		return status.Errorf(codes.Unavailable, "open %s: %v", id, err)
	}
	p.devs[id] = &device{id: id, info: info, bus: bus, opts: opts}
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	d := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bus.Close()
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

// transient opens the interface for one call when the device is not open.
func (p *Plugin) transient(id string, args map[string]any, own bool) (*device, func(), error) {
	if d := p.device(id); d != nil {
		return d, func() {}, nil
	}
	info, err := p.find(id)
	if err != nil {
		return nil, nil, err
	}
	base := defaultOptions(info)
	base.Own = own
	opts, err := parseOptions(base, info, args)
	if err != nil {
		return nil, nil, argErr(err)
	}
	bus, err := p.open(id, opts)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "open %s: %v", id, err)
	}
	return &device{id: id, info: info, bus: bus, opts: opts}, func() { _ = bus.Close() }, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if req.GetCapability() != pluginv1.CapCANTransport {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", req.GetCapability())
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	var (
		res map[string]any
		err error
	)
	switch req.GetMethod() {
	case "status":
		res, err = p.status(req.GetDeviceId(), args)
	case "recv":
		res, err = p.recv(ctx, req.GetDeviceId(), args)
	case "send":
		res, err = p.send(req.GetDeviceId(), args)
	case "inject":
		res, err = p.inject(req.GetDeviceId(), args)
	default:
		return nil, status.Errorf(codes.Unimplemented, "method %q", req.GetMethod())
	}
	if err != nil {
		return nil, err
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func optionsMap(o Options) map[string]any {
	fs := make([]any, len(o.Filters))
	for i, f := range o.Filters {
		fs[i] = fmt.Sprintf("%X:%X", f.ID, f.Mask)
		if f.Invert {
			fs[i] = fmt.Sprintf("%X~%X", f.ID, f.Mask)
		}
	}
	return map[string]any{"fd": o.FD, "own": o.Own, "error_frames": o.ErrorFrames, "filters": fs}
}

func (p *Plugin) status(id string, args map[string]any) (map[string]any, error) {
	info, err := p.find(id)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"interface": id, "backend": info.Backend, "fd_capable": info.FD, "up": info.Up, "virtual": info.Virtual, "mtu": info.MTU}
	if d := p.device(id); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		res["open"], res["capturing"], res["options"] = true, d.cap != nil, optionsMap(d.opts)
		res["frames_rx"], res["frames_tx"] = d.rx, d.tx
	} else {
		res["open"] = false
	}
	return res, nil
}

func intArg(args map[string]any, key string, def, min, max int) (int, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	f, isNum := v.(float64)
	if !isNum || f != math.Trunc(f) || f < float64(min) || f > float64(max) {
		return 0, argErr(fmt.Errorf("%s %v: want an integer %d..%d", key, v, min, max))
	}
	return int(f), nil
}

func frameJSON(f can.Frame, tMs float64, kernel bool) map[string]any {
	return map[string]any{
		"t_ms": tMs, "dir": f.Dir(), "kind": f.Kind(), "id": "0x" + f.IDString(), "extended": f.Extended,
		"dlc": f.Len(), "data": hex.EncodeToString(f.Data), "summary": f.Summary(), "kernel_timestamp": kernel,
	}
}

func (p *Plugin) recv(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	maxFrames, err := intArg(args, "max_frames", defaultRecv, 1, maxRecvFrames)
	if err != nil {
		return nil, err
	}
	timeoutMs, err := intArg(args, "timeout_ms", 1000, 1, 600_000)
	if err != nil {
		return nil, err
	}
	d, release, err := p.transient(id, args, false)
	if err != nil {
		return nil, err
	}
	defer release()
	d.rmu.Lock()
	defer d.rmu.Unlock()
	d.mu.Lock()
	if d.cap != nil {
		d.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "recv is not possible while a capture runs: the capture owns the receiver")
	}
	bus := d.bus
	d.mu.Unlock()

	start := time.Now()
	deadline := start.Add(time.Duration(timeoutMs) * time.Millisecond)
	frames := []any{}
	for len(frames) < maxFrames && ctx.Err() == nil {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		rx, err := bus.Recv(min(left, 250*time.Millisecond))
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			if len(frames) > 0 {
				break
			}
			return nil, status.Errorf(codes.Unavailable, "receive: %v", err)
		}
		d.mu.Lock()
		d.rx++
		d.mu.Unlock()
		frames = append(frames, frameJSON(rx.Frame, float64(rx.At.Sub(start).Microseconds())/1000, rx.Kernel))
	}
	return map[string]any{"interface": id, "count": len(frames), "frames": frames, "waited_ms": time.Since(start).Milliseconds()}, nil
}

func parseID(v any) (uint32, error) {
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) || t < 0 || t > 0x1FFFFFFF {
			return 0, fmt.Errorf("id %v: want 0..0x1FFFFFFF", t)
		}
		return uint32(t), nil
	case string:
		s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(t), "0x"), "0X")
		n, err := strconv.ParseUint(s, 16, 32)
		if err != nil || n > 0x1FFFFFFF {
			return 0, fmt.Errorf("id %q: want hex 0..1FFFFFFF", t)
		}
		return uint32(n), nil
	}
	return 0, fmt.Errorf("id %v: want a hex string or number", v)
}

func boolArg(args map[string]any, key string) (bool, bool, error) {
	v, ok := args[key]
	if !ok {
		return false, false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, true, fmt.Errorf("%s %v: want true or false", key, v)
	}
	return b, true, nil
}

// buildFrame turns send arguments into a validated frame.
func buildFrame(iface string, args map[string]any) (can.Frame, error) {
	f := can.Frame{Interface: iface, Own: true}
	rawID, ok := args["id"]
	if !ok {
		return f, errors.New("id is required")
	}
	var err error
	if f.ID, err = parseID(rawID); err != nil {
		return f, err
	}
	ext, set, err := boolArg(args, "extended")
	if err != nil {
		return f, err
	}
	f.Extended = f.ID > 0x7FF
	if set {
		f.Extended = ext
	}
	for _, k := range []string{"fd", "brs", "esi", "rtr"} {
		b, _, err := boolArg(args, k)
		if err != nil {
			return f, err
		}
		switch k {
		case "fd":
			f.FD = b
		case "brs":
			f.BRS = b
		case "esi":
			f.ESI = b
		case "rtr":
			f.RTR = b
		}
	}
	if (f.BRS || f.ESI) && !f.FD {
		return f, errors.New("brs/esi are CAN FD flags: set fd")
	}
	if s, ok := args["data"].(string); ok {
		clean := strings.NewReplacer(" ", "", ":", "", "0x", "", "0X", "").Replace(s)
		if f.Data, err = hex.DecodeString(clean); err != nil {
			return f, fmt.Errorf("data %q: not hex", s)
		}
	} else if _, present := args["data"]; present {
		return f, errors.New("data: want a hex string")
	}
	if f.RTR {
		dlc, err := intArg(args, "dlc", len(f.Data), 0, can.MaxClassic)
		if err != nil {
			return f, err
		}
		f.DLC = uint8(dlc)
		f.Data = nil
	}
	return f, f.Validate()
}

func (p *Plugin) send(id string, args map[string]any) (map[string]any, error) {
	f, err := buildFrame(id, args)
	if err != nil {
		return nil, argErr(err)
	}
	d, release, err := p.transient(id, args, false)
	if err != nil {
		return nil, err
	}
	defer release()
	d.wmu.Lock()
	defer d.wmu.Unlock()
	d.mu.Lock()
	bus, c, own := d.bus, d.cap, d.opts.Own
	d.mu.Unlock()

	// A capture on an interface that does not echo our own frames still has to
	// show them: stamp before the write, like a UART tx chunk.
	var at time.Time
	if c != nil && !own {
		at = time.Now()
	}
	if err := bus.Send(f); err != nil {
		return nil, status.Errorf(codes.Unavailable, "send: %v", err)
	}
	if c != nil && !own {
		c.onTx(f, at)
	}
	d.mu.Lock()
	d.tx++
	d.mu.Unlock()
	return map[string]any{"interface": id, "sent": f.Summary(), "id": "0x" + f.IDString(), "kind": f.Kind(), "dlc": f.Len(), "data": hex.EncodeToString(f.Data)}, nil
}

// inject delivers a frame on the in-process virtual bus as a foreign node.
func (p *Plugin) inject(id string, args map[string]any) (map[string]any, error) {
	if p.hub == nil || id != VirtualName {
		return nil, status.Errorf(codes.FailedPrecondition, "inject exists only on %s (start the plugin with %s=1)", VirtualName, VirtualEnv)
	}
	f, err := buildFrame(id, args)
	if err != nil {
		return nil, argErr(err)
	}
	f.Own = false
	p.hub.Inject(f)
	return map[string]any{"interface": id, "injected": f.Summary(), "synthetic": true}, nil
}
