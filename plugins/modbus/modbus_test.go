package modbusplugin

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/modbus"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/uart"
)

func fakePlugin(t *testing.T) (*Plugin, *Fake) {
	t.Helper()
	f := NewFake()
	list := func() ([]Entry, error) {
		return []Entry{{ID: FakeID, Source: "fake", Fake: f}, {ID: "plc", Source: "env", Addr: Addr{Kind: "tcp", Target: "192.0.2.1:502"}}}, nil
	}
	open := func(e Entry, _ time.Duration) (modbus.Transport, error) {
		if e.Fake != nil {
			return e.Fake.Dial(), nil
		}
		return nil, os.ErrNotExist
	}
	return NewWith(list, open), f
}

func invoke(t *testing.T, p *Plugin, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: FakeID, Capability: pluginv1.CapModbus, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func values(t *testing.T, res map[string]any, key string) []any {
	t.Helper()
	resp, _ := res["response"].(map[string]any)
	v, _ := resp[key].([]any)
	return v
}

func modeOf(baud, bits int, parity, stop string) uart.Mode {
	return uart.Mode{Baud: baud, DataBits: bits, Parity: parity, StopBits: stop, Flow: uart.FlowNone}
}

func TestParseAddr(t *testing.T) {
	good := map[string]Addr{
		"tcp://10.0.0.5":                {Kind: "tcp", Target: "10.0.0.5:502"},
		"tcp://10.0.0.5:1502":           {Kind: "tcp", Target: "10.0.0.5:1502"},
		"rtu:/dev/ttyUSB0":              {Kind: "rtu", Target: "/dev/ttyUSB0", Mode: modeOf(19200, 8, "even", "1")},
		"rtu:/dev/ttyUSB0@9600":         {Kind: "rtu", Target: "/dev/ttyUSB0", Mode: modeOf(9600, 8, "even", "1")},
		"rtu:/dev/ttyUSB0@115200:8N2":   {Kind: "rtu", Target: "/dev/ttyUSB0", Mode: modeOf(115200, 8, "none", "2")},
		"rtu:/dev/ttyS1@19200:7O1":      {Kind: "rtu", Target: "/dev/ttyS1", Mode: modeOf(19200, 7, "odd", "1")},
		"rtu:/dev/serial/by-id/x@38400": {Kind: "rtu", Target: "/dev/serial/by-id/x", Mode: modeOf(38400, 8, "even", "1")},
	}
	for in, want := range good {
		got, err := ParseAddr(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "tcp://", "rtu:", "serial:/dev/x", "rtu:/dev/x@fast", "rtu:/dev/x@9600:8X1", "rtu:/dev/x@9600:8E3", "rtu:/dev/x@9600:9N1", "rtu:/dev/x@9600:8E"} {
		if _, err := ParseAddr(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	es := parseHosts("plc=tcp://10.0.0.5, meter=rtu:/dev/ttyUSB0@9600:8E1,bad=nope,=x")
	if len(es) != 2 || es[0].ID != "plc" || es[1].Addr.Mode.Baud != 9600 {
		t.Fatalf("%+v", es)
	}
}

func TestListAndDanger(t *testing.T) {
	p, _ := fakePlugin(t)
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 2 {
		t.Fatalf("%v %v", devs, err)
	}
	if devs[0].Properties["synthetic"] != "true" || devs[1].Properties["synthetic"] != "" {
		t.Errorf("synthetic flags: %v %v", devs[0].Properties, devs[1].Properties)
	}
	if !pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapModbus) {
		t.Errorf("no Modbus capability")
	}
	for _, ms := range p.Info().Methods {
		safe := ms.Name == "status" || strings.HasPrefix(ms.Name, "read_") && ms.Name != "read_write_registers"
		if safe != (ms.Danger == pluginv1.Danger_DANGER_SAFE) {
			t.Errorf("%s: danger %v", ms.Name, ms.Danger)
		}
	}
}

func TestReadsOnFake(t *testing.T) {
	p, _ := fakePlugin(t)
	res, err := invoke(t, p, "read_holding_registers", map[string]any{"address": 0.0, "quantity": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(t, res, "registers"); len(v) != 3 || v[0] != 100.0 || v[2] != 300.0 {
		t.Errorf("registers %v", v)
	}
	if res["request_hex"] != "010300000003"+crcHex(t, "010300000003") {
		t.Errorf("request_hex %v", res["request_hex"])
	}
	res, err = invoke(t, p, "read_holding_registers", map[string]any{"address": 10.0, "quantity": 2.0, "as": "f32"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := res["values"].([]any); len(v) != 1 || v[0] != 23.5 {
		t.Errorf("f32 %v", res["values"])
	}
	a, _ := invoke(t, p, "read_input_registers", map[string]any{"address": 0.0})
	b, _ := invoke(t, p, "read_input_registers", map[string]any{"address": 0.0})
	if values(t, b, "registers")[0].(float64) != values(t, a, "registers")[0].(float64)+1 {
		t.Errorf("input counter %v then %v", values(t, a, "registers"), values(t, b, "registers"))
	}
	res, err = invoke(t, p, "read_coils", map[string]any{"address": 0.0, "quantity": 4.0})
	if err != nil {
		t.Fatal(err)
	}
	if v := values(t, res, "bits"); len(v) != 4 || v[0] != true || v[1] != false || v[2] != true {
		t.Errorf("coils %v", v)
	}
	if res, err = invoke(t, p, "status", nil); err != nil || res["synthetic"] != true || res["validated"] != false {
		t.Errorf("status %v %v", res, err)
	}
}

func crcHex(t *testing.T, h string) string {
	t.Helper()
	b := make([]byte, len(h)/2)
	for i := range b {
		var v byte
		for _, c := range []byte(h[2*i : 2*i+2]) {
			v <<= 4
			switch {
			case c >= 'a':
				v |= c - 'a' + 10
			default:
				v |= c - '0'
			}
		}
		b[i] = v
	}
	c := modbus.CRC16(b)
	return modbus.Hex([]byte{byte(c), byte(c >> 8)})
}

func TestWritesOnFake(t *testing.T) {
	p, f := fakePlugin(t)
	steps := []struct {
		method string
		args   map[string]any
	}{
		{"write_register", map[string]any{"address": 1.0, "value": 4321.0}},
		{"write_registers", map[string]any{"address": 20.0, "values": []any{1.0, -2.0, 65535.0}}},
		{"write_coil", map[string]any{"address": 1.0, "value": true}},
		{"write_coils", map[string]any{"address": 8.0, "values": []any{true, true, false}}},
		{"mask_write_register", map[string]any{"address": 0.0, "and_mask": 0xF0F0, "or_mask": 0x000A}},
		{"read_write_registers", map[string]any{"read_address": 20.0, "read_quantity": 3.0, "address": 30.0, "values": []any{7.0}}},
	}
	for _, s := range steps {
		if _, err := invoke(t, p, s.method, s.args); err != nil {
			t.Fatalf("%s: %v", s.method, err)
		}
	}
	if h := f.Slave.Holding(1, 1); h[0] != 4321 {
		t.Errorf("write_register: %v", h)
	}
	if h := f.Slave.Holding(20, 3); h[0] != 1 || h[1] != 0xFFFE || h[2] != 0xFFFF {
		t.Errorf("write_registers: %v", h)
	}
	if !f.Slave.Coil(1) || !f.Slave.Coil(8) || !f.Slave.Coil(9) || f.Slave.Coil(10) {
		t.Errorf("coils wrong")
	}
	if h := f.Slave.Holding(0, 1); h[0] != (100&0xF0F0)|(0x000A&^0xF0F0) {
		t.Errorf("mask write: %04x", h[0])
	}
	if h := f.Slave.Holding(30, 1); h[0] != 7 {
		t.Errorf("read_write: %v", h)
	}
	res, err := invoke(t, p, "raw", map[string]any{"pdu": "03 000a 0002"})
	if err != nil || len(values(t, res, "registers")) != 2 {
		t.Errorf("raw read: %v %v", res, err)
	}
}

func TestErrorsAndValidation(t *testing.T) {
	p, _ := fakePlugin(t)
	if err := p.OpenDevice(context.Background(), FakeID, map[string]any{"timeout_ms": 60.0}); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), FakeID)

	if _, err := invoke(t, p, "read_holding_registers", map[string]any{"address": 1000.0, "quantity": 2.0}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "illegal_data_address") {
		t.Errorf("exception: %v", err)
	}
	if _, err := invoke(t, p, "read_holding_registers", map[string]any{"address": 0.0, "unit": 9.0}); status.Code(err) != codes.Unavailable {
		t.Errorf("wrong unit should time out: %v", err)
	}
	if res, err := invoke(t, p, "write_register", map[string]any{"address": 0.0, "value": 1.0, "unit": 0.0}); err != nil || res["broadcast"] != true {
		t.Errorf("broadcast: %v %v", res, err)
	}
	bad := []struct {
		method string
		args   map[string]any
	}{
		{"read_holding_registers", nil},
		{"read_holding_registers", map[string]any{"address": 70000.0}},
		{"read_holding_registers", map[string]any{"address": 1.5}},
		{"read_holding_registers", map[string]any{"address": 0.0, "quantity": 126.0}},
		{"read_holding_registers", map[string]any{"address": 0.0, "quantity": 0.0}},
		{"read_holding_registers", map[string]any{"address": 0.0, "as": "f32"}},
		{"read_coils", map[string]any{"address": 0.0, "as": "u16"}},
		{"write_register", map[string]any{"address": 0.0, "value": 70000.0}},
		{"write_register", map[string]any{"address": 0.0}},
		{"write_registers", map[string]any{"address": 0.0, "values": []any{}}},
		{"write_registers", map[string]any{"address": 0.0, "values": make([]any, 124)}},
		{"write_coil", map[string]any{"address": 0.0, "value": "yes"}},
		{"write_coils", map[string]any{"address": 0.0, "values": []any{1.0}}},
		{"raw", map[string]any{"pdu": "zz"}},
		{"raw", map[string]any{"pdu": ""}},
		{"read_holding_registers", map[string]any{"address": 0.0, "unit": 300.0}},
	}
	for i, b := range bad {
		if b.args != nil && b.method == "write_registers" && len(b.args["values"].([]any)) == 124 {
			for j := range b.args["values"].([]any) {
				b.args["values"].([]any)[j] = 0.0
			}
		}
		if _, err := invoke(t, p, b.method, b.args); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d %s %v: %v", i, b.method, b.args, err)
		}
	}
	if _, err := invoke(t, p, "nonsense", nil); status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown method: %v", err)
	}
	if err := p.OpenDevice(context.Background(), FakeID, map[string]any{"bogus": 1.0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown setting: %v", err)
	}
	if err := p.OpenDevice(context.Background(), "nope", nil); status.Code(err) != codes.NotFound {
		t.Errorf("unknown device: %v", err)
	}
}

func TestTCPPath(t *testing.T) {
	slave := modbus.NewSlave(1, 64)
	slave.SetHolding(0, 11, 22)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback:", err)
	}
	defer l.Close()
	go slave.ServeTCP(l)
	list := func() ([]Entry, error) {
		return []Entry{{ID: "plc", Source: "env", Addr: Addr{Kind: "tcp", Target: l.Addr().String()}}}, nil
	}
	p := NewWith(list, defaultOpen)
	s, _ := structpb.NewStruct(map[string]any{"address": 0.0, "quantity": 2.0})
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "plc", Capability: pluginv1.CapModbus, Method: "read_holding_registers", Args: s})
	if err != nil {
		t.Fatal(err)
	}
	m := r.Result.AsMap()
	if v := values(t, m, "registers"); len(v) != 2 || v[1] != 22.0 {
		t.Errorf("%v", m)
	}
	if !strings.HasPrefix(m["request_hex"].(string), "0001000000060103") {
		t.Errorf("MBAP request %v", m["request_hex"])
	}
}

func startCap(t *testing.T, p *Plugin, cfg map[string]any, ctx context.Context) (pluginsdk.Capture, *pluginsdk.Sink, error) {
	t.Helper()
	sink := pluginsdk.NewTestSink(256)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: FakeID, CaptureID: "c1", Config: cfg, Clock: pluginsdk.NewClock(1_000)}, sink)
	return c, sink, err
}

func TestPollingCapture(t *testing.T) {
	p, _ := fakePlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c, sink, err := startCap(t, p, map[string]any{"polls": []any{
		map[string]any{"name": "counter", "function": "read_input_registers", "address": 0.0, "quantity": 2.0, "interval_ms": 40.0},
		map[string]any{"name": "level", "address": 10.0, "quantity": 2.0, "interval_ms": 100.0, "as": "f32"},
		map[string]any{"name": "bad", "address": 5000.0, "interval_ms": 150.0},
	}}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	evs := sink.Drain()
	byStream := map[string]int{}
	var last int64
	states := map[string]int{}
	for _, e := range evs {
		byStream[e.StreamId]++
		if e.TimestampNs < 1_000 {
			t.Errorf("timestamp %d before session start", e.TimestampNs)
		}
		if d := e.GetDecoded(); d != nil {
			if d.Protocol != "modbus" {
				t.Errorf("protocol %q", d.Protocol)
			}
			f := d.Fields.AsMap()
			states[f["status"].(string)]++
			if f["poll"] == "level" {
				if v, _ := f["values"].([]any); len(v) != 1 || v[0] != 23.5 {
					t.Errorf("level values %v", f["values"])
				}
			}
			if e.TimestampNs < last {
				t.Errorf("transactions out of order")
			}
			last = e.TimestampNs
		}
		if pk := e.GetPacket(); pk != nil {
			if pk.Protocol != "modbus-rtu" || len(pk.Data) < 4 || modbus.CRC16(pk.Data[:len(pk.Data)-2]) != uint16(pk.Data[len(pk.Data)-2])|uint16(pk.Data[len(pk.Data)-1])<<8 {
				t.Errorf("packet %q % x", pk.Protocol, pk.Data)
			}
		}
	}
	if byStream[StreamTransaction] < 6 || byStream[StreamRequest] != byStream[StreamTransaction] || byStream[StreamResponse] != byStream[StreamTransaction] {
		t.Errorf("streams %v", byStream)
	}
	if states["ok"] == 0 || states["exception"] == 0 {
		t.Errorf("states %v", states)
	}
	res := c.Result()
	if res.Stats.Events != uint64(len(evs)) || !strings.Contains(res.Clock.Source, "synthetic") {
		t.Errorf("result %+v clock %+v (events %d)", res.Stats, res.Clock, len(evs))
	}
}

func TestCaptureValidationAndExclusivity(t *testing.T) {
	p, _ := fakePlugin(t)
	for name, cfg := range map[string]map[string]any{
		"no polls":    {},
		"write poll":  {"polls": []any{map[string]any{"function": "write_single_register", "address": 0.0}}},
		"unknown fn":  {"polls": []any{map[string]any{"function": "nope", "address": 0.0}}},
		"fast":        {"polls": []any{map[string]any{"address": 0.0, "interval_ms": 1.0}}},
		"as on coils": {"polls": []any{map[string]any{"function": "read_coils", "address": 0.0, "as": "u16"}}},
		"odd f32":     {"polls": []any{map[string]any{"address": 0.0, "quantity": 3.0, "as": "f32"}}},
		"bogus key":   {"polls": []any{map[string]any{"address": 0.0}}, "speed": 1.0},
		"not object":  {"polls": []any{"x"}},
	} {
		if _, _, err := startCap(t, p, cfg, context.Background()); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.OpenDevice(ctx, FakeID, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), FakeID)
	cfg := map[string]any{"polls": []any{map[string]any{"address": 0.0, "interval_ms": 20.0}}}
	c, sink, err := startCap(t, p, cfg, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := startCap(t, p, cfg, context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatalf("cancel is not an error: %v", err)
	}
	if n := len(sink.Drain()); n == 0 {
		t.Errorf("no events before cancel")
	}
	// The slot is free again.
	if c2, _, err := startCap(t, p, cfg, ctx); err != nil {
		t.Errorf("restart: %v", err)
	} else {
		_ = c2.Wait()
	}
}
