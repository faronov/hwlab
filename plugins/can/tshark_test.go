package canplugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/can"
	"github.com/faronov/hwlab/internal/decode"
)

// Wireshark must decode the artifact natively; skipped without tshark.
func TestPcapngDecodesInTshark(t *testing.T) {
	ts := &decode.Tshark{}
	if _, err := ts.Available(); err != nil {
		t.Skip("tshark not installed")
	}
	p, hub := hubPlugin(t)
	if err := p.OpenDevice(context.Background(), VirtualName, nil); err != nil {
		t.Fatal(err)
	}
	c, cancel, _ := startCap(t, p, nil, t.TempDir())
	hub.Inject(can.Frame{ID: 0x123, Data: []byte{1, 2, 3}})
	hub.Inject(can.Frame{ID: 0x1ABCDE00, Extended: true, Data: []byte{0xFF}})
	hub.Inject(can.Frame{ID: 0x10, RTR: true, DLC: 3})
	hub.Inject(can.Frame{ID: 0x55, FD: true, BRS: true, Data: make([]byte, 20)})
	time.Sleep(150 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	sums, err := ts.Summaries(context.Background(), c.Result().Artifacts[0].Path)
	if err != nil || len(sums) != 4 {
		t.Fatalf("tshark: %d packets, %v", len(sums), err)
	}
	for i, s := range sums {
		if !strings.HasPrefix(s.Protocols, "can") {
			t.Errorf("packet %d dissected as %q", i+1, s.Protocols)
		}
	}
	if !strings.Contains(sums[3].Info, "Length: 20") {
		t.Errorf("FD length lost: %q", sums[3].Info)
	}
	if !strings.Contains(sums[1].Info, "Ext. ID") || !strings.Contains(sums[2].Info, "Remote Transmission Request") {
		t.Errorf("flags lost: %q / %q", sums[1].Info, sums[2].Info)
	}
}
