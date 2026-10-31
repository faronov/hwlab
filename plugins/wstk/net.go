package wstk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/clockmap"
	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/wstk/protocol"
)

const (
	adminTimeout = 3 * time.Second
	// Packet events are batched like the nrf802154 sniffer's.
	batchMax   = 64
	batchEvery = 50 * time.Millisecond
)

func invalidArg(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

// withAdmin runs fn on a fresh admin console connection.
func (p *Plugin) withAdmin(ctx context.Context, host string, fn func(*protocol.Admin) error) error {
	conn, err := p.env.Dial(ctx, net.JoinHostPort(host, strconv.Itoa(AdminPort)))
	if err != nil {
		return status.Errorf(codes.Unavailable, "wstk admin console %s: %v", host, err)
	}
	defer func() { _ = conn.Close() }()
	a := protocol.NewAdmin(conn)
	if err := a.Sync(ctx, adminTimeout); err != nil {
		return status.Errorf(codes.Unavailable, "wstk admin console %s: %v", host, err)
	}
	return fn(a)
}

// adminCurrent reads the AEM current (A) and the command that produced it.
// "aem current" is tried first; "dbg info" (whose format is known from a
// recorded session) is the fallback.
func adminCurrent(ctx context.Context, a *protocol.Admin, prefer string) (float64, string, error) {
	cmds := []string{"aem current", "dbg info"}
	if prefer == "dbg info" {
		cmds = cmds[1:]
	}
	var outs []string
	for _, cmd := range cmds {
		out, err := a.Command(ctx, cmd, adminTimeout)
		if err != nil {
			return 0, "", err
		}
		if v, ok := protocol.ParseAdminCurrent(out); ok {
			return v, cmd, nil
		}
		outs = append(outs, fmt.Sprintf("%s: %q", cmd, out))
	}
	return 0, "", fmt.Errorf("no AEM current in the admin console's answers (%s)", strings.Join(outs, "; "))
}

func adminVoltage(ctx context.Context, a *protocol.Admin) (float64, bool) {
	for _, cmd := range []string{"aem voltage", "dbg info"} {
		if out, err := a.Command(ctx, cmd, adminTimeout); err == nil {
			if v, ok := protocol.ParseAdminVoltage(out); ok {
				return v, true
			}
		}
	}
	return 0, false
}

func (p *Plugin) invokeNet(ctx context.Context, t target, method string, args map[string]any) (map[string]any, error) {
	res := map[string]any{"host": t.host}
	var err error
	switch method {
	case pluginv1.CapPowerMeasurement + ".info":
		err = p.withAdmin(ctx, t.host, func(a *protocol.Admin) error {
			m := &kitMeta{admin: map[string]string{}}
			for _, cmd := range []string{"sys ver", "boardid list"} {
				out, err := a.Command(ctx, cmd, adminTimeout)
				if err != nil {
					return err
				}
				m.admin[strings.ReplaceAll(cmd, " ", "_")] = out
			}
			p.setMeta(t.id, m)
			maps.Copy(res, metaResult(m))
			return nil
		})
	case pluginv1.CapPowerMeasurement + ".read_current":
		err = p.withAdmin(ctx, t.host, func(a *protocol.Admin) error {
			i, cmd, err := adminCurrent(ctx, a, "")
			if err != nil {
				return err
			}
			res["current_ma"], res["command"], res["source"] = i*1e3, cmd, "admin console"
			if v, ok := adminVoltage(ctx, a); ok {
				res["voltage_v"] = v
			}
			return nil
		})
	case pluginv1.CapPowerMeasurement + ".admin_read", pluginv1.CapPacketCapture + ".pti_status":
		cmd := "pti info"
		if strings.HasSuffix(method, ".admin_read") {
			cmd, _ = args["command"].(string)
			if !protocol.AdminAllowed(cmd) {
				return nil, invalidArg(fmt.Errorf("command must be one of %s", strings.Join(protocol.AdminCommands(), ", ")))
			}
		}
		err = p.withAdmin(ctx, t.host, func(a *protocol.Admin) error {
			out, err := a.Command(ctx, cmd, adminTimeout)
			if err != nil {
				return err
			}
			res["command"], res["output"] = cmd, out
			fields := map[string]any{}
			for k, v := range protocol.ParseAdminKV(out) {
				fields[k] = v
			}
			res["fields"] = fields
			return nil
		})
	default:
		return nil, status.Errorf(codes.Unimplemented, "unsupported method %s", method)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// --- Ethernet AEM: admin console polling ---

func (p *Plugin) startNetAEM(ctx context.Context, t target, o options, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	conn, err := p.env.Dial(ctx, net.JoinHostPort(t.host, strconv.Itoa(AdminPort)))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "wstk admin console %s: %v", t.host, err)
	}
	a := protocol.NewAdmin(conn)
	fail := func(err error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		_ = conn.Close()
		return nil, nil, status.Errorf(codes.Unavailable, "wstk admin console %s: %v", t.host, err)
	}
	if err := a.Sync(ctx, adminTimeout); err != nil {
		return fail(err)
	}
	// One reading up front proves the console answers and picks the command.
	_, cmd, err := adminCurrent(ctx, a, "")
	if err != nil {
		return fail(err)
	}
	c := &pollCapture{p: p, id: t.id, conn: conn, a: a, cmd: cmd, period: time.Duration(o.pollMs) * time.Millisecond,
		sink: sink, clock: req.Clock, duration: req.Duration, done: make(chan struct{})}
	go c.run(ctx)
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: powerStream, Kind: "power", Attributes: map[string]string{
		"sample_period_ns": fmt.Sprint(c.period.Nanoseconds()), "source": "admin console polling (" + cmd + ") over Ethernet",
		"note": "low-rate readings of the kit's AEM; how the kit averages a reading is not documented. Use USB for 10 kHz samples",
	}}}}, nil
}

type pollCapture struct {
	p        *Plugin
	id       string
	conn     net.Conn
	a        *protocol.Admin
	cmd      string
	period   time.Duration
	sink     *pluginsdk.Sink
	clock    *pluginsdk.Clock
	duration time.Duration
	done     chan struct{}
	err      error
	samples  uint64
	maxRTT   time.Duration
}

func (c *pollCapture) Wait() error { <-c.done; return c.err }

func (c *pollCapture) Result() *pluginv1.StopCaptureResponse {
	<-c.done
	return &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{Events: c.samples, Samples: c.samples, DroppedBatches: c.sink.Dropped()},
		Clock: &pluginv1.ClockInfo{Source: "host clock at the middle of each admin console exchange", ResolutionNs: c.period.Nanoseconds(),
			ErrorMinNs: -c.maxRTT.Nanoseconds() / 2, ErrorMinBounded: true, ErrorMaxNs: c.maxRTT.Nanoseconds() / 2, ErrorMaxBounded: true,
			Note: "each sample is one console reading stamped at the middle of its request/reply exchange; the reading's own averaging window is unknown"}}
}

func (c *pollCapture) run(ctx context.Context) {
	defer close(c.done)
	defer c.p.clearBusy(c.id)
	defer func() { _ = c.conn.Close() }()
	start := time.Now()
	volt, haveV := 0.0, false
	for n := 0; ctx.Err() == nil && (c.duration == 0 || time.Since(start) < c.duration); n++ {
		if n%10 == 0 {
			if v, ok := adminVoltage(ctx, c.a); ok {
				volt, haveV = v, true
			}
		}
		t0 := time.Now()
		i, _, err := adminCurrent(ctx, c.a, c.cmd)
		t1 := time.Now()
		if err != nil {
			if ctx.Err() == nil {
				c.err = fmt.Errorf("wstk admin poll: %w", err)
			}
			return
		}
		c.maxRTT = max(c.maxRTT, t1.Sub(t0))
		pb := &pluginv1.PowerBatch{SamplePeriodNs: c.period.Nanoseconds(), CurrentNa: []int32{int32(math.Round(i * 1e9))}}
		if haveV {
			pb.VoltageUv = []uint32{uint32(math.Round(volt * 1e6))}
		}
		ev := &pluginv1.Event{TimestampNs: c.clock.At(t0.Add(t1.Sub(t0) / 2)), StreamId: powerStream, Payload: &pluginv1.Event_Power{Power: pb}}
		if c.sink.TryEmit(ev) {
			c.samples++
		}
		if sleepCtx(ctx, time.Until(t0.Add(c.period))) != nil {
			return
		}
	}
}

// --- Ethernet PTI: Debug Channel ---

func (p *Plugin) startPTI(ctx context.Context, t target, o options, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	conn, err := p.env.Dial(ctx, net.JoinHostPort(t.host, strconv.Itoa(DCHPort)))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "wstk debug channel %s:%d: %v", t.host, DCHPort, err)
	}
	c := &ptiCapture{p: p, id: t.id, host: t.host, conn: conn, sink: sink, clock: req.Clock, cm: clockmap.NewClockMap(),
		duration: req.Duration, channel: o.channel, wallAt: time.Now(), sessAt: req.Clock.Now(), done: make(chan struct{})}
	if req.ArtifactDir != "" {
		if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		c.pcapPath = filepath.Join(req.ArtifactDir, "wstk-pti.pcapng")
		if c.file, err = os.Create(c.pcapPath); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		c.buf = bufio.NewWriterSize(c.file, 64<<10)
		desc := fmt.Sprintf("Silicon Labs WSTK %s packet trace (PTI), timestamps are hwlab session ns", t.id)
		if c.ng, err = pcap.NewNGWriter(c.buf, "hwlab wstk", pcap.NGInterface{LinkType: pcap.LinkIEEE802154TAP, Name: "pti", Description: desc}); err != nil {
			_ = c.file.Close()
			_ = conn.Close()
			return nil, nil, err
		}
	}
	go c.run(ctx)
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: packetStream, Kind: "packet", Attributes: map[string]string{
		"link_type": fmt.Sprint(pcap.LinkIEEE802154NoFCS), "source": "EFR32 packet trace (PTI) via the kit's debug channel, TCP " + strconv.Itoa(DCHPort),
		"device_timer": "kit debug-channel timestamps", "note": "PTI shows only what the target radio sends and receives, and only when its firmware enables PTI",
		"clock": "kit timer mapped to session time with a windowed minimum-offset estimator",
	}}}}, nil
}

type ptiCapture struct {
	p        *Plugin
	id, host string
	conn     net.Conn
	sink     *pluginsdk.Sink
	clock    *pluginsdk.Clock
	cm       *clockmap.ClockMap
	duration time.Duration
	channel  int
	wallAt   time.Time
	sessAt   int64
	done     chan struct{}
	err      error

	framer   protocol.DCHFramer
	batch    []*pluginv1.Event
	batchAt  time.Time
	packets  uint64
	other    uint64 // trace records that are not IEEE 802.15.4
	bad      uint64 // trace records that did not decode
	rssiSum  int64
	rssiN    int64
	pcapPath string
	file     *os.File
	buf      *bufio.Writer
	ng       *pcap.NGWriter
	writeErr error
	res      *pluginv1.StopCaptureResponse
}

func (c *ptiCapture) Wait() error { <-c.done; return c.err }

func (c *ptiCapture) Result() *pluginv1.StopCaptureResponse { <-c.done; return c.res }

func (c *ptiCapture) run(ctx context.Context) {
	defer close(c.done)
	defer c.p.clearBusy(c.id)
	start := time.Now()
	buf := make([]byte, 64<<10)
	for ctx.Err() == nil && (c.duration == 0 || time.Since(start) < c.duration) {
		_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := c.conn.Read(buf)
		rx := time.Now()
		for _, m := range c.framer.Feed(buf[:n]) {
			c.onMessage(m, rx)
		}
		if len(c.batch) > 0 && (len(c.batch) >= batchMax || rx.Sub(c.batchAt) >= batchEvery) {
			c.flush(false)
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if ctx.Err() == nil {
				c.err = fmt.Errorf("wstk debug channel %s: %w", c.host, err)
			}
			break
		}
	}
	_ = c.conn.Close()
	c.flush(true)
	if c.packets == 0 {
		c.emitLog("info", "no packet trace records arrived; PTI needs target firmware that enables it (RAIL/EmberZNet/OpenThread apps with the PTI component) and the kit's PTI input")
	}
	if c.other > 0 || c.bad > 0 || c.framer.Skipped > 0 {
		c.emitLog("info", fmt.Sprintf("%d non-802.15.4 trace records, %d undecodable records, %d bytes skipped while resynchronising", c.other, c.bad, c.framer.Skipped))
	}
	c.finishFile()
	c.buildResult()
}

func (c *ptiCapture) onMessage(m protocol.DCHMessage, rx time.Time) {
	if m.Type != protocol.DCHEFRTX && m.Type != protocol.DCHEFRRX && m.Type != protocol.DCHEFROther {
		return
	}
	f, err := protocol.ParsePTI(m.Payload)
	if err != nil {
		c.bad++
		return
	}
	if !f.IEEE802154 {
		c.other++
		return
	}
	hostNs := c.clock.At(rx)
	ts := c.cm.Map(int64(m.TimeNs/1000), hostNs)
	ch := f.Channel
	if (ch < 11 || ch > 26) && c.channel != 0 {
		ch = c.channel
	}
	mpdu := f.MPDU()
	c.packets++
	dir := "tx"
	if f.RX {
		dir = "rx"
	}
	pkt := &pluginv1.Packet{Protocol: "ieee802154", LinkType: pcap.LinkIEEE802154NoFCS, Data: mpdu, Channel: uint32(ch),
		Summary: fmt.Sprintf("%s ch%d %d B", dir, ch, len(mpdu)),
		Attributes: map[string]string{
			"direction": dir, "dev_ts_ns": fmt.Sprint(m.TimeNs), "host_rx_ns": fmt.Sprint(hostNs), "latency_ns": fmt.Sprint(hostNs - ts),
			"dch_seq": fmt.Sprint(m.Seq), "pti_protocol": fmt.Sprint(f.Protocol), "pti_error": fmt.Sprint(f.Error),
			"hw_end": fmt.Sprintf("%#02x", f.HWEnd), "aborted": fmt.Sprint(f.Aborted), "raw_channel": fmt.Sprint(f.RawChannel),
		}}
	meta := pcap.TAPMeta{Channel: uint16(ch)}
	if f.HasRSSI {
		pkt.RssiDbm = float64(f.RSSIdBm)
		pkt.Summary += fmt.Sprintf(" rssi %d dBm", f.RSSIdBm)
		meta.RSSIdBm = float32(f.RSSIdBm)
		c.rssiSum += int64(f.RSSIdBm)
		c.rssiN++
	}
	if f.Aborted {
		pkt.Summary += " (aborted)"
	}
	if c.ng != nil && c.writeErr == nil {
		c.writeErr = c.ng.WritePacket(ts, pcap.EncodeTAP15_4(meta, mpdu))
	}
	if len(c.batch) == 0 {
		c.batchAt = rx
	}
	c.batch = append(c.batch, &pluginv1.Event{TimestampNs: ts, StreamId: packetStream, Payload: &pluginv1.Event_Packet{Packet: pkt}})
}

func (c *ptiCapture) flush(final bool) {
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

func (c *ptiCapture) emitLog(level, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.sink.Emit(ctx, &pluginv1.Event{TimestampNs: c.clock.Now(), StreamId: packetStream,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: level, Source: "wstk", Message: msg}}})
}

func (c *ptiCapture) clockMeta() map[string]string {
	s := c.cm.Stats()
	return map[string]string{
		"clock.source":          "kit debug-channel timestamps, mapped by minimum-offset estimator",
		"clock.offset_ns":       fmt.Sprint(s.OffsetNs),
		"clock.latency_min_ns":  fmt.Sprint(s.LatencyMinNs),
		"clock.latency_mean_ns": fmt.Sprint(s.LatencyAvgNs),
		"clock.latency_max_ns":  fmt.Sprint(s.LatencyMaxNs),
		"clock.drift_ppm":       fmt.Sprintf("%.2f", s.DriftPPM),
		"clock.note":            "relative timing between frames follows the kit timer; absolute alignment is late by the unknown minimum network transit",
		"wall.start_utc":        c.wallAt.UTC().Format(time.RFC3339Nano),
		"session.start_ns":      fmt.Sprint(c.sessAt),
	}
}

func (c *ptiCapture) finishFile() {
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

func (c *ptiCapture) buildResult() {
	s := c.cm.Stats()
	ci := &pluginv1.ClockInfo{Source: "kit debug-channel timestamps mapped by minimum-offset estimator", ResolutionNs: 1000,
		ErrorMinNs: 0, ErrorMinBounded: true, DriftPpm: s.DriftPPM,
		Note: "relative timing between frames follows the kit timer; absolute alignment is late by the unknown minimum network transit"}
	if s.Samples > 0 {
		ci.JitterNs = s.LatencyMaxNs - s.LatencyMinNs
	}
	res := &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{Events: c.packets, DroppedBatches: c.sink.Dropped()}, Clock: ci}
	if c.pcapPath != "" && c.writeErr == nil {
		meta := c.clockMeta()
		meta["link_type"] = fmt.Sprint(pcap.LinkIEEE802154TAP)
		meta["packets"] = fmt.Sprint(c.packets)
		meta["non_802154_records"] = fmt.Sprint(c.other)
		if c.rssiN > 0 {
			meta["rssi_mean_dbm"] = fmt.Sprintf("%.1f", float64(c.rssiSum)/float64(c.rssiN))
		}
		res.Artifacts = append(res.Artifacts, &pluginv1.ArtifactRef{Name: filepath.Base(c.pcapPath), Kind: "pcap",
			MimeType: "application/vnd.tcpdump.pcapng", Path: c.pcapPath, Metadata: meta})
	}
	c.res = res
}
