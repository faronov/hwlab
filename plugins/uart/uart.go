// Package uart is the hwlab plugin for generic serial (UART) lines.
//
// It is a transport, not a protocol: it knows nothing about Modbus, AT
// commands or NMEA. Received and transmitted bytes enter the timeline as
// timestamped "uart" packet events (optionally also as text lines), and the
// SerialTransport capability offers read, write and line configuration. Protocol
// layers (Modbus RTU, AT, NMEA, bootloaders) are built on top of this
// capability and must not open serial ports themselves.
package uart

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

const (
	maxWriteBytes = 64 << 10
	maxReadBytes  = 1 << 20
)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

// device is one open port.
type device struct {
	id   string
	path string

	mu   sync.Mutex // guards port, mode and cap
	port Port
	mode Mode
	cap  *capture

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

// New returns the plugin using the real serial ports of the host.
func New() *Plugin { return NewWith(ListWithExtra, OpenPort) }

// NewWith injects port enumeration and opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func safe(name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapSerialTransport, Name: name, Description: desc, Danger: pluginv1.Danger_DANGER_SAFE, ArgsSchemaJson: schema}
}

func confirm(name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapSerialTransport, Name: name, Description: desc, Danger: pluginv1.Danger_DANGER_CONFIRM, ArgsSchemaJson: schema}
}

const lineProps = `"baud":{"type":"number"},"data_bits":{"enum":[5,6,7,8]},"parity":{"enum":["none","even","odd","mark","space"]},` +
	`"stop_bits":{"enum":[1,1.5,2]},"flow_control":{"enum":["none","rtscts","xonxoff"]}`

func deviceCaps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapSerialTransport, pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "uart", Name: "Generic serial (UART) transport", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: deviceCaps(),
		// Asserting DTR/RTS resets many boards and enters bootloaders.
		DangerousConfigKeys: []string{"dtr", "rts"},
		Methods: []*pluginv1.MethodSpec{
			safe("status", "port path, line settings and whether a capture is running", `{"type":"object"}`),
			safe("configure", "change baud, framing or flow control of the open port", `{"type":"object","properties":{`+lineProps+`}}`),
			safe("read", "read bytes (not while a capture runs): waits up to timeout_ms for the first byte, then until idle_gap_ms of silence or max_bytes",
				`{"type":"object","properties":{`+lineProps+`,"max_bytes":{"type":"number"},"timeout_ms":{"type":"number"},"idle_gap_ms":{"type":"number"}}}`),
			confirm("write", "transmit bytes to whatever is attached; give data (text, optional eol none|cr|lf|crlf), hex, or a binary payload (max 64 KiB)",
				`{"type":"object","properties":{`+lineProps+`,"data":{"type":"string"},"hex":{"type":"string"},"eol":{"enum":["none","cr","lf","crlf"]}}}`),
			confirm("set_lines", "drive the DTR and/or RTS modem output lines (often resets the target)", `{"type":"object","properties":{"dtr":{"type":"boolean"},"rts":{"type":"boolean"}}}`),
			safe("flush", "discard received bytes that were not read yet", `{"type":"object"}`),
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
		props := map[string]string{"path": i.Path}
		transport, name := "serial", i.ID()
		if i.Alias != "" {
			transport = "virtual"
		}
		if i.USB {
			transport = "usb"
			props["vid"], props["pid"] = strings.ToLower(i.VID), strings.ToLower(i.PID)
			if i.Product != "" {
				name = i.Product + " (" + i.ID() + ")"
			}
			if dev, ifc := usbInterface(i.Path); ifc != "" {
				props["usb_device"], props["usb_interface"] = dev, ifc
			}
		}
		out = append(out, &pluginv1.Device{Id: i.ID(), Name: name, Serial: i.Serial, Transport: transport,
			Capabilities: deviceCaps(), Properties: props})
	}
	return out, nil
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

// resolve finds the port path of a device id. A "path" config key or an
// absolute id selects any port, including virtual ones (pty, socat).
func (p *Plugin) resolve(id string, cfg map[string]any) (string, error) {
	if v, ok := cfg["path"]; ok {
		s, isStr := v.(string)
		if !isStr || s == "" {
			return "", argErr(errors.New("path must be a non-empty string"))
		}
		return s, nil
	}
	infos, err := p.list()
	if err != nil {
		return "", status.Error(codes.Unavailable, err.Error())
	}
	for _, i := range infos {
		if i.ID() == id || i.Path == id {
			return i.Path, nil
		}
	}
	if filepath.IsAbs(id) {
		return id, nil
	}
	return "", status.Errorf(codes.NotFound, "serial port %q not found (try `hwlab devices`, or pass path: /dev/...)", id)
}

func openErr(path string, err error) error {
	msg := fmt.Sprintf("open %s: %v", path, err)
	if os.IsPermission(err) || strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		msg += " (your user probably lacks access to the port; on Linux add it to the dialout/uucp group)"
	}
	return status.Error(codes.Unavailable, msg)
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	mode, err := parseMode(Mode{}, cfg)
	if err != nil {
		return argErr(err)
	}
	path, err := p.resolve(id, cfg)
	if err != nil {
		return err
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
		old.port.Close()
		delete(p.devs, id)
	}
	port, err := p.open(path, mode)
	if err != nil {
		return openErr(path, err)
	}
	p.devs[id] = &device{id: id, path: path, port: port, mode: mode}
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
	return d.port.Close()
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

// StartCapture implements pluginsdk.Plugin: every received and transmitted
// byte of the port becomes a timestamped "uart" packet event.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cc, err := parseCaptureConfig(req.Config)
	if err != nil {
		return nil, nil, argErr(err)
	}
	flush := true
	if v, ok := req.Config["flush_input"].(bool); ok {
		flush = v
	}

	c := &capture{cfg: cc, clock: req.Clock, sink: sink}
	if d := p.device(req.DeviceID); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.cap != nil {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID)
		}
		mode, err := parseMode(d.mode, req.Config)
		if err != nil {
			return nil, nil, argErr(err)
		}
		if !mode.sameLine(d.mode) {
			if err := d.port.SetMode(mode); err != nil {
				return nil, nil, status.Errorf(codes.Unavailable, "set line mode: %v", err)
			}
			d.mode = mode
		}
		c.dev, c.port, c.path, c.mode = d, d.port, d.path, d.mode
	} else {
		mode, err := parseMode(Mode{}, req.Config)
		if err != nil {
			return nil, nil, argErr(err)
		}
		path, err := p.resolve(req.DeviceID, req.Config)
		if err != nil {
			return nil, nil, err
		}
		port, err := p.open(path, mode)
		if err != nil {
			return nil, nil, openErr(path, err)
		}
		d := &device{id: req.DeviceID, path: path, port: port, mode: mode}
		d.mu.Lock()
		defer d.mu.Unlock()
		c.dev, c.port, c.path, c.mode = d, port, path, mode
		c.closePort = func() { port.Close() }
	}
	if flush {
		_ = c.port.ResetInput()
	}
	if cc.lines {
		c.lines = &lineSplitter{c: c}
	}
	c.dev.cap = c
	c.wg.Add(1)
	// ctx outlives this call: the SDK cancels it on duration expiry and StopCapture.
	go c.run(ctx)

	streams := []*pluginv1.StreamInfo{
		{StreamId: StreamRX, Kind: "packet", Attributes: c.attrs("rx")},
		{StreamId: StreamTX, Kind: "packet", Attributes: c.attrs("tx")},
	}
	if cc.lines {
		streams = append(streams, &pluginv1.StreamInfo{StreamId: StreamLines, Kind: "log",
			Attributes: map[string]string{"source": LogSource, "port": c.path}})
	}
	return c, &pluginsdk.CaptureStart{Streams: streams}, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if req.GetCapability() != pluginv1.CapSerialTransport {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", req.GetCapability())
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	var (
		res map[string]any
		bin []byte
		err error
	)
	switch req.GetMethod() {
	case "status":
		res, err = p.status(req.GetDeviceId(), args)
	case "configure":
		res, err = p.configure(req.GetDeviceId(), args)
	case "read":
		res, bin, err = p.read(ctx, req.GetDeviceId(), args)
	case "write":
		res, err = p.write(req.GetDeviceId(), args, req.GetBinary())
	case "set_lines":
		res, err = p.setLines(req.GetDeviceId(), args)
	case "flush":
		res, err = p.flush(req.GetDeviceId())
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
	return &pluginv1.InvokeResponse{Result: s, Binary: bin}, nil
}

func (p *Plugin) openDevice(id, method string) (*device, error) {
	d := p.device(id)
	if d == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%s needs an open port: open the device first (with its baud/framing config)", method)
	}
	return d, nil
}

func (p *Plugin) status(id string, args map[string]any) (map[string]any, error) {
	if d := p.device(id); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return map[string]any{"open": true, "path": d.path, "mode": d.mode.String(), "baud": d.mode.Baud,
			"framing": d.mode.Framing(), "flow_control": d.mode.Flow, "capturing": d.cap != nil}, nil
	}
	path, err := p.resolve(id, args)
	if err != nil {
		return nil, err
	}
	return map[string]any{"open": false, "path": path}, nil
}

func (p *Plugin) configure(id string, args map[string]any) (map[string]any, error) {
	d, err := p.openDevice(id, "configure")
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cap != nil {
		return nil, status.Error(codes.FailedPrecondition, "cannot reconfigure the port while a capture runs")
	}
	mode, err := parseMode(d.mode, args)
	if err != nil {
		return nil, argErr(err)
	}
	if !mode.sameLine(d.mode) {
		if err := d.port.SetMode(mode); err != nil {
			return nil, status.Errorf(codes.Unavailable, "set line mode: %v", err)
		}
	}
	d.mode = mode
	return map[string]any{"mode": mode.String()}, nil
}

// transient opens the port for one call when the device is not open.
func (p *Plugin) transient(id string, args map[string]any) (*device, func(), error) {
	if d := p.device(id); d != nil {
		return d, func() {}, nil
	}
	mode, err := parseMode(Mode{}, args)
	if err != nil {
		return nil, nil, argErr(err)
	}
	path, err := p.resolve(id, args)
	if err != nil {
		return nil, nil, err
	}
	port, err := p.open(path, mode)
	if err != nil {
		return nil, nil, openErr(path, err)
	}
	return &device{id: id, path: path, port: port, mode: mode}, func() { port.Close() }, nil
}

func intArg(args map[string]any, key string, def, min, max int) (int, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	n, err := toInt(v)
	if err != nil || n < min || n > max {
		return 0, argErr(fmt.Errorf("%s %v: want %d..%d", key, v, min, max))
	}
	return n, nil
}

func (p *Plugin) read(ctx context.Context, id string, args map[string]any) (map[string]any, []byte, error) {
	maxBytes, err := intArg(args, "max_bytes", 256, 1, maxReadBytes)
	if err != nil {
		return nil, nil, err
	}
	timeoutMs, err := intArg(args, "timeout_ms", 1000, 1, 600_000)
	if err != nil {
		return nil, nil, err
	}
	gapMs, err := intArg(args, "idle_gap_ms", 50, 1, 10_000)
	if err != nil {
		return nil, nil, err
	}
	d, release, err := p.transient(id, args)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	d.mu.Lock()
	capturing := d.cap != nil
	port := d.port
	d.mu.Unlock()
	if capturing {
		return nil, nil, status.Error(codes.FailedPrecondition, "read is unavailable while a capture runs; the capture already records received bytes")
	}
	d.rmu.Lock()
	defer d.rmu.Unlock()

	var (
		out      []byte
		buf      = make([]byte, 4096)
		deadline = time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
		gap      = time.Duration(gapMs) * time.Millisecond
	)
	for len(out) < maxBytes {
		if err := ctx.Err(); err != nil {
			return nil, nil, status.FromContextError(err).Err()
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			break
		}
		if wait > 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
		_ = port.SetReadTimeout(wait)
		n, err := port.Read(buf)
		if n > 0 {
			room := maxBytes - len(out)
			if n > room {
				n = room
			}
			out = append(out, buf[:n]...)
			deadline = time.Now().Add(gap)
		}
		if err != nil {
			return nil, nil, status.Errorf(codes.Unavailable, "serial read: %v", err)
		}
	}
	res := map[string]any{"n": len(out), "text": preview(out, 200)}
	if len(out) <= 256 {
		res["hex"] = hex.EncodeToString(out)
	}
	return res, out, nil
}

func decodeWrite(args map[string]any, binary []byte) ([]byte, error) {
	sources := 0
	var data []byte
	if len(binary) > 0 {
		sources++
		data = binary
	}
	if v, ok := args["hex"]; ok {
		sources++
		s, _ := v.(string)
		b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, argErr(fmt.Errorf("hex: %v", err))
		}
		data = b
	}
	if v, ok := args["data"]; ok {
		sources++
		s, isStr := v.(string)
		if !isStr {
			return nil, argErr(errors.New("data must be a string"))
		}
		data = []byte(s)
	}
	if sources != 1 {
		return nil, argErr(errors.New("give exactly one of data, hex or a binary payload"))
	}
	if v, ok := args["eol"]; ok {
		eol, found := map[string]string{"none": "", "cr": "\r", "lf": "\n", "crlf": "\r\n"}[strings.ToLower(fmt.Sprint(v))]
		if !found {
			return nil, argErr(fmt.Errorf("eol %v: want none, cr, lf or crlf", v))
		}
		data = append(append([]byte(nil), data...), eol...)
	}
	if len(data) == 0 || len(data) > maxWriteBytes {
		return nil, argErr(fmt.Errorf("write length %d: want 1..%d bytes", len(data), maxWriteBytes))
	}
	return data, nil
}

func (p *Plugin) write(id string, args map[string]any, binary []byte) (map[string]any, error) {
	data, err := decodeWrite(args, binary)
	if err != nil {
		return nil, err
	}
	d, release, err := p.transient(id, args)
	if err != nil {
		return nil, err
	}
	defer release()
	d.wmu.Lock()
	defer d.wmu.Unlock()
	d.mu.Lock()
	port, cap := d.port, d.cap
	d.mu.Unlock()

	var at int64
	if cap != nil {
		at = cap.clock.Now()
	}
	written := 0
	for written < len(data) {
		n, err := port.Write(data[written:])
		written += n
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "serial write after %d of %d bytes: %v", written, len(data), err)
		}
	}
	if cap != nil {
		cap.txEvent(data, at)
	}
	return map[string]any{"written": written}, nil
}

func (p *Plugin) setLines(id string, args map[string]any) (map[string]any, error) {
	d, err := p.openDevice(id, "set_lines")
	if err != nil {
		return nil, err
	}
	dtr, hasDTR := args["dtr"].(bool)
	rts, hasRTS := args["rts"].(bool)
	if !hasDTR && !hasRTS {
		return nil, argErr(errors.New("give dtr and/or rts (true or false)"))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res := map[string]any{}
	if hasDTR {
		if err := d.port.SetDTR(dtr); err != nil {
			return nil, status.Errorf(codes.Unavailable, "set DTR: %v", err)
		}
		res["dtr"] = dtr
	}
	if hasRTS {
		if err := d.port.SetRTS(rts); err != nil {
			return nil, status.Errorf(codes.Unavailable, "set RTS: %v", err)
		}
		res["rts"] = rts
	}
	return res, nil
}

func (p *Plugin) flush(id string) (map[string]any, error) {
	d, err := p.openDevice(id, "flush")
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.port.ResetInput(); err != nil {
		return nil, status.Errorf(codes.Unavailable, "flush: %v", err)
	}
	return map[string]any{"flushed": true}, nil
}

var _ pluginsdk.Plugin = (*Plugin)(nil)
