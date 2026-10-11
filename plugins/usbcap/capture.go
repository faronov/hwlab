package usbcapplugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/pcap"
	"github.com/faronov/hwlab/internal/usbcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// StreamPackets is the stream id of every USB event.
const StreamPackets = "packets"

const (
	batchMax  = 128
	batchAge  = 25 * time.Millisecond
	finalEmit = 2 * time.Second
)

// capture reads one source into timestamped "usb" packet events and a pcapng
// artifact until its context ends or an offline source reaches its end.
type capture struct {
	dev      *device
	src      Source
	opts     Options
	rd       Reader
	clock    *pluginsdk.Clock
	sink     *pluginsdk.Sink
	wg       sync.WaitGroup
	pcapPath string
	file     *os.File
	buf      *bufio.Writer
	ng       *pcap.NGWriter

	mu                                    sync.Mutex
	packets, submits, completes, errs     uint64
	bytes, truncated, events, droppedBatc uint64
	byTransfer                            [4]uint64
	eof                                   bool
	err                                   error
	res                                   *pluginv1.StopCaptureResponse

	pending []*pluginv1.Event // run goroutine only
	pendAt  time.Time
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
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
		c.droppedBatc++
	}
	c.mu.Unlock()
}

// record keeps one packet: counters, pcapng (always) and a live event (batched).
func (c *capture) record(p usbcap.Packet, at int64) {
	if n := c.opts.Snaplen; len(p.Data) > n {
		p.Data = p.Data[:n]
		c.mu.Lock()
		c.truncated++
		c.mu.Unlock()
	}
	p.TimestampNs = at
	enc := p.Encode()

	c.mu.Lock()
	c.packets++
	c.byTransfer[p.Transfer&3]++
	switch p.Event {
	case usbcap.Submit:
		c.submits++
	default:
		c.completes++
		c.bytes += uint64(len(p.Data))
		if p.Status != 0 {
			c.errs++
		}
	}
	c.mu.Unlock()

	if c.ng != nil {
		if err := c.ng.WritePacket(at, enc); err != nil {
			c.fail(fmt.Errorf("pcapng write: %w", err))
			c.ng = nil
		}
	}
	if len(c.pending) == 0 {
		c.pendAt = time.Now()
	}
	c.pending = append(c.pending, &pluginv1.Event{TimestampNs: at, StreamId: StreamPackets, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
		Protocol: usbcap.Protocol, LinkType: usbcap.LinkType, Data: enc, Summary: p.Summary(), Attributes: p.Attributes(),
	}}})
}

func (c *capture) run(ctx context.Context) {
	defer c.wg.Done()
	defer c.flush(true)
	var (
		anchor time.Time
		first  int64
		have   bool
	)
	for ctx.Err() == nil {
		// Flush on age even while waiting for the next packet.
		rctx, cancel := ctx, context.CancelFunc(func() {})
		if len(c.pending) > 0 {
			rctx, cancel = context.WithTimeout(ctx, batchAge)
		}
		p, err := c.rd.Next(rctx)
		cancel()
		switch {
		case errors.Is(err, io.EOF):
			c.mu.Lock()
			c.eof = true
			c.mu.Unlock()
			return
		case err != nil && ctx.Err() != nil:
			return
		case err != nil && errors.Is(err, context.DeadlineExceeded):
			c.flush(false)
			continue
		case err != nil:
			c.fail(fmt.Errorf("read %s: %w", c.src.ID, err))
			return
		}
		if !have {
			anchor, first, have = time.Now(), p.TimestampNs, true
		}
		target := anchor.Add(time.Duration(float64(p.TimestampNs-first) / c.opts.Speed))
		if !c.src.Live {
			if d := time.Until(target); d > 0 {
				c.flush(false)
				t := time.NewTimer(d)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return
				}
			}
		}
		c.record(p, c.clock.At(target))
		if len(c.pending) >= batchMax || (len(c.pending) > 0 && time.Since(c.pendAt) >= batchAge) {
			c.flush(false)
		}
	}
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error {
	c.wg.Wait()
	_ = c.rd.Close()
	c.dev.mu.Lock()
	if c.dev.cap == c {
		c.dev.cap = nil
	}
	c.dev.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	res := &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.events, DroppedBatches: c.droppedBatc},
		Clock: c.clockInfo(),
	}
	if c.ng != nil {
		comment := fmt.Sprintf("hwlab usbcap %s: %d packets", c.src.ID, c.packets)
		if err := c.ng.Close(c.clock.Now(), c.packets, 0, comment); err != nil && c.err == nil {
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
	c.res = res
	return c.err
}

func (c *capture) meta() map[string]string {
	skipped, first := c.rd.Skipped()
	m := map[string]string{
		"link_type": fmt.Sprint(usbcap.LinkType), "source": c.src.ID, "backend": c.src.Backend,
		"packets": fmt.Sprint(c.packets), "submissions": fmt.Sprint(c.submits), "completions": fmt.Sprint(c.completes),
		"errors": fmt.Sprint(c.errs), "snaplen": fmt.Sprint(c.opts.Snaplen), "truncated_by_snaplen": fmt.Sprint(c.truncated),
		"events_dropped_batches": fmt.Sprint(c.droppedBatc), "skipped_lines": fmt.Sprint(skipped),
		"layout":      "usbmon mmapped header; URB level",
		"isochronous": fmt.Sprint(c.byTransfer[usbcap.Isochronous]), "interrupt": fmt.Sprint(c.byTransfer[usbcap.Interrupt]),
		"control": fmt.Sprint(c.byTransfer[usbcap.Control]), "bulk": fmt.Sprint(c.byTransfer[usbcap.Bulk]),
		"reached_end_of_source": fmt.Sprint(c.eof),
	}
	if first != "" {
		m["first_skip_error"] = first
	}
	if c.src.Synthetic {
		m["synthetic"] = "true"
	}
	return m
}

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse { return c.res }

// clockInfo states the timing quality honestly. usbmon stamps are taken by the
// kernel when the URB is submitted or completed, in microseconds, on a clock
// that is not related to the host's: only differences are used, anchored at the
// arrival of the first packet. Neither the anchor nor the stamp accuracy was
// measured, and the stamps are URB (host controller driver) times, not bus
// times: a completion is late by the interrupt, a submission early by the
// queueing.
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{ErrorMinBounded: true}
	switch {
	case c.src.Synthetic:
		ci.Source = "synthetic generator timeline, paced on the host clock"
		ci.ResolutionNs = 1000
		ci.Note = "synthetic data: the timing is made up and says nothing about any bus"
	case c.src.Live:
		ci.Source = "usbmon kernel timestamps (µs, differences only) anchored at the first packet's arrival on the host clock"
		ci.ResolutionNs = 1000
		ci.Note = fmt.Sprintf("%d packets; URB submission/completion times of the host stack, not bus times; the anchor offset (read latency of the first packet) and the clock drift were not measured", c.packets)
	default:
		ci.Source = "recorded usbmon timestamps (µs, differences only), replayed on the host clock"
		ci.ResolutionNs = 1000
		ci.Note = fmt.Sprintf("offline source replayed at %gx: the session times are the recording's relative times, not when the traffic really happened", c.opts.Speed)
	}
	return ci
}

// StartCapture implements pluginsdk.Plugin: every URB event becomes a
// timestamped "usb" packet event and a record in a pcapng artifact (link type
// 220, usbmon mmapped) that Wireshark decodes natively.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	d := p.device(req.DeviceID)
	var src Source
	opts := defaultOptions()
	if d != nil {
		src, opts = d.src, d.opts
	} else {
		var err error
		if src, err = p.find(req.DeviceID); err != nil {
			return nil, nil, err
		}
		d = &device{src: src, opts: opts}
	}
	opts, err := parseOptions(opts, req.Config)
	if err != nil {
		return nil, nil, argErr(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cap != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID)
	}
	rd, err := openReader(src)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "open %s: %v (usbmon needs root or a readable %s)", src.ID, err, DebugfsDir)
	}
	c := &capture{dev: d, src: src, opts: opts, rd: rd, clock: req.Clock, sink: sink}
	if req.ArtifactDir != "" {
		if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
			_ = rd.Close()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
		c.pcapPath = filepath.Join(req.ArtifactDir, fmt.Sprintf("usb-%s.pcapng", src.ID))
		f, err := os.Create(c.pcapPath)
		if err != nil {
			_ = rd.Close()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
		c.file, c.buf = f, bufio.NewWriterSize(f, 64<<10)
		desc := fmt.Sprintf("USB source %s (%s), URB level, timestamps are hwlab session ns", src.ID, src.Backend)
		if src.Synthetic {
			desc += ", SYNTHETIC data"
		}
		if c.ng, err = pcap.NewNGWriter(c.buf, "hwlab usbcap", pcap.NGInterface{LinkType: usbcap.LinkType, Name: src.ID, Description: desc}); err != nil {
			_ = f.Close()
			_ = rd.Close()
			return nil, nil, status.Error(codes.Internal, err.Error())
		}
	}
	d.cap = c
	c.wg.Add(1)
	// ctx outlives this call: the SDK cancels it on duration expiry and StopCapture.
	go c.run(ctx)

	attrs := map[string]string{
		"source": src.ID, "backend": src.Backend, "link_type": fmt.Sprint(usbcap.LinkType), "snaplen": fmt.Sprint(opts.Snaplen),
		"live": fmt.Sprint(src.Live),
	}
	if src.Synthetic {
		attrs["synthetic"] = "true"
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: StreamPackets, Kind: "packet", Attributes: attrs}}}, nil
}
