package xds

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// Transport carries one request frame and returns the response payload
// (status word followed by data). Implementations: usbfs bulk, and the
// simulator/replay used in tests.
type Transport interface {
	Exchange(req []byte, timeout time.Duration) ([]byte, error)
	Close() error
	Describe() string
}

// DefaultTimeout bounds one command.
const DefaultTimeout = 4 * time.Second

// Vendor interface layout of the XDS110 variants the plugin knows. The debug
// protocol lives on a vendor-class bulk interface; the CDC and HID
// interfaces are never touched.
type Layout struct {
	VID, PID  uint16
	Interface int
	Out, In   uint8
}

// Layouts lists the supported USB layouts. The stand-alone LP-XDS110ET
// (0451:bef3) is hardware-verified; the others are listed from public
// documentation and unverified.
var Layouts = []Layout{
	{0x0451, 0xbef3, 2, 0x02, 0x83},
	{0x0451, 0xbef4, 2, 0x02, 0x83},
	{0x1cbe, 0x02a5, 2, 0x02, 0x83},
}

// LayoutFor finds the layout of a VID:PID.
func LayoutFor(vid, pid uint16) (Layout, bool) {
	for _, l := range Layouts {
		if l.VID == vid && l.PID == pid {
			return l, true
		}
	}
	return Layout{}, false
}

// Match is a usb.Match for the supported probes.
func Match(vid, pid uint16) bool {
	_, ok := LayoutFor(vid, pid)
	return ok
}

func clearHalts(p usb.Port, l Layout) {
	if hc, ok := p.(usb.HaltClearer); ok {
		_ = hc.ClearHalt(l.Out)
		_ = hc.ClearHalt(l.In)
	}
}

type usbTransport struct {
	mu     sync.Mutex
	port   usb.Port
	dev    usb.Device
	layout Layout
	closed bool
}

// OpenUSB claims only the vendor debug interface of d.
func OpenUSB(d usb.Device) (Transport, error) {
	l, ok := LayoutFor(d.VID, d.PID)
	if !ok {
		return nil, fmt.Errorf("xds110: %04x:%04x is not a known XDS110", d.VID, d.PID)
	}
	p, err := usb.Open(d, l.Interface, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	t := &usbTransport{port: p, dev: d, layout: l}
	// A previous owner may have died between request and response; drop any
	// stale halt condition so the first command starts clean.
	clearHalts(p, l)
	return t, nil
}

func (t *usbTransport) Describe() string {
	return fmt.Sprintf("XDS110 usbfs %04x:%04x if%d out=0x%02x in=0x%02x", t.dev.VID, t.dev.PID, t.layout.Interface, t.layout.Out, t.layout.In)
}

func (t *usbTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	return t.port.Close()
}

const readChunk = 1024

func (t *usbTransport) Exchange(req []byte, timeout time.Duration) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, usb.ErrClosed
	}
	ctx := context.Background()
	if _, err := t.port.BulkOut(ctx, t.layout.Out, req, timeout); err != nil {
		return nil, t.stall(err)
	}
	deadline := time.Now().Add(timeout)
	pkt, err := t.in(ctx, deadline)
	if err != nil {
		return nil, err
	}
	n, err := parseHeader(pkt)
	if err != nil {
		return nil, err
	}
	payload := append(make([]byte, 0, n), pkt[headerLen:]...)
	for len(payload) < n {
		more, err := t.in(ctx, deadline)
		if err != nil {
			return nil, fmt.Errorf("%w: got %d of %d payload bytes: %v", ErrShortReply, len(payload), n, err)
		}
		payload = append(payload, more...)
	}
	if len(payload) != n {
		return nil, fmt.Errorf("%w: %d payload bytes, announced %d", ErrShortReply, len(payload), n)
	}
	return payload, nil
}

func (t *usbTransport) in(ctx context.Context, deadline time.Time) ([]byte, error) {
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil, fmt.Errorf("%w: XDS110 reply", usb.ErrTimeout)
	}
	b, err := t.port.BulkIn(ctx, t.layout.In, readChunk, wait)
	if err != nil {
		return nil, t.stall(err)
	}
	return b, nil
}

// stall clears a halted pipe once so the next command can succeed, and
// leaves the original error in place.
func (t *usbTransport) stall(err error) error {
	if errors.Is(err, usb.ErrStall) {
		clearHalts(t.port, t.layout)
	}
	return err
}

// Probe is an open XDS110.
type Probe struct {
	mu      sync.Mutex
	tr      Transport
	timeout time.Duration
	// Info is filled by Connect.
	Info Info
	// Log, if set, receives every exchange (for fixtures).
	Log func(cmd byte, req, resp []byte, err error)
}

// Info is the probe identification from XDS_VERSION.
type Info struct {
	Firmware      uint32 `json:"firmware"`
	FirmwareText  string `json:"firmware_text"`
	Hardware      uint16 `json:"hardware"`
	SupplyCapable bool   `json:"supply_capable"`
}

// NewProbe wraps a transport. It sends nothing.
func NewProbe(tr Transport) *Probe { return &Probe{tr: tr, timeout: DefaultTimeout} }

// SetTimeout changes the per-command timeout.
func (p *Probe) SetTimeout(d time.Duration) {
	p.mu.Lock()
	p.timeout = d
	p.mu.Unlock()
}

// Describe names the transport.
func (p *Probe) Describe() string { return p.tr.Describe() }

// Close closes the transport. It does not send XDS_DISCONNECT; call
// [Probe.Disconnect] first for an orderly shutdown.
func (p *Probe) Close() error { return p.tr.Close() }

// Do runs one command and returns the data after the status word. A non-zero
// status is returned as *Error together with whatever data came back.
func (p *Probe) Do(cmd byte, args []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.do(cmd, args)
}

func (p *Probe) do(cmd byte, args []byte) ([]byte, error) {
	req := frame(cmd, args)
	if len(req) > headerLen+maxPayload {
		return nil, fmt.Errorf("xds110: request of %d bytes is too large", len(req))
	}
	resp, err := p.tr.Exchange(req, p.timeout)
	if p.Log != nil {
		p.Log(cmd, req, resp, err)
	}
	if err != nil {
		return nil, err
	}
	if len(resp) < statusLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrShortReply, len(resp))
	}
	st := Status(int32(binary.LittleEndian.Uint32(resp)))
	data := resp[statusLen:]
	if st != StatusOK {
		return data, &Error{Cmd: cmd, Status: st}
	}
	return data, nil
}

func want(data []byte, n int, what string) error {
	if len(data) < n {
		return fmt.Errorf("%w: %s needs %d bytes, got %d", ErrShortReply, what, n, len(data))
	}
	return nil
}

// Connect opens the probe session (XDS_CONNECT) and reads its version.
func (p *Probe) Connect() (Info, error) {
	if _, err := p.Do(CmdConnect, nil); err != nil {
		return Info{}, err
	}
	return p.Version()
}

// Version reads XDS_VERSION and caches it in p.Info.
func (p *Probe) Version() (Info, error) {
	d, err := p.Do(CmdVersion, nil)
	if err != nil {
		return Info{}, err
	}
	if err := want(d, 6, "XDS_VERSION"); err != nil {
		return Info{}, err
	}
	fw := binary.LittleEndian.Uint32(d)
	hw := binary.LittleEndian.Uint16(d[4:])
	in := Info{Firmware: fw, FirmwareText: FirmwareString(fw), Hardware: hw, SupplyCapable: hw == HWStandAlone}
	p.mu.Lock()
	p.Info = in
	p.mu.Unlock()
	return in, nil
}

// Disconnect ends the session (XDS_DISCONNECT).
func (p *Probe) Disconnect() error {
	_, err := p.Do(CmdDisconnect, nil)
	return err
}

// SetClock programs TCK/SWCLK and reports the plan used. The result is the
// probe's nominal setting, not a measured frequency.
func (p *Probe) SetClock(khz uint32) (ClockPlan, error) {
	p.mu.Lock()
	fw := p.Info.Firmware
	p.mu.Unlock()
	plan := PlanClock(fw, khz)
	_, err := p.Do(CmdSetTCK, le32(plan.Delay))
	return plan, err
}

// CycleTCK clocks TCK count times with the TAP state unchanged.
func (p *Probe) CycleTCK(count uint32) error {
	_, err := p.Do(CmdCycleTCK, le32(count))
	return err
}

// SetTRST drives the test-reset line.
func (p *Probe) SetTRST(level bool) error {
	_, err := p.Do(CmdSetTRST, []byte{b2u(level)})
	return err
}

// SetSRST drives the system-reset line (nRESET). level true means released.
func (p *Probe) SetSRST(level bool) error {
	_, err := p.Do(CmdSetSRST, []byte{b2u(level)})
	return err
}

// CJTAGConnect switches the target to 4-pin JTAG (format 1) via the cJTAG
// escape sequence.
func (p *Probe) CJTAGConnect() error {
	_, err := p.Do(CmdCJTAGConnect, le32(1))
	return err
}

// CJTAGDisconnect undoes [Probe.CJTAGConnect].
func (p *Probe) CJTAGDisconnect() error {
	_, err := p.Do(CmdCJTAGDisc, nil)
	return err
}

// SWDConnect selects the SWD interface.
func (p *Probe) SWDConnect() error {
	_, err := p.Do(CmdSWDConnect, nil)
	return err
}

// SWDDisconnect releases the SWD interface.
func (p *Probe) SWDDisconnect() error {
	_, err := p.Do(CmdSWDDisconnect, nil)
	return err
}

// ErrNoSupply is returned by SetSupply on hardware without a switchable target
// supply. The command is not even sent there: the LP-XDS110ET accepts it with
// a success status and changes nothing (measured, docs/notes/phase27).
var ErrNoSupply = errors.New("this XDS110 hardware has no software-switchable target supply")

// SetSupply sets the programmable target supply of a stand-alone XDS110
// (hardware id 0x21; UNVERIFIED on hardware, no such probe is attached). It is
// refused with ErrNoSupply on every other variant. The caller (plugin layer)
// is responsible for the safety gate.
func (p *Probe) SetSupply(mV uint32, enable bool) error {
	p.mu.Lock()
	in := p.Info
	p.mu.Unlock()
	if !in.SupplyCapable {
		return fmt.Errorf("%w: only the stand-alone XDS110 (hardware id 0x%02x) has one; this probe reports 0x%02x", ErrNoSupply, HWStandAlone, in.Hardware)
	}
	if enable && (mV < SupplyMinMV || mV > SupplyMaxMV) {
		return fmt.Errorf("supply %d mV outside %d..%d mV", mV, SupplyMinMV, SupplyMaxMV)
	}
	args := append(le32(mV), b2u(enable))
	_, err := p.Do(CmdSetSupply, args)
	return err
}

func b2u(b bool) byte {
	if b {
		return 1
	}
	return 0
}
