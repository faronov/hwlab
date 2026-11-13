package nrf802154

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

// Transmit guards. They are deliberately tighter than the firmware's own
// limits: an injection is a test stimulus, not a load generator, and it is
// heard by every network on the channel.
const (
	// MaxTxRepeat bounds one transmit call.
	MaxTxRepeat = 100
	// MinTxIntervalMs is the shortest gap between repeats.
	MinTxIntervalMs = 10
	// MaxTxBurst bounds repeat * interval of one call.
	MaxTxBurst = 30 * time.Second
	// TxBudgetFrames frames may be sent per port in any TxBudgetWindow.
	TxBudgetFrames = 300
	TxBudgetWindow = time.Minute
	// maxNote bounds the free-text audit note.
	maxNote = 512
)

// txLimiter is a per-port sliding-window frame budget.
type txLimiter struct {
	mu   sync.Mutex
	sent map[string][]time.Time
}

// reserve books n frames on port at now, or says when it may be retried.
func (l *txLimiter) reserve(port string, n int, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent == nil {
		l.sent = map[string][]time.Time{}
	}
	keep := l.sent[port][:0]
	for _, t := range l.sent[port] {
		if now.Sub(t) < TxBudgetWindow {
			keep = append(keep, t)
		}
	}
	l.sent[port] = keep
	if len(keep)+n > TxBudgetFrames {
		// The oldest frames that must expire before n more fit.
		need := len(keep) + n - TxBudgetFrames
		retry := TxBudgetWindow - now.Sub(keep[need-1])
		return status.Errorf(codes.ResourceExhausted,
			"transmit rate limit: %d frames already sent on %s in the last %s (budget %d); retry in %s",
			len(keep), port, TxBudgetWindow, TxBudgetFrames, retry.Round(time.Second))
	}
	for range n {
		l.sent[port] = append(l.sent[port], now)
	}
	return nil
}

// txArgs is a validated transmit invocation.
type txArgs struct {
	req     protocol.TxRequest
	channel int // 0 = use the active capture's channel
	power   *int
	note    string
}

func parseTxArgs(args map[string]any) (txArgs, error) {
	bad := func(format string, a ...any) (txArgs, error) {
		return txArgs{}, status.Errorf(codes.InvalidArgument, format, a...)
	}
	var t txArgs
	s, ok := args["psdu"].(string)
	if !ok || s == "" {
		return bad("psdu is required: the MAC frame as hex, without FCS (the radio appends it)")
	}
	s = strings.NewReplacer(" ", "", ":", "", "-", "").Replace(s)
	if len(s) > 2*protocol.MaxTxPSDU {
		return bad("psdu is %d hex digits; at most %d bytes (127 with the FCS the radio appends)", len(s), protocol.MaxTxPSDU)
	}
	psdu, err := hex.DecodeString(s)
	if err != nil {
		return bad("psdu: %v", err)
	}
	t.req = protocol.TxRequest{PSDU: psdu, Repeat: 1}
	if v, ok := args["channel"]; ok {
		if t.channel, err = ValidateChannel(v); err != nil {
			return bad("%v", err)
		}
	}
	flag := func(k string) (bool, error) {
		v, ok := args[k]
		if !ok {
			return false, nil
		}
		b, ok := v.(bool)
		if !ok {
			return false, status.Errorf(codes.InvalidArgument, "%s must be a boolean", k)
		}
		return b, nil
	}
	if t.req.CCA, err = flag("cca"); err != nil {
		return txArgs{}, err
	}
	if t.req.AckWait, err = flag("ack"); err != nil {
		return txArgs{}, err
	}
	integer := func(k string) (int, bool, error) {
		v, ok := args[k]
		if !ok {
			return 0, false, nil
		}
		f, ok := v.(float64)
		if !ok || f != float64(int(f)) {
			return 0, false, status.Errorf(codes.InvalidArgument, "%s must be an integer", k)
		}
		return int(f), true, nil
	}
	if n, ok, err := integer("repeat"); err != nil {
		return txArgs{}, err
	} else if ok {
		if n < 1 || n > MaxTxRepeat {
			return bad("repeat must be 1..%d", MaxTxRepeat)
		}
		t.req.Repeat = n
	}
	if n, ok, err := integer("interval_ms"); err != nil {
		return txArgs{}, err
	} else if ok {
		t.req.IntervalMs = n
	}
	if t.req.Repeat > 1 {
		if t.req.IntervalMs < MinTxIntervalMs || t.req.IntervalMs > protocol.FirmwareMaxIntervalMs {
			return bad("interval_ms must be %d..%d when repeat > 1", MinTxIntervalMs, protocol.FirmwareMaxIntervalMs)
		}
		if burst := time.Duration(t.req.Repeat-1) * time.Duration(t.req.IntervalMs) * time.Millisecond; burst > MaxTxBurst {
			return bad("repeat x interval_ms spans %s; at most %s per call", burst, MaxTxBurst)
		}
	} else {
		t.req.IntervalMs = 0
	}
	if n, ok, err := integer("power_dbm"); err != nil {
		return txArgs{}, err
	} else if ok {
		if !slices.Contains(protocol.TxPowerLevels, n) {
			return bad("power_dbm must be one of %v", protocol.TxPowerLevels)
		}
		t.power = &n
	}
	if v, ok := args["note"]; ok {
		n, ok := v.(string)
		if !ok || len(n) > maxNote {
			return bad("note must be a string of at most %d bytes", maxNote)
		}
		t.note = n
	}
	if err := t.req.Validate(); err != nil {
		return bad("%v", err)
	}
	return t, nil
}

// txTimeout bounds waiting for the firmware's tx_done.
func txTimeout(r protocol.TxRequest) time.Duration {
	return time.Duration(r.Repeat-1)*time.Duration(r.IntervalMs)*time.Millisecond +
		time.Duration(r.Repeat)*50*time.Millisecond + 3*time.Second
}

// txAudit is one line of the transmit audit log.
type txAudit struct {
	Time       string              `json:"time"`
	Phase      string              `json:"phase"` // request | result
	ID         string              `json:"id"`
	Device     string              `json:"device"`
	Port       string              `json:"port"`
	Via        string              `json:"via"` // capture | direct
	CaptureID  string              `json:"capture_id,omitempty"`
	Pcap       string              `json:"pcap,omitempty"`
	Channel    int                 `json:"channel"`
	PowerDbm   *int                `json:"power_dbm,omitempty"`
	PSDU       string              `json:"psdu"`
	CCA        bool                `json:"cca"`
	Ack        bool                `json:"ack"`
	Repeat     int                 `json:"repeat"`
	IntervalMs int                 `json:"interval_ms,omitempty"`
	Note       string              `json:"note,omitempty"`
	Firmware   string              `json:"firmware,omitempty"`
	Summary    *protocol.TxSummary `json:"summary,omitempty"`
	Attempts   []map[string]any    `json:"attempts,omitempty"`
	Error      string              `json:"error,omitempty"`
}

func (p *Plugin) auditPath() string {
	if p.auditDir != "" {
		return filepath.Join(p.auditDir, "audit.jsonl")
	}
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return filepath.Join(h, "artifacts", "nrf802154-tx", "audit.jsonl")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab", "artifacts", "nrf802154-tx", "audit.jsonl")
}

// audit appends one record; a transmit is refused when its request cannot
// be recorded.
func (p *Plugin) audit(rec txAudit) error {
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	path := p.auditPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// transmit implements PacketInjection.transmit.
func (p *Plugin) transmit(ctx context.Context, deviceID string, args map[string]any) (map[string]any, error) {
	t, err := parseTxArgs(args)
	if err != nil {
		return nil, err
	}
	port, err := p.resolvePort(deviceID, p.config(deviceID))
	if err != nil {
		return nil, err
	}
	c := p.activeCapture(port)
	via := "direct"
	if c != nil {
		via = "capture"
		if t.channel != 0 && t.channel != c.channel {
			return nil, status.Errorf(codes.FailedPrecondition,
				"a capture on %s is listening on channel %d; transmit on channel %d would retune it (stop the capture or use its channel)", port, c.channel, t.channel)
		}
		t.channel = c.channel
		if !c.dev.Firmware.CanTransmit() {
			return nil, noTransmit(c.dev.Firmware)
		}
	} else if t.channel == 0 {
		return nil, status.Error(codes.InvalidArgument, "channel is required when no capture is running on the device")
	}
	if err := p.txl.reserve(port, t.req.Repeat, time.Now()); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := txAudit{
		Time: now.Format(time.RFC3339Nano), Phase: "request", ID: fmt.Sprintf("%s-%d", deviceID, now.UnixNano()),
		Device: deviceID, Port: port, Via: via, Channel: t.channel, PowerDbm: t.power,
		PSDU: hex.EncodeToString(t.req.PSDU), CCA: t.req.CCA, Ack: t.req.AckWait, Repeat: t.req.Repeat, IntervalMs: t.req.IntervalMs, Note: t.note,
	}
	if c != nil {
		rec.CaptureID, rec.Pcap, rec.Firmware = c.id, c.pcapPath, c.dev.Firmware.Name+" "+c.dev.Firmware.Version
	}
	if err := p.audit(rec); err != nil {
		return nil, status.Errorf(codes.Internal, "transmit refused: cannot write the audit log %s: %v", p.auditPath(), err)
	}

	var evs []protocol.TxEvent
	var fw protocol.Firmware
	if c != nil {
		fw = c.dev.Firmware
		evs, err = c.transmit(ctx, t, rec.ID)
	} else {
		evs, fw, err = p.transmitDirect(ctx, port, t)
	}

	res := txResult(evs)
	rec.Phase, rec.Time, rec.Firmware = "result", time.Now().UTC().Format(time.RFC3339Nano), strings.TrimSpace(fw.Name+" "+fw.Version)
	rec.Attempts, _ = res["attempts"].([]map[string]any)
	if sum, ok := res["summary"].(protocol.TxSummary); ok {
		rec.Summary = &sum
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if aerr := p.audit(rec); aerr != nil && err == nil {
		err = status.Errorf(codes.Internal, "frames were transmitted but the audit result could not be written to %s: %v", p.auditPath(), aerr)
	}
	if err != nil {
		return nil, err
	}
	res["id"] = rec.ID
	res["via"] = via
	res["channel"] = float64(t.channel)
	res["port"] = port
	res["audit"] = p.auditPath()
	res["firmware"] = firmwareMap(fw)
	if c != nil {
		res["capture_id"] = c.id
		if c.pcapPath != "" {
			res["pcap"] = c.pcapPath
		}
	}
	if sum, ok := res["summary"].(protocol.TxSummary); ok {
		res["summary"] = summaryMap(sum)
	}
	if a, ok := res["attempts"].([]map[string]any); ok {
		l := make([]any, len(a))
		for i := range a {
			l[i] = a[i]
		}
		res["attempts"] = l
	}
	return res, nil
}

func noTransmit(fw protocol.Firmware) error {
	return status.Errorf(codes.FailedPrecondition,
		"the sniffer firmware (%s%s) is receive-only; transmit needs the zigbee-rs radio lab firmware (tools/nrf52840-radio-lab, flash with dfu_program)",
		fw.Name, map[bool]string{true: " " + fw.Version, false: ""}[fw.Version != ""])
}

// transmitDirect opens the port for one transmit when no capture owns it.
func (p *Plugin) transmitDirect(ctx context.Context, port string, t txArgs) ([]protocol.TxEvent, protocol.Firmware, error) {
	release, err := p.acquire(port)
	if err != nil {
		return nil, protocol.Firmware{}, err
	}
	defer release()
	dev, err := p.openSniffer(port)
	if err != nil {
		return nil, protocol.Firmware{}, err
	}
	defer func() { _ = dev.Close() }()
	if err := dev.Setup(ctx, t.channel); err != nil {
		return nil, dev.Firmware, status.Errorf(codes.Unavailable, "prepare %s: %v", port, err)
	}
	p.rememberFirmware(port, dev.Firmware)
	if !dev.Firmware.CanTransmit() {
		return nil, dev.Firmware, noTransmit(dev.Firmware)
	}
	if t.power != nil {
		if err := dev.SetPower(ctx, *t.power); err != nil {
			return nil, dev.Firmware, status.Errorf(codes.Unavailable, "%v", err)
		}
	}
	evs, err := dev.Transmit(ctx, t.req, txTimeout(t.req))
	if err != nil {
		return evs, dev.Firmware, status.Errorf(codes.DeadlineExceeded, "transmit on %s: %v", port, err)
	}
	return evs, dev.Firmware, txError(evs)
}

// txError turns a firmware "error:" reply into an InvalidArgument error.
func txError(evs []protocol.TxEvent) error {
	for _, ev := range evs {
		if ev.Kind == protocol.TxError {
			return status.Errorf(codes.InvalidArgument, "firmware rejected transmit: %s", ev.Message)
		}
	}
	return nil
}

// txResult summarises transmit output for the invoke result.
func txResult(evs []protocol.TxEvent) map[string]any {
	var attempts []map[string]any
	var frame *protocol.TxEvent
	res := map[string]any{}
	for i := range evs {
		ev := evs[i]
		switch ev.Kind {
		case protocol.TxFrame:
			frame = &evs[i]
		case protocol.TxAttempt:
			a := map[string]any{"attempt": float64(ev.Attempt), "status": ev.Status, "dev_ts_us": float64(ev.DevUs)}
			if frame != nil && ev.Status != "cca_busy" && ev.Status != "error" {
				a["fcs"] = hex.EncodeToString(frame.FCS)
				a["power_dbm"] = float64(frame.PowerDb)
			}
			frame = nil
			attempts = append(attempts, a)
		case protocol.TxDone:
			res["summary"] = ev.Summary
		}
	}
	res["attempts"] = attempts
	return res
}

func summaryMap(s protocol.TxSummary) map[string]any {
	return map[string]any{"attempts": float64(s.Attempts), "sent": float64(s.Sent), "acked": float64(s.Acked),
		"no_ack": float64(s.NoAck), "cca_busy": float64(s.CCABusy), "error": float64(s.Error), "result": s.Result}
}

func firmwareMap(fw protocol.Firmware) map[string]any {
	caps := make([]any, len(fw.Caps))
	for i, c := range fw.Caps {
		caps[i] = c
	}
	return map[string]any{"name": fw.Name, "version": fw.Version, "protocol": float64(fw.Protocol), "caps": caps,
		"transmit": fw.CanTransmit(), "raw": fw.Raw}
}

// firmwareInfo implements PacketCapture.firmware_info: the version probe.
// With a capture running it reports what the capture probed at start.
func (p *Plugin) firmwareInfo(ctx context.Context, deviceID string) (map[string]any, error) {
	port, err := p.resolvePort(deviceID, p.config(deviceID))
	if err != nil {
		return nil, err
	}
	if c := p.activeCapture(port); c != nil {
		return map[string]any{"port": port, "via": "capture", "capture_id": c.id, "firmware": firmwareMap(c.dev.Firmware)}, nil
	}
	release, err := p.acquire(port)
	if err != nil {
		return nil, err
	}
	defer release()
	dev, err := p.openSniffer(port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dev.Close() }()
	if err := dev.Identify(ctx); err != nil {
		return nil, status.Errorf(codes.Unavailable, "probe %s: %v", port, err)
	}
	p.rememberFirmware(port, dev.Firmware)
	return map[string]any{"port": port, "via": "direct", "firmware": firmwareMap(dev.Firmware)}, nil
}

// ── capture integration ──────────────────────────────────────

// transmit sends a transmit command through a running capture. The
// capture's reader delivers the firmware's transmit lines; "transmitted:"
// frames are written to the pcapng and the timeline as direction=tx.
func (c *capture) transmit(ctx context.Context, t txArgs, id string) ([]protocol.TxEvent, error) {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if t.power != nil {
		// The radio keeps receiving; "power" is silent on success.
		if err := c.dev.Send(fmt.Sprintf("power %d", *t.power)); err != nil {
			return nil, status.Errorf(codes.Unavailable, "%v", err)
		}
	}
	ch := make(chan protocol.TxEvent, 4*MaxTxRepeat+8)
	c.setTxWaiter(ch, id)
	defer c.setTxWaiter(nil, "")
	c.emitLog("info", fmt.Sprintf("transmit %s requested: %s", id, t.req.Command()))
	if err := c.dev.Send(t.req.Command()); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	timer := time.NewTimer(txTimeout(t.req))
	defer timer.Stop()
	var evs []protocol.TxEvent
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			switch ev.Kind {
			case protocol.TxDone:
				c.emitLog("info", fmt.Sprintf("transmit %s done: sent %d/%d acked %d no_ack %d cca_busy %d error %d (%s)",
					id, ev.Summary.Sent, ev.Summary.Attempts, ev.Summary.Acked, ev.Summary.NoAck, ev.Summary.CCABusy, ev.Summary.Error, ev.Summary.Result))
				return evs, nil
			case protocol.TxError:
				c.emitLog("warn", fmt.Sprintf("transmit %s rejected by firmware: %s", id, ev.Message))
				return evs, txError(evs)
			}
		case <-timer.C:
			c.emitLog("error", fmt.Sprintf("transmit %s: %v", id, protocol.ErrNoTxResult))
			return evs, status.Errorf(codes.DeadlineExceeded, "transmit: %v", protocol.ErrNoTxResult)
		case <-c.done:
			return evs, status.Error(codes.Aborted, "the capture ended during transmit")
		case <-ctx.Done():
			// The firmware aborts a burst on the next command line.
			_ = c.dev.Send("")
			return evs, status.FromContextError(ctx.Err()).Err()
		}
	}
}

func (c *capture) setTxWaiter(ch chan protocol.TxEvent, id string) {
	c.txWaitMu.Lock()
	c.txWait, c.txID = ch, id
	c.txWaitMu.Unlock()
}

// onTx runs on the capture's reader goroutine.
func (c *capture) onTx(ev protocol.TxEvent, rx time.Time) {
	c.txWaitMu.Lock()
	ch, id := c.txWait, c.txID
	c.txWaitMu.Unlock()
	if ev.Kind == protocol.TxFrame {
		c.recordTx(ev, rx, id)
	}
	if ch != nil {
		select {
		case ch <- ev:
		default:
		}
	}
}

// recordTx writes an injected frame to the pcapng and the timeline.
func (c *capture) recordTx(ev protocol.TxEvent, rx time.Time, id string) {
	hostNs := c.clock.At(rx)
	ts := c.cm.Map(ev.DevUs, hostNs)
	c.txFrames++
	if c.ng != nil && c.writeErr == nil {
		tap := pcap.EncodeTAP15_4(pcap.TAPMeta{Channel: uint16(ev.Channel), NoRadioMeta: true}, ev.Data)
		c.writeErr = c.ng.WritePacketOut(ts, tap, fmt.Sprintf("hwlab nrf802154 transmit %s, %d dBm, fcs %x", id, ev.PowerDb, ev.FCS))
		c.tsList = append(c.tsList, ts)
	}
	if len(c.batch) == 0 {
		c.batchAt = rx
	}
	c.batch = append(c.batch, &pluginv1.Event{TimestampNs: ts, StreamId: packetStream, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
		Protocol: "ieee802154", LinkType: pcap.LinkIEEE802154NoFCS, Data: ev.Data, Channel: uint32(ev.Channel),
		Summary: fmt.Sprintf("TX ch%d %d B %d dBm", ev.Channel, len(ev.Data), ev.PowerDb),
		Attributes: map[string]string{
			"direction":    "tx",
			"tx_id":        id,
			"tx_power_dbm": fmt.Sprint(ev.PowerDb),
			"fcs":          hex.EncodeToString(ev.FCS),
			"dev_ts_us":    fmt.Sprint(ev.DevUs),
			"host_rx_ns":   fmt.Sprint(hostNs),
		}}}})
	if len(c.batch) >= batchMax {
		c.flush(false)
	}
}
