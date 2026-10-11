package uart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Stream ids and the protocol name carried by every serial event.
const (
	StreamRX    = "rx"
	StreamTX    = "tx"
	StreamLines = "lines"
	Protocol    = "uart"
	LogSource   = "uart"

	maxLineLen = 4096
	emitWait   = 2 * time.Second
)

// capture reads one open port into timestamped byte chunks (and optionally
// text lines) until its context ends.
type capture struct {
	dev       *device
	port      Port
	path      string
	mode      Mode
	cfg       captureConfig
	clock     *pluginsdk.Clock
	sink      *pluginsdk.Sink
	closePort func() // set when the capture opened the port itself
	wg        sync.WaitGroup

	mu      sync.Mutex
	rxBytes uint64
	txBytes uint64
	events  uint64
	err     error
	res     *pluginv1.StopCaptureResponse

	lines *lineSplitter
}

func (c *capture) attrs(dir string) map[string]string {
	return map[string]string{"port": c.path, "dir": dir, "mode": c.mode.Framing(), "baud": strconv.Itoa(c.mode.Baud)}
}

func (c *capture) emit(evs ...*pluginv1.Event) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), emitWait)
	defer cancel()
	if err := c.sink.Emit(ctx, evs...); err != nil {
		c.fail(fmt.Errorf("event delivery: %w", err))
		return
	}
	c.mu.Lock()
	c.events += uint64(len(evs))
	c.mu.Unlock()
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *capture) packet(stream, dir string, data []byte, at int64) *pluginv1.Event {
	return &pluginv1.Event{TimestampNs: at, StreamId: stream, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
		Protocol:   Protocol,
		Data:       append([]byte(nil), data...),
		Summary:    fmt.Sprintf("%s %d B %s", dir, len(data), preview(data, 32)),
		Attributes: c.attrs(dir),
	}}}
}

// txEvent records bytes written while the capture runs, stamped before the
// write call so a tx chunk never appears after the reply it provoked.
func (c *capture) txEvent(data []byte, at int64) {
	c.mu.Lock()
	c.txBytes += uint64(len(data))
	c.mu.Unlock()
	c.emit(c.packet(StreamTX, "tx", data, at))
}

func (c *capture) run(ctx context.Context) {
	defer c.wg.Done()
	_ = c.port.SetReadTimeout(c.cfg.idleGap)
	var (
		buf      = make([]byte, 4096)
		chunk    []byte
		chunkAt  int64
		chunkT0  time.Time
		pending  []*pluginv1.Event
		flushAll = func() {
			for len(chunk) > 0 {
				n := min(len(chunk), c.cfg.maxChunkBytes)
				pending = append(pending, c.packet(StreamRX, "rx", chunk[:n], chunkAt))
				chunk = chunk[n:]
			}
			chunk = nil
			if c.lines != nil {
				pending = append(pending, c.lines.drain()...)
			}
			c.emit(pending...)
			pending = nil // the sink keeps the slice
		}
	)
	defer func() {
		if c.lines != nil {
			pending = append(pending, c.lines.finish()...)
		}
		flushAll()
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		n, err := c.port.Read(buf)
		at := c.clock.Now()
		now := time.Now()
		if n > 0 {
			if len(chunk) == 0 {
				chunkAt, chunkT0 = at, now
			}
			chunk = append(chunk, buf[:n]...)
			c.mu.Lock()
			c.rxBytes += uint64(n)
			c.mu.Unlock()
			if c.lines != nil {
				pending = append(pending, c.lines.feed(buf[:n], at)...)
			}
		}
		if err != nil {
			if ctx.Err() == nil {
				if errors.Is(err, io.EOF) {
					err = errors.New("the port was closed or the device disconnected")
				}
				c.fail(fmt.Errorf("serial read: %w", err))
			}
			return
		}
		if len(chunk) > 0 && (n == 0 || len(chunk) >= c.cfg.maxChunkBytes || now.Sub(chunkT0) >= c.cfg.maxChunkAge) {
			flushAll()
		} else if n == 0 && len(pending) > 0 {
			flushAll()
		}
	}
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error {
	c.wg.Wait()
	c.dev.endCapture(c)
	if c.closePort != nil {
		c.closePort()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res = &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.events},
		Clock: clockInfo(c.rxBytes, c.txBytes, c.mode),
	}
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse { return c.res }

// clockInfo states the timing quality honestly. A received chunk is stamped
// when the host read returns: after the byte arrived, after the USB/driver
// latency (an FTDI latency timer is 16 ms by default), never before. A
// transmitted chunk is stamped before the write call, so it may be early by
// the driver latency plus the time the bytes need on the wire.
func clockInfo(rx, tx uint64, m Mode) *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{
		Source:       "host clock at serial read return (rx) / before write call (tx)",
		ResolutionNs: 1_000_000,
		Note: fmt.Sprintf("%s; rx %d bytes, tx %d bytes; the chunk stamp is the first read of the chunk, "+
			"late by the unmeasured USB and driver latency", m, rx, tx),
	}
	if tx == 0 {
		ci.ErrorMinNs, ci.ErrorMinBounded = 0, true
	}
	return ci
}

// preview renders up to max bytes of data as a quoted string for summaries.
func preview(data []byte, max int) string {
	cut := len(data) > max
	if cut {
		data = data[:max]
	}
	s := strconv.Quote(strings.ToValidUTF8(string(data), "\uFFFD"))
	if cut {
		s += "…"
	}
	return s
}

// lineSplitter turns a byte stream into text lines, each stamped with the
// time its first byte was read.
type lineSplitter struct {
	c     *capture
	buf   []byte
	start int64
	out   []*pluginv1.Event
}

func (l *lineSplitter) feed(data []byte, at int64) []*pluginv1.Event {
	for _, b := range data {
		if len(l.buf) == 0 {
			l.start = at
		}
		if b == '\n' {
			l.flushLine()
			continue
		}
		l.buf = append(l.buf, b)
		if len(l.buf) >= maxLineLen {
			l.flushLine()
		}
	}
	return l.drain()
}

func (l *lineSplitter) flushLine() {
	line := strings.TrimRight(string(l.buf), "\r")
	l.buf = l.buf[:0]
	if strings.TrimSpace(line) == "" {
		return
	}
	if !utf8.ValidString(line) {
		line = strings.ToValidUTF8(line, "\uFFFD")
	}
	l.out = append(l.out, &pluginv1.Event{TimestampNs: l.start, StreamId: StreamLines,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: "info", Source: LogSource, Message: line,
			Attributes: map[string]string{"port": l.c.path, "dir": "rx"}}}})
}

func (l *lineSplitter) drain() []*pluginv1.Event {
	out := l.out
	l.out = nil
	return out
}

// finish emits an unterminated last line.
func (l *lineSplitter) finish() []*pluginv1.Event {
	if len(l.buf) > 0 {
		l.flushLine()
	}
	return l.drain()
}
