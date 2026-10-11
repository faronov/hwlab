// Package usbcapplugin is the hwlab plugin for USB packet capture.
//
// It is a capture source, not an analyzer: every backend produces usbcap.Packet
// values (URB events) and the analysis lives in internal/usbcap. Backends:
//
//   - the Linux usbmon text API (/sys/kernel/debug/usb/usbmon/Nu), which needs
//     root or a readable debugfs;
//   - a recorded usbmon text file or FIFO (HWLAB_USBCAP_SOURCES), also useful for
//     replaying a capture taken elsewhere;
//   - a synthetic generator (HWLAB_USBCAP_SYNTH=1) for demos and tests.
//
// A hardware USB analyzer works at bus level (tokens, handshakes, SOF) and does
// not fit this URB-level model; it needs its own link type and backend. The
// package does not claim to cover it.
package usbcapplugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usbcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Environment variables.
const (
	SynthEnv   = "HWLAB_USBCAP_SYNTH"
	SourcesEnv = "HWLAB_USBCAP_SOURCES" // name=/path[,name=/path...]
	// DebugfsDir is where the kernel exposes usbmon.
	DebugfsDir = "/sys/kernel/debug/usb/usbmon"
	// SynthID is the device id of the synthetic source.
	SynthID = "synthetic"
)

const (
	defaultSnaplen = 256
	maxSnaplen     = 65535
	maxPeek        = 256
	defaultPeek    = 32
)

// Source is one place packets come from.
type Source struct {
	ID        string
	Backend   string // "usbmonbin", "usbmon", "file" or "synthetic"
	Path      string
	Live      bool // data arrives in real time; offline sources are paced by speed
	Synthetic bool
	Reports   int // synthetic: HID reports generated
}

// Reader yields packets in capture order; Next returns io.EOF at the end of an
// offline source.
type Reader interface {
	Next(ctx context.Context) (usbcap.Packet, error)
	Close() error
	// Skipped reports the unparsable lines dropped so far and the first error.
	Skipped() (uint64, string)
}

// Lister enumerates the sources.
type Lister func() []Source

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister

	mu   sync.Mutex
	devs map[string]*device
}

type device struct {
	src  Source
	opts Options

	mu  sync.Mutex
	cap *capture
}

// Options are the capture settings.
type Options struct {
	// Speed scales the replay clock of an offline source (1 = recorded speed).
	Speed float64
	// Snaplen is the most data bytes kept per URB; 0 keeps headers only. USB
	// traffic can carry keystrokes and other secrets.
	Snaplen int
}

func defaultOptions() Options { return Options{Speed: 1, Snaplen: defaultSnaplen} }

// New returns the plugin serving usbmon (when readable), HWLAB_USBCAP_SOURCES
// files and, with HWLAB_USBCAP_SYNTH=1, the synthetic source.
func New() *Plugin {
	return NewWith(func() []Source { return hostSources(os.Getenv(SynthEnv) == "1", os.Getenv(SourcesEnv)) })
}

// NewWith injects the source list (tests).
func NewWith(l Lister) *Plugin { return &Plugin{list: l, devs: map[string]*device{}} }

func hostSources(synth bool, files string) []Source {
	var out []Source
	if synth {
		out = append(out, Source{ID: SynthID, Backend: "synthetic", Synthetic: true, Reports: 20})
	}
	for _, kv := range strings.Split(files, ",") {
		name, path, ok := strings.Cut(kv, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if ok && name != "" && path != "" {
			out = append(out, Source{ID: name, Backend: "file", Path: path})
		}
	}
	// usbmon: the binary device (full payloads) is preferred over the text
	// interface (payload limited to 32 bytes) for the same bus.
	binary := map[string]bool{}
	devs, _ := filepath.Glob(filepath.Join(DevDir, "usbmon[0-9]*"))
	for _, e := range devs {
		f, err := os.Open(e)
		if err != nil {
			continue
		}
		_ = f.Close()
		n := strings.TrimPrefix(filepath.Base(e), "usbmon")
		binary[n] = true
		out = append(out, Source{ID: "usbmon" + n, Backend: "usbmonbin", Path: e, Live: true})
	}
	ents, _ := filepath.Glob(filepath.Join(DebugfsDir, "*u"))
	for _, e := range ents {
		n := strings.TrimSuffix(filepath.Base(e), "u")
		if binary[n] {
			continue
		}
		f, err := os.Open(e)
		if err != nil {
			continue
		}
		_ = f.Close()
		out = append(out, Source{ID: "usbmon" + n, Backend: "usbmon", Path: e, Live: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func method(name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapUSBPacketCapture, Name: name, Description: desc, Danger: pluginv1.Danger_DANGER_SAFE, ArgsSchemaJson: schema}
}

const optionProps = `"speed":{"type":"number","description":"offline sources only: replay speed factor, default 1"},` +
	`"snaplen":{"type":"number","description":"data bytes kept per URB (default 256, 0 = headers only); USB traffic can contain keystrokes and secrets"}`

func deviceCaps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapUSBPacketCapture, pluginv1.CapPacketCapture, pluginv1.CapTimedCapture, pluginv1.CapArtifactExport}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "usbcap", Name: "USB packet capture (usbmon / recorded / synthetic)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: deviceCaps(),
		Methods: []*pluginv1.MethodSpec{
			method("info", "describe the source: backend, whether it is live, synthetic, its path and the capture options", `{"type":"object"}`),
			method("peek", "read up to max_packets (256) from the source for at most timeout_ms and return decoded URB summaries; offline sources start from their beginning; not while a capture runs",
				`{"type":"object","properties":{`+optionProps+`,"max_packets":{"type":"number"},"timeout_ms":{"type":"number"}}}`),
		},
	}
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	srcs := p.list()
	out := make([]*pluginv1.Device, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, &pluginv1.Device{Id: s.ID, Name: s.ID, Transport: s.Backend, Capabilities: deviceCaps(), Properties: map[string]string{
			"backend": s.Backend, "live": strconv.FormatBool(s.Live), "synthetic": strconv.FormatBool(s.Synthetic), "path": s.Path,
			"privacy": "USB payloads can contain keystrokes and secrets; snaplen limits what is kept",
		}})
	}
	return out, nil
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func (p *Plugin) find(id string) (Source, error) {
	for _, s := range p.list() {
		if s.ID == id {
			return s, nil
		}
	}
	return Source{}, status.Errorf(codes.NotFound, "USB packet source %q not found (try `hwlab devices`; usbmon needs root or a readable %s, or set %s / %s)", id, DebugfsDir, SourcesEnv, SynthEnv)
}

func parseOptions(base Options, cfg map[string]any) (Options, error) {
	o := base
	for k, v := range cfg {
		switch k {
		case "speed":
			f, ok := v.(float64)
			if !ok || f <= 0 || f > 1000 {
				return o, fmt.Errorf("speed %v: want a number in (0, 1000]", v)
			}
			o.Speed = f
		case "snaplen":
			f, ok := v.(float64)
			if !ok || f < 0 || f > maxSnaplen || f != float64(int(f)) {
				return o, fmt.Errorf("snaplen %v: want an integer 0..%d", v, maxSnaplen)
			}
			o.Snaplen = int(f)
		default:
			return o, fmt.Errorf("unknown option %q (want speed, snaplen)", k)
		}
	}
	return o, nil
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	src, err := p.find(id)
	if err != nil {
		return err
	}
	opts, err := parseOptions(defaultOptions(), cfg)
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
	}
	p.devs[id] = &device{src: src, opts: opts}
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	delete(p.devs, id)
	p.mu.Unlock()
	return nil
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

// openReader opens a source for reading.
func openReader(s Source) (Reader, error) {
	switch s.Backend {
	case "synthetic":
		return &sliceReader{pkts: usbcap.Synthetic(s.Reports)}, nil
	case "usbmonbin":
		f, err := os.Open(s.Path)
		if err != nil {
			return nil, err
		}
		return newBinReader(f), nil
	case "usbmon", "file":
		f, err := os.Open(s.Path)
		if err != nil {
			return nil, err
		}
		return newTextReader(f), nil
	}
	return nil, fmt.Errorf("unknown backend %q", s.Backend)
}

type sliceReader struct {
	pkts []usbcap.Packet
	i    int
}

func (r *sliceReader) Next(ctx context.Context) (usbcap.Packet, error) {
	if err := ctx.Err(); err != nil {
		return usbcap.Packet{}, err
	}
	if r.i >= len(r.pkts) {
		return usbcap.Packet{}, io.EOF
	}
	r.i++
	return r.pkts[r.i-1], nil
}
func (r *sliceReader) Close() error              { return nil }
func (r *sliceReader) Skipped() (uint64, string) { return 0, "" }

type textResult struct {
	p   usbcap.Packet
	err error
}

// textReader parses usbmon text lines on its own goroutine so that Next can
// honour cancellation while a read blocks (a FIFO or the live usbmon file).
type textReader struct {
	f       *os.File
	ch      chan textResult
	done    chan struct{}
	once    sync.Once
	skipped atomic.Uint64
	mu      sync.Mutex
	first   string
}

func newTextReader(f *os.File) *textReader {
	r := &textReader{f: f, ch: make(chan textResult, 256), done: make(chan struct{})}
	go r.run()
	return r
}

func (r *textReader) run() {
	defer close(r.ch)
	sc := bufio.NewScanner(r.f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var tp usbcap.TextParser
	for sc.Scan() {
		p, ok, err := tp.Parse(sc.Text())
		if err != nil {
			r.skipped.Add(1)
			r.mu.Lock()
			if r.first == "" {
				r.first = err.Error()
			}
			r.mu.Unlock()
			continue
		}
		if !ok {
			continue
		}
		select {
		case r.ch <- textResult{p: p}:
		case <-r.done:
			return
		}
	}
	if err := sc.Err(); err != nil {
		select {
		case r.ch <- textResult{err: err}:
		case <-r.done:
		}
	}
}

func (r *textReader) Next(ctx context.Context) (usbcap.Packet, error) {
	select {
	case <-ctx.Done():
		return usbcap.Packet{}, ctx.Err()
	case res, ok := <-r.ch:
		if !ok {
			return usbcap.Packet{}, io.EOF
		}
		return res.p, res.err
	}
}

func (r *textReader) Close() error {
	r.once.Do(func() { close(r.done) })
	return r.f.Close()
}

func (r *textReader) Skipped() (uint64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped.Load(), r.first
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if req.GetCapability() != pluginv1.CapUSBPacketCapture {
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
	case "info":
		res, err = p.info(req.GetDeviceId())
	case "peek":
		res, err = p.peek(ctx, req.GetDeviceId(), args)
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

func (p *Plugin) resolve(id string) (Source, Options, *device, error) {
	if d := p.device(id); d != nil {
		return d.src, d.opts, d, nil
	}
	s, err := p.find(id)
	return s, defaultOptions(), nil, err
}

func (p *Plugin) info(id string) (map[string]any, error) {
	src, opts, d, err := p.resolve(id)
	if err != nil {
		return nil, err
	}
	running := false
	if d != nil {
		d.mu.Lock()
		running = d.cap != nil
		d.mu.Unlock()
	}
	return map[string]any{
		"id": src.ID, "backend": src.Backend, "path": src.Path, "live": src.Live, "synthetic": src.Synthetic,
		"speed": opts.Speed, "snaplen": float64(opts.Snaplen), "capturing": running,
		"layout": fmt.Sprintf("usbmon mmapped header, pcap link type %d; URB level: no SOF, tokens or handshakes", usbcap.LinkType),
	}, nil
}

func (p *Plugin) peek(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	src, opts, d, err := p.resolve(id)
	if err != nil {
		return nil, err
	}
	if d != nil {
		d.mu.Lock()
		busy := d.cap != nil
		d.mu.Unlock()
		if busy {
			return nil, status.Errorf(codes.FailedPrecondition, "%s has a running capture; peek would compete for its packets", id)
		}
	}
	optArgs := map[string]any{}
	n, timeout := defaultPeek, 1000
	for k, v := range args {
		switch k {
		case "max_packets", "timeout_ms":
			f, ok := v.(float64)
			if !ok || f != float64(int(f)) || f < 1 {
				return nil, argErr(fmt.Errorf("%s %v: want a positive integer", k, v))
			}
			if k == "max_packets" {
				n = int(f)
				if n > maxPeek {
					return nil, argErr(fmt.Errorf("max_packets %d: at most %d", n, maxPeek))
				}
			} else {
				timeout = int(f)
				if timeout > 60000 {
					return nil, argErr(errors.New("timeout_ms: at most 60000"))
				}
			}
		default:
			optArgs[k] = v
		}
	}
	if _, err := parseOptions(opts, optArgs); err != nil {
		return nil, argErr(err)
	}
	rd, err := openReader(src)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", id, err)
	}
	defer rd.Close()
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	var pkts []usbcap.Packet
	for len(pkts) < n {
		pk, err := rd.Next(cctx)
		if err != nil {
			break
		}
		pkts = append(pkts, pk)
	}
	list := make([]any, 0, len(pkts))
	var t0 int64
	for i, pk := range pkts {
		if i == 0 {
			t0 = pk.TimestampNs
		}
		list = append(list, map[string]any{"t_ms": float64(pk.TimestampNs-t0) / 1e6, "summary": pk.Summary()})
	}
	skipped, first := rd.Skipped()
	out := map[string]any{"packets": list, "count": float64(len(list))}
	if skipped > 0 {
		out["skipped_lines"], out["first_skip_error"] = float64(skipped), first
	}
	return out, nil
}
