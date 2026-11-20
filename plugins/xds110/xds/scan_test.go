package xds

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/jtag"
)

func cc26Probe(t *testing.T) (*Probe, *Sim) {
	t.Helper()
	s := NewSimCC26x2()
	p := NewProbe(s)
	if _, err := p.Connect(); err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestScanEngineSplitsLargeBatches(t *testing.T) {
	p, s := cc26Probe(t)
	if err := p.CJTAGConnectFormat(FormatOScan1); err != nil {
		t.Fatal(err)
	}
	var ops []jtag.ScanOp
	for i := 0; i < 600; i++ {
		ops = append(ops, jtag.ScanOp{IR: true, Bits: 6, Out: []byte{0x04}, End: jtag.RunTestIdle},
			jtag.ScanOp{Bits: 32, End: jtag.RunTestIdle}, jtag.ScanOp{Idle: 4, End: jtag.RunTestIdle})
	}
	before := len(s.Commands)
	res, err := (&ScanEngine{P: p}).Scans(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1200 {
		t.Fatalf("%d results", len(res))
	}
	for i := 0; i < len(res); i += 2 {
		if res[i][0] != 0x01 || len(res[i+1]) != 4 || res[i+1][0] != 0x2F || res[i+1][3] != 0x3B {
			t.Fatalf("scan %d: %x %x", i, res[i], res[i+1])
		}
	}
	n := 0
	for _, c := range s.Commands[before:] {
		if c == CmdScanRequest {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("%d OCD_SCAN_REQUEST commands: the batch was not split at the 4 KiB block", n)
	}
}

func TestScanEngineRefusesTestLogicReset(t *testing.T) {
	p, _ := cc26Probe(t)
	_, err := (&ScanEngine{P: p}).Scans(context.Background(), []jtag.ScanOp{{Bits: 1, End: jtag.TestLogicReset}})
	if err == nil {
		t.Fatal("a scan ending in Test-Logic-Reset was accepted")
	}
}
