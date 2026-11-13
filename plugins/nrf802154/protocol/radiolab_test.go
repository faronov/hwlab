package protocol

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	fw := ParseVersion("\x1b[m\r\nversion: zigbee-rs-radio-lab 0.1.0 protocol: 1 caps: receive,transmit,cca,ack-wait,repeat,power,stats\r\n")
	if fw.Name != RadioLabName || fw.Version != "0.1.0" || fw.Protocol != 1 || !fw.CanTransmit() || !fw.Has("cca") || fw.Has("x") {
		t.Fatalf("radio lab: %+v", fw)
	}
	// Nordic's sniffer does not know the command.
	fw = ParseVersion("version: command not found\r\n\x1b[1;32muart:~$ \x1b[m")
	if fw.Name != NordicSnifferName || fw.CanTransmit() || !fw.Has("receive") {
		t.Fatalf("nordic: %+v", fw)
	}
	if fw = ParseVersion(""); fw.Name != "unknown" || fw.CanTransmit() {
		t.Fatalf("silent: %+v", fw)
	}
	// A different firmware claiming "transmit" is not trusted.
	if fw = ParseVersion("version: other 1.0 protocol: 1 caps: transmit"); fw.CanTransmit() {
		t.Fatalf("foreign firmware trusted: %+v", fw)
	}
}

func TestTxRequest(t *testing.T) {
	r := TxRequest{PSDU: []byte{0x03, 0x08, 0x01, 0xff, 0xff, 0xff, 0xff, 0x07}, Repeat: 1}
	if err := r.Validate(); err != nil || r.Command() != "transmit 030801ffffffff07" {
		t.Fatalf("%v %q", err, r.Command())
	}
	r.CCA, r.AckWait, r.Repeat, r.IntervalMs = true, true, 3, 20
	if err := r.Validate(); err != nil || r.Command() != "transmit 030801ffffffff07 cca ack repeat 3 20" {
		t.Fatalf("%v %q", err, r.Command())
	}
	bad := []TxRequest{
		{Repeat: 1},
		{PSDU: make([]byte, MaxTxPSDU+1), Repeat: 1},
		{PSDU: []byte{1, 2}, AckWait: true, Repeat: 1},
		{PSDU: []byte{1}, Repeat: 0},
		{PSDU: []byte{1}, Repeat: FirmwareMaxRepeat + 1, IntervalMs: 10},
		{PSDU: []byte{1}, Repeat: 2, IntervalMs: 1},
	}
	for i, b := range bad {
		if b.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := (TxRequest{PSDU: make([]byte, MaxTxPSDU), Repeat: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFCS(t *testing.T) {
	// CRC-16/KERMIT check value 0x2189, transmitted low byte first.
	if got := FCS([]byte("123456789")); !bytes.Equal(got, []byte{0x89, 0x21}) {
		t.Fatalf("% X", got)
	}
	frame := []byte{0x02, 0x00, 0x56}
	fcs := FCS(frame)
	full := append(bytes.Clone(frame), fcs...)
	// The FCS over frame+FCS is zero (residue property of KERMIT).
	if r := FCS(full); !bytes.Equal(r, []byte{0, 0}) {
		t.Fatalf("residue % X", r)
	}
}

func TestParseTxLine(t *testing.T) {
	ev, ok := ParseTxLine("transmitted: 030801ffffffff07132d channel: 26 power: 0 time: 123456789\r")
	if !ok || ev.Kind != TxFrame || ev.Channel != 26 || ev.PowerDb != 0 || ev.DevUs != 123456789 ||
		!bytes.Equal(ev.Data, []byte{3, 8, 1, 0xff, 0xff, 0xff, 0xff, 7}) || !bytes.Equal(ev.FCS, []byte{0x13, 0x2d}) {
		t.Fatalf("%+v", ev)
	}
	ev, ok = ParseTxLine("\x1b[mtx: 2/5 status: cca_busy time: 42")
	if !ok || ev.Kind != TxAttempt || ev.Attempt != 2 || ev.Total != 5 || ev.Status != "cca_busy" || ev.DevUs != 42 {
		t.Fatalf("%+v", ev)
	}
	ev, ok = ParseTxLine("tx_done: attempts: 5 sent: 4 acked: 1 no_ack: 3 cca_busy: 1 error: 0 result: aborted")
	want := TxSummary{Attempts: 5, Sent: 4, Acked: 1, NoAck: 3, CCABusy: 1, Result: "aborted"}
	if !ok || ev.Kind != TxDone || ev.Summary != want {
		t.Fatalf("%+v", ev)
	}
	ev, ok = ParseTxLine("error: frame must be 1..125 bytes")
	if !ok || ev.Kind != TxError || ev.Message != "frame must be 1..125 bytes" {
		t.Fatalf("%+v", ev)
	}
	if ev, ok = ParseTxLine("transmit: command not found"); !ok || ev.Kind != TxError {
		t.Fatalf("nordic reply: %+v", ev)
	}
	for _, l := range []string{
		"received: 0200560000 power: -40 lqi: 200 time: 5",
		"transmitted: 0 channel: 26 power: 0 time: 1",       // odd hex
		"transmitted: 0102 channel: 26 power: 0 time: 1",    // FCS only
		"tx: 1/1 status: maybe time: 1",                     // unknown status
		"tx_done: attempts: 1 sent: 1",                      // truncated
		"transmitted: 030801 channel: 26 power: 0 time: -1", // negative time
		"uart:~$ ",
	} {
		if ev, ok := ParseTxLine(l); ok {
			t.Errorf("%q parsed as %+v", l, ev)
		}
	}
}

func TestDeviceTransmitRadioLab(t *testing.T) {
	sim := NewSim()
	sim.RadioLab, sim.AckStatus = true, "acked"
	d, err := Open(sim.Opener(), "sim")
	if err != nil {
		t.Fatal(err)
	}
	d.Quiet = 20 * time.Millisecond
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	if err := d.Setup(ctx, 26); err != nil {
		t.Fatal(err)
	}
	if !d.Firmware.CanTransmit() || sim.Channel() != 26 {
		t.Fatalf("firmware %+v channel %d", d.Firmware, sim.Channel())
	}
	if err := d.SetPower(ctx, 4); err != nil || sim.Power() != 4 {
		t.Fatalf("power: %v %d", err, sim.Power())
	}
	if err := d.SetPower(ctx, 1); err == nil {
		t.Fatal("unsupported power accepted")
	}
	req := TxRequest{PSDU: []byte{0x03, 0x08, 0x01, 0xff, 0xff, 0xff, 0xff, 0x07}, AckWait: true, Repeat: 2, IntervalMs: 5}
	evs, err := d.Transmit(ctx, req, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []TxKind
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if len(evs) != 5 || kinds[0] != TxFrame || kinds[1] != TxAttempt || kinds[4] != TxDone {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].Channel != 26 || evs[0].PowerDb != 4 || !bytes.Equal(evs[0].FCS, FCS(req.PSDU)) || evs[1].Status != "acked" {
		t.Fatalf("frame %+v attempt %+v", evs[0], evs[1])
	}
	if s := evs[4].Summary; s.Attempts != 2 || s.Acked != 2 || s.Result != "ok" {
		t.Fatalf("summary %+v", s)
	}
	if tx := sim.Transmits(); len(tx) != 1 || tx[0] != req.Command() {
		t.Fatalf("transmits %q", tx)
	}
}

func TestDeviceTransmitNordicFirmware(t *testing.T) {
	sim := NewSim()
	d, _ := Open(sim.Opener(), "sim")
	d.Quiet = 20 * time.Millisecond
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Setup(context.Background(), 15); err != nil {
		t.Fatal(err)
	}
	if d.Firmware.Name != NordicSnifferName || d.Firmware.CanTransmit() {
		t.Fatalf("%+v", d.Firmware)
	}
	// If a caller transmits anyway, the reply is an error, not a hang.
	evs, err := d.Transmit(context.Background(), TxRequest{PSDU: []byte{1, 2, 3}, Repeat: 1}, time.Second)
	if err != nil || len(evs) != 1 || evs[0].Kind != TxError || !strings.Contains(evs[0].Message, "not found") {
		t.Fatalf("%v %+v", err, evs)
	}
}
