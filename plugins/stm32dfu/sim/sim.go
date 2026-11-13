// Package sim is a SYNTHETIC STM32WB55 ROM bootloader in USB DFU mode
// (DfuSe, 0483:df11). It implements usb.Port so the plugin and the dfu
// client run unchanged against it. It models the DFU state machine
// (including dfuDNBUSY and dfuMANIFEST), wTransferSize, the alternate
// setting strings, the STM32WB option bytes with complements, the secure
// flash area, the double-word programming rule, SRAM downloads, the SRAM2a
// FUS tables, the AN3156 error codes (errTARGET, errVENDOR) and the jump
// on leave, and it counts every command a safe host must never send.
package sim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// Memory map of the simulated STM32WB55CG.
const (
	FlashBase   = 0x08000000
	FlashKB     = 1024
	PageSize    = 4096
	OTPBase     = 0x1FFF7000
	OTPSize     = 0x1000 // OTP + engineering bytes (flash size at 0x1FFF75E0, UID64 at 0x1FFF7580)
	OBBase      = 0x1FFF8000
	OBSize      = 0x80
	SRAMBase    = 0x20000000
	SRAMSize    = 0x40000
	BootRAMEnd  = 0x20004000 // the bootloader's own RAM: a download below is unsafe
	SRAM1End    = 0x20030000 // SRAM2a (FUS tables) starts here: a download at or above is unsafe
	BCD         = 0x2200     // synthetic bcdDevice: bootloader version 2.2 (AN3156: BCD in the MSB)
	DBGMCUAddr  = 0xE0042000
	DefaultXfer = 1024
)

// FUS table variants.
const (
	FUS053 = 0x00050300
	FUS120 = 0x01020000
)

// Config shapes one simulated device.
type Config struct {
	Serial string
	SFSA   uint8
	// FlashAltPages is the page count the flash alt string reports (default
	// 256, the whole flash).
	FlashAltPages int
	// Xfer is wTransferSize (default 1024).
	Xfer int
	// FUSVersion selects the SRAM2a table (0: empty SRAM2a).
	FUSVersion uint32
	// NoDBGMCU makes reads of DBGMCU_IDCODE fail.
	NoDBGMCU bool
	// BadOBComplement corrupts the complement of FLASH_SFR.
	BadOBComplement bool
	// NoOBAlt drops the option-bytes alternate setting.
	NoOBAlt bool
	// RDPActive makes memory uploads, downloads and erases fail with
	// errVENDOR (read protection active).
	RDPActive bool
	// NoRAMUpload makes uploads from SRAM fail with errTARGET.
	NoRAMUpload bool
	// NoReadUnprotect drops 0x92 from the command list (as on STM32H5).
	NoReadUnprotect bool
}

// Device is the simulated bootloader.
type Device struct {
	mu  sync.Mutex
	cfg Config

	flash []byte
	ob    []byte
	otp   []byte
	sram  []byte
	alts  []string

	state     uint8
	status    uint8
	ptr       uint32
	alt       uint8
	pending   []byte
	pendBlock uint16
	busyPolls int
	gone      bool

	// Counters of what the host made the device do. RAMWrites counts SRAM
	// data downloads.
	Erases, Programs, MassErases, Unprotects, OBWrites, Leaves, BadDownloads, RAMWrites int
	// JumpBase, JumpSP and JumpPC record the vector table the bootloader
	// jumped to on leave.
	JumpBase, JumpSP, JumpPC uint32
	// LeaveDrop makes the device drop off USB instead of answering the
	// manifestation GETSTATUS; LeaveState (non-zero) makes it answer that
	// state and stay in DFU.
	LeaveDrop  bool
	LeaveState uint8
	// Log records every class request.
	Log []string
	// FailUploadAt makes an UPLOAD covering this address fail (0: never).
	FailUploadAt uint32
	// FailProgram makes the next data download fail with errPROG.
	FailProgram bool
	// ExtraBusy adds dfuDNBUSY polls to every command.
	ExtraBusy int
	// CorruptProgram stores data with one bit flipped.
	CorruptProgram bool
}

// New builds a device. Flash below SFSA holds a pattern with erased pages;
// the secure area holds a different pattern and cannot be read.
func New(c Config) *Device {
	if c.Serial == "" {
		c.Serial = "SIMDFU0001"
	}
	if c.Xfer == 0 {
		c.Xfer = DefaultXfer
	}
	if c.FlashAltPages == 0 {
		c.FlashAltPages = FlashKB * 1024 / PageSize
	}
	d := &Device{cfg: c, state: 2,
		flash: make([]byte, FlashKB*1024), ob: make([]byte, OBSize), otp: make([]byte, OTPSize), sram: make([]byte, SRAMSize)}
	for i := range d.flash {
		page := i / PageSize
		switch {
		case page >= int(c.SFSA):
			d.flash[i] = byte(0xA5 ^ i)
		case page%3 == 2:
			d.flash[i] = 0xFF
		default:
			d.flash[i] = byte(i*7 + page)
		}
	}
	for i := range d.otp {
		d.otp[i] = 0xFF
	}
	binary.LittleEndian.PutUint16(d.otp[0x5E0:], FlashKB)
	binary.LittleEndian.PutUint64(d.otp[0x580:], 0x0080E12500001234)
	ob := func(off int, v uint32) {
		binary.LittleEndian.PutUint32(d.ob[off:], v)
		binary.LittleEndian.PutUint32(d.ob[off+4:], ^v)
	}
	for off := 0; off < OBSize; off += 8 {
		ob(off, 0xFFFFFFFF)
	}
	ob(0x00, 0x3DFFF1AA)                // OPTR, RDP level 0
	ob(0x08, 0x000001FF)                // PCROP1ASR
	ob(0x10, 0x00000000)                // PCROP1AER
	ob(0x18, 0x000000FF)                // WRP1AR (start > end: none)
	ob(0x20, 0x000000FF)                // WRP1BR
	ob(0x28, 0x000001FF)                // PCROP1BSR
	ob(0x30, 0x00000000)                // PCROP1BER
	ob(0x68, 0x00000000)                // IPCCBR
	ob(0x70, 0x00001000|uint32(c.SFSA)) // SFR: SFSA, FSD=0, DDS=1
	ob(0x78, 0x1F<<18|0x10)             // SRRVR
	if c.BadOBComplement {
		d.ob[0x74] ^= 0x01
	}
	if c.FUSVersion != 0 {
		w := func(a, v uint32) { binary.LittleEndian.PutUint32(d.sram[a-SRAMBase:], v) }
		w(0x20030000, 0x20030024)
		w(0x20030024, 0xA94656B9) // FUS table key
		w(0x20030028, 0)
		w(0x2003002C, 0)
		w(0x20030030, c.FUSVersion)
		w(0x20030034, 0x00000006)
		w(0x20030038, 0x01000000) // stack 1.0.0
		w(0x2003003C, 0x00000020)
		binary.LittleEndian.PutUint64(d.sram[0x20030024+40-SRAMBase:], 0x0080E12500001234)
		binary.LittleEndian.PutUint16(d.sram[0x20030024+48-SRAMBase:], 0x495)
	}
	d.alts = []string{fmt.Sprintf("@Internal Flash  /0x08000000/%d*04Kg", c.FlashAltPages)}
	if !c.NoOBAlt {
		d.alts = append(d.alts, "@Option Bytes  /0x1FFF8000/01*128 e")
	}
	d.alts = append(d.alts, "@OTP Memory /0x1FFF7000/01*01Ke")
	return d
}

// Serial is the USB serial.
func (d *Device) Serial() string { return d.cfg.Serial }

// Flash returns a copy of the flash.
func (d *Device) Flash() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.flash...)
}

// SetFlash changes flash behind the host's back (tests).
func (d *Device) SetFlash(addr uint32, b []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	copy(d.flash[addr-FlashBase:], b)
}

// SRAM returns a copy of n bytes of SRAM at addr.
func (d *Device) SRAM(addr uint32, n int) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.sram[addr-SRAMBase:addr-SRAMBase+uint32(n)]...)
}

// SetSRAM changes SRAM behind the host's back (tests).
func (d *Device) SetSRAM(addr uint32, b []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	copy(d.sram[addr-SRAMBase:], b)
}

// OptionBytes returns a copy of the option bytes.
func (d *Device) OptionBytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.ob...)
}

// Gone reports whether the device left DFU.
func (d *Device) Gone() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gone
}

// UnsafeCount sums the commands a safe host never sends.
func (d *Device) UnsafeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.MassErases + d.Unprotects + d.OBWrites + d.BadDownloads
}

// USBDevice is the descriptor the device presents to discovery.
func (d *Device) USBDevice() usb.Device {
	return usb.Device{VID: 0x0483, PID: 0xdf11, BCD: BCD, Manufacturer: "STMicroelectronics", Product: "DFU in FS Mode",
		Serial: d.cfg.Serial, SerialRaw: d.cfg.Serial, SysName: "sim-" + d.cfg.Serial, Configuration: 1, SpeedMbps: 12,
		Interfaces: []usb.Interface{{Number: 0, Class: 0xfe, Name: d.alts[0]}}}
}

// Port returns a port to the device.
func (d *Device) Port() usb.Port { return &port{d: d} }

type port struct {
	d      *Device
	closed bool
}

var errGone = fmt.Errorf("sim: %w", usb.ErrDisconnected)

func (p *port) Describe() string       { return "sim stm32 dfu" }
func (p *port) DetachedDriver() string { return "" }
func (p *port) Close() error           { p.closed = true; return nil }
func (p *port) BulkOut(context.Context, uint8, []byte, time.Duration) (int, error) {
	return 0, errors.New("sim: no bulk endpoints")
}
func (p *port) BulkIn(context.Context, uint8, int, time.Duration) ([]byte, error) {
	return nil, errors.New("sim: no bulk endpoints")
}

func (p *port) SetAltSetting(_ context.Context, alt uint8) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone {
		return errGone
	}
	if int(alt) >= len(d.alts) {
		return usb.ErrStall
	}
	d.alt = alt
	d.Log = append(d.Log, fmt.Sprintf("SETALT %d", alt))
	return nil
}

func strDesc(s string) []byte {
	b := []byte{0, 3}
	for _, r := range s {
		b = binary.LittleEndian.AppendUint16(b, uint16(r))
	}
	b[0] = byte(len(b))
	return b
}

func (d *Device) configDesc() []byte {
	var b []byte
	b = append(b, 9, 2, 0, 0, 1, 1, 0, 0xC0, 0x32)
	for i := range d.alts {
		b = append(b, 9, 4, 0, byte(i), 0, 0xFE, 0x01, 0x02, byte(4+i))
	}
	b = append(b, 9, 0x21, 0x0B, 0xFF, 0x00, byte(d.cfg.Xfer), byte(d.cfg.Xfer>>8), 0x1A, 0x01)
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	return b
}

func (p *port) ControlIn(_ context.Context, rt, req uint8, value, index uint16, length int) ([]byte, error) {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone || p.closed {
		return nil, errGone
	}
	clip := func(b []byte) []byte { return b[:min(len(b), length)] }
	if rt == 0x80 && req == 6 {
		switch value >> 8 {
		case 1:
			b := []byte{18, 1, 0x00, 0x01, 0, 0, 0, 64, 0x83, 0x04, 0x11, 0xdf, BCD & 0xFF, BCD >> 8, 1, 2, 3, 1}
			return clip(b), nil
		case 2:
			return clip(d.configDesc()), nil
		case 3:
			idx := int(value & 0xFF)
			var s string
			switch {
			case idx == 0:
				return clip([]byte{4, 3, 0x09, 0x04}), nil
			case idx == 1:
				s = "STMicroelectronics"
			case idx == 2:
				s = "DFU in FS Mode"
			case idx == 3:
				s = d.cfg.Serial
			case idx >= 4 && idx-4 < len(d.alts):
				s = d.alts[idx-4]
			default:
				return nil, usb.ErrStall
			}
			return clip(strDesc(s)), nil
		}
		return nil, usb.ErrStall
	}
	if rt != 0xA1 || index != 0 {
		return nil, usb.ErrStall
	}
	switch req {
	case 3: // GETSTATUS
		d.Log = append(d.Log, "GETSTATUS")
		poll := uint32(0)
		switch d.state {
		case 3: // DNLOAD-SYNC: start the command
			d.state, d.busyPolls = 4, d.ExtraBusy
			poll = 5
			if len(d.pending) == 5 && d.pending[0] == 0x41 && d.pendBlock == 0 {
				poll = 25
			}
		case 4:
			if d.busyPolls > 0 {
				d.busyPolls--
				poll = 5
			} else {
				d.execute()
			}
		case 6: // MANIFEST-SYNC: the leave runs now
			switch {
			case d.LeaveDrop:
				d.gone = true
				d.Leaves++
				d.jump()
				return nil, errGone
			case d.LeaveState != 0:
				d.state = 2
				return []byte{0, 0, 0, 0, d.LeaveState, 0}, nil
			}
			d.state = 7
			d.Leaves++
			d.jump()
			b := []byte{d.status, 10, 0, 0, d.state, 0}
			d.gone = true
			return b, nil
		}
		return []byte{d.status, byte(poll), byte(poll >> 8), byte(poll >> 16), d.state, 0}, nil
	case 5: // GETSTATE
		return []byte{d.state}, nil
	case 2: // UPLOAD
		d.Log = append(d.Log, fmt.Sprintf("UPLOAD %d %d", value, length))
		if d.state != 2 && d.state != 9 {
			d.fail(0x0F) // errSTALLEDPKT
			return nil, usb.ErrStall
		}
		if value == 0 {
			d.state = 9
			if d.cfg.NoReadUnprotect {
				return clip([]byte{0x00, 0x21, 0x41}), nil
			}
			return clip([]byte{0x00, 0x21, 0x41, 0x92}), nil
		}
		// AN3156: 2..2048 bytes; blocks above 2 only with full transfers
		// (where both readings of its address formula agree).
		if value == 1 || length > d.cfg.Xfer || length < 2 || length > 2048 || (value > 2 && length != d.cfg.Xfer) {
			d.fail(0x0F)
			return nil, usb.ErrStall
		}
		if d.cfg.RDPActive {
			d.fail(0x0B) // errVENDOR
			return nil, usb.ErrStall
		}
		addr := uint64(d.ptr) + uint64(value-2)*uint64(d.cfg.Xfer)
		b, ok := d.read(addr, length)
		if d.cfg.NoRAMUpload && addr < SRAMBase+SRAMSize && addr+uint64(length) > SRAMBase {
			ok = false
		}
		if !ok || (d.FailUploadAt != 0 && uint64(d.FailUploadAt) >= addr && uint64(d.FailUploadAt) < addr+uint64(length)) {
			d.fail(0x01) // errTARGET
			return nil, usb.ErrStall
		}
		d.state = 9
		return b, nil
	}
	return nil, usb.ErrStall
}

func (p *port) ControlOut(_ context.Context, rt, req uint8, value, index uint16, data []byte) error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone || p.closed {
		return errGone
	}
	if rt == 0x01 && req == 0x0B {
		if int(value) >= len(d.alts) {
			return usb.ErrStall
		}
		d.alt = uint8(value)
		return nil
	}
	if rt != 0x21 || index != 0 {
		return usb.ErrStall
	}
	switch req {
	case 1: // DNLOAD
		d.Log = append(d.Log, fmt.Sprintf("DNLOAD %d %x", value, data))
		if d.state != 2 && d.state != 5 {
			d.fail(0x0F)
			return usb.ErrStall
		}
		if len(data) == 0 {
			d.state = 6
			return nil
		}
		if len(data) > d.cfg.Xfer || value >= 2 && (len(data) < 2 || len(data) > 2048) {
			d.fail(0x0F)
			return usb.ErrStall
		}
		d.pending, d.pendBlock, d.state = append([]byte(nil), data...), value, 3
		return nil
	case 4: // CLRSTATUS
		d.Log = append(d.Log, "CLRSTATUS")
		if d.state == 10 {
			d.state, d.status = 2, 0
		}
		return nil
	case 6: // ABORT
		d.Log = append(d.Log, "ABORT")
		if d.state == 2 || d.state == 5 || d.state == 9 {
			d.state = 2
		}
		return nil
	}
	return usb.ErrStall
}

func (d *Device) fail(code uint8) { d.state, d.status = 10, code }

func (d *Device) secureBase() uint64 { return FlashBase + uint64(d.cfg.SFSA)*PageSize }

// read returns memory at addr; the secure flash area and unmapped
// addresses fail.
func (d *Device) read(addr uint64, n int) ([]byte, bool) {
	end := addr + uint64(n)
	in := func(base uint64, mem []byte) ([]byte, bool) {
		if addr >= base && end <= base+uint64(len(mem)) {
			return append([]byte(nil), mem[addr-base:end-base]...), true
		}
		return nil, false
	}
	if addr >= FlashBase && end <= d.secureBase() {
		return in(FlashBase, d.flash)
	}
	for _, r := range []struct {
		base uint64
		mem  []byte
	}{{OBBase, d.ob}, {OTPBase, d.otp}, {SRAMBase, d.sram}} {
		if b, ok := in(r.base, r.mem); ok {
			return b, true
		}
	}
	if addr == DBGMCUAddr && n == 4 && !d.cfg.NoDBGMCU {
		return []byte{0x95, 0x64, 0x01, 0x20}, true // dev 0x495, rev 0x2001
	}
	return nil, false
}

// execute runs the pending download at the end of dfuDNBUSY.
func (d *Device) execute() {
	data, blk := d.pending, d.pendBlock
	d.pending = nil
	d.state = 5
	if blk == 0 {
		switch {
		case len(data) == 5 && data[0] == 0x21:
			d.ptr = binary.LittleEndian.Uint32(data[1:])
		case len(data) == 5 && data[0] == 0x41:
			a := uint64(binary.LittleEndian.Uint32(data[1:]))
			if d.cfg.RDPActive {
				d.fail(0x0B)
				return
			}
			if a < FlashBase || a >= d.secureBase() {
				d.BadDownloads++
				d.fail(0x01)
				return
			}
			pg := (a - FlashBase) / PageSize * PageSize
			for i := pg; i < pg+PageSize; i++ {
				d.flash[i] = 0xFF
			}
			d.Erases++
		case len(data) == 1 && data[0] == 0x41:
			d.MassErases++
			for i := uint64(0); i < d.secureBase()-FlashBase; i++ {
				d.flash[i] = 0xFF
			}
		case len(data) == 1 && data[0] == 0x92:
			d.Unprotects++
		default:
			d.BadDownloads++
			d.fail(0x0B)
		}
		return
	}
	if blk != 2 {
		// Block 1 is reserved; above 2 the AN3156 address formula is
		// ambiguous and a safe host never relies on it.
		d.BadDownloads++
		d.fail(0x0F)
		return
	}
	addr := uint64(d.ptr)
	end := addr + uint64(len(data))
	if strings.Contains(d.alts[d.alt], "Option") || addr < OBBase+OBSize && end > OBBase {
		d.OBWrites++
		d.fail(0x03)
		return
	}
	if d.cfg.RDPActive {
		d.fail(0x0B)
		return
	}
	if addr < SRAMBase+SRAMSize && end > SRAMBase {
		if addr < BootRAMEnd || end > SRAM1End || !strings.Contains(d.alts[d.alt], "Flash") {
			d.BadDownloads++
			d.fail(0x01)
			return
		}
		copy(d.sram[addr-SRAMBase:], data)
		d.ptr = uint32(addr) // AN3156 5.5: the pointer of the last download is kept
		if d.CorruptProgram {
			d.CorruptProgram = false
			d.sram[addr-SRAMBase] ^= 0x01
		}
		d.RAMWrites++
		return
	}
	if addr < FlashBase || end > d.secureBase() || len(data)%8 != 0 || addr%8 != 0 || !strings.Contains(d.alts[d.alt], "Flash") {
		d.BadDownloads++
		d.fail(0x01)
		return
	}
	if d.FailProgram {
		d.FailProgram = false
		d.fail(0x06)
		return
	}
	off := addr - FlashBase
	for i := 0; i < len(data); i += 8 {
		cur := binary.LittleEndian.Uint64(d.flash[off+uint64(i):])
		want := binary.LittleEndian.Uint64(data[i:])
		if cur != ^uint64(0) && want != 0 {
			d.fail(0x06) // errPROG: double word not erased
			return
		}
	}
	copy(d.flash[off:], data)
	d.ptr = uint32(addr)
	if d.CorruptProgram {
		d.CorruptProgram = false
		d.flash[off] ^= 0x01
	}
	d.Programs++
}

// jump records the vector table at the address pointer (AN3156: the MSP
// from ptr, the reset handler from ptr+4).
func (d *Device) jump() {
	d.JumpBase = d.ptr
	if b, ok := d.read(uint64(d.ptr), 8); ok {
		d.JumpSP, d.JumpPC = binary.LittleEndian.Uint32(b), binary.LittleEndian.Uint32(b[4:])
	}
}
