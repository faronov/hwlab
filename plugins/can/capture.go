package canplugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/can"
	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// StreamFrames is the stream id of every CAN event.
const StreamFrames = "frames"

const (
	pollEvery  = 100 * time.Millisecond
	batchMax   = 128
	batchAge   = 25 * time.Millisecond
	finalEmit  = 2 * time.Second
	optionKeys = "fd, own, error_frames, filters"
)

// capture reads one open interface into timestamped CAN packet events and a
// pcapng artifact until its context ends.
type capture struct {
	dev      *device
	bus      Bus
	info     IfaceInfo
	opts     Options
	clock    *pluginsdk.Clock
	sink     *pluginsdk.Sink
	closeBus func() // set when the capture opened the interface itself
	wg       sync.WaitGroup

	pmu      sync.Mutex // guards ng and the pcapng write error
	pcapPath string
	file     *os.File
	buf      *bufio.Writer
	ng       *pcap.NGWriter

	mu                                    sync.Mutex
	frames, rxFrames, txFrames            uint64
	fdFrames, rtrFrames, errFrames        uint64
	kernelStamped, events, droppedBatches uint64
	err                                   error
	res                                   *pluginv1.StopCaptureResponse

	// pending is only touched by the run goroutine.
	pending []*pluginv1.Event
	pendAt  time.Time
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *capture) event(f can.Frame, at int64) *pluginv1.Event {
	f.TimestampNs = at
	return &pluginv1.Event{TimestampNs: at, StreamId: StreamFrames, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
		Protocol:   can.Protocol,
		LinkType:   can.LinkType,
		Data:       f.Encode(),
		Summary:    f.Summary(),
		Attributes: f.Attributes(),
	}}}
}

// count updates the frame counters.
func (c *capture) count(f can.Frame, kernel bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames++
	switch {
	case f.Error:
		c.errFrames++
	case f.Own:
		c.txFrames++
	default:
		c.rxFrames++
	}
	if f.FD {
		c.fdFrames++
	}
	if f.RTR {
		c.rtrFrames++
	}
	if kernel {
		c.kernelStamped++
	}
}

// writePcap appends a frame to the artifact. Frames reach the artifact even
// when the live event batch is dropped, so the file is the complete record.
func (c *capture) writePcap(f can.Frame, at int64) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.ng == nil {
		return
	}
	if err := c.ng.WritePacket(at, f.Encode()); err != nil {
		c.fail(fmt.Errorf("pcapng write: %w", err))
		c.ng = nil
	}
}

func (c *capture) record(f can.Frame, at int64, kernel bool) {
	c.count(f, kernel)
	c.writePcap(f, at)
	if len(c.pending) == 0 {
		c.pendAt = time.Now()
	}
	c.pending = append(c.pending, c.event(f, at))
}

func (c *capture) flush(final bool) {
	if len(c.pending) == 0 {
		return
	}
	batch := c.pending
	c.pending = nil // the sink keeps the slice
	ok := false
	if final {
		ctx, cancel := context.WithTimeout(context.Background(), finalEmit)
		defer cancel()
		ok = c.sink.Emit(ctx, batch...) == nil
	} else {
		ok = c.sink.TryEmit(batch...)
	}
	c.mu.Lock()
	if ok {
		c.events += uint64(len(batch))
	} else {
		c.droppedBatches++
	}
	c.mu.Unlock()
}

// onTx records a frame the plugin sent while the capture runs on an interface
// that does not echo its own frames. at was taken before the write.
func (c *capture) onTx(f can.Frame, at time.Time) {
	f.Own = true
	f.Interface = c.info.Name
	c.count(f, false)
	ns := c.clock.At(at)
	c.writePcap(f, ns)
	ok := c.sink.TryEmit(c.event(f, ns))
	c.mu.Lock()
	if ok {
		c.events++
	} else {
		c.droppedBatches++
	}
	c.mu.Unlock()
}

func (c *capture) run(ctx context.Context) {
	defer c.wg.Done()
	defer c.flush(true)
	for ctx.Err() == nil {
		rx, err := c.bus.Recv(pollEvery)
		switch {
		case errors.Is(err, ErrTimeout):
		case err != nil:
			if ctx.Err() == nil {
				c.fail(fmt.Errorf("receive: %w", err))
			}
			return
		default:
			c.record(rx.Frame, c.clock.At(rx.At), rx.Kernel)
		}
		if len(c.pending) >= batchMax || (len(c.pending) > 0 && time.Since(c.pendAt) >= batchAge) {
			c.flush(false)
		}
	}
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error {
	c.wg.Wait()
	c.dev.endCapture(c)
	if c.closeBus != nil {
		c.closeBus()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dev.mu.Lock()
	c.dev.rx += c.rxFrames
	c.dev.mu.Unlock()
	res := &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.events, DroppedBatches: c.droppedBatches},
		Clock: c.clockInfo(),
	}
	c.pmu.Lock()
	if c.ng != nil {
		if err := c.ng.Close(c.clock.Now(), c.frames, 0, fmt.Sprintf("hwlab can %s: %d frames", c.info.Name, c.frames)); err != nil && c.err == nil {
			c.err = err
		}
		if err := c.buf.Flush(); err != nil && c.err == nil {
			c.err = err
		}
		if err := c.file.Close(); err != nil && c.err == nil {
			c.err = err
		}
		if c.err == nil {
			res.Artifacts = append(res.Artifacts, &pluginv1.ArtifactRef{Name: filepath.Base(c.pcapPath), Kind: "pcap",
				MimeType: "application/vnd.tcpdump.pcapng", Path: c.pcapPath, Metadata: c.meta()})
		}
	} else if c.file != nil {
		_ = c.file.Close()
	}
	c.pmu.Unlock()
	c.res = res
	return c.err
}

func (c *capture) meta() map[string]string {
	m := map[string]string{
		"link_type": fmt.Sprint(can.LinkType), "interface": c.info.Name, "backend": c.info.Backend,
		"frames": fmt.Sprint(c.frames), "rx": fmt.Sprint(c.rxFrames), "tx": fmt.Sprint(c.txFrames),
		"fd_frames": fmt.Sprint(c.fdFrames), "rtr_frames": fmt.Sprint(c.rtrFrames), "error_frames": fmt.Sprint(c.errFrames),
		"events_dropped_batches": fmt.Sprint(c.droppedBatches), "fd_enabled": fmt.Sprint(c.opts.FD),
		"timestamps": "hwlab session ns",
	}
	if c.info.Virtual {
		m["synthetic"] = "true"
	}
	return m
}

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse { return c.res }

// clockInfo states the timing quality honestly. A kernel receive timestamp is
// taken by the driver when the frame arrives, so it is late only by the
// interrupt latency; a host stamp is late by the wakeup and read latency on
// top. Neither was measured against a bus reference. The in-process virtual
// bus has no bus timing at all.
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{ErrorMinNs: 0, ErrorMinBounded: true}
	switch {
	case c.info.Backend == "virtual":
		ci.Source = "host clock at in-process delivery (synthetic bus)"
		ci.ResolutionNs = 1
		ci.Note = "synthetic virtual interface: no physical bus, no bit timing; the stamps order frames and nothing more"
	case c.frames > 0 && c.kernelStamped == c.frames:
		ci.Source = "kernel receive timestamp (SO_TIMESTAMPNS) mapped to session time"
		ci.ResolutionNs = 1
		ci.Note = fmt.Sprintf("%d frames; late only by the driver interrupt latency, which was not measured; the frame started earlier than its stamp by its bus time", c.frames)
	case c.kernelStamped > 0:
		ci.Source = "kernel receive timestamp (SO_TIMESTAMPNS), host clock for the rest, mapped to session time"
		ci.ResolutionNs = 1000
		ci.Note = fmt.Sprintf("%d of %d frames carry a kernel stamp; the others (frames the plugin sent on a non-echoing interface) are stamped before the write", c.kernelStamped, c.frames)
	default:
		ci.Source = "host clock at socket read return"
		ci.ResolutionNs = 1_000_000
		ci.Note = "no kernel timestamps: late by the unmeasured wakeup and read latency"
	}
	if c.info.Virtual && c.info.Backend != "virtual" {
		ci.Note += "; a vcan has no physical bus timing"
	}
	return ci
}

// StartCapture implements pluginsdk.Plugin: every frame on the interface
// becomes a timestamped "can" packet event and a record in a pcapng artifact
// (link type 227, SocketCAN), which Wireshark decodes natively.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	c := &capture{clock: req.Clock, sink: sink}
	if d := p.device(req.DeviceID); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.cap != nil {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID)
		}
		if hasOption(req.Config) {
			o, err := parseOptions(d.opts, d.info, req.Config)
			if err != nil {
				return nil, nil, argErr(err)
			}
			if !reflect.DeepEqual(optionsMap(o), optionsMap(d.opts)) {
				return nil, nil, status.Errorf(codes.FailedPrecondition,
					"%s was opened with different options (%s are fixed at open): close it or set them on the device", req.DeviceID, optionKeys)
			}
		}
		c.dev, c.bus, c.info, c.opts = d, d.bus, d.info, d.opts
	} else {
		info, err := p.find(req.DeviceID)
		if err != nil {
			return nil, nil, err
		}
		opts, err := parseOptions(defaultOptions(info), info, req.Config)
		if err != nil {
			return nil, nil, argErr(err)
		}
		bus, err := p.open(req.DeviceID, opts)
		if err != nil {
			return nil, nil, status.Errorf(codes.Unavailable, "open %s: %v", req.DeviceID, err)
		}
		d := &device{id: req.DeviceID, info: info, bus: bus, opts: opts}
		d.mu.Lock()
		defer d.mu.Unlock()
		c.dev, c.bus, c.info, c.opts = d, bus, info, opts
		c.closeBus = func() { _ = bus.Close() }
	}
	closeOnErr := func() {
		if c.closeBus != nil {
			c.closeBus()
		}
	}

	if req.ArtifactDir != "" {
		if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
			closeOnErr()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
		c.pcapPath = filepath.Join(req.ArtifactDir, fmt.Sprintf("can-%s.pcapng", c.info.Name))
		f, err := os.Create(c.pcapPath)
		if err != nil {
			closeOnErr()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
		c.file = f
		c.buf = bufio.NewWriterSize(f, 64<<10)
		desc := fmt.Sprintf("CAN interface %s (%s), timestamps are hwlab session ns", c.info.Name, c.info.Backend)
		if c.info.Virtual {
			desc += ", SYNTHETIC virtual bus"
		}
		if c.ng, err = pcap.NewNGWriter(c.buf, "hwlab can", pcap.NGInterface{LinkType: pcap.LinkCANSocket, Name: c.info.Name, Description: desc}); err != nil {
			_ = f.Close()
			closeOnErr()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
	}
	c.dev.cap = c
	c.wg.Add(1)
	// ctx outlives this call: the SDK cancels it on duration expiry and StopCapture.
	go c.run(ctx)

	attrs := map[string]string{
		"interface": c.info.Name, "backend": c.info.Backend, "link_type": fmt.Sprint(can.LinkType),
		"fd": fmt.Sprint(c.opts.FD), "own": fmt.Sprint(c.opts.Own), "error_frames": fmt.Sprint(c.opts.ErrorFrames),
	}
	if c.info.Virtual {
		attrs["synthetic"] = "true"
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: StreamFrames, Kind: "packet", Attributes: attrs}}}, nil
}

func hasOption(cfg map[string]any) bool {
	for _, k := range []string{"fd", "own", "error_frames", "filters"} {
		if _, ok := cfg[k]; ok {
			return true
		}
	}
	return false
}
