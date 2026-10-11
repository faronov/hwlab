package canplugin

import (
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/can"
)

// VirtualName is the interface served by the in-process virtual bus.
const VirtualName = "virtual-can0"

// VirtualEnv enables the virtual bus in the real plugin binary
// (HWLAB_CAN_VIRTUAL=1). It is a software loopback with no hardware behind it;
// sessions recorded on it are synthetic and must be labelled so.
const VirtualEnv = "HWLAB_CAN_VIRTUAL"

const virtualQueue = 4096

// Hub is an in-process loopback bus: a frame sent through one handle reaches
// every other open handle, and the sender too when its Own option is set.
// It behaves like vcan for a single plugin process.
type Hub struct {
	mu   sync.Mutex
	subs map[*vbus]struct{}
}

// NewHub returns an empty virtual bus.
func NewHub() *Hub { return &Hub{subs: map[*vbus]struct{}{}} }

// Info lists the single virtual interface.
func (h *Hub) Info() IfaceInfo {
	return IfaceInfo{Name: VirtualName, FD: true, Up: true, Virtual: true, MTU: can.KernelFDSize, Backend: "virtual"}
}

// Open attaches a handle to the hub.
func (h *Hub) Open(name string, o Options) (Bus, error) {
	b := &vbus{hub: h, name: name, opt: o, q: make(chan Rx, virtualQueue), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[b] = struct{}{}
	h.mu.Unlock()
	return b, nil
}

// Inject delivers a frame as if another node had transmitted it.
func (h *Hub) Inject(f can.Frame) {
	h.deliver(nil, f)
}

func (h *Hub) deliver(from *vbus, f can.Frame) {
	h.mu.Lock()
	subs := make([]*vbus, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	now := time.Now()
	for _, s := range subs {
		own := s == from
		if own && !s.opt.Own {
			continue
		}
		fr := f
		fr.Interface = s.name
		fr.Own = own
		if fr.FD && !s.opt.FD || fr.Error && !s.opt.ErrorFrames || !can.MatchAny(s.opt.Filters, fr) {
			continue
		}
		select {
		case s.q <- Rx{Frame: fr, At: now}:
		default: // a slow reader loses frames, as a kernel socket buffer would
		}
	}
}

type vbus struct {
	hub  *Hub
	name string
	opt  Options
	q    chan Rx

	once sync.Once
	done chan struct{}
}

func (b *vbus) Recv(timeout time.Duration) (Rx, error) {
	select {
	case <-b.done:
		return Rx{}, ErrClosed
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case rx := <-b.q:
		return rx, nil
	case <-b.done:
		return Rx{}, ErrClosed
	case <-t.C:
		return Rx{}, ErrTimeout
	}
}

func (b *vbus) Send(f can.Frame) error {
	select {
	case <-b.done:
		return ErrClosed
	default:
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if f.FD && !b.opt.FD {
		return errFDDisabled
	}
	b.hub.deliver(b, f)
	return nil
}

func (b *vbus) Close() error {
	b.once.Do(func() {
		close(b.done)
		b.hub.mu.Lock()
		delete(b.hub.subs, b)
		b.hub.mu.Unlock()
	})
	return nil
}
