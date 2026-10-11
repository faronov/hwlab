// Package protocol is the native J-Link probe command layer: one method per
// probe command, with typed replies. It owns the command ids and reply
// framing and nothing else; SWD and JTAG wire semantics live above it
// (plugins/jlinknative/swd, .../jtag) and Cortex-M logic lives in
// internal/cortexm.
//
// The J-Link USB command set is not a published standard. Every command id and
// reply layout below was established by observing a real probe (see
// docs/adr/0034-native-jlink.md for the evidence and the clean-implementation
// boundary); nothing is taken from GPL implementations. Commands whose
// behaviour has not been observed are not implemented.
package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/usb"
)

// Command ids observed on the probe. The name is ours.
const (
	CmdVersion     = 0x01 // reply: u16 length, then that many bytes of text
	CmdSetSpeed    = 0x05 // arg u16 kHz, no reply
	CmdGetHWStatus = 0x07 // reply: 8 bytes
	CmdGetSpeeds   = 0xC0 // reply: u32 base clock Hz, u16 minimum divider
	CmdSelectIF    = 0xC7 // arg u8 interface (0xFF: query mask), reply u32
	CmdSequence    = 0xCF // bit-level vectors, see Sequence
	CmdResetLow    = 0xDC // drive nRESET low
	CmdResetHigh   = 0xDD // release nRESET
	CmdTRSTLow     = 0xDE
	CmdTRSTHigh    = 0xDF
	CmdGetCaps     = 0xE8 // reply: u32
	CmdGetExtCaps  = 0xED // reply: 32 bytes
	CmdGetHWVer    = 0xF0 // reply: u32
)

// Interface is a target interface selected with SELECT_IF.
type Interface uint8

// Interfaces whose numbering was confirmed on hardware: 0 is the power-on
// default and JTAG, 1 selects SWD.
const (
	IfJTAG Interface = 0
	IfSWD  Interface = 1
)

func (i Interface) String() string {
	switch i {
	case IfJTAG:
		return "JTAG"
	case IfSWD:
		return "SWD"
	}
	return fmt.Sprintf("interface-%d", uint8(i))
}

const (
	// MaxSequenceBits is the largest vector this layer sends in one command.
	// The command carries a u16 bit count and the probe accepted 32768 bits
	// during the investigation; the cap keeps one command's USB occupancy
	// bounded.
	MaxSequenceBits = 8192
)

// Client talks to one probe. It is safe for concurrent use; commands are
// serialised so that replies never interleave.
type Client struct {
	t  usb.Transport
	mu sync.Mutex

	selected Interface
	haveSel  bool
	speedKHz uint16

	// Unknown collects statuses and values the layer did not expect so that
	// diagnostics can show them instead of silently dropping them.
	unknown []string
}

// New returns a Client on t.
func New(t usb.Transport) *Client { return &Client{t: t} }

// Transport returns the underlying transport.
func (c *Client) Transport() usb.Transport { return c.t }

// Unknown returns the unexpected values seen so far.
func (c *Client) Unknown() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.unknown...)
}

func (c *Client) note(format string, a ...any) {
	if len(c.unknown) < 64 {
		c.unknown = append(c.unknown, fmt.Sprintf(format, a...))
	}
}

func (c *Client) exchange(ctx context.Context, op string, cmd []byte, n int) ([]byte, error) {
	b, err := c.t.Exchange(ctx, cmd, n)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, jlerr.New(jlerr.ProtocolError, op, fmt.Errorf("short reply: %d of %d bytes", len(b), n))
	}
	return b, nil
}

// VersionInfo is the probe's firmware identification string.
type VersionInfo struct {
	Raw string // text up to the first NUL, as reported
	// Full is the whole string block, NULs shown as newlines, including the
	// copyright line the probe appends.
	Full string
	// Bootloader is true when the probe reports a bootloader image ("BTL" in
	// the firmware string). A bootloader does not run the command set.
	Bootloader bool
}

// Version reads the firmware string (command 0x01). The probe answers with a
// length in one transfer and the text in another; both are always read.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hdr, err := c.exchange(ctx, "version length", []byte{CmdVersion}, 2)
	if err != nil {
		return VersionInfo{}, err
	}
	n := int(binary.LittleEndian.Uint16(hdr))
	if n == 0 || n > 1024 {
		return VersionInfo{}, jlerr.New(jlerr.ProtocolError, "version", fmt.Errorf("implausible string length %d", n))
	}
	body, err := c.exchange(ctx, "version text", nil, n)
	if err != nil {
		return VersionInfo{}, err
	}
	v := VersionInfo{}
	text := string(body)
	if i := strings.IndexByte(text, 0); i >= 0 {
		v.Raw = strings.TrimRight(text[:i], " ")
		v.Full = strings.TrimSpace(strings.ReplaceAll(strings.Trim(text, "\x00"), "\x00", "\n"))
	} else {
		v.Raw, v.Full = strings.TrimRight(text, " "), text
	}
	v.Bootloader = strings.Contains(v.Raw, "BTL")
	return v, nil
}

// Caps reads the capability word (command 0xE8). Individual bits are not
// named: their meaning was not established independently, so they are
// reported raw.
func (c *Client) Caps(ctx context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "get caps", []byte{CmdGetCaps}, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// ExtCaps reads the 32-byte extended capability block (command 0xED).
func (c *Client) ExtCaps(ctx context.Context) ([32]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [32]byte
	b, err := c.exchange(ctx, "get extended caps", []byte{CmdGetExtCaps}, 32)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

// HWVersion is the hardware version word (command 0xF0).
type HWVersion struct {
	Raw uint32
}

// String formats the word as the probe's tools print hardware versions:
// 97000 is "V9.70".
func (h HWVersion) String() string {
	v := h.Raw / 100
	return fmt.Sprintf("V%d.%02d", v/100, v%100)
}

// HWVersion reads the hardware version word.
func (c *Client) HWVersion(ctx context.Context) (HWVersion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "get hardware version", []byte{CmdGetHWVer}, 4)
	if err != nil {
		return HWVersion{}, err
	}
	return HWVersion{Raw: binary.LittleEndian.Uint32(b)}, nil
}

// HWStatus is the probe's target-side status block (command 0x07).
type HWStatus struct {
	Raw [8]byte
	// VTargetMV is the target voltage sense in millivolts: the first u16.
	VTargetMV uint16
	// Pins are the six bytes after it, raw. Their assignment to TCK, TDI, TDO,
	// TMS, nRESET and nTRST is NOT established, so they stay unnamed here.
	Pins [6]byte
}

// HWStatus reads the target voltage and pin block.
func (c *Client) HWStatus(ctx context.Context) (HWStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "get hardware status", []byte{CmdGetHWStatus}, 8)
	if err != nil {
		return HWStatus{}, err
	}
	var s HWStatus
	copy(s.Raw[:], b)
	s.VTargetMV = binary.LittleEndian.Uint16(b)
	copy(s.Pins[:], b[2:])
	return s, nil
}

// Interfaces returns the mask of selectable interfaces (SELECT_IF 0xFF).
func (c *Client) Interfaces(ctx context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "query interfaces", []byte{CmdSelectIF, 0xFF}, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// SelectInterface selects the target interface and returns the previous one.
// The request is refused here when the probe's mask does not offer it.
func (c *Client) SelectInterface(ctx context.Context, i Interface) (Interface, error) {
	if i != IfJTAG && i != IfSWD {
		return 0, jlerr.New(jlerr.UnsupportedProbeCommand, "select interface", fmt.Errorf("%s is not a verified interface", i))
	}
	mask, err := c.Interfaces(ctx)
	if err != nil {
		return 0, err
	}
	if mask&(1<<i) == 0 {
		return 0, jlerr.New(jlerr.UnsupportedProbeCommand, "select interface", fmt.Errorf("the probe does not offer %s (mask 0x%x)", i, mask))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "select interface", []byte{CmdSelectIF, byte(i)}, 4)
	if err != nil {
		return 0, err
	}
	prev := binary.LittleEndian.Uint32(b)
	c.selected, c.haveSel = i, true
	return Interface(prev), nil
}

// Speeds describes the clock the probe derives its speeds from.
type Speeds struct {
	BaseHz     uint32
	MinDivider uint16
}

// MaxKHz is the highest clock the divider range allows.
func (s Speeds) MaxKHz() uint32 {
	if s.MinDivider == 0 {
		return 0
	}
	return s.BaseHz / uint32(s.MinDivider) / 1000
}

// Speeds reads the speed description (command 0xC0).
func (c *Client) Speeds(ctx context.Context) (Speeds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "get speeds", []byte{CmdGetSpeeds}, 6)
	if err != nil {
		return Speeds{}, err
	}
	return Speeds{BaseHz: binary.LittleEndian.Uint32(b), MinDivider: binary.LittleEndian.Uint16(b[4:])}, nil
}

// SetSpeed sets the interface clock in kHz (command 0x05). The probe sends no
// reply, so acceptance cannot be confirmed by the command itself; callers who
// need the rate measure it (see MeasureClock).
func (c *Client) SetSpeed(ctx context.Context, kHz uint16) error {
	if kHz == 0 || kHz == 0xFFFF {
		return jlerr.New(jlerr.UnsupportedProbeCommand, "set speed", fmt.Errorf("%d kHz: fixed rates 1..65534 only (adaptive clocking is not implemented)", kHz))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.exchange(ctx, "set speed", []byte{CmdSetSpeed, byte(kHz), byte(kHz >> 8)}, 0); err != nil {
		return err
	}
	c.speedKHz = kHz
	return nil
}

// SpeedKHz is the last value sent with SetSpeed (0 before any).
func (c *Client) SpeedKHz() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.speedKHz
}

// Selected reports the interface selected through this client.
func (c *Client) Selected() (Interface, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selected, c.haveSel
}

// Sequence is one bit-level command. Vector A and B are packed LSB first.
// In SWD mode A is the direction of each bit (1: the probe drives SWDIO) and B
// the value driven; in JTAG mode A is TMS and B is TDI. The reply holds the
// level sampled for each bit.
type Sequence struct {
	Bits int
	A, B []byte
}

// SequenceResult is the sampled vector and the probe's status byte.
type SequenceResult struct {
	In     []byte
	Status byte
}

// Run executes one sequence (command 0xCF). A non-zero status byte is an
// error and keeps the raw status in the message.
func (c *Client) Run(ctx context.Context, s Sequence) (SequenceResult, error) {
	if s.Bits <= 0 || s.Bits > MaxSequenceBits {
		return SequenceResult{}, jlerr.New(jlerr.ProtocolError, "sequence", fmt.Errorf("%d bits: want 1..%d", s.Bits, MaxSequenceBits))
	}
	nb := (s.Bits + 7) / 8
	if len(s.A) != nb || len(s.B) != nb {
		return SequenceResult{}, jlerr.New(jlerr.ProtocolError, "sequence", fmt.Errorf("vectors are %d and %d bytes, want %d", len(s.A), len(s.B), nb))
	}
	cmd := make([]byte, 4, 4+2*nb)
	cmd[0], cmd[1] = CmdSequence, 0
	binary.LittleEndian.PutUint16(cmd[2:], uint16(s.Bits))
	cmd = append(append(cmd, s.A...), s.B...)
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.exchange(ctx, "sequence", cmd, nb+1)
	if err != nil {
		return SequenceResult{}, err
	}
	r := SequenceResult{In: b[:nb], Status: b[nb]}
	if r.Status != 0 {
		c.note("sequence status 0x%02x", r.Status)
		return r, jlerr.New(jlerr.ProtocolError, "sequence", fmt.Errorf("probe status 0x%02x", r.Status))
	}
	return r, nil
}

// SetReset drives (low=true) or releases the probe's nRESET output. Whether
// the pin reaches the target depends on the wiring, which the caller has to
// verify from the target side.
func (c *Client) SetReset(ctx context.Context, low bool) error {
	cmd := byte(CmdResetHigh)
	if low {
		cmd = CmdResetLow
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.exchange(ctx, "set nRESET", []byte{cmd}, 0)
	return err
}
