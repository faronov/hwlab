// Package dap is a host-side CMSIS-DAP client: the DAP command protocol, SWD
// and ADIv5 access, and Cortex-M debug control. It is written from the public
// CMSIS-DAP and Arm Debug Interface specifications.
//
// Nothing here has run against a real probe: the only counterpart is the
// in-process [Sim], which was written by the same author from the same
// reading of the specifications.
package dap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
)

// Transport moves whole DAP command packets. HID (CMSIS-DAP v1) and bulk
// (v2) transports both fit.
type Transport interface {
	// Exchange sends one command packet and returns the response packet. The
	// response may be longer than the useful bytes (HID pads to a report).
	Exchange(req []byte, timeout time.Duration) ([]byte, error)
	// PacketSize is the largest command packet the probe takes; the Probe
	// replaces it with the value from DAP_Info when the probe reports one.
	PacketSize() int
	// Describe names the transport for diagnostics.
	Describe() string
	Close() error
}

// DAP command ids.
const (
	cmdInfo         = 0x00
	cmdHostStatus   = 0x01
	cmdConnect      = 0x02
	cmdDisconnect   = 0x03
	cmdTransferCfg  = 0x04
	cmdTransfer     = 0x05
	cmdTransferBlk  = 0x06
	cmdWriteABORT   = 0x08
	cmdDelay        = 0x09
	cmdResetTarget  = 0x0A
	cmdSWJPins      = 0x10
	cmdSWJClock     = 0x11
	cmdSWJSequence  = 0x12
	cmdSWDConfigure = 0x13
	respInvalid     = 0xFF
)

// DAP_Info ids.
const (
	InfoVendor      = 0x01
	InfoProduct     = 0x02
	InfoSerial      = 0x03
	InfoProtocolVer = 0x04 // CMSIS-DAP protocol version string, e.g. "2.1.0"
	InfoTargetVend  = 0x05
	InfoTargetName  = 0x06
	InfoBoardVend   = 0x07
	InfoBoardName   = 0x08
	InfoProductFW   = 0x09
	InfoCaps        = 0xF0
	InfoPacketCount = 0xFE
	InfoPacketSize  = 0xFF
)

// Port numbers for DAP_Connect.
const (
	PortDefault = 0
	PortSWD     = 1
	PortJTAG    = 2
)

// Capability bits of DAP_Info(Capabilities), first byte.
const (
	CapSWD  = 1 << 0
	CapJTAG = 1 << 1
)

// Pin bits of DAP_SWJ_Pins.
const (
	PinSWCLK  = 1 << 0
	PinSWDIO  = 1 << 1
	PinnTRST  = 1 << 5
	PinnRESET = 1 << 7
)

// Transfer ACK values (bits 2:0 of the response byte).
const (
	AckOK    = 1
	AckWait  = 2
	AckFault = 4
	AckNone  = 7
)

// Errors a transfer can end with; they are defined by the ADIv5 layer so that
// every probe reports them the same way.
var (
	ErrWait     = adiv5.ErrWait
	ErrFault    = adiv5.ErrFault
	ErrNoACK    = adiv5.ErrNoACK
	ErrProtocol = adiv5.ErrProtocol
	ErrMismatch = adiv5.ErrMismatch
	// ErrUnsupported is a DAP command the probe does not implement.
	ErrUnsupported = errors.New("command not supported by the probe")
)

// ProbeInfo is what the probe reports about itself; empty fields were not
// reported.
type ProbeInfo struct {
	Vendor, Product, Serial string
	// ProtocolVersion is DAP_Info 0x04, the CMSIS-DAP protocol version the
	// probe claims; FirmwareVersion is DAP_Info 0x09, the product firmware
	// version, which many probes leave empty.
	ProtocolVersion, FirmwareVersion string
	TargetVendor, TargetName         string
	BoardVendor, BoardName           string
	Capabilities                     byte
	Caps                             Caps
	PacketSize                       int
	PacketCount                      int
}

// SupportsSWD reports the SWD capability bit.
func (i ProbeInfo) SupportsSWD() bool { return i.Capabilities&CapSWD != 0 }

// SupportsJTAG reports the JTAG capability bit.
func (i ProbeInfo) SupportsJTAG() bool { return i.Capabilities&CapJTAG != 0 }

// Probe is a CMSIS-DAP probe. It is safe for concurrent use; every command is
// serialised.
type Probe struct {
	mu      sync.Mutex
	t       Transport
	ps      int
	info    ProbeInfo
	timeout time.Duration
}

const (
	minPacket      = 16
	defaultTimeout = 2 * time.Second
	maxTransferOps = 255
	maxBlockWords  = 0xFFFF
)

// NewProbe wraps an open transport and reads the probe's identity.
func NewProbe(t Transport) (*Probe, error) {
	p := &Probe{t: t, ps: t.PacketSize(), timeout: defaultTimeout}
	if p.ps < minPacket {
		p.ps = 64
	}
	if err := p.readInfo(); err != nil {
		return nil, err
	}
	return p, nil
}

// Info returns what the probe reported when it was opened.
func (p *Probe) Info() ProbeInfo { return p.info }

// Close closes the transport.
func (p *Probe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.t.Close()
}

// Describe names the transport.
func (p *Probe) Describe() string { return p.t.Describe() }

// SetTimeout changes the per-command USB timeout.
func (p *Probe) SetTimeout(d time.Duration) {
	p.mu.Lock()
	p.timeout = d
	p.mu.Unlock()
}

func (p *Probe) cmd(req []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmdLocked(req)
}

func (p *Probe) cmdLocked(req []byte) ([]byte, error) {
	if len(req) > p.ps {
		return nil, fmt.Errorf("command of %d bytes exceeds the %d-byte packet size", len(req), p.ps)
	}
	resp, err := p.t.Exchange(req, p.timeout)
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, errors.New("empty DAP response")
	}
	if resp[0] == respInvalid && req[0] != respInvalid {
		return nil, fmt.Errorf("%w: 0x%02X", ErrUnsupported, req[0])
	}
	if resp[0] != req[0] {
		if r, ok := p.t.(Resyncer); ok {
			r.Resync()
		}
		return nil, fmt.Errorf("DAP response 0x%02X does not answer command 0x%02X (out of sync)", resp[0], req[0])
	}
	return resp, nil
}

// infoRaw runs DAP_Info; ok is false when the probe has no value for id.
func (p *Probe) infoRaw(id byte) (data []byte, ok bool, err error) {
	resp, err := p.cmdLocked([]byte{cmdInfo, id})
	if err != nil {
		return nil, false, err
	}
	if len(resp) < 2 {
		return nil, false, errors.New("short DAP_Info response")
	}
	n := int(resp[1])
	if n == 0 {
		return nil, false, nil
	}
	if len(resp) < 2+n {
		return nil, false, errors.New("truncated DAP_Info response")
	}
	return resp[2 : 2+n], true, nil
}

func (p *Probe) readInfo() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	str := func(id byte) string {
		d, ok, err := p.infoRaw(id)
		if err != nil || !ok {
			return ""
		}
		return strings.TrimRight(string(d), "\x00")
	}
	if d, ok, err := p.infoRaw(InfoPacketSize); err != nil {
		return fmt.Errorf("DAP_Info: %w (is this a CMSIS-DAP probe?)", err)
	} else if ok && len(d) >= 2 {
		if n := int(binary.LittleEndian.Uint16(d)); n >= minPacket {
			p.info.PacketSize = n
			if n < p.ps {
				p.ps = n
			}
		}
	}
	if sz, ok := p.t.(PacketSizer); ok {
		sz.SetPacketSize(p.ps)
	}
	if d, ok, _ := p.infoRaw(InfoPacketCount); ok && len(d) >= 1 {
		p.info.PacketCount = int(d[0])
	}
	if d, ok, _ := p.infoRaw(InfoCaps); ok {
		p.info.Capabilities = d[0]
		p.info.Caps = DecodeCaps(d)
	}
	p.info.Vendor, p.info.Product, p.info.Serial = str(InfoVendor), str(InfoProduct), str(InfoSerial)
	p.info.ProtocolVersion = str(InfoProtocolVer)
	p.info.FirmwareVersion = str(InfoProductFW)
	p.info.TargetVendor, p.info.TargetName = str(InfoTargetVend), str(InfoTargetName)
	p.info.BoardVendor, p.info.BoardName = str(InfoBoardVend), str(InfoBoardName)
	return nil
}

// Connect selects the debug port and returns the one the probe chose.
func (p *Probe) Connect(port byte) (byte, error) {
	resp, err := p.cmd([]byte{cmdConnect, port})
	if err != nil {
		return 0, err
	}
	if len(resp) < 2 || resp[1] == 0 {
		return 0, errors.New("the probe refused to connect (port not supported)")
	}
	return resp[1], nil
}

// Disconnect releases the target pins.
func (p *Probe) Disconnect() error {
	_, err := p.cmd([]byte{cmdDisconnect})
	return err
}

func (p *Probe) status(req []byte, what string) error {
	resp, err := p.cmd(req)
	if err != nil {
		return err
	}
	if len(resp) < 2 || resp[1] != 0 {
		return fmt.Errorf("%s failed", what)
	}
	return nil
}

// HostStatus drives the probe's connected/running LEDs.
func (p *Probe) HostStatus(led byte, on bool) error {
	v := byte(0)
	if on {
		v = 1
	}
	return p.status([]byte{cmdHostStatus, led, v}, "DAP_HostStatus")
}

// SetClock sets the SWD/JTAG clock.
func (p *Probe) SetClock(hz uint32) error {
	r := []byte{cmdSWJClock, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(r[1:], hz)
	return p.status(r, "DAP_SWJ_Clock")
}

// Configure sets idle cycles and retry counts for DAP_Transfer.
func (p *Probe) Configure(idle byte, waitRetry, matchRetry uint16) error {
	r := []byte{cmdTransferCfg, idle, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(r[2:], waitRetry)
	binary.LittleEndian.PutUint16(r[4:], matchRetry)
	return p.status(r, "DAP_TransferConfigure")
}

// SWDConfigure sets the SWD turnaround period and data-phase behaviour.
func (p *Probe) SWDConfigure(cfg byte) error {
	return p.status([]byte{cmdSWDConfigure, cfg}, "DAP_SWD_Configure")
}

// SWJSequence clocks out bits (LSB first of data[0]); 1..256 bits.
func (p *Probe) SWJSequence(bits int, data []byte) error {
	if bits < 1 || bits > 256 || len(data) != (bits+7)/8 {
		return fmt.Errorf("SWJ sequence of %d bits needs %d bytes, got %d", bits, (bits+7)/8, len(data))
	}
	r := append([]byte{cmdSWJSequence, byte(bits)}, data...) // 256 bits encode as 0
	return p.status(r, "DAP_SWJ_Sequence")
}

// Pins sets the output pins selected by sel, waits up to waitUs for the
// selected pins to reach their requested level, and returns the input state.
func (p *Probe) Pins(out, sel byte, waitUs uint32) (byte, error) {
	r := []byte{cmdSWJPins, out, sel, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(r[3:], waitUs)
	resp, err := p.cmd(r)
	if err != nil {
		return 0, err
	}
	if len(resp) < 2 {
		return 0, errors.New("short DAP_SWJ_Pins response")
	}
	return resp[1], nil
}

// Delay makes the probe wait. A delay of 0 is raised to 1 µs: the BL702
// DAPLink firmware was seen to stop answering after DAP_Delay(0) (see
// docs/notes/phase22-cmsisdap-results.md), and 0 waits for nothing anyway.
func (p *Probe) Delay(us uint16) error {
	if us == 0 {
		us = 1
	}
	return p.status([]byte{cmdDelay, byte(us), byte(us >> 8)}, "DAP_Delay")
}

// WriteABORT writes the DP ABORT register.
func (p *Probe) WriteABORT(v uint32) error {
	r := []byte{cmdWriteABORT, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(r[2:], v)
	return p.status(r, "DAP_WriteABORT")
}

// ResetTarget runs the probe's device-specific reset sequence. executed is
// false when the probe has none.
func (p *Probe) ResetTarget() (executed bool, err error) {
	resp, err := p.cmd([]byte{cmdResetTarget})
	if err != nil {
		return false, err
	}
	if len(resp) < 3 || resp[1] != 0 {
		return false, errors.New("DAP_ResetTarget failed")
	}
	return resp[2] != 0, nil
}

// Op and TransferError are the ADIv5 layer's types.
type (
	Op            = adiv5.Op
	TransferError = adiv5.TransferError
)

// opRequest encodes the DAP_Transfer request byte: APnDP, RnW and A[3:2].
func opRequest(o Op) byte {
	b := o.Reg & 0x0C
	if o.AP {
		b |= 1
	}
	if o.Read {
		b |= 2
	}
	return b
}

func ackError(resp byte) error {
	switch {
	case resp&0x08 != 0:
		return ErrProtocol
	case resp&7 == AckOK && resp&0x10 != 0:
		return ErrMismatch
	}
	switch resp & 7 {
	case AckOK:
		return nil
	case AckWait:
		return ErrWait
	case AckFault:
		return ErrFault
	}
	return ErrNoACK
}

// Transfer runs ops in order and returns the values of the reads, in order.
// The probe pipelines AP reads internally, so each read returns its own
// value. On failure it returns the reads that completed and a *TransferError
// whose Done counts completed ops across the whole batch.
func (p *Probe) Transfer(ops []Op) ([]uint32, error) {
	var out []uint32
	base := 0
	for len(ops) > 0 {
		n, rd, wr := 0, 0, 0
		for n < len(ops) && n < maxTransferOps {
			// request: cmd, index, count + 1 byte per op + 4 per write;
			// response: cmd, count, ack + 4 per read.
			r, w := rd, wr
			if ops[n].Read {
				r++
			} else {
				w++
			}
			if 3+n+1+4*w > p.ps || 3+4*r > p.ps {
				break
			}
			rd, wr = r, w
			n++
		}
		if n == 0 {
			return out, errors.New("packet size too small for a transfer")
		}
		vals, err := p.transferChunk(ops[:n])
		out = append(out, vals...)
		if err != nil {
			var te *TransferError
			if errors.As(err, &te) {
				te.Done += base
			}
			return out, err
		}
		base += n
		ops = ops[n:]
	}
	return out, nil
}

func (p *Probe) transferChunk(ops []Op) ([]uint32, error) {
	req := []byte{cmdTransfer, 0, byte(len(ops))}
	for _, o := range ops {
		req = append(req, opRequest(o))
		if !o.Read {
			req = binary.LittleEndian.AppendUint32(req, o.Value)
		}
	}
	resp, err := p.cmd(req)
	if err != nil {
		return nil, err
	}
	if len(resp) < 3 {
		return nil, errors.New("short DAP_Transfer response")
	}
	done, ack := int(resp[1]), resp[2]
	body := resp[3:]
	reads := 0
	for i := 0; i < done && i < len(ops); i++ {
		if ops[i].Read {
			reads++
		}
	}
	if len(body) < 4*reads {
		return nil, errors.New("truncated DAP_Transfer response")
	}
	vals := make([]uint32, reads)
	for i := range vals {
		vals[i] = binary.LittleEndian.Uint32(body[4*i:])
	}
	if e := ackError(ack); e != nil {
		return vals, &TransferError{Done: done, Ack: ack & 7, Err: e}
	}
	if done != len(ops) {
		return vals, &TransferError{Done: done, Ack: ack & 7, Err: fmt.Errorf("probe completed %d of %d transfers", done, len(ops))}
	}
	return vals, nil
}

// BlockRead reads words from one AP register (usually DRW), one command per
// packet-sized run.
func (p *Probe) BlockRead(ap bool, reg byte, words int) ([]uint32, error) {
	var out []uint32
	maxW := (p.ps - 4) / 4
	for words > 0 {
		n := words
		if n > maxW {
			n = maxW
		}
		if n > maxBlockWords {
			n = maxBlockWords
		}
		op := Op{AP: ap, Read: true, Reg: reg}
		resp, err := p.cmd([]byte{cmdTransferBlk, 0, byte(n), byte(n >> 8), opRequest(op)})
		if err != nil {
			return out, err
		}
		if len(resp) < 4 {
			return out, errors.New("short DAP_TransferBlock response")
		}
		done, ack := int(binary.LittleEndian.Uint16(resp[1:])), resp[3]
		got := (len(resp) - 4) / 4
		if got > done {
			got = done
		}
		for i := 0; i < got; i++ {
			out = append(out, binary.LittleEndian.Uint32(resp[4+4*i:]))
		}
		if e := ackError(ack); e != nil {
			return out, &TransferError{Done: len(out), Ack: ack & 7, Err: e}
		}
		if done != n || got != n {
			return out, &TransferError{Done: len(out), Ack: ack & 7, Err: fmt.Errorf("probe completed %d of %d words", done, n)}
		}
		words -= n
	}
	return out, nil
}

// BlockWrite writes words to one AP register.
func (p *Probe) BlockWrite(ap bool, reg byte, data []uint32) error {
	maxW := (p.ps - 5) / 4
	written := 0
	for len(data) > 0 {
		n := len(data)
		if n > maxW {
			n = maxW
		}
		if n > maxBlockWords {
			n = maxBlockWords
		}
		op := Op{AP: ap, Reg: reg}
		req := []byte{cmdTransferBlk, 0, byte(n), byte(n >> 8), opRequest(op)}
		for _, w := range data[:n] {
			req = binary.LittleEndian.AppendUint32(req, w)
		}
		resp, err := p.cmd(req)
		if err != nil {
			return err
		}
		if len(resp) < 4 {
			return errors.New("short DAP_TransferBlock response")
		}
		done, ack := int(binary.LittleEndian.Uint16(resp[1:])), resp[3]
		if e := ackError(ack); e != nil {
			return &TransferError{Done: written + done, Ack: ack & 7, Err: e}
		}
		if done != n {
			return &TransferError{Done: written + done, Ack: ack & 7, Err: fmt.Errorf("probe completed %d of %d words", done, n)}
		}
		written += n
		data = data[n:]
	}
	return nil
}
