package fake

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

const chunk = 50 * time.Millisecond

type config struct {
	first        time.Duration // time of the first TX burst
	period       time.Duration // burst repetition period
	burst        time.Duration // burst length
	idleNA       int32
	burstNA      int32
	samplePeriod time.Duration
	realtime     bool
	seed         uint64
	channel      uint32
	supplyMV     float64
	supplyV      float64
}

func defaults() config {
	return config{
		first: 12340 * time.Microsecond, period: 100 * time.Millisecond, burst: 2261 * time.Microsecond,
		idleNA: 8_000, burstNA: 16_200_000, samplePeriod: 10 * time.Microsecond,
		realtime: true, seed: 1, channel: 15,
	}
}

func parseConfig(maps ...map[string]any) config {
	c := defaults()
	for _, m := range maps {
		for k, v := range m {
			f, ok := num(v)
			switch k {
			case "first_tx_ms":
				c.first = time.Duration(f * float64(time.Millisecond))
			case "period_ms":
				c.period = time.Duration(f * float64(time.Millisecond))
			case "burst_us":
				c.burst = time.Duration(f * float64(time.Microsecond))
			case "idle_ua":
				c.idleNA = int32(f * 1000)
			case "burst_ma":
				c.burstNA = int32(f * 1e6)
			case "sample_period_us":
				c.samplePeriod = time.Duration(f * float64(time.Microsecond))
			case "realtime":
				if b, isBool := v.(bool); isBool {
					c.realtime = b
				}
			case "seed":
				c.seed = uint64(f)
			case "channel":
				c.channel = uint32(f)
			case "supply_mv":
				c.supplyMV = f
			}
			_ = ok
		}
	}
	if c.period <= c.burst {
		c.period = c.burst * 2
	}
	if c.samplePeriod <= 0 {
		c.samplePeriod = 10 * time.Microsecond
	}
	return c
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func newInvokeResponse(m map[string]any) (*pluginv1.InvokeResponse, error) {
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

// Offsets (relative to the start of each TX cycle) of the simulated signals.
const (
	irqRise  = 1120 * time.Nanosecond
	csFall   = 1880 * time.Nanosecond
	csRise   = 9880 * time.Nanosecond
	irqFall  = 20 * time.Microsecond
	rttStart = 2100 * time.Nanosecond
	pktTX    = 15220 * time.Nanosecond
	pktAck   = 640 * time.Microsecond
)

type generator struct {
	cfg config
	dev string
}

func newGenerator(cfg config, dev string) *generator { return &generator{cfg: cfg, dev: dev} }

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// cycleRange returns the cycles that may produce events in [from,to).
func (g *generator) cycleRange(from, to time.Duration) (int64, int64) {
	k0 := int64((from - g.cfg.first - g.cfg.period) / g.cfg.period)
	if k0 < 0 {
		k0 = 0
	}
	k1 := int64((to-g.cfg.first)/g.cfg.period) + 1
	if to < g.cfg.first {
		k1 = -1
	}
	return k0, k1
}

func (g *generator) cycleStart(k int64) time.Duration {
	return g.cfg.first + time.Duration(k)*g.cfg.period
}

func (g *generator) window(c *capture, from, to time.Duration) []*pluginv1.Event {
	if to <= from {
		return nil
	}
	switch g.dev {
	case DevPower:
		return g.power(c, from, to)
	case DevLogic:
		return g.logic(c, from, to)
	case DevRadio:
		return g.radio(c, from, to)
	case DevDebug:
		return g.debug(c, from, to)
	}
	return nil
}

func (g *generator) power(c *capture, from, to time.Duration) []*pluginv1.Event {
	sp := g.cfg.samplePeriod
	i0 := int64((from + sp - 1) / sp)
	i1 := int64((to + sp - 1) / sp)
	n := int(i1 - i0)
	if n <= 0 {
		return nil
	}
	cur := make([]int32, n)
	volt := make([]uint32, n)
	for j := 0; j < n; j++ {
		i := i0 + int64(j)
		t := time.Duration(i) * sp
		base, spread := g.cfg.idleNA, int64(g.cfg.idleNA)/20
		if t >= g.cfg.first && (t-g.cfg.first)%g.cfg.period < g.cfg.burst {
			base, spread = g.cfg.burstNA, int64(g.cfg.burstNA)/50
		}
		noise := int64(splitmix(g.cfg.seed^uint64(i))%uint64(2*spread+1)) - spread
		cur[j] = base + int32(noise)
		volt[j] = uint32(g.cfg.supplyV*1e6) + uint32(splitmix(g.cfg.seed^uint64(i)<<1)%2000)
	}
	c.samples += uint64(n)
	c.events++
	return []*pluginv1.Event{{
		TimestampNs: c.req.Clock.Offset(time.Duration(i0) * sp), StreamId: "current",
		Payload: &pluginv1.Event_Power{Power: &pluginv1.PowerBatch{SamplePeriodNs: int64(sp), CurrentNa: cur, VoltageUv: volt}},
	}}
}

type edge struct {
	t     time.Duration
	state uint32
}

// logicState returns the IRQ (bit0) / SPI_CS (bit1, idle high) word at t.
func (g *generator) logicState(t time.Duration) uint32 {
	state := uint32(0b10)
	if t < g.cfg.first {
		return state
	}
	rel := (t - g.cfg.first) % g.cfg.period
	if rel >= irqRise && rel < irqFall {
		state |= 0b01
	}
	if rel >= csFall && rel < csRise {
		state &^= 0b10
	}
	return state
}

func (g *generator) logic(c *capture, from, to time.Duration) []*pluginv1.Event {
	k0, k1 := g.cycleRange(from, to)
	var times []time.Duration
	for k := k0; k <= k1; k++ {
		s := g.cycleStart(k)
		for _, o := range []time.Duration{irqRise, csFall, csRise, irqFall} {
			if t := s + o; t >= from && t < to {
				times = append(times, t)
			}
		}
	}
	if len(times) == 0 {
		return nil
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	edges := &pluginv1.DigitalEdges{InitialState: g.logicState(from - 1)}
	for _, t := range times {
		edges.OffsetNs = append(edges.OffsetNs, int64(t-from))
		edges.State = append(edges.State, g.logicState(t))
	}
	c.events++
	return []*pluginv1.Event{{
		TimestampNs: c.req.Clock.Offset(from), StreamId: "digital",
		Payload: &pluginv1.Event_Digital{Digital: &pluginv1.DigitalBatch{
			Channels: []string{"IRQ", "SPI_CS"}, Encoding: &pluginv1.DigitalBatch_Edges{Edges: edges}}},
	}}
}

func (g *generator) radio(c *capture, from, to time.Duration) []*pluginv1.Event {
	k0, k1 := g.cycleRange(from, to)
	type pk struct {
		t       time.Duration
		data    []byte
		summary string
	}
	var pks []pk
	for k := k0; k <= k1; k++ {
		s := g.cycleStart(k)
		seq := byte(k)
		data := []byte{0x61, 0x88, seq, 0x62, 0x1a, 0x00, 0x00, 0x6f, 0x79, 0x08, 0x00, 0x01, byte(k >> 8), seq}
		if t := s + pktTX; t >= from && t < to {
			pks = append(pks, pk{t, pcap.AppendFCS(data), fmt.Sprintf("Zigbee APS TX seq=%d (simulated)", k)})
		}
		if t := s + pktAck; t >= from && t < to {
			pks = append(pks, pk{t, pcap.AppendFCS([]byte{0x02, 0x00, seq}), fmt.Sprintf("802.15.4 ACK seq=%d", seq)})
		}
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].t < pks[j].t })
	out := make([]*pluginv1.Event, 0, len(pks))
	for _, p := range pks {
		ts := c.req.Clock.Offset(p.t)
		if c.pcapW != nil {
			_ = c.pcapW.WritePacket(ts, p.data)
		}
		out = append(out, &pluginv1.Event{TimestampNs: ts, StreamId: "packets", Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
			Protocol: "ieee802154", LinkType: pcap.LinkIEEE802154WithFCS, Data: p.data, RssiDbm: -52.5, Lqi: 220,
			Channel: g.cfg.channel, Summary: p.summary, Attributes: map[string]string{"simulated": "true"}}}})
	}
	c.events += uint64(len(out))
	return out
}

func (g *generator) debug(c *capture, from, to time.Duration) []*pluginv1.Event {
	k0, k1 := g.cycleRange(from, to)
	type ev struct {
		t time.Duration
		e *pluginv1.Event
	}
	var evs []ev
	for k := k0; k <= k1; k++ {
		s := g.cycleStart(k)
		add := func(t time.Duration, e *pluginv1.Event) {
			if t >= from && t < to {
				e.TimestampNs = c.req.Clock.Offset(t)
				evs = append(evs, ev{t, e})
			}
		}
		add(s, &pluginv1.Event{StreamId: "markers", Payload: &pluginv1.Event_Marker{Marker: &pluginv1.Marker{
			Name: fmt.Sprintf("tx_cycle_%d", k), Description: "start of simulated TX cycle"}}})
		add(s+rttStart, &pluginv1.Event{StreamId: "rtt", Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{
			Level: "info", Source: "rtt", Message: "radio_tx_start"}}})
		add(s+g.cfg.burst-10*time.Microsecond, &pluginv1.Event{StreamId: "rtt", Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{
			Level: "info", Source: "rtt", Message: "radio_tx_done"}}})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].t < evs[j].t })
	out := make([]*pluginv1.Event, len(evs))
	for i, e := range evs {
		out[i] = e.e
	}
	c.events += uint64(len(out))
	return out
}
