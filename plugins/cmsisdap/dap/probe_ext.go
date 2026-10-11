package dap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/jtag"
)

// More command and DAP_Info ids (CMSIS-DAP 2.1).
const (
	cmdTransferAbort = 0x07
	cmdJTAGSequence  = 0x14
	cmdJTAGConfigure = 0x15
	cmdJTAGIDCODE    = 0x16

	InfoTestDomainTimer = 0xF1
	InfoUARTRxBuffer    = 0xFB
	InfoUARTTxBuffer    = 0xFC
	InfoSWOTraceBuffer  = 0xFD
)

// Capability bits of the first DAP_Info(Capabilities) byte; CapSWD and
// CapJTAG are defined with the other constants.
const (
	CapSWOUART        = 1 << 2
	CapSWOManchester  = 1 << 3
	CapAtomicCommands = 1 << 4
	CapTestDomainTmr  = 1 << 5
	CapSWOStreaming   = 1 << 6
	CapUARTPort       = 1 << 7
)

// Capability bit of the second byte.
const CapUSBCOMPort = 1 << 0

// Caps is the decoded capability bitmap. Only what the probe reported is
// true; nothing is inferred.
type Caps struct {
	Raw              []byte `json:"raw"`
	SWD              bool   `json:"swd"`
	JTAG             bool   `json:"jtag"`
	SWOUART          bool   `json:"swo_uart"`
	SWOManchester    bool   `json:"swo_manchester"`
	AtomicCommands   bool   `json:"atomic_commands"`
	TestDomainTimer  bool   `json:"test_domain_timer"`
	SWOStreaming     bool   `json:"swo_streaming"`
	UARTPort         bool   `json:"uart_port"`
	USBCOMPort       bool   `json:"usb_com_port"`
	TestDomainTimerB []byte `json:"test_domain_timer_raw,omitempty"`
	UARTRxBuffer     uint32 `json:"uart_rx_buffer,omitempty"`
	UARTTxBuffer     uint32 `json:"uart_tx_buffer,omitempty"`
	SWOTraceBuffer   uint32 `json:"swo_trace_buffer,omitempty"`
}

// DecodeCaps decodes the DAP_Info capability bytes.
func DecodeCaps(raw []byte) Caps {
	c := Caps{Raw: append([]byte(nil), raw...)}
	if len(raw) > 0 {
		b := raw[0]
		c.SWD, c.JTAG = b&CapSWD != 0, b&CapJTAG != 0
		c.SWOUART, c.SWOManchester = b&CapSWOUART != 0, b&CapSWOManchester != 0
		c.AtomicCommands, c.TestDomainTimer = b&CapAtomicCommands != 0, b&CapTestDomainTmr != 0
		c.SWOStreaming, c.UARTPort = b&CapSWOStreaming != 0, b&CapUARTPort != 0
	}
	if len(raw) > 1 {
		c.USBCOMPort = raw[1]&CapUSBCOMPort != 0
	}
	return c
}

// InfoRaw runs DAP_Info for one id; ok is false when the probe reports no
// value (a zero-length answer).
func (p *Probe) InfoRaw(id byte) (data []byte, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.infoRaw(id)
}

// Caps reads and decodes the capability bytes and the optional buffer sizes
// again from the probe.
func (p *Probe) Caps() (Caps, error) {
	raw, _, err := p.InfoRaw(InfoCaps)
	if err != nil {
		return Caps{}, err
	}
	c := DecodeCaps(raw)
	u32 := func(id byte) uint32 {
		d, ok, err := p.InfoRaw(id)
		if err != nil || !ok || len(d) < 4 {
			return 0
		}
		return binary.LittleEndian.Uint32(d)
	}
	if d, ok, _ := p.InfoRaw(InfoTestDomainTimer); ok {
		c.TestDomainTimerB = append([]byte(nil), d...)
	}
	c.UARTRxBuffer, c.UARTTxBuffer, c.SWOTraceBuffer = u32(InfoUARTRxBuffer), u32(InfoUARTTxBuffer), u32(InfoSWOTraceBuffer)
	return c, nil
}

// Sender is implemented by transports that can send a command whose response
// does not exist (DAP_TransferAbort).
type Sender interface {
	Send(req []byte, timeout time.Duration) error
}

// TransferAbort cancels a DAP_Transfer in progress. The command has no
// response, so it is sent on its own.
func (p *Probe) TransferAbort() error {
	s, ok := p.t.(Sender)
	if !ok {
		return errors.New("this transport cannot send DAP_TransferAbort")
	}
	return s.Send([]byte{cmdTransferAbort}, p.timeout)
}

// JTAGSeq is one DAP_JTAG_Sequence entry: Bits TCK cycles (1..64) with TMS
// held at TMS and TDI shifted out LSB first from TDI.
type JTAGSeq struct {
	Bits    int
	TMS     bool
	Capture bool
	TDI     []byte
}

// JTAGSequence runs sequences in order and returns the captured TDO bytes of
// each entry that asked for capture, in order. A command that would not fit
// the packet size is split between entries.
func (p *Probe) JTAGSequence(seqs []JTAGSeq) ([][]byte, error) {
	var out [][]byte
	for len(seqs) > 0 {
		req := []byte{cmdJTAGSequence, 0}
		respLen := 2
		n := 0
		for n < len(seqs) && n < 255 {
			s := seqs[n]
			if s.Bits < 1 || s.Bits > 64 {
				return out, fmt.Errorf("JTAG sequence entry of %d bits: want 1..64", s.Bits)
			}
			nb := (s.Bits + 7) / 8
			rl := respLen
			if s.Capture {
				rl += nb
			}
			if len(req)+1+nb > p.ps || rl > p.ps {
				break
			}
			info := byte(s.Bits & 0x3F) // 64 encodes as 0
			if s.TMS {
				info |= 1 << 6
			}
			if s.Capture {
				info |= 1 << 7
			}
			req = append(req, info)
			tdi := make([]byte, nb)
			copy(tdi, s.TDI)
			req = append(req, tdi...)
			respLen = rl
			n++
		}
		if n == 0 {
			return out, errors.New("packet size too small for a JTAG sequence")
		}
		req[1] = byte(n)
		resp, err := p.cmd(req)
		if err != nil {
			return out, err
		}
		if len(resp) < 2 || resp[1] != 0 {
			return out, errors.New("DAP_JTAG_Sequence failed")
		}
		off := 2
		for _, s := range seqs[:n] {
			if !s.Capture {
				continue
			}
			nb := (s.Bits + 7) / 8
			if len(resp) < off+nb {
				return out, errors.New("truncated DAP_JTAG_Sequence response")
			}
			out = append(out, append([]byte(nil), resp[off:off+nb]...))
			off += nb
		}
		seqs = seqs[n:]
	}
	return out, nil
}

// JTAGConfigure sets the IR length of every TAP in the chain (needed for
// DAP_Transfer and DAP_JTAG_IDCODE in JTAG mode).
func (p *Probe) JTAGConfigure(irLens []byte) error {
	if len(irLens) == 0 || len(irLens)+2 > p.ps {
		return fmt.Errorf("JTAG chain of %d TAPs does not fit a DAP packet", len(irLens))
	}
	return p.status(append([]byte{cmdJTAGConfigure, byte(len(irLens))}, irLens...), "DAP_JTAG_Configure")
}

// JTAGIDCODE reads the IDCODE of the TAP at index with the probe's own
// command.
func (p *Probe) JTAGIDCODE(index byte) (uint32, error) {
	resp, err := p.cmd([]byte{cmdJTAGIDCODE, index})
	if err != nil {
		return 0, err
	}
	if len(resp) < 6 || resp[1] != 0 {
		return 0, errors.New("DAP_JTAG_IDCODE failed")
	}
	return binary.LittleEndian.Uint32(resp[2:]), nil
}

// JTAGPort adapts a probe in JTAG mode to the generic JTAG driver, so the
// TAP state machine, chain scan and IDCODE decoding are the shared ones.
type JTAGPort struct{ P *Probe }

var _ jtag.Port = JTAGPort{}

// TMS implements jtag.Port.
func (j JTAGPort) TMS(_ context.Context, tms []bool, tdi bool) error {
	var seqs []JTAGSeq
	for _, v := range tms {
		if n := len(seqs); n > 0 && seqs[n-1].TMS == v && seqs[n-1].Bits < 64 {
			seqs[n-1].Bits++
			continue
		}
		seqs = append(seqs, JTAGSeq{Bits: 1, TMS: v})
	}
	for i := range seqs {
		if tdi {
			seqs[i].TDI = jtag.Ones(seqs[i].Bits)
		}
	}
	_, err := j.P.JTAGSequence(seqs)
	return err
}

// Shift implements jtag.Port.
func (j JTAGPort) Shift(_ context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("shift of fewer than 1 bit")
	}
	var seqs []JTAGSeq
	body := nbits
	if exit {
		body--
	}
	bit := func(i int) bool { return tdi != nil && i/8 < len(tdi) && jtag.GetBit(tdi, i) }
	pos := 0
	for body > 0 {
		n := body
		if n > 64 {
			n = 64
		}
		seg := make([]byte, (n+7)/8)
		for i := 0; i < n; i++ {
			jtag.SetBit(seg, i, bit(pos+i))
		}
		seqs = append(seqs, JTAGSeq{Bits: n, Capture: capture, TDI: seg})
		pos += n
		body -= n
	}
	if exit {
		seg := []byte{0}
		if bit(pos) {
			seg[0] = 1
		}
		seqs = append(seqs, JTAGSeq{Bits: 1, TMS: true, Capture: capture, TDI: seg})
	}
	caps, err := j.P.JTAGSequence(seqs)
	if err != nil || !capture {
		return nil, err
	}
	res := make([]byte, (nbits+7)/8)
	at := 0
	ci := 0
	for _, s := range seqs {
		if ci >= len(caps) {
			return nil, errors.New("missing captured TDO data")
		}
		for i := 0; i < s.Bits; i++ {
			jtag.SetBit(res, at, jtag.GetBit(caps[ci], i))
			at++
		}
		ci++
	}
	return res, nil
}

var _ adiv5.Link = (*Probe)(nil)
