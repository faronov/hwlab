//go:build hardware

package uart

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Run with: go test -tags=hardware ./plugins/uart/ -run Hardware -v
//
// Needs a real serial port. Environment:
//
//	HWLAB_UART_PORT      device id (e.g. ttyACM0); default: the first port found.
//	HWLAB_UART_BAUD      baud rate, default 115200.
//	HWLAB_UART_LOOPBACK=1  the port's TX is wired to its RX (jumper, or an
//	                     echoing device); enables the receive test.

func hwPort(t *testing.T) (*Plugin, string) {
	t.Helper()
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := os.Getenv("HWLAB_UART_PORT")
	for _, d := range devs {
		if want == "" || d.Id == want {
			t.Logf("port: %s (%s)", d.Id, d.Name)
			return p, d.Id
		}
	}
	t.Skip("no serial port found")
	return nil, ""
}

func hwBaud() float64 {
	if os.Getenv("HWLAB_UART_BAUD") == "" {
		return 115200
	}
	var b float64
	for _, c := range os.Getenv("HWLAB_UART_BAUD") {
		b = b*10 + float64(c-'0')
	}
	return b
}

func TestHardwareOpenAndWrite(t *testing.T) {
	p, id := hwPort(t)
	ctx := context.Background()
	if err := p.OpenDevice(ctx, id, map[string]any{"baud": hwBaud()}); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(ctx, id)
	args, _ := structpb.NewStruct(map[string]any{"data": "hwlab\r\n"})
	resp, err := p.Invoke(ctx, &pluginv1.InvokeRequest{DeviceId: id, Capability: "SerialTransport", Method: "write", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetResult().AsMap()["written"]; got != float64(7) {
		t.Errorf("written = %v", got)
	}
}

func TestHardwareLoopbackCapture(t *testing.T) {
	if os.Getenv("HWLAB_UART_LOOPBACK") != "1" {
		t.Skip("HWLAB_UART_LOOPBACK not set")
	}
	p, id := hwPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := pluginsdk.NewTestSink(256)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, CaptureID: "hw",
		Config: map[string]any{"baud": hwBaud(), "lines": true}, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	args, _ := structpb.NewStruct(map[string]any{"data": "loop-test\r\n"})
	if _, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: "SerialTransport", Method: "write", Args: args}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	var rx strings.Builder
	for _, e := range sink.Drain() {
		if pk, ok := e.Payload.(*pluginv1.Event_Packet); ok && e.StreamId == StreamRX {
			rx.Write(pk.Packet.Data)
		}
	}
	if rx.String() != "loop-test\r\n" {
		t.Errorf("received %q", rx.String())
	}
}
