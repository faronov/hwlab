package sws

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Link is the serial line to the programmer. plugins/uart.Port satisfies it.
type Link interface {
	io.ReadWriter
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
	// ResetInput discards received bytes that were not read yet.
	ResetInput() error
}

const (
	// DefaultBaud is the programmer's power-on UART speed.
	DefaultBaud = 230400
	// MaxBlock is the largest read the programmer answers in one command.
	MaxBlock = 1024
	// FlashSector is the erase unit; FlashPage the largest write per command.
	FlashSector = 4096
	FlashPage   = 256

	defaultIOTimeout = 200 * time.Millisecond
)

// Version is what the programmer reports about itself.
type Version struct {
	Firmware  uint16 // BCD-style, e.g. 0x0008
	ChipID    uint16
	Chip      string
	Power     bool
	SWDiv     byte
	AddrLen   byte
	SWBuf     [6]byte
	BitRateMb float64 // SWire bit rate in Mbit/s
}

// ChipName names a Telink chip id (the same ids identify programmers and
// targets).
func ChipName(id uint16) string {
	switch id {
	case 0x5562:
		return "TLSR825x"
	case 0x5325:
		return "TLSR8266"
	case 0x5326:
		return "TLSR8267"
	case 0x5327:
		return "TLSR8269"
	case 0x5591:
		return "TLSR8208"
	}
	return "unknown"
}

// Programmer talks to one SWire programmer. It is not safe for concurrent use.
type Programmer struct {
	link    Link
	timeout time.Duration
	ver     Version
	haveVer bool
	ctx     context.Context
}

// New wraps an open serial link.
func New(l Link) *Programmer {
	return &Programmer{link: l, timeout: defaultIOTimeout, ctx: context.Background()}
}

// SetContext makes every following transaction fail with the context's error
// once it is done. A transaction in flight completes first, so a cancelled
// flash read stops at the next block boundary.
func (p *Programmer) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.ctx = ctx
}

// SetTimeout changes the per-command answer timeout.
func (p *Programmer) SetTimeout(d time.Duration) { p.timeout = d }

func readFull(l Link, buf []byte, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	n := 0
	for n < len(buf) {
		k, err := l.Read(buf[n:])
		n += k
		if err != nil {
			if errors.Is(err, io.EOF) && n < len(buf) {
				return n, nil
			}
			return n, err
		}
		if n < len(buf) && k == 0 && time.Now().After(deadline) {
			return n, nil
		}
	}
	return n, nil
}

// transact sends req (CRC appended) and reads one answer carrying dataLen
// data bytes on success. The answer's count field is returned: for most
// commands it is the data length, for writes it is the number of bytes
// written, and for FLASH_RDCRC it carries the CRC.
func (p *Programmer) transact(req []byte, dataLen int, timeout time.Duration) (count uint16, data []byte, err error) {
	if err = p.ctx.Err(); err != nil {
		return 0, nil, fmt.Errorf("cancelled: %w", err)
	}
	if timeout <= 0 {
		timeout = p.timeout
	}
	if err = p.link.SetReadTimeout(timeout / 4); err != nil {
		return 0, nil, err
	}
	if err = p.link.ResetInput(); err != nil {
		return 0, nil, err
	}
	frame := withCRC(append([]byte(nil), req...))
	if _, err = p.link.Write(frame); err != nil {
		return 0, nil, err
	}
	var hdr [4]byte
	n, err := readFull(p.link, hdr[:], timeout)
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return 0, nil, ErrTimeout
	}
	if n < 4 || hdr[0] != req[0] {
		return 0, nil, ErrBadResponse
	}
	count = binary.LittleEndian.Uint16(hdr[2:4])
	if hdr[1] != 0 {
		var tail [2]byte
		if k, _ := readFull(p.link, tail[:], timeout); k < 2 || !crcOK(append(hdr[:], tail[:]...)) {
			return 0, nil, ErrBadResponse
		}
		return 0, nil, &Error{Cmd: req[0], Code: hdr[1]}
	}
	rest := make([]byte, dataLen+2)
	k, err := readFull(p.link, rest, timeout)
	if err != nil {
		return 0, nil, err
	}
	if k < len(rest) || !crcOK(append(hdr[:], rest...)) {
		return 0, nil, ErrBadResponse
	}
	return count, rest[:dataLen], nil
}

func addr3(cmd byte, addr uint32, extra ...byte) []byte {
	b := []byte{cmd, byte(addr), byte(addr >> 8), byte(addr >> 16)}
	return append(b, extra...)
}

func funcs(sub byte, arg uint16, extra ...byte) []byte {
	return append([]byte{cmdFuncs, sub, byte(arg), byte(arg >> 8)}, extra...)
}

// Version reads the programmer's identity and SWire settings.
func (p *Programmer) Version() (Version, error) {
	_, d, err := p.transact(funcs(fnVersion, 0), 13, 0)
	if err != nil {
		return Version{}, err
	}
	v := Version{
		Firmware: uint16(d[1])<<8 | uint16(d[0]),
		ChipID:   binary.LittleEndian.Uint16(d[2:4]),
		Power:    d[4] != 0,
		SWDiv:    d[5],
		AddrLen:  d[6],
	}
	copy(v.SWBuf[:], d[7:13])
	if v.ChipID == 0 && v.Firmware == 0 {
		return Version{}, errors.New("the device answered but reports no programmer identity (not a SWire programmer?)")
	}
	v.Chip = ChipName(v.ChipID)
	if v.SWDiv != 0 {
		clk := 32.0
		if v.ChipID == 0x5562 {
			clk = 24
		}
		v.BitRateMb = clk / 5 / float64(v.SWDiv)
	}
	p.ver, p.haveVer = v, true
	return v, nil
}

// Cached returns the last Version result.
func (p *Programmer) Cached() (Version, bool) { return p.ver, p.haveVer }

// DefaultSWBuf is the SWire framing template used by the reference tool for a
// 3-byte-address target.
var DefaultSWBuf = [6]byte{0x5a, 0x00, 0x06, 0x02, 0x00, 0x05}

// Configure sets the SWire divisor, address length and framing template.
func (p *Programmer) Configure(swdiv, addrLen byte, swbuf [6]byte) error {
	req := funcs(2, 0, swdiv, addrLen)
	req = append(req, swbuf[:]...)
	_, d, err := p.transact(req, 8, 0)
	if err != nil {
		return err
	}
	p.ver.SWDiv, p.ver.AddrLen = d[0], d[1]
	copy(p.ver.SWBuf[:], d[2:8])
	return nil
}

// SetReset drives the target's RST/power pin. Level false holds it in reset.
func (p *Programmer) SetReset(level bool) error {
	v := uint16(0)
	if level {
		v = 1
	}
	_, _, err := p.transact(funcs(fnReset, v), 1, 0)
	return err
}

// Activate issues the SWire activation sequence for about ms milliseconds and
// returns the target's program counter at activation.
func (p *Programmer) Activate(ms int) (uint32, error) {
	if ms <= 0 {
		return 0, errors.New("activation time must be positive")
	}
	ver, ok := p.Cached()
	if !ok {
		var err error
		if ver, err = p.Version(); err != nil {
			return 0, err
		}
	}
	div := float64(ver.SWDiv)
	if div == 0 {
		div = 5
	}
	count := int(float64(ms) / ((float64(ver.AddrLen) + 5.7) * 5 * div / 2400))
	if count > 0xffff {
		count = 0xffff
	}
	timeout := time.Duration(ms)*time.Millisecond + 500*time.Millisecond
	_, d, err := p.transact(funcs(fnActivate, uint16(count)), 4, timeout)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d), nil
}

// ReadChipID reads the target's chip revision and id over SWire.
func (p *Programmer) ReadChipID() (rev byte, id uint16, err error) {
	d, err := p.SWRead(0x7d, 3)
	if err != nil {
		return 0, 0, err
	}
	return d[0], binary.LittleEndian.Uint16(d[1:3]), nil
}

func (p *Programmer) readBlocks(cmd byte, addr uint32, n int, what string) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("%s: negative length", what)
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		chunk := n - len(out)
		if chunk > MaxBlock {
			chunk = MaxBlock
		}
		a := (addr + uint32(len(out))) & 0xffffff
		c, d, err := p.transact(addr3(cmd, a, byte(chunk), byte(chunk>>8)), chunk, 0)
		if err != nil {
			return nil, fmt.Errorf("%s at 0x%06x: %w", what, a, err)
		}
		if int(c) != chunk {
			return nil, fmt.Errorf("%s at 0x%06x: short answer (%d of %d bytes)", what, a, c, chunk)
		}
		out = append(out, d...)
	}
	return out, nil
}

// SWRead reads target memory and registers over SWire (peripherals at
// 0x800000.., SRAM at 0x840000..).
func (p *Programmer) SWRead(addr uint32, n int) ([]byte, error) {
	return p.readBlocks(cmdSWRead, addr, n, "SWire read")
}

// SWWrite writes target memory or registers over SWire, in blocks. There is
// no readback here; callers that need verification read back themselves.
func (p *Programmer) SWWrite(addr uint32, data []byte) error {
	for off := 0; off < len(data); {
		chunk := len(data) - off
		if chunk > MaxBlock {
			chunk = MaxBlock
		}
		a := (addr + uint32(off)) & 0xffffff
		c, _, err := p.transact(addr3(cmdSWWrite, a, data[off:off+chunk]...), 0, 0)
		if err != nil {
			return fmt.Errorf("SWire write at 0x%06x: %w", a, err)
		}
		if int(c) != chunk {
			return fmt.Errorf("SWire write at 0x%06x: the programmer accepted %d of %d bytes", a, c, chunk)
		}
		off += chunk
	}
	return nil
}

// JEDEC reads the flash JEDEC id (manufacturer, type, capacity code).
func (p *Programmer) JEDEC() (uint32, error) {
	c, d, err := p.transact(addr3(cmdFlashJEDEC, 0), 3, 0)
	if err != nil {
		return 0, err
	}
	if c != 3 {
		return 0, ErrBadResponse
	}
	return uint32(d[0])<<16 | uint32(d[1])<<8 | uint32(d[2]), nil
}

// FlashStatus reads the flash status register.
func (p *Programmer) FlashStatus() (byte, error) {
	c, d, err := p.transact(addr3(cmdFlashStat, 0), 1, 0)
	if err != nil {
		return 0, err
	}
	if c != 1 {
		return 0, ErrBadResponse
	}
	return d[0], nil
}

// WaitFlashReady polls the busy bit for up to tries reads.
func (p *Programmer) WaitFlashReady(tries int) error {
	var st byte
	for i := 0; i < tries; i++ {
		var err error
		if st, err = p.FlashStatus(); err != nil {
			return err
		}
		if st&1 == 0 {
			return nil
		}
	}
	return fmt.Errorf("flash still busy (status 0x%02x)", st)
}

// FlashRead reads flash through the programmer's flash path.
func (p *Programmer) FlashRead(addr uint32, n int) ([]byte, error) {
	return p.readBlocks(cmdFlashRead, addr, n, "flash read")
}

// wrrd sends one raw flash command and reads rd bytes back.
func (p *Programmer) wrrd(rd int, flashCmd byte, arg ...byte) ([]byte, error) {
	req := []byte{cmdFlashWrRd, 0, 0, 0, byte(rd), byte(rd >> 8), flashCmd}
	req = append(req, arg...)
	_, d, err := p.transact(req, rd, 0)
	return d, err
}

// FlashUIDBytes is the length of the flash unique id read by FlashUID.
const FlashUIDBytes = 8

// FlashUID reads the flash chip's unique id (SPI command 0x4B: four dummy
// bytes, then the id). It is a read-only operation like JEDEC.
func (p *Programmer) FlashUID() ([]byte, error) {
	d, err := p.wrrd(4+FlashUIDBytes, 0x4B)
	if err != nil {
		return nil, fmt.Errorf("flash unique id: %w", err)
	}
	if len(d) != 4+FlashUIDBytes {
		return nil, ErrBadResponse
	}
	return d[4:], nil
}

// UnlockFlash clears the flash block-protection bits (write enable, then
// write status register = 0) and checks they read back as zero. This is a
// persistent change to the flash chip, which is why callers must treat it as
// part of a write operation.
func (p *Programmer) UnlockFlash() error {
	if _, err := p.wrrd(0, 0x06); err != nil {
		return fmt.Errorf("flash write enable: %w", err)
	}
	if _, err := p.wrrd(0, 0x01, 0); err != nil {
		return fmt.Errorf("flash write status: %w", err)
	}
	if err := p.WaitFlashReady(300); err != nil {
		return err
	}
	st, err := p.FlashStatus()
	if err != nil {
		return err
	}
	if st != 0 {
		return fmt.Errorf("flash did not unlock (status 0x%02x)", st)
	}
	return nil
}

// EraseSector erases the 4 KiB sector containing addr and waits until the
// flash is ready. It performs no safety checks; see Layout.
func (p *Programmer) EraseSector(addr uint32) error {
	a := addr &^ (FlashSector - 1) & 0xffffff
	if _, _, err := p.transact(addr3(cmdSectErase, a), 0, 0); err != nil {
		return fmt.Errorf("erase sector 0x%06x: %w", a, err)
	}
	return p.WaitFlashReady(300)
}

// WriteFlashPage programs up to 256 bytes that must not cross a page boundary,
// then waits for ready and, on programmers with firmware 3 or newer, compares
// the flash CRC with the data.
func (p *Programmer) WriteFlashPage(addr uint32, data []byte) error {
	if len(data) == 0 || len(data) > FlashPage {
		return fmt.Errorf("flash page write of %d bytes (1..%d)", len(data), FlashPage)
	}
	if addr&(FlashPage-1)+uint32(len(data)) > FlashPage {
		return fmt.Errorf("flash write at 0x%06x crosses a 256-byte page", addr)
	}
	c, _, err := p.transact(addr3(cmdFlashWrite, addr&0xffffff, data...), 0, 0)
	if err != nil {
		return fmt.Errorf("flash write at 0x%06x: %w", addr, err)
	}
	if int(c) != len(data) {
		return fmt.Errorf("flash write at 0x%06x: the programmer accepted %d of %d bytes", addr, c, len(data))
	}
	if err := p.WaitFlashReady(300); err != nil {
		return err
	}
	if v, ok := p.Cached(); ok && v.Firmware >= 3 {
		got, err := p.FlashCRC(addr, len(data))
		if err != nil {
			return err
		}
		if want := CRC16(data); got != want {
			return fmt.Errorf("flash CRC mismatch at 0x%06x: programmed 0x%04x, read 0x%04x", addr, want, got)
		}
	}
	return nil
}

// FlashCRC asks the programmer for the CRC16 of a flash range (firmware 3+).
// The programmer returns it in the answer's count field.
func (p *Programmer) FlashCRC(addr uint32, n int) (uint16, error) {
	c, _, err := p.transact(addr3(cmdFlashCRC, addr&0xffffff, byte(n), byte(n>>8)), 0, 0)
	return c, err
}
