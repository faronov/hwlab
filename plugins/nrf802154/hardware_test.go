//go:build hardware

package nrf802154

import (
	"context"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usbserial"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

// Run with: go test -tags=hardware ./plugins/nrf802154/ -run Hardware
// Needs an nRF52840 running the nRF 802.15.4 Sniffer firmware (USB 1915:154b).
// HWLAB_TEST_CHANNEL selects the channel (default 15); traffic is not required.

func realSniffer(t *testing.T) string {
	t.Helper()
	infos, err := usbserial.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range usbserial.Match(infos, "1915", "154b") {
		return i.Name
	}
	t.Skip("no nRF 802.15.4 sniffer (1915:154b) connected")
	return ""
}

func TestHardwareSnifferStartStopLeavesRadioAsleep(t *testing.T) {
	name := realSniffer(t)
	for i := 0; i < 3; i++ {
		d, err := protocol.Open(usbserial.Open(115200), name)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		if err := d.Start(ctx, 15); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := d.Run(ctx, func(protocol.Frame, time.Time) {}, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		cancel()
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Reopen and check that nothing is streaming any more.
	p, err := usbserial.Open(115200)(name)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_ = p.SetReadTimeout(100 * time.Millisecond)
	buf := make([]byte, 4096)
	deadline := time.Now().Add(1500 * time.Millisecond)
	n := 0
	for time.Now().Before(deadline) {
		k, _ := p.Read(buf)
		n += k
	}
	if n > 0 {
		t.Errorf("radio still streaming %d bytes after Close", n)
	}
}
