package decode

import (
	"context"
	"errors"
	"testing"
)

func TestParseFields(t *testing.T) {
	out := "1\t0.000123\twpan:zbee_nwk\tData, Dst: 0x0000\n2\t0.5\twpan\tAck\n\n"
	got, err := ParseFields(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Frame != 1 || got[0].EpochSec != 0.000123 || got[0].Info != "Data, Dst: 0x0000" || got[1].Protocols != "wpan" {
		t.Fatalf("got %+v", got)
	}
	if _, err := ParseFields("x\t1\tp\ti\n"); err == nil {
		t.Error("expected frame number error")
	}
	if _, err := ParseFields("1\n"); err == nil {
		t.Error("expected field count error")
	}
}

func TestSummariesRunner(t *testing.T) {
	ts := &Tshark{Path: "/bin/tshark", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/bin/tshark" || args[1] != "x.pcap" {
			t.Errorf("unexpected invocation %s %v", name, args)
		}
		return []byte("1\t1.5\twpan\tAck\n"), nil
	}}
	got, err := ts.Summaries(context.Background(), "x.pcap")
	if err != nil || len(got) != 1 || got[0].Info != "Ack" {
		t.Fatalf("got %+v, %v", got, err)
	}
	ts.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := ts.Summaries(context.Background(), "x.pcap"); err == nil {
		t.Error("expected runner error")
	}
}

func TestUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := (&Tshark{}).Summaries(context.Background(), "x.pcap"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}
