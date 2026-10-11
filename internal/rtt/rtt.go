// Package rtt reads and writes SEGGER Real-Time Transfer ring buffers through
// plain target-memory access. It does not depend on any debug probe: it needs
// only a Memory (the adiv5/cortexm layer provides one), so the same code runs
// over every probe backend and needs no SEGGER software.
//
// The control block layout is the documented RTT one (a 16-byte identifier,
// two buffer counts and then 24-byte buffer descriptors, up buffers first).
// The host owns RdOff of an up buffer and WrOff of a down buffer; the target
// owns the other offset. The host never touches target-owned fields.
package rtt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Memory is word-addressable target memory. Reads and writes must not halt
// the core.
type Memory interface {
	Read(ctx context.Context, addr uint32, n int) ([]byte, error)
	Write(ctx context.Context, addr uint32, data []byte) error
}

// ID is the start of the control block identifier.
const ID = "SEGGER RTT"

const (
	headerSize = 24 // identifier (16) + up count + down count
	descSize   = 24
	maxBuffers = 32
	// MaxBufferSize bounds a plausible ring buffer; a larger value means a
	// false match or corruption.
	MaxBufferSize = 1 << 20
)

// Range is a window of target memory in which a control block may live.
type Range struct{ Start, Size uint32 }

func (r Range) contains(addr uint32, n uint32) bool {
	end := uint64(r.Start) + uint64(r.Size)
	return addr >= r.Start && uint64(addr)+uint64(n) <= end
}

// Errors. They wrap, so callers can use errors.Is.
var (
	ErrNotFound = errors.New("RTT control block not found")
	ErrCorrupt  = errors.New("RTT control block is corrupt or implausible")
)

// Buffer describes one ring buffer.
type Buffer struct {
	Index int
	Name  string
	// Desc is the target address of this buffer's descriptor.
	Desc    uint32
	Storage uint32
	Size    uint32
	Flags   uint32
}

// ControlBlock is a validated control block.
type ControlBlock struct {
	Addr uint32
	Up   []Buffer
	Down []Buffer
}

// Find scans ranges for the control block identifier and returns the address
// of the first candidate that validates. Reads use chunk-sized windows with
// an overlap, so an identifier that straddles two windows is found. An error
// from the memory (for instance a bus fault past the end of RAM) stops the
// scan and is reported with the address.
func Find(ctx context.Context, m Memory, ranges []Range, chunk int) (uint32, error) {
	if chunk < 64 || chunk%4 != 0 {
		chunk = 1024
	}
	var lastCorrupt error
	for _, r := range ranges {
		for off := uint32(0); off < r.Size; {
			n := uint32(chunk)
			if r.Size-off < n {
				n = r.Size - off
			}
			if n < 16 {
				break
			}
			addr := r.Start + off
			data, err := m.Read(ctx, addr, int(n))
			if err != nil {
				return 0, fmt.Errorf("scan for the RTT control block: read %d bytes at 0x%08x: %w", n, addr, err)
			}
			for i := 0; i+16 <= len(data); i += 4 {
				if !bytes.HasPrefix(data[i:], []byte(ID)) {
					continue
				}
				cand := addr + uint32(i)
				if _, err := Parse(ctx, m, cand, ranges); err != nil {
					lastCorrupt = err
					continue
				}
				return cand, nil
			}
			if off+n >= r.Size {
				break
			}
			off += n - 16 // keep an identifier that straddles two windows
		}
	}
	if lastCorrupt != nil {
		return 0, fmt.Errorf("%w (identifier found but rejected: %v)", ErrNotFound, lastCorrupt)
	}
	return 0, ErrNotFound
}

// Parse reads and validates the control block at addr. Buffer storage must lie
// inside one of allowed.
func Parse(ctx context.Context, m Memory, addr uint32, allowed []Range) (*ControlBlock, error) {
	hdr, err := m.Read(ctx, addr, headerSize)
	if err != nil {
		return nil, fmt.Errorf("read control block header at 0x%08x: %w", addr, err)
	}
	if len(hdr) != headerSize {
		return nil, fmt.Errorf("%w: short header read (%d bytes)", ErrCorrupt, len(hdr))
	}
	if !bytes.HasPrefix(hdr, []byte(ID)) {
		return nil, fmt.Errorf("%w: no identifier at 0x%08x", ErrNotFound, addr)
	}
	for _, b := range hdr[len(ID):16] {
		if b != 0 {
			return nil, fmt.Errorf("%w: identifier at 0x%08x is not zero padded", ErrCorrupt, addr)
		}
	}
	nUp := binary.LittleEndian.Uint32(hdr[16:])
	nDown := binary.LittleEndian.Uint32(hdr[20:])
	if nUp < 1 || nUp > maxBuffers || nDown > maxBuffers {
		return nil, fmt.Errorf("%w: %d up and %d down buffers", ErrCorrupt, nUp, nDown)
	}
	raw, err := m.Read(ctx, addr+headerSize, int(nUp+nDown)*descSize)
	if err != nil {
		return nil, fmt.Errorf("read buffer descriptors at 0x%08x: %w", addr+headerSize, err)
	}
	if len(raw) != int(nUp+nDown)*descSize {
		return nil, fmt.Errorf("%w: short descriptor read", ErrCorrupt)
	}
	cb := &ControlBlock{Addr: addr}
	for i := 0; i < int(nUp+nDown); i++ {
		d := raw[i*descSize:]
		b := Buffer{
			Desc:    addr + headerSize + uint32(i)*descSize,
			Storage: binary.LittleEndian.Uint32(d[4:]),
			Size:    binary.LittleEndian.Uint32(d[8:]),
			Flags:   binary.LittleEndian.Uint32(d[20:]),
		}
		wr, rd := binary.LittleEndian.Uint32(d[12:]), binary.LittleEndian.Uint32(d[16:])
		if b.Size > MaxBufferSize {
			return nil, fmt.Errorf("%w: buffer %d size %d", ErrCorrupt, i, b.Size)
		}
		if b.Size > 0 {
			if !inAny(allowed, b.Storage, b.Size) {
				return nil, fmt.Errorf("%w: buffer %d storage 0x%08x+%d is outside the searched memory", ErrCorrupt, i, b.Storage, b.Size)
			}
			if wr >= b.Size || rd >= b.Size {
				return nil, fmt.Errorf("%w: buffer %d offsets wr=%d rd=%d exceed size %d", ErrCorrupt, i, wr, rd, b.Size)
			}
		}
		b.Name = readName(ctx, m, binary.LittleEndian.Uint32(d[0:]), allowed)
		if i < int(nUp) {
			b.Index = i
			cb.Up = append(cb.Up, b)
		} else {
			b.Index = i - int(nUp)
			cb.Down = append(cb.Down, b)
		}
	}
	return cb, nil
}

func inAny(rs []Range, addr, n uint32) bool {
	for _, r := range rs {
		if r.contains(addr, n) {
			return true
		}
	}
	return false
}

// readName reads the buffer's name when it points into searched memory;
// names in flash are not read (their address is outside the RAM window).
func readName(ctx context.Context, m Memory, p uint32, allowed []Range) string {
	if p == 0 || !inAny(allowed, p, 1) {
		return ""
	}
	b, err := m.Read(ctx, p, 32)
	if err != nil {
		return ""
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return string(b)
}

// Channel reads up buffers and writes down buffers of one control block.
type Channel struct {
	m  Memory
	cb *ControlBlock
}

// New wraps a validated control block.
func New(m Memory, cb *ControlBlock) *Channel { return &Channel{m: m, cb: cb} }

func (c *Channel) offsets(ctx context.Context, b Buffer) (wr, rd uint32, err error) {
	d, err := c.m.Read(ctx, b.Desc+12, 8)
	if err != nil {
		return 0, 0, err
	}
	if len(d) != 8 {
		return 0, 0, fmt.Errorf("%w: short offset read", ErrCorrupt)
	}
	wr, rd = binary.LittleEndian.Uint32(d), binary.LittleEndian.Uint32(d[4:])
	if b.Size == 0 || wr >= b.Size || rd >= b.Size {
		return 0, 0, fmt.Errorf("%w: buffer offsets wr=%d rd=%d, size %d", ErrCorrupt, wr, rd, b.Size)
	}
	return wr, rd, nil
}

func (c *Channel) readExact(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	d, err := c.m.Read(ctx, addr, n)
	if err != nil {
		return nil, err
	}
	if len(d) != n {
		return nil, fmt.Errorf("%w: short data read (%d of %d bytes)", ErrCorrupt, len(d), n)
	}
	return d, nil
}

// ReadUp returns the bytes the target has written to up buffer ch since the
// last call and then advances the target's RdOff. RdOff is written only after
// the data was read completely, so a failed read loses nothing.
func (c *Channel) ReadUp(ctx context.Context, ch int) ([]byte, error) {
	if ch < 0 || ch >= len(c.cb.Up) {
		return nil, fmt.Errorf("up channel %d: the target has %d", ch, len(c.cb.Up))
	}
	b := c.cb.Up[ch]
	wr, rd, err := c.offsets(ctx, b)
	if err != nil {
		return nil, err
	}
	if wr == rd {
		return nil, nil
	}
	var out []byte
	if wr > rd {
		out, err = c.readExact(ctx, b.Storage+rd, int(wr-rd))
		if err != nil {
			return nil, err
		}
	} else { // wrapped
		first, err := c.readExact(ctx, b.Storage+rd, int(b.Size-rd))
		if err != nil {
			return nil, err
		}
		second, err := c.readExact(ctx, b.Storage, int(wr))
		if err != nil {
			return nil, err
		}
		out = append(first, second...)
	}
	var w [4]byte
	binary.LittleEndian.PutUint32(w[:], wr)
	if err := c.m.Write(ctx, b.Desc+16, w[:]); err != nil {
		return nil, fmt.Errorf("advance RdOff: %w", err)
	}
	return out, nil
}

// WriteDown copies as much of data as fits into down buffer ch and returns the
// count written. The data is written before WrOff, so the target never sees a
// partial update. It never overwrites unread data.
func (c *Channel) WriteDown(ctx context.Context, ch int, data []byte) (int, error) {
	if ch < 0 || ch >= len(c.cb.Down) {
		return 0, fmt.Errorf("down channel %d: the target has %d", ch, len(c.cb.Down))
	}
	b := c.cb.Down[ch]
	wr, rd, err := c.offsets(ctx, b)
	if err != nil {
		return 0, err
	}
	free := (rd + b.Size - wr - 1) % b.Size
	n := uint32(len(data))
	if n > free {
		n = free
	}
	if n == 0 {
		return 0, nil
	}
	first := n
	if wr+first > b.Size {
		first = b.Size - wr
	}
	if err := c.m.Write(ctx, b.Storage+wr, data[:first]); err != nil {
		return 0, err
	}
	if n > first {
		if err := c.m.Write(ctx, b.Storage, data[first:n]); err != nil {
			return 0, err
		}
	}
	var w [4]byte
	binary.LittleEndian.PutUint32(w[:], (wr+n)%b.Size)
	if err := c.m.Write(ctx, b.Desc+12, w[:]); err != nil {
		return 0, fmt.Errorf("advance WrOff: %w", err)
	}
	return int(n), nil
}

// Backoff is the polling schedule: fast while data flows, slower while idle.
type Backoff struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns the delay before the next poll; gotData resets it to Min.
func (b *Backoff) Next(gotData bool) time.Duration {
	if b.Min <= 0 {
		b.Min = 5 * time.Millisecond
	}
	if b.Max < b.Min {
		b.Max = 100 * time.Millisecond
	}
	if gotData || b.cur == 0 {
		b.cur = b.Min
		return b.cur
	}
	b.cur *= 2
	if b.cur > b.Max {
		b.cur = b.Max
	}
	return b.cur
}
