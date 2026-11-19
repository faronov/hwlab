// Package bsl is a native client of the TI CC13xx/CC26xx/CC2538 ROM serial
// bootloader (TRM SWCU117/SWCU185 "ROM Bootloader", SWRU319 for CC2538).
//
// Framing: the host sends 0x55 0x55 once so the ROM measures the baud rate
// and answers ACK. Every command is a packet [size][checksum][cmd data...],
// size counting itself and the checksum, checksum the 8-bit sum of cmd and
// data. The ROM answers ACK (0x00 0xCC) or NACK (0x00 0x33); data replies are
// packets the host ACKs in turn. Multi-byte numbers are big-endian.
//
// The client was written from the TRM and checked against the behaviour of
// the de-facto cc2538-bsl tool; no code was copied from it.
package bsl

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Command codes.
const (
	CmdPing        = 0x20
	CmdDownload    = 0x21
	CmdGetStatus   = 0x23
	CmdSendData    = 0x24
	CmdReset       = 0x25
	CmdSectorErase = 0x26 // CC26xx: address; CC2538: address + size
	CmdCRC32       = 0x27 // CC26xx: address, size, read repeat count; CC2538: address, size
	CmdGetChipID   = 0x28
	CmdMemoryRead  = 0x2A
	CmdMemoryWrite = 0x2B // never sent by hwlab
	CmdBankErase   = 0x2C // CC26xx only
	CmdSetCCFG     = 0x2D // CC26xx only; never sent by hwlab
)

// GET_STATUS answers.
const (
	StatusSuccess    = 0x40
	StatusUnknownCmd = 0x41
	StatusInvalidCmd = 0x42
	StatusInvalidAdr = 0x43
	StatusFlashFail  = 0x44
)

const (
	ackByte  = 0xCC
	nackByte = 0x33
	syncByte = 0x55

	// MaxSendData is the largest SEND_DATA payload (size byte 255 = 3 + 252).
	MaxSendData = 252
	// MaxReadWords is the largest 32-bit MEMORY_READ count on CC26xx.
	MaxReadWords = 63
)

var (
	// ErrTimeout means the ROM did not answer in time.
	ErrTimeout = errors.New("no answer from the ROM bootloader")
	// ErrNACK means the ROM rejected a packet.
	ErrNACK = errors.New("the ROM bootloader answered NACK")
	// ErrProtocol is a malformed answer.
	ErrProtocol = errors.New("malformed answer from the ROM bootloader")
)

// StatusError is a GET_STATUS answer other than success.
type StatusError struct {
	Cmd    byte
	Status byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("command 0x%02x: ROM status 0x%02x (%s)", e.Cmd, e.Status, StatusName(e.Status))
}

// StatusName names a GET_STATUS code.
func StatusName(s byte) string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusUnknownCmd:
		return "unknown command"
	case StatusInvalidCmd:
		return "invalid command"
	case StatusInvalidAdr:
		return "invalid address"
	case StatusFlashFail:
		return "flash operation failed"
	}
	return "undocumented"
}

// Link is the serial line.
type Link interface {
	io.ReadWriter
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
	// ResetInput discards bytes received and not read.
	ResetInput() error
}

// Family selects the command dialect.
type Family int

const (
	// FamilyCC26xx covers CC13x0/CC26x0, CC13x2/CC26x2 (incl. x7, P, R) and CC13x1/CC26x1.
	FamilyCC26xx Family = iota
	// FamilyCC2538 is the CC2538 dialect (ERASE with a size, 4-byte reversed MEMORY_READ).
	FamilyCC2538
)

func (f Family) String() string {
	if f == FamilyCC2538 {
		return "CC2538"
	}
	return "CC13xx/CC26xx"
}

// Stats counts link events.
type Stats struct {
	Commands  int
	Resyncs   int
	JunkBytes int
}

// Client talks to one ROM bootloader.
type Client struct {
	l      Link
	ctx    context.Context
	Family Family
	// Timeout bounds one ACK or reply packet; long commands scale it.
	Timeout time.Duration
	stats   Stats
	buf     []byte
}

// New returns a client on l.
func New(l Link) *Client {
	return &Client{l: l, ctx: context.Background(), Timeout: 2 * time.Second}
}

// SetContext makes later calls abort when ctx is done.
func (c *Client) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ctx = ctx
}

// Stats returns the link counters.
func (c *Client) Stats() Stats { return c.stats }

const pollSlice = 20 * time.Millisecond

func (c *Client) readByte(deadline time.Time) (byte, error) {
	if len(c.buf) > 0 {
		b := c.buf[0]
		c.buf = c.buf[1:]
		return b, nil
	}
	var tmp [512]byte
	for {
		if err := c.ctx.Err(); err != nil {
			return 0, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return 0, ErrTimeout
		}
		if left > pollSlice {
			left = pollSlice
		}
		if err := c.l.SetReadTimeout(left); err != nil {
			return 0, err
		}
		n, err := c.l.Read(tmp[:])
		if n > 0 {
			c.buf = append(c.buf, tmp[1:n]...)
			return tmp[0], nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
	}
}

func (c *Client) write(p []byte) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	for len(p) > 0 {
		n, err := c.l.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// maxJunk bounds the bytes skipped while waiting for an ACK.
const maxJunk = 4096

// waitAck waits for 0x00 followed by ACK or NACK, skipping other bytes.
func (c *Client) waitAck(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	prev := byte(0xFF)
	for skipped := 0; ; skipped++ {
		b, err := c.readByte(deadline)
		if err != nil {
			return err
		}
		if prev == 0x00 {
			switch b {
			case ackByte:
				return nil
			case nackByte:
				return ErrNACK
			}
		}
		if b != 0x00 {
			c.stats.JunkBytes++
		}
		if skipped > maxJunk {
			return fmt.Errorf("%w: no ACK within %d bytes", ErrProtocol, maxJunk)
		}
		prev = b
	}
}

// Packet frames cmd and data.
func Packet(cmd byte, data []byte) []byte {
	sum := cmd
	for _, b := range data {
		sum += b
	}
	out := make([]byte, 0, 3+len(data))
	out = append(out, byte(len(data)+3), sum, cmd)
	return append(out, data...)
}

// send writes one command packet and waits for its ACK.
func (c *Client) send(cmd byte, data []byte, ackTimeout time.Duration) error {
	if len(data) > MaxSendData {
		return fmt.Errorf("command 0x%02x: %d data bytes, at most %d", cmd, len(data), MaxSendData)
	}
	c.stats.Commands++
	if err := c.write(Packet(cmd, data)); err != nil {
		return err
	}
	if err := c.waitAck(ackTimeout); err != nil {
		return fmt.Errorf("command 0x%02x: %w", cmd, err)
	}
	return nil
}

// recv reads one reply packet and acknowledges it.
func (c *Client) recv(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var size byte
	for {
		b, err := c.readByte(deadline)
		if err != nil {
			return nil, err
		}
		if b != 0x00 {
			size = b
			break
		}
	}
	if size < 3 {
		return nil, fmt.Errorf("%w: reply size %d", ErrProtocol, size)
	}
	sum, err := c.readByte(deadline)
	if err != nil {
		return nil, err
	}
	data := make([]byte, int(size)-2)
	var calc byte
	for i := range data {
		if data[i], err = c.readByte(deadline); err != nil {
			return nil, err
		}
		calc += data[i]
	}
	if calc != sum {
		_ = c.write([]byte{0x00, nackByte})
		return nil, fmt.Errorf("%w: reply checksum 0x%02x, computed 0x%02x", ErrProtocol, sum, calc)
	}
	return data, c.write([]byte{0x00, ackByte})
}

// Status sends GET_STATUS and returns the ROM's answer.
func (c *Client) Status() (byte, error) {
	if err := c.send(CmdGetStatus, nil, c.Timeout); err != nil {
		return 0, err
	}
	d, err := c.recv(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("status reply: %w", err)
	}
	if len(d) != 1 {
		return 0, fmt.Errorf("%w: status reply of %d bytes", ErrProtocol, len(d))
	}
	return d[0], nil
}

func (c *Client) checkStatus(cmd byte) error {
	s, err := c.Status()
	if err != nil {
		return err
	}
	if s != StatusSuccess {
		return &StatusError{Cmd: cmd, Status: s}
	}
	return nil
}

// explain turns a NACK or a missing reply into the ROM's status code when
// it reports one: a refused command (bad address) is NACKed or unanswered.
func (c *Client) explain(cmd byte, err error) error {
	if errors.Is(err, ErrNACK) || errors.Is(err, ErrTimeout) {
		if s, serr := c.Status(); serr == nil && s != StatusSuccess {
			return &StatusError{Cmd: cmd, Status: s}
		}
	}
	return err
}

// exec sends a command without a reply packet and checks GET_STATUS.
func (c *Client) exec(cmd byte, data []byte, ackTimeout time.Duration) error {
	if err := c.send(cmd, data, ackTimeout); err != nil {
		return c.explain(cmd, err)
	}
	return c.checkStatus(cmd)
}

// query sends a command with a reply packet and checks GET_STATUS.
func (c *Client) query(cmd byte, data []byte, timeout time.Duration) ([]byte, error) {
	if err := c.send(cmd, data, timeout); err != nil {
		return nil, c.explain(cmd, err)
	}
	r, err := c.recv(timeout)
	if err != nil {
		return nil, c.explain(cmd, fmt.Errorf("command 0x%02x reply: %w", cmd, err))
	}
	if err := c.checkStatus(cmd); err != nil {
		return nil, err
	}
	return r, nil
}

func be32(v ...uint32) []byte {
	out := make([]byte, 0, 4*len(v))
	for _, x := range v {
		out = binary.BigEndian.AppendUint32(out, x)
	}
	return out
}

// Sync sends the auto-baud sequence and waits for ACK.
func (c *Client) Sync(timeout time.Duration) error {
	_ = c.l.ResetInput()
	c.buf = nil
	if err := c.write([]byte{syncByte, syncByte}); err != nil {
		return err
	}
	if err := c.waitAck(timeout); err != nil {
		return fmt.Errorf("sync 0x55 0x55: %w", err)
	}
	return nil
}

// Connect synchronizes. If the ROM was already synchronized by an earlier
// session it reads the two 0x55 as the start of an 85-byte packet; the
// client then completes that packet with zeros (the ROM NACKs its checksum)
// and checks the link with PING. resynced reports that path.
func (c *Client) Connect(timeout time.Duration) (resynced bool, err error) {
	serr := c.Sync(timeout)
	if serr == nil {
		return false, nil
	}
	if !errors.Is(serr, ErrTimeout) && !errors.Is(serr, ErrNACK) {
		return false, serr
	}
	if errors.Is(serr, ErrTimeout) {
		if err := c.write(make([]byte, syncByte-2)); err != nil {
			return false, err
		}
		_ = c.waitAck(timeout)
	}
	c.stats.Resyncs++
	if err := c.Ping(); err != nil {
		return false, fmt.Errorf("%w (resynchronizing a running bootloader also failed: %w)", serr, err)
	}
	return true, nil
}

// Ping checks the link.
func (c *Client) Ping() error { return c.send(CmdPing, nil, c.Timeout) }

// ChipID returns the GET_CHIP_ID word.
func (c *Client) ChipID() (uint32, error) {
	r, err := c.query(CmdGetChipID, nil, c.Timeout)
	if err != nil {
		return 0, err
	}
	if len(r) != 4 {
		return 0, fmt.Errorf("%w: chip id of %d bytes", ErrProtocol, len(r))
	}
	return binary.BigEndian.Uint32(r), nil
}

// Reset sends COMMAND_RESET: the chip restarts (the ROM sends only the ACK).
func (c *Client) Reset() error { return c.send(CmdReset, nil, c.Timeout) }

// readWords reads n (1..63) 32-bit words starting at a 4-byte aligned addr
// in memory order (CC26xx).
func (c *Client) readWords(addr uint32, n int) ([]byte, error) {
	d := append(be32(addr), 1, byte(n))
	r, err := c.query(CmdMemoryRead, d, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("read 0x%08x: %w", addr, err)
	}
	if len(r) != 4*n {
		return nil, fmt.Errorf("%w: read 0x%08x: %d bytes, want %d", ErrProtocol, addr, len(r), 4*n)
	}
	return r, nil
}

// readBytes reads n (1..253) bytes with 8-bit accesses (CC26xx).
func (c *Client) readBytes(addr uint32, n int) ([]byte, error) {
	d := append(be32(addr), 0, byte(n))
	r, err := c.query(CmdMemoryRead, d, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("read 0x%08x: %w", addr, err)
	}
	if len(r) != n {
		return nil, fmt.Errorf("%w: read 0x%08x: %d bytes, want %d", ErrProtocol, addr, len(r), n)
	}
	return r, nil
}

// read2538 reads one word on CC2538: the ROM sends it most significant byte
// first, so it is reversed to memory order.
func (c *Client) read2538(addr uint32) ([]byte, error) {
	r, err := c.query(CmdMemoryRead, append(be32(addr), 4), c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("read 0x%08x: %w", addr, err)
	}
	if len(r) != 4 {
		return nil, fmt.Errorf("%w: read 0x%08x: %d bytes, want 4", ErrProtocol, addr, len(r))
	}
	return []byte{r[3], r[2], r[1], r[0]}, nil
}

// Word reads one 32-bit little-endian word.
func (c *Client) Word(addr uint32) (uint32, error) {
	b, err := c.Read(addr, 4, nil)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// Read reads n bytes from addr in memory order. Aligned spans use 32-bit
// accesses, an unaligned head or tail 8-bit ones (CC26xx). progress, when
// set, gets the bytes done so far.
func (c *Client) Read(addr uint32, n int, progress func(int)) ([]byte, error) {
	out := make([]byte, 0, n)
	if c.Family == FamilyCC2538 {
		start := addr &^ 3
		for a := start; a < addr+uint32(n); a += 4 {
			w, err := c.read2538(a)
			if err != nil {
				return nil, err
			}
			out = append(out, w...)
			if progress != nil && (a-start)%1024 == 0 {
				progress(len(out))
			}
		}
		lead := int(addr - start)
		return out[lead : lead+n], nil
	}
	end := addr + uint32(n)
	for a := addr; a < end; {
		var b []byte
		var err error
		left := int(end - a)
		switch {
		case a%4 != 0:
			b, err = c.readBytes(a, min(left, int(4-a%4)))
		case left < 4:
			b, err = c.readBytes(a, left)
		default:
			b, err = c.readWords(a, min(left/4, MaxReadWords))
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		a += uint32(len(b))
		if progress != nil {
			progress(len(out))
		}
	}
	return out, nil
}

// CRC32 has the ROM compute the IEEE CRC-32 of [addr, addr+n). The ROM sends
// it most significant byte first.
func (c *Client) CRC32(addr uint32, n int) (uint32, error) {
	d := be32(addr, uint32(n))
	if c.Family == FamilyCC26xx {
		d = append(d, be32(0)...)
	}
	r, err := c.query(CmdCRC32, d, c.Timeout+time.Duration(n>>10)*5*time.Millisecond)
	if err != nil {
		return 0, fmt.Errorf("crc32 0x%08x+%d: %w", addr, n, err)
	}
	if len(r) != 4 {
		return 0, fmt.Errorf("%w: crc of %d bytes", ErrProtocol, len(r))
	}
	return binary.BigEndian.Uint32(r), nil
}

// eraseTimeout bounds one sector erase.
const eraseTimeout = 3 * time.Second

// SectorErase erases the sector at addr (CC26xx).
func (c *Client) SectorErase(addr uint32) error {
	if c.Family != FamilyCC26xx {
		return errors.New("SECTOR_ERASE with an address only is the CC26xx dialect")
	}
	if err := c.exec(CmdSectorErase, be32(addr), eraseTimeout); err != nil {
		return fmt.Errorf("erase sector 0x%08x: %w", addr, err)
	}
	return nil
}

// EraseRange erases [addr, addr+n) on CC2538 (whole pages).
func (c *Client) EraseRange(addr uint32, n int) error {
	if c.Family != FamilyCC2538 {
		return errors.New("ERASE with a size is the CC2538 dialect")
	}
	t := eraseTimeout + time.Duration(n>>11)*25*time.Millisecond
	if err := c.exec(CmdSectorErase, be32(addr, uint32(n)), t); err != nil {
		return fmt.Errorf("erase 0x%08x+%d: %w", addr, n, err)
	}
	return nil
}

// BankErase erases the whole main flash bank including the CCFG (CC26xx).
func (c *Client) BankErase() error {
	if c.Family != FamilyCC26xx {
		return errors.New("BANK_ERASE is the CC26xx dialect")
	}
	return c.exec(CmdBankErase, nil, 10*time.Second)
}

// Write programs data (length a multiple of 4) at addr with DOWNLOAD and
// SEND_DATA; the range must be erased. progress, when set, gets bytes done.
func (c *Client) Write(addr uint32, data []byte, progress func(int)) error {
	if len(data) == 0 || len(data)%4 != 0 || addr%4 != 0 {
		return fmt.Errorf("download 0x%08x+%d: address and length must be multiples of 4", addr, len(data))
	}
	if err := c.exec(CmdDownload, be32(addr, uint32(len(data))), c.Timeout); err != nil {
		return fmt.Errorf("download 0x%08x+%d: %w", addr, len(data), err)
	}
	for off := 0; off < len(data); off += MaxSendData {
		chunk := data[off:min(off+MaxSendData, len(data))]
		if err := c.exec(CmdSendData, chunk, c.Timeout); err != nil {
			return fmt.Errorf("send data at 0x%08x: %w", addr+uint32(off), err)
		}
		if progress != nil {
			progress(off + len(chunk))
		}
	}
	return nil
}
