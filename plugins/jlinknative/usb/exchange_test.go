package usb

import (
	"context"
	"errors"
	"testing"
	"time"

	iusb "github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
)

// scriptPort replays IN transfers and records OUT transfers and IN lengths.
type scriptPort struct {
	in     [][]byte
	out    [][]byte
	inLens []int
}

func (p *scriptPort) ControlIn(context.Context, uint8, uint8, uint16, uint16, int) ([]byte, error) {
	return nil, errors.New("unused")
}
func (p *scriptPort) ControlOut(context.Context, uint8, uint8, uint16, uint16, []byte) error {
	return errors.New("unused")
}
func (p *scriptPort) BulkOut(_ context.Context, _ uint8, d []byte, _ time.Duration) (int, error) {
	p.out = append(p.out, append([]byte(nil), d...))
	return len(d), nil
}
func (p *scriptPort) BulkIn(_ context.Context, _ uint8, n int, _ time.Duration) ([]byte, error) {
	p.inLens = append(p.inLens, n)
	if len(p.in) == 0 {
		return nil, iusb.ErrTimeout
	}
	b := p.in[0]
	p.in = p.in[1:]
	return b, nil
}
func (p *scriptPort) Describe() string       { return "script" }
func (p *scriptPort) DetachedDriver() string { return "" }
func (p *scriptPort) Close() error           { return nil }

func TestExchangeUpToReturnsFirstTransfer(t *testing.T) {
	p := &scriptPort{in: [][]byte{{}, {4, 0, 0, 0}, {1, 2, 3, 4}}}
	c := &Conn{port: p, timeout: time.Second}
	b, err := c.ExchangeUpTo(context.Background(), []byte{0xee, 0}, 68)
	if err != nil || len(b) != 4 || b[0] != 4 {
		t.Fatalf("got %x, %v", b, err)
	}
	if p.inLens[0] != 68 || p.inLens[1] != 68 || len(p.out) != 1 {
		t.Fatalf("in lengths %v, out %d", p.inLens, len(p.out))
	}
	rest, err := c.Exchange(context.Background(), nil, 4)
	if err != nil || rest[3] != 4 {
		t.Fatalf("rest %x, %v", rest, err)
	}
}

func TestExchangeUpToTimeoutMarksDirty(t *testing.T) {
	c := &Conn{port: &scriptPort{}, timeout: time.Second}
	_, err := c.ExchangeUpTo(context.Background(), nil, 64)
	if !jlerr.Is(err, jlerr.USBTimeout) || !c.dirty {
		t.Fatalf("err %v dirty %v", err, c.dirty)
	}
}
