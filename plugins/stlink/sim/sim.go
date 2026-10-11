// Package sim is a SYNTHETIC ST-Link/V2 probe: a usb.Port that speaks the
// V2 command set (including the DFU-exit step and the stall behaviour of the
// real probe) in front of a cortexm.Sim target. It exists for tests, replay
// and demos. Nothing it reports is evidence about real hardware.
package sim

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
)

// RealVersion is the GET_VERSION reply the attached probe returned (V2J39S7,
// VID 0483, PID 3748), kept so the parser is tested on real bytes.
var RealVersion = [6]byte{0x29, 0xc7, 0x83, 0x04, 0x48, 0x37}

// Config configures a Device.
type Config struct {
	// Serial is the raw USB serial string as the probe reports it.
	Serial string
	// Version overrides RealVersion.
	Version *[6]byte
	// Target is the simulated Cortex-M behind the probe. A shared target
	// keeps its state across reopening the port.
	Target *cortexm.Sim
	// DPIDCode is returned by READ_IDCODES (default 0x2BA01477).
	DPIDCode uint32
	// NoTarget makes ENTER_SWD fail with the "no ACK" status the real probe
	// reports for a missing or unpowered target.
	NoTarget bool
	// StartMode is the mode the probe powers up in (default dfu).
	StartMode int
	// MaxMemXfer is the largest READMEM/WRITEMEM payload (default 1024).
	MaxMemXfer int
	// WedgeOnStale makes the OUT pipe time out while a reply is waiting
	// unread, which the real probe does.
	WedgeOnStale bool
	// StaleIn is delivered by the first BulkIn calls (a previous owner's
	// unread reply).
	StaleIn []byte
	// OnNRST observes DRIVE_NRST (low=true while the line is held low).
	OnNRST func(low bool)
	// Gone is the physical presence switch: while it is true every transfer
	// of every port fails as a vanished device and the plugin hides the
	// probe. Set by NewConfig.
	Gone *atomic.Bool
}

// NewConfig returns a Config with its own shared target and presence switch,
// for a probe that is reopened several times.
func NewConfig(serial string, t *cortexm.Sim) *Config {
	if t == nil {
		t = cortexm.NewSim()
	}
	return &Config{Serial: serial, Target: t, Gone: new(atomic.Bool)}
}

// Port opens a new simulated port on the probe. The target is shared, so its
// state survives reopening; the probe itself starts in DFU mode again, like
// a freshly enumerated one.
func (c *Config) Port() *Device { return New(*c) }

// Unplug removes the probe; Replug brings it back.
func (c *Config) Unplug() {
	if c.Gone != nil {
		c.Gone.Store(true)
	}
}

// Replug makes the probe present again.
func (c *Config) Replug() {
	if c.Gone != nil {
		c.Gone.Store(false)
	}
}

// Present reports whether the probe is attached.
func (c *Config) Present() bool { return c.Gone == nil || !c.Gone.Load() }

// Device is the simulated port.
type Device struct {
	mu   sync.Mutex
	cfg  Config
	T    *cortexm.Sim
	mode int

	out, in uint8

	queue      []byte
	stalledIn  bool
	stalledOut bool
	gone       bool
	closed     bool

	pend      *pendingWrite
	lastRW    byte
	lastRWAdr uint32

	// Observations for tests.
	Commands   int
	Opcodes    []byte
	FreqDiv    int
	Stalls     int
	Halts      int
	NRSTLow    bool
	EnteredSWD bool
}

type pendingWrite struct {
	op   byte
	addr uint32
	n    int
}

// New returns a Device behind the standard endpoint layout: OUT 0x02,
// IN 0x81 (replies), IN 0x83 (SWO).
func New(cfg Config) *Device {
	if cfg.Target == nil {
		cfg.Target = cortexm.NewSim()
	}
	if cfg.DPIDCode == 0 {
		cfg.DPIDCode = 0x2BA01477
	}
	if cfg.MaxMemXfer == 0 {
		cfg.MaxMemXfer = 1024
	}
	if cfg.Version == nil {
		v := RealVersion
		cfg.Version = &v
	}
	d := &Device{cfg: cfg, T: cfg.Target, mode: cfg.StartMode, out: 0x02, in: 0x81}
	if cfg.StartMode == 0 {
		d.mode = 0
	}
	d.queue = append(d.queue, cfg.StaleIn...)
	return d
}

// Unplug makes every later transfer fail as a vanished device.
func (d *Device) Unplug() {
	d.mu.Lock()
	d.gone = true
	d.mu.Unlock()
}

// Mode reports the probe mode (0 dfu, 1 mass, 2 debug).
func (d *Device) Mode() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mode
}

// OpCount reports how many times a debug sub-command was received.
func (d *Device) OpCount(op byte) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, o := range d.Opcodes {
		if o == op {
			n++
		}
	}
	return n
}

func (d *Device) isGone() bool { return d.gone || !d.cfg.Present() }

func (d *Device) stall() {
	d.Stalls++
	d.stalledIn, d.stalledOut = true, true
	d.queue = nil
	d.pend = nil
}

// ControlIn serves GET_DESCRIPTOR for the serial number string (index 3) as
// UTF-16LE with one unit per serial byte, like the real probe.
func (d *Device) ControlIn(_ context.Context, rt, req uint8, value, _ uint16, length int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.isGone() {
		return nil, usb.ErrDisconnected
	}
	if rt == 0x80 && req == 6 && value == 0x0303 {
		units := []rune(d.cfg.Serial)
		b := []byte{byte(2 + 2*len(units)), 3}
		for _, u := range units {
			b = binary.LittleEndian.AppendUint16(b, uint16(u))
		}
		if length < len(b) {
			b = b[:length]
		}
		return b, nil
	}
	return nil, usb.ErrStall
}

// ControlOut is accepted and ignored.
func (d *Device) ControlOut(context.Context, uint8, uint8, uint16, uint16, []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.isGone() {
		return usb.ErrDisconnected
	}
	return nil
}

// ClearHalt clears an endpoint halt.
func (d *Device) ClearHalt(ep uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.isGone() {
		return usb.ErrDisconnected
	}
	if ep == d.in {
		d.stalledIn = false
	}
	if ep == d.out {
		d.stalledOut = false
	}
	return nil
}

// Describe implements usb.Port.
func (d *Device) Describe() string { return "simulated ST-Link/V2 (SYNTHETIC)" }

// DetachedDriver implements usb.Port.
func (d *Device) DetachedDriver() string { return "" }

// Close implements usb.Port.
func (d *Device) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

// BulkIn implements usb.Port.
func (d *Device) BulkIn(ctx context.Context, ep uint8, length int, _ time.Duration) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.isGone():
		return nil, usb.ErrDisconnected
	case d.closed:
		return nil, usb.ErrClosed
	case ep != d.in:
		return nil, usb.ErrTimeout
	case d.stalledIn:
		return nil, usb.ErrStall
	case len(d.queue) == 0:
		return nil, usb.ErrTimeout
	}
	n := min(length, len(d.queue))
	b := append([]byte(nil), d.queue[:n]...)
	d.queue = d.queue[n:]
	return b, nil
}

// BulkOut implements usb.Port.
func (d *Device) BulkOut(_ context.Context, ep uint8, data []byte, _ time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.isGone():
		return 0, usb.ErrDisconnected
	case d.closed:
		return 0, usb.ErrClosed
	case ep != d.out:
		return 0, usb.ErrTimeout
	case d.stalledOut:
		return 0, usb.ErrStall
	case d.cfg.WedgeOnStale && len(d.queue) > 0:
		return 0, usb.ErrTimeout
	}
	if d.pend != nil {
		d.finishWrite(data)
		return len(data), nil
	}
	if len(data) == 0 {
		return 0, nil
	}
	d.Commands++
	d.command(data)
	return len(data), nil
}

func (d *Device) reply(b ...byte) { d.queue = append(d.queue, b...) }

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func (d *Device) status(code byte, n int) {
	b := make([]byte, n)
	b[0] = code
	d.reply(b...)
}

func (d *Device) command(c []byte) {
	switch c[0] {
	case 0xF1:
		d.reply(d.cfg.Version[:]...)
	case 0xF5:
		d.reply(byte(d.mode), 0)
	case 0xF7:
		d.reply(append(le32(0x5e4), le32(0x7fa)...)...)
	case 0xF3:
		if c[1] == 0x07 {
			d.mode = 1
			return
		}
		d.stall()
	case 0xF2:
		if d.mode == 0 {
			d.stall()
			return
		}
		d.debug(c)
	default:
		d.stall()
	}
}

func (d *Device) rw(err error, addr uint32) {
	if err != nil {
		d.lastRW, d.lastRWAdr = 0x12, addr
		return
	}
	d.lastRW, d.lastRWAdr = 0x80, 0
}

func (d *Device) debug(c []byte) {
	op := c[1]
	d.Opcodes = append(d.Opcodes, op)
	ctx := context.Background()
	addr := binary.LittleEndian.Uint32(c[2:])
	switch op {
	case 0x21: // EXIT
		d.mode = 1 // the real probe sends no reply
	case 0x30: // ENTER
		if d.cfg.NoTarget {
			d.status(0x09, 2)
			return
		}
		d.mode = 2
		d.EnteredSWD = c[2] == 0xA3
		d.status(0x80, 2)
	case 0x31: // READ_IDCODES
		b := make([]byte, 12)
		b[0] = 0x80
		copy(b[4:], le32(d.cfg.DPIDCode))
		d.reply(b...)
	case 0x43:
		d.FreqDiv = int(c[2]) | int(c[3])<<8
		d.status(0x80, 2)
	case 0x36: // READDEBUGREG
		v, err := d.T.Read32(ctx, binary.LittleEndian.Uint32(c[2:]))
		d.rw(err, addr)
		if err != nil {
			d.status(0x12, 8)
			return
		}
		d.reply(append([]byte{0x80, 0, 0, 0}, le32(v)...)...)
	case 0x35: // WRITEDEBUGREG
		err := d.T.Write32(ctx, addr, binary.LittleEndian.Uint32(c[6:]))
		d.rw(err, addr)
		if err != nil {
			d.status(0x12, 2)
			return
		}
		d.status(0x80, 2)
	case 0x33: // READREG
		v, err := d.T.ReadCoreReg(ctx, cortexm.RegisterID(c[2]))
		if err != nil {
			d.status(0x12, 8)
			return
		}
		d.reply(append([]byte{0x80, 0, 0, 0}, le32(v)...)...)
	case 0x34: // WRITEREG
		if err := d.T.WriteCoreReg(ctx, cortexm.RegisterID(c[2]), binary.LittleEndian.Uint32(c[3:])); err != nil {
			d.status(0x12, 2)
			return
		}
		d.status(0x80, 2)
	case 0x3A: // READALLREGS
		regs, err := d.T.ReadCoreRegs(ctx)
		if err != nil {
			d.status(0x12, 88)
			return
		}
		b := make([]byte, 88)
		b[0] = 0x80
		for i, v := range regs {
			copy(b[4+4*i:], le32(v))
		}
		d.reply(b...)
	case 0x07: // READMEM_32BIT
		n := int(binary.LittleEndian.Uint16(c[6:]))
		if n > d.cfg.MaxMemXfer || addr&3 != 0 || n&3 != 0 || int(1024-addr&1023) < n {
			d.rw(errBadXfer, addr)
			d.reply(make([]byte, n)...)
			return
		}
		buf := make([]byte, n)
		err := d.T.ReadBlock(ctx, addr, buf)
		d.rw(err, addr)
		d.reply(buf...)
	case 0x08, 0x0D: // WRITEMEM_32BIT, WRITEMEM_8BIT
		d.pend = &pendingWrite{op: op, addr: addr, n: int(binary.LittleEndian.Uint16(c[6:]))}
	case 0x3E: // GETLASTRWSTATUS2
		b := make([]byte, 12)
		b[0] = d.lastRW
		copy(b[4:], le32(d.lastRWAdr))
		d.reply(b...)
	case 0x3B:
		d.status(d.lastRW, 2)
	case 0x3C: // DRIVE_NRST
		d.NRSTLow = c[2] == 0
		if d.cfg.OnNRST != nil {
			d.cfg.OnNRST(d.NRSTLow)
		}
		d.status(0x80, 2)
	default:
		d.stall()
	}
}

type simErr string

func (e simErr) Error() string { return string(e) }

const errBadXfer = simErr("sim: transfer length or alignment not supported")

func (d *Device) finishWrite(data []byte) {
	p := d.pend
	d.pend = nil
	ctx := context.Background()
	if len(data) != p.n || p.n > d.cfg.MaxMemXfer {
		d.rw(errBadXfer, p.addr)
		return
	}
	var err error
	if p.op == 0x08 {
		if p.addr&3 != 0 || p.n&3 != 0 || int(1024-p.addr&1023) < p.n {
			err = errBadXfer
		} else {
			err = d.T.WriteBlock(ctx, p.addr, data)
		}
	} else {
		err = d.writeBytes(ctx, p.addr, data)
	}
	d.rw(err, p.addr)
}

// writeBytes is the probe's 8-bit write: only the addressed bytes change.
func (d *Device) writeBytes(ctx context.Context, addr uint32, data []byte) error {
	for i, b := range data {
		a := addr + uint32(i)
		w, err := d.T.Read32(ctx, a&^3)
		if err != nil {
			return err
		}
		sh := (a & 3) * 8
		w = w&^(0xFF<<sh) | uint32(b)<<sh
		if err := d.T.Write32(ctx, a&^3, w); err != nil {
			return err
		}
	}
	return nil
}
