// Package mpsse builds and executes FTDI MPSSE command streams. It knows
// nothing about JTAG or any target: it turns clocking, shifting and GPIO
// requests into the documented opcode bytes (AN_108) and validates the
// responses it expects back.
//
// Provenance: opcodes and semantics come from FTDI AN_108 "Command Processor
// for MPSSE and MCU Host Bus Emulation Modes", public and not copyrightable as
// facts; the encoder is written from scratch.
package mpsse

import (
	"errors"
	"fmt"
)

// Opcodes.
const (
	OpSetLow        = 0x80
	OpGetLow        = 0x81
	OpSetHigh       = 0x82
	OpGetHigh       = 0x83
	OpLoopbackOn    = 0x84
	OpLoopbackOff   = 0x85
	OpSetDivisor    = 0x86
	OpSendImmediate = 0x87
	OpWaitIOHigh    = 0x88
	OpWaitIOLow     = 0x89
	OpDiv5Off       = 0x8A // H parts only
	OpDiv5On        = 0x8B // H parts only
	Op3PhaseOn      = 0x8C // H parts only
	Op3PhaseOff     = 0x8D // H parts only
	OpClockBits     = 0x8E // H parts only
	OpClockBytes    = 0x8F // H parts only
	OpAdaptiveOn    = 0x96 // H parts only
	OpAdaptiveOff   = 0x97 // H parts only
	OpBadCommand    = 0xAA // deliberately invalid: the device answers FA AA
	OpBadCommandAlt = 0xAB
	// ReplyBadCommand starts the two-byte answer to an unknown opcode.
	ReplyBadCommand = 0xFA
)

// Data-shifting opcode flag bits.
const (
	FlagWriteNeg = 0x01 // change TDI/data-out on the falling edge
	FlagBits     = 0x02 // length is in bits (1..8), not bytes
	FlagReadNeg  = 0x04 // sample data-in on the falling edge
	FlagLSB      = 0x08 // LSB first
	FlagWrite    = 0x10 // clock data out (TDI)
	FlagRead     = 0x20 // clock data in (TDO)
	FlagTMS      = 0x40 // the data are TMS values
)

// Mode is the edge/bit-order mode of a data command.
type Mode struct {
	// LSBFirst shifts the least significant bit first.
	LSBFirst bool
	// WriteNeg changes the data-out line on the falling clock edge.
	WriteNeg bool
	// ReadNeg samples data-in on the falling edge (default: rising).
	ReadNeg bool
}

// JTAGMode is the mode every JTAG shift uses: LSB first, data out changes on
// the falling edge, data in is sampled on the rising edge.
var JTAGMode = Mode{LSBFirst: true, WriteNeg: true}

func (m Mode) flags() byte {
	var f byte
	if m.LSBFirst {
		f |= FlagLSB
	}
	if m.WriteNeg {
		f |= FlagWriteNeg
	}
	if m.ReadNeg {
		f |= FlagReadNeg
	}
	return f
}

// MaxBytesPerCommand is the longest byte-length data command (length is
// encoded as n-1 in 16 bits).
const MaxBytesPerCommand = 65536

// Resp locates the reply bytes of one command inside the whole reply of a
// Builder.
type Resp struct {
	Off, Len int
}

// Slice returns the reply bytes r refers to.
func (r Resp) Slice(reply []byte) []byte {
	if r.Off < 0 || r.Off+r.Len > len(reply) {
		return nil
	}
	return reply[r.Off : r.Off+r.Len]
}

// span is one command of a Builder: its bytes and how many reply bytes it
// produces. Commands are never split across USB chunks by the engine, so a
// chunk boundary is always a command boundary.
type span struct {
	off, n, reply int
}

// Builder accumulates MPSSE commands and tracks the reply bytes they cause.
// It is not safe for concurrent use.
type Builder struct {
	buf   []byte
	spans []span
	reply int
	err   error
	// MaxCmdBytes bounds the data bytes of one shift command so the reply of
	// a single command stays below the device's receive buffer. Zero selects
	// DefaultMaxCmdBytes.
	MaxCmdBytes int
}

// DefaultMaxCmdBytes keeps one command's reply within 128 bytes, the receive
// FIFO of the FT2232C/D.
const DefaultMaxCmdBytes = 112

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{} }

// Bytes returns the encoded command stream.
func (b *Builder) Bytes() []byte { return b.buf }

// Reply returns the total number of reply bytes the stream will produce.
func (b *Builder) Reply() int { return b.reply }

// Err returns the first encoding error.
func (b *Builder) Err() error { return b.err }

// Len returns the number of commands.
func (b *Builder) Len() int { return len(b.spans) }

func (b *Builder) fail(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf(format, args...)
	}
}

func (b *Builder) cmd(reply int, bytes ...byte) Resp {
	b.spans = append(b.spans, span{off: len(b.buf), n: len(bytes), reply: reply})
	b.buf = append(b.buf, bytes...)
	r := Resp{Off: b.reply, Len: reply}
	b.reply += reply
	return r
}

func (b *Builder) maxCmd() int {
	if b.MaxCmdBytes > 0 {
		return min(b.MaxCmdBytes, MaxBytesPerCommand)
	}
	return DefaultMaxCmdBytes
}

// SetLow sets the low GPIO byte (ADBUS) values and directions (1 = output).
func (b *Builder) SetLow(value, dir byte) { b.cmd(0, OpSetLow, value, dir) }

// SetHigh sets the high GPIO byte (ACBUS) values and directions.
func (b *Builder) SetHigh(value, dir byte) { b.cmd(0, OpSetHigh, value, dir) }

// GetLow reads the low GPIO byte; the reply is one byte.
func (b *Builder) GetLow() Resp { return b.cmd(1, OpGetLow) }

// GetHigh reads the high GPIO byte; the reply is one byte.
func (b *Builder) GetHigh() Resp { return b.cmd(1, OpGetHigh) }

// Loopback connects the data-out line to the data-in line inside the chip.
func (b *Builder) Loopback(on bool) {
	if on {
		b.cmd(0, OpLoopbackOn)
	} else {
		b.cmd(0, OpLoopbackOff)
	}
}

// SetDivisor sets the clock divisor: TCK = base / ((1 + div) * 2).
func (b *Builder) SetDivisor(div uint16) { b.cmd(0, OpSetDivisor, byte(div), byte(div>>8)) }

// DivideBy5 enables (true, 12 MHz base) or disables (60 MHz base) the
// divide-by-five prescaler of the H parts.
func (b *Builder) DivideBy5(on bool) {
	if on {
		b.cmd(0, OpDiv5On)
	} else {
		b.cmd(0, OpDiv5Off)
	}
}

// ThreePhase enables or disables 3-phase data clocking (H parts).
func (b *Builder) ThreePhase(on bool) {
	if on {
		b.cmd(0, Op3PhaseOn)
	} else {
		b.cmd(0, Op3PhaseOff)
	}
}

// Adaptive enables or disables adaptive clocking (H parts).
func (b *Builder) Adaptive(on bool) {
	if on {
		b.cmd(0, OpAdaptiveOn)
	} else {
		b.cmd(0, OpAdaptiveOff)
	}
}

// ClockBits clocks n (1..8) cycles without data (H parts).
func (b *Builder) ClockBits(n int) {
	if n < 1 || n > 8 {
		b.fail("mpsse: ClockBits %d out of range 1..8", n)
		return
	}
	b.cmd(0, OpClockBits, byte(n-1))
}

// ClockBytes clocks n*8 (n in 1..65536) cycles without data (H parts).
func (b *Builder) ClockBytes(n int) {
	if n < 1 || n > 65536 {
		b.fail("mpsse: ClockBytes %d out of range 1..65536", n)
		return
	}
	n--
	b.cmd(0, OpClockBytes, byte(n), byte(n>>8))
}

// SendImmediate flushes the device's reply buffer to the host.
func (b *Builder) SendImmediate() { b.cmd(0, OpSendImmediate) }

// BadCommand appends a deliberately invalid opcode, which a conforming
// device answers with 0xFA followed by the opcode.
func (b *Builder) BadCommand() Resp { return b.cmd(2, OpBadCommand) }

// TMS clocks n (1..7) TMS values (bit 0 first) with TDI held at tdi. With
// read, TDO is sampled for every clock and returned in one byte whose valid
// bits are the top n (the first sample ends up at bit 8-n).
func (b *Builder) TMS(tms byte, n int, tdi, read bool) Resp {
	if n < 1 || n > 7 {
		b.fail("mpsse: TMS clock count %d out of range 1..7", n)
		return Resp{}
	}
	// TMS out on the falling edge, LSB first, bit mode: 0x4B writes only and
	// 0x6B also samples TDO on the rising edge.
	op := byte(FlagTMS | FlagLSB | FlagWriteNeg | FlagBits)
	reply := 0
	if read {
		op |= FlagRead
		reply = 1
	}
	data := tms & (1<<n - 1)
	if tdi {
		data |= 0x80
	}
	return b.cmd(reply, op, byte(n-1), data)
}

// ShiftBytes shifts data out LSB first (data changes on the falling edge). If
// read, the bytes sampled on the rising edge are returned, one per byte
// written. The returned Resps (one per underlying command) must be
// concatenated by the caller; use ReadAll to do so.
func (b *Builder) ShiftBytes(data []byte, read bool) []Resp {
	var out []Resp
	step := b.maxCmd()
	for off := 0; off < len(data); off += step {
		chunk := data[off:min(off+step, len(data))]
		op := byte(FlagWrite | FlagLSB | FlagWriteNeg) // 0x19
		reply := 0
		if read {
			op |= FlagRead // 0x39
			reply = len(chunk)
		}
		n := len(chunk) - 1
		buf := append([]byte{op, byte(n), byte(n >> 8)}, chunk...)
		out = append(out, b.cmd(reply, buf...))
	}
	return out
}

// ShiftBits shifts n (1..8) bits of value out LSB first. With read, the bits
// sampled are returned in one byte, first sample at bit 8-n (shifted in from
// the top).
func (b *Builder) ShiftBits(value byte, n int, read bool) Resp {
	if n < 1 || n > 8 {
		b.fail("mpsse: ShiftBits count %d out of range 1..8", n)
		return Resp{}
	}
	op := byte(FlagWrite | FlagLSB | FlagWriteNeg | FlagBits) // 0x1B
	reply := 0
	if read {
		op |= FlagRead // 0x3B
		reply = 1
	}
	return b.cmd(reply, op, byte(n-1), value)
}

// ReadAll concatenates the reply slices of rs.
func ReadAll(reply []byte, rs []Resp) []byte {
	var out []byte
	for _, r := range rs {
		out = append(out, r.Slice(reply)...)
	}
	return out
}

// Divisor computes the divisor for the requested clock and the clock that
// results. base is the effective master clock (after any divide-by-5).
func Divisor(base, hz uint32) (div uint16, actual uint32, err error) {
	if hz == 0 {
		return 0, 0, errors.New("mpsse: clock must be positive")
	}
	maxHz := base / 2
	if hz > maxHz {
		return 0, 0, fmt.Errorf("mpsse: %d Hz exceeds the %d Hz this master clock allows", hz, maxHz)
	}
	// TCK = base / ((1+div) * 2); round the divisor up so TCK never exceeds hz.
	d := (uint64(base) + uint64(2*hz) - 1) / uint64(2*hz)
	if d < 1 {
		d = 1
	}
	d--
	if d > 0xFFFF {
		return 0, 0, fmt.Errorf("mpsse: %d Hz is below the %d Hz minimum of this master clock", hz, base/(2*65536))
	}
	return uint16(d), uint32(uint64(base) / (2 * (d + 1))), nil
}
