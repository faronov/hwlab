package nrf802154

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

const (
	packetStream = "packets"
	// Frames are batched so the sink sees one message per ~50 ms or 64 frames.
	batchMax   = 64
	batchEvery = 50 * time.Millisecond
	decodeWait = 60 * time.Second
)

type capture struct {
	p       *Plugin
	dev     *protocol.Device
	release func()
	channel int
	clock   *pluginsdk.Clock
	cm      *protocol.ClockMap
	sink    *pluginsdk.Sink

	pcapPath string
	file     *os.File
	buf      *bufio.Writer
	ng       *pcap.NGWriter
	wallAt   time.Time // wall time matching session time sessionAt
	sessAt   int64
	wantDec  bool

	batch    []*pluginv1.Event
	batchAt  time.Time
	tsList   []int64 // session ns per written frame, for mapping decode output back
	packets  uint64
	rssiSum  int64
	decoded  uint64
	writeErr error
	runErr   error
	decMeta  string

	done chan struct{}
	res  *pluginv1.StopCaptureResponse
}

// StartCapture implements pluginsdk.Plugin. It configures the radio before
// returning, so a missing or misbehaving device is reported to the caller
// instead of producing an empty capture.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	v, ok := req.Config["channel"]
	if !ok {
		return nil, nil, status.Error(codes.InvalidArgument, "config.channel is required (11-26)")
	}
	ch, err := ValidateChannel(v)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	port, err := p.resolvePort(req.DeviceID, req.Config)
	if err != nil {
		return nil, nil, err
	}
	release, err := p.acquire(port)
	if err != nil {
		return nil, nil, err
	}
	dev, err := p.openSniffer(port)
	if err != nil {
		release()
		return nil, nil, err
	}
	fail := func(err error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		_ = dev.Close()
		release()
		return nil, nil, err
	}
	if err := dev.Start(ctx, ch); err != nil {
		return fail(status.Errorf(codes.Unavailable, "start sniffer on %s: %v", port, err))
	}
	c := &capture{p: p, dev: dev, release: release, channel: ch, clock: req.Clock, cm: protocol.NewClockMap(), sink: sink,
		wallAt: time.Now(), sessAt: req.Clock.Now(), done: make(chan struct{}), wantDec: true}
	if b, ok := req.Config["decode"].(bool); ok {
		c.wantDec = b
	}
	if req.ArtifactDir != "" {
		if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
			return fail(err)
		}
		c.pcapPath = filepath.Join(req.ArtifactDir, fmt.Sprintf("nrf802154-ch%d.pcapng", ch))
		if c.file, err = os.Create(c.pcapPath); err != nil {
			return fail(err)
		}
		c.buf = bufio.NewWriterSize(c.file, 64<<10)
		desc := fmt.Sprintf("nRF 802.15.4 sniffer %s, channel %d, timestamps are hwlab session ns", req.DeviceID, ch)
		if c.ng, err = pcap.NewNGWriter(c.buf, "hwlab nrf802154", pcap.NGInterface{LinkType: pcap.LinkIEEE802154TAP, Name: fmt.Sprintf("ch%d", ch), Description: desc}); err != nil {
			_ = c.file.Close()
			return fail(err)
		}
	}
	go c.run(ctx)
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{
		StreamId: packetStream, Kind: "packet",
		Attributes: map[string]string{
			"channel": fmt.Sprint(ch), "link_type": fmt.Sprint(pcap.LinkIEEE802154NoFCS), "device_timer": "us since boot",
			"clock": "device timer mapped to session time with a windowed minimum-offset estimator",
		}}}}, nil
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error { <-c.done; return c.runErr }

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse { <-c.done; return c.res }

func (c *capture) run(ctx context.Context) {
	defer close(c.done)
	c.runErr = c.dev.Run(ctx, c.onFrame, c.onTick)
	c.flush(true)
	_ = c.dev.Close()
	c.release()
	if c.runErr != nil {
		c.emitLog("error", fmt.Sprintf("sniffer capture aborted: %v", c.runErr))
	}
	if c.dev.Malformed > 0 {
		c.emitLog("warn", fmt.Sprintf("%d garbled lines from the sniffer were discarded", c.dev.Malformed))
	}
	c.finishFile()
	c.decode()
	c.buildResult()
}

func (c *capture) onFrame(f protocol.Frame, rx time.Time) {
	hostNs := c.clock.At(rx)
	ts := c.cm.Map(f.DevUs, hostNs)
	c.packets++
	c.rssiSum += int64(f.RSSI)
	if c.ng != nil && c.writeErr == nil {
		tap := pcap.EncodeTAP15_4(pcap.TAPMeta{Channel: uint16(c.channel), RSSIdBm: float32(f.RSSI), LQI: uint8(f.LQI)}, f.Data)
		c.writeErr = c.ng.WritePacket(ts, tap)
		c.tsList = append(c.tsList, ts)
	}
	if len(c.batch) == 0 {
		c.batchAt = rx
	}
	c.batch = append(c.batch, &pluginv1.Event{TimestampNs: ts, StreamId: packetStream, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
		Protocol: "ieee802154", LinkType: pcap.LinkIEEE802154NoFCS, Data: f.Data, RssiDbm: float64(f.RSSI), Lqi: uint32(f.LQI),
		Channel: uint32(c.channel),
		Summary: fmt.Sprintf("ch%d %d B rssi %d dBm lqi %d", c.channel, len(f.Data), f.RSSI, f.LQI),
		Attributes: map[string]string{
			"dev_ts_us":  fmt.Sprint(f.DevUs),
			"host_rx_ns": fmt.Sprint(hostNs),
			"latency_ns": fmt.Sprint(hostNs - ts),
			"seq":        fmt.Sprint(c.packets),
		}}}})
	if len(c.batch) >= batchMax {
		c.flush(false)
	}
}

func (c *capture) onTick(now time.Time) {
	if len(c.batch) > 0 && now.Sub(c.batchAt) >= batchEvery {
		c.flush(false)
	}
}

// flush hands the batch to the sink. A live radio cannot be paused, so a full
// sink drops the batch (counted and reported to the core) instead of blocking;
// the pcap artifact still contains every frame. The final flush blocks briefly.
func (c *capture) flush(final bool) {
	if len(c.batch) == 0 {
		return
	}
	b := c.batch
	c.batch = nil
	if final {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.sink.Emit(ctx, b...)
		return
	}
	c.sink.TryEmit(b...)
}

func (c *capture) emitLog(level, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.sink.Emit(ctx, &pluginv1.Event{TimestampNs: c.clock.Now(), StreamId: packetStream,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: level, Source: "nrf802154", Message: msg}}})
}

// clockComment is the human/machine readable clock summary stored in the pcapng
// interface statistics block and in the artifact metadata.
func (c *capture) clockMeta() map[string]string {
	s := c.cm.Stats()
	return map[string]string{
		"clock.source":          "device timer (us since boot), mapped by minimum-offset estimator",
		"clock.offset_ns":       fmt.Sprint(s.OffsetNs),
		"clock.latency_min_ns":  fmt.Sprint(s.LatencyMinNs),
		"clock.latency_mean_ns": fmt.Sprint(s.LatencyAvgNs),
		"clock.latency_max_ns":  fmt.Sprint(s.LatencyMaxNs),
		"clock.drift_ppm":       fmt.Sprintf("%.2f", s.DriftPPM),
		"clock.device_resets":   fmt.Sprint(s.Resets),
		"clock.resolution_ns":   "1000",
		"clock.note":            "relative timing between frames is device-timer accurate; absolute alignment is late by the unknown minimum USB transit latency",
		"wall.start_utc":        c.wallAt.UTC().Format(time.RFC3339Nano),
		"session.start_ns":      fmt.Sprint(c.sessAt),
	}
}

func (c *capture) finishFile() {
	if c.file == nil {
		return
	}
	var sb strings.Builder
	meta := c.clockMeta()
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		fmt.Fprintf(&sb, "%s=%s\n", k, meta[k])
	}
	if c.writeErr == nil {
		c.writeErr = c.ng.Close(c.clock.Now(), c.packets, c.sink.Dropped(), sb.String())
	}
	if err := c.buf.Flush(); c.writeErr == nil {
		c.writeErr = err
	}
	if err := c.file.Close(); c.writeErr == nil {
		c.writeErr = err
	}
	if c.writeErr != nil {
		c.emitLog("error", fmt.Sprintf("writing %s: %v", c.pcapPath, c.writeErr))
	}
}

// decode runs the optional tshark pass and emits normalized events for every
// frame that is not a bare acknowledgement. Missing tshark is not an error.
func (c *capture) decode() {
	if c.pcapPath == "" || c.writeErr != nil || !c.wantDec || c.packets == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), decodeWait)
	defer cancel()
	sums, err := c.p.dec.Summaries(ctx, c.pcapPath)
	if err != nil {
		c.decMeta = "skipped: " + err.Error()
		return
	}
	var evs []*pluginv1.Event
	for _, s := range sums {
		if s.Frame < 1 || s.Frame > len(c.tsList) || strings.HasPrefix(s.Info, "Ack") {
			continue
		}
		layers := strings.Split(s.Protocols, ":")
		top := layers[len(layers)-1]
		fields, _ := structpb.NewStruct(map[string]any{"frame": float64(s.Frame), "layers": s.Protocols})
		evs = append(evs, &pluginv1.Event{TimestampNs: c.tsList[s.Frame-1], StreamId: packetStream, Payload: &pluginv1.Event_Decoded{
			Decoded: &pluginv1.DecodedProtocolEvent{Protocol: top, Summary: s.Info, Fields: fields}}})
	}
	c.decoded = uint64(len(evs))
	c.decMeta = fmt.Sprintf("tshark: %d frames decoded, %d events", len(sums), len(evs))
	for len(evs) > 0 {
		n := min(len(evs), 256)
		ectx, ecancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.sink.Emit(ectx, evs[:n]...)
		ecancel()
		if err != nil {
			c.decMeta += " (event delivery stopped: " + err.Error() + ")"
			return
		}
		evs = evs[n:]
	}
}

// clockInfo reports the timing quality. Stamped times are the device timer
// mapped by a minimum-offset estimator, so they are late by the (unknown)
// minimum USB transit and never early; the late side cannot be bounded
// without a shared trigger.
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	s := c.cm.Stats()
	note := "relative timing between frames is device-timer accurate (1 us); absolute alignment is late by the unknown minimum USB transit latency"
	if s.Samples > 0 {
		note += fmt.Sprintf("; host receive minus stamped time was %.1f ms mean, %.1f ms max (not a bound)",
			float64(s.LatencyAvgNs)/1e6, float64(s.LatencyMaxNs)/1e6)
	}
	ci := &pluginv1.ClockInfo{
		Source:          "device timer (us since boot), mapped by minimum-offset estimator",
		ResolutionNs:    1000,
		ErrorMinNs:      0,
		ErrorMinBounded: true,
		DriftPpm:        s.DriftPPM,
		Note:            note,
	}
	if s.Samples > 0 {
		ci.JitterNs = s.LatencyMaxNs - s.LatencyMinNs
	}
	return ci
}

func (c *capture) buildResult() {
	res := &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{Events: c.packets + c.decoded}, Clock: c.clockInfo()}
	if c.pcapPath != "" && c.writeErr == nil {
		meta := c.clockMeta()
		meta["channel"] = fmt.Sprint(c.channel)
		meta["link_type"] = fmt.Sprint(pcap.LinkIEEE802154TAP)
		meta["packets"] = fmt.Sprint(c.packets)
		if c.packets > 0 {
			meta["rssi_mean_dbm"] = fmt.Sprintf("%.1f", float64(c.rssiSum)/float64(c.packets))
		}
		if c.decMeta != "" {
			meta["decode"] = c.decMeta
		}
		res.Artifacts = append(res.Artifacts, &pluginv1.ArtifactRef{Name: filepath.Base(c.pcapPath), Kind: "pcap",
			MimeType: "application/vnd.tcpdump.pcapng", Path: c.pcapPath, Metadata: meta})
	}
	c.res = res
}
