package cortexm

import (
	"context"
	"encoding/binary"
	"fmt"
)

// BlockBus is implemented by buses that move many bytes per probe command.
// Addresses and lengths follow the probe's own alignment rules; Memory
// handles the rest.
type BlockBus interface {
	// ReadBlock reads len(p) bytes at a word-aligned address; len(p) is a
	// multiple of 4.
	ReadBlock(ctx context.Context, addr uint32, p []byte) error
	// WriteBlock writes data at a word-aligned address; len(data) is a
	// multiple of 4.
	WriteBlock(ctx context.Context, addr uint32, data []byte) error
}

// ByteWriter is implemented by buses with a native 8-bit write (ST-Link
// WRITEMEM_8BIT). Without it, sub-word writes are done as read-modify-write
// of the containing word.
type ByteWriter interface {
	WriteBytes(ctx context.Context, addr uint32, data []byte) error
}

// Read reads size bytes of target memory at any alignment.
func (c *Core) Read(ctx context.Context, addr uint32, size int) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("negative length %d", size)
	}
	if size == 0 {
		return []byte{}, nil
	}
	if uint64(addr)+uint64(size) > 1<<32 {
		return nil, fmt.Errorf("read of %d bytes at 0x%08x wraps the 32-bit address space", size, addr)
	}
	start := addr &^ 3
	end := (uint64(addr) + uint64(size) + 3) &^ 3
	buf := make([]byte, end-uint64(start))
	if bb, ok := c.Bus.(BlockBus); ok {
		if err := bb.ReadBlock(ctx, start, buf); err != nil {
			return nil, err
		}
	} else {
		for i := 0; i < len(buf); i += 4 {
			v, err := c.Bus.Read32(ctx, start+uint32(i))
			if err != nil {
				return nil, err
			}
			binary.LittleEndian.PutUint32(buf[i:], v)
		}
	}
	off := int(addr - start)
	return buf[off : off+size], nil
}

// Write writes data at any alignment. Partial words are written with an
// explicit read-modify-write (or the probe's 8-bit write) so neighbouring
// bytes are preserved. The caller is responsible for the address policy.
func (c *Core) Write(ctx context.Context, addr uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if uint64(addr)+uint64(len(data)) > 1<<32 {
		return fmt.Errorf("write of %d bytes at 0x%08x wraps the 32-bit address space", len(data), addr)
	}
	// head: bytes up to the first word boundary
	if head := int(addr & 3); head != 0 {
		n := 4 - head
		if n > len(data) {
			n = len(data)
		}
		if err := c.writePartial(ctx, addr, data[:n]); err != nil {
			return err
		}
		addr += uint32(n)
		data = data[n:]
	}
	if whole := len(data) &^ 3; whole > 0 {
		if err := c.writeWords(ctx, addr, data[:whole]); err != nil {
			return err
		}
		addr += uint32(whole)
		data = data[whole:]
	}
	if len(data) > 0 {
		return c.writePartial(ctx, addr, data)
	}
	return nil
}

func (c *Core) writeWords(ctx context.Context, addr uint32, data []byte) error {
	if bb, ok := c.Bus.(BlockBus); ok {
		return bb.WriteBlock(ctx, addr, data)
	}
	for i := 0; i < len(data); i += 4 {
		if err := c.Bus.Write32(ctx, addr+uint32(i), binary.LittleEndian.Uint32(data[i:])); err != nil {
			return err
		}
	}
	return nil
}

// writePartial writes 1..3 bytes that lie inside one word.
func (c *Core) writePartial(ctx context.Context, addr uint32, data []byte) error {
	if bw, ok := c.Bus.(ByteWriter); ok {
		return bw.WriteBytes(ctx, addr, data)
	}
	word := addr &^ 3
	v, err := c.Bus.Read32(ctx, word)
	if err != nil {
		return err
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	copy(b[addr-word:], data)
	return c.Bus.Write32(ctx, word, binary.LittleEndian.Uint32(b[:]))
}

// WriteVerified writes and reads back; any difference is an error.
func (c *Core) WriteVerified(ctx context.Context, addr uint32, data []byte) error {
	if err := c.Write(ctx, addr, data); err != nil {
		return err
	}
	got, err := c.Read(ctx, addr, len(data))
	if err != nil {
		return err
	}
	for i := range data {
		if got[i] != data[i] {
			return fmt.Errorf("%w at 0x%08x: wrote 0x%02x, read 0x%02x", ErrVerify, addr+uint32(i), data[i], got[i])
		}
	}
	return nil
}
