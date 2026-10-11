package app_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/modbus"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

// buildModbusBusSession writes a SYNTHETIC session: the serial packets a uart
// capture would record for a master polling slave 1, with a good read, an
// exception, an unanswered request and a response whose CRC is wrong.
func buildModbusBusSession(t *testing.T) *app.App {
	t.Helper()
	a := newApp(t)
	s, err := session.Create(a.Cfg.SessionsDir, "modbus-bus", "test")
	if err != nil {
		t.Fatal(err)
	}
	ms := int64(1_000_000)
	emit := func(at int64, dir string, frame []byte) {
		t.Helper()
		_, err := s.AppendEvent(timeline.Event{TimestampNs: at, SourceID: "bus", Type: timeline.TypePacket,
			Data: map[string]any{"protocol": "uart", "hex": hex.EncodeToString(frame), "len": len(frame),
				"attr.dir": dir, "attr.port": "/dev/synthetic", "attr.baud": "19200"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(addr, qty uint16) []byte {
		return modbus.EncodeRTU(1, []byte{3, byte(addr >> 8), byte(addr), byte(qty >> 8), byte(qty)})
	}
	emit(100*ms, "tx", read(0, 2))
	emit(104*ms, "rx", modbus.EncodeRTU(1, []byte{3, 4, 0, 100, 0, 200}))
	emit(300*ms, "tx", read(9000, 1))
	emit(304*ms, "rx", modbus.EncodeRTU(1, []byte{0x83, 2}))
	emit(500*ms, "tx", read(5, 1)) // never answered
	emit(700*ms, "tx", read(6, 1))
	bad := modbus.EncodeRTU(1, []byte{3, 2, 0, 7})
	bad[len(bad)-1] ^= 0xFF
	emit(704*ms, "rx", bad)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Update(func(m *session.Meta) {
		m.Devices = []session.DeviceMeta{{Alias: "bus", DeviceID: "synthetic", PluginID: "uart"}}
	})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	return a
}

func TestDecodeModbusPairsRequestsAndResponses(t *testing.T) {
	a := buildModbusBusSession(t)
	rep, err := a.DecodeModbus(app.ModbusQuery{Session: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Streams) != 1 || rep.Streams[0].Source != "bus" {
		t.Fatalf("streams = %+v", rep.Streams)
	}
	s := rep.Streams[0]
	// The bad-CRC response is never repaired or paired: its request counts as
	// unanswered and the corrupt frame is a transaction of its own.
	if st := s.Stats; st.Transactions != 5 || st.OK != 1 || st.Exceptions != 1 || st.NoResponse != 2 || st.CRCErrors != 1 {
		t.Errorf("stats = %+v", st)
	}
	if len(s.Transactions) == 0 || s.Transactions[0].Status != "ok" || !strings.Contains(s.Transactions[0].Summary, "read_holding_registers") {
		t.Errorf("first transaction = %+v", s.Transactions)
	}
	if s.Transactions[0].LatencyNs <= 0 {
		t.Error("latency must come from the chunk stamps")
	}
	if rep.Note == "" {
		t.Error("report must carry the latency caveat")
	}
	t.Logf("%+v", s.Stats)

	if _, err := a.DecodeModbus(app.ModbusQuery{Session: "latest", Source: "nope"}); err == nil {
		t.Error("an unknown source must be an error")
	}
	if _, err := a.DecodeModbus(app.ModbusQuery{Session: "latest", Mode: "bogus"}); err == nil {
		t.Error("an unknown mode must be an error")
	}
	if rep, err := a.DecodeModbus(app.ModbusQuery{Session: "latest", Limit: 1}); err != nil || !rep.Streams[0].Truncated || len(rep.Streams[0].Transactions) != 1 {
		t.Errorf("limit: %+v err=%v", rep, err)
	}
}

func TestDecodeModbusNeedsSerialPackets(t *testing.T) {
	a := buildExplainSession(t, nil) // a power + 802.15.4 session
	_, err := a.DecodeModbus(app.ModbusQuery{Session: "latest"})
	if err == nil || !strings.Contains(err.Error(), "uart") {
		t.Fatalf("err = %v, want a hint about uart packets", err)
	}
	_ = context.Background()
}
