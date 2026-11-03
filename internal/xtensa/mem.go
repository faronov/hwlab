package xtensa

import (
	"context"
	"encoding/binary"
	"fmt"
)

// memChunk is how many words one checked memory transfer moves (each chunk
// re-loads the pointer and ends with a DSR check).
const memChunk = 1024

// ReadWords reads n consecutive 32-bit words from a word-aligned address
// with LDDR32.P: DDR = mem[a3]; a3 += 4, re-executed by every DDREXEC read.
// The core must be halted.
func (c *Core) ReadWords(ctx context.Context, addr uint32, n int) ([]uint32, error) {
	if err := c.needHalted(); err != nil {
		return nil, err
	}
	if addr%4 != 0 {
		return nil, fmt.Errorf("xtensa: unaligned word address %#x", addr)
	}
	if n < 0 || uint64(addr)+4*uint64(n) > 1<<32 {
		return nil, fmt.Errorf("xtensa: read of %d word(s) at %#x leaves the address space", n, addr)
	}
	out := make([]uint32, 0, n)
	for done := 0; done < n; {
		k := min(memChunk, n-done)
		a := addr + uint32(4*done)
		var q Queue
		q.Write(RegDDR, a)
		q.Exec(RSR(SRDDR, Scratch))
		q.Exec(LDDR32P(Scratch))
		for i := 0; i < k; i++ {
			if i == k-1 {
				q.Read(RegDDR)
			} else {
				q.Read(RegDDREXEC)
			}
		}
		r, err := c.check(ctx, &q, fmt.Sprintf("read %d word(s) at %#x", k, a))
		if err != nil {
			return nil, err
		}
		out = append(out, r[:k]...)
		done += k
	}
	return out, nil
}

// WriteWords writes consecutive 32-bit words to a word-aligned address with
// SDDR32.P: the first word goes through DDR + DIR0EXEC, the rest through
// DDREXEC writes that re-execute it. The core must be halted.
func (c *Core) WriteWords(ctx context.Context, addr uint32, vals []uint32) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if addr%4 != 0 {
		return fmt.Errorf("xtensa: unaligned word address %#x", addr)
	}
	if uint64(addr)+4*uint64(len(vals)) > 1<<32 {
		return fmt.Errorf("xtensa: write of %d word(s) at %#x leaves the address space", len(vals), addr)
	}
	for done := 0; done < len(vals); {
		k := min(memChunk, len(vals)-done)
		a := addr + uint32(4*done)
		var q Queue
		q.Write(RegDDR, a)
		q.Exec(RSR(SRDDR, Scratch))
		q.Write(RegDDR, vals[done])
		q.Exec(SDDR32P(Scratch))
		for _, v := range vals[done+1 : done+k] {
			q.Write(RegDDREXEC, v)
		}
		if _, err := c.check(ctx, &q, fmt.Sprintf("write %d word(s) at %#x", k, a)); err != nil {
			return err
		}
		done += k
	}
	return nil
}

// ReadMem reads n bytes at any alignment (whole words are read).
func (c *Core) ReadMem(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if n <= 0 {
		return []byte{}, nil
	}
	if uint64(addr)+uint64(n) > 1<<32 {
		return nil, fmt.Errorf("xtensa: read of %d byte(s) at %#x leaves the address space", n, addr)
	}
	start := addr &^ 3
	end := (uint64(addr) + uint64(n) + 3) &^ 3
	w, err := c.ReadWords(ctx, start, int((end-uint64(start))/4))
	if err != nil {
		return nil, err
	}
	b := make([]byte, 4*len(w))
	for i, v := range w {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	off := addr - start
	return b[off : off+uint32(n)], nil
}

// WriteMem writes data at any alignment. Memory is only ever accessed as
// aligned 32-bit words (instruction RAM on many Xtensa parts rejects narrow
// stores), so partial head/tail words are read, merged and written back.
func (c *Core) WriteMem(ctx context.Context, addr uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if uint64(addr)+uint64(len(data)) > 1<<32 {
		return fmt.Errorf("xtensa: write of %d byte(s) at %#x leaves the address space", len(data), addr)
	}
	start := addr &^ 3
	end := (uint64(addr) + uint64(len(data)) + 3) &^ 3
	nw := int((end - uint64(start)) / 4)
	buf := make([]byte, 4*nw)
	off := int(addr - start)
	if off != 0 {
		w, err := c.ReadWords(ctx, start, 1)
		if err != nil {
			return err
		}
		binary.LittleEndian.PutUint32(buf, w[0])
	}
	if tail := off + len(data); tail%4 != 0 && (nw > 1 || off == 0) {
		w, err := c.ReadWords(ctx, start+uint32(4*(nw-1)), 1)
		if err != nil {
			return err
		}
		binary.LittleEndian.PutUint32(buf[4*(nw-1):], w[0])
	}
	copy(buf[off:], data)
	vals := make([]uint32, nw)
	for i := range vals {
		vals[i] = binary.LittleEndian.Uint32(buf[4*i:])
	}
	return c.WriteWords(ctx, start, vals)
}

// MemOp is one access of a MemBatch: a read of Words words or a write of
// Vals, at a word-aligned address.
type MemOp struct {
	Write bool
	Addr  uint32
	Words int
	Vals  []uint32
}

// MemBatch runs word reads and writes in order as one queue: the JTAG
// accesses of all operations are pipelined (split only at DM.MaxOps) and the
// exec-exception check is done once, at the end. A fault anywhere fails the
// whole batch with an *ExecError; operations after the faulting one may
// already have executed, so a batch must only contain accesses that are safe
// to repeat or abandon (peripheral register sequences of a driver). It
// returns the read words per operation (nil for writes). The core must be
// halted.
func (c *Core) MemBatch(ctx context.Context, ops []MemOp) ([][]uint32, error) {
	if err := c.needHalted(); err != nil {
		return nil, err
	}
	var q Queue
	type span struct{ first, n int }
	spans := make([]span, len(ops))
	// ptr tracks the scratch pointer within the batch: an operation close
	// to where the previous one left it moves the pointer with one ADDI
	// instead of a DDR load plus RSR.
	ptr, known := uint32(0), false
	for i, op := range ops {
		n := op.Words
		if op.Write {
			n = len(op.Vals)
		}
		if op.Addr%4 != 0 {
			return nil, fmt.Errorf("xtensa: unaligned word address %#x", op.Addr)
		}
		if n < 0 || uint64(op.Addr)+4*uint64(n) > 1<<32 {
			return nil, fmt.Errorf("xtensa: %d word(s) at %#x leave the address space", n, op.Addr)
		}
		spans[i] = span{-1, 0}
		if n == 0 {
			continue
		}
		if d := int64(op.Addr) - int64(ptr); known && d >= -128 && d <= 127 {
			if d != 0 {
				q.Exec(ADDI(Scratch, Scratch, int8(d)))
			}
		} else {
			q.Write(RegDDR, op.Addr)
			q.Exec(RSR(SRDDR, Scratch))
		}
		ptr, known = op.Addr+4*uint32(n), true
		if op.Write {
			q.Write(RegDDR, op.Vals[0])
			q.Exec(SDDR32P(Scratch))
			for _, v := range op.Vals[1:] {
				q.Write(RegDDREXEC, v)
			}
			continue
		}
		q.Exec(LDDR32P(Scratch))
		first := -1
		for k := 0; k < n; k++ {
			reg := RegDDREXEC
			if k == n-1 {
				reg = RegDDR
			}
			if idx := q.Read(reg); first < 0 {
				first = idx
			}
		}
		spans[i] = span{first, n}
	}
	if q.Len() == 0 {
		return make([][]uint32, len(ops)), nil
	}
	what := fmt.Sprintf("memory batch of %d operation(s) starting at %#x", len(ops), ops[0].Addr)
	r, err := c.check(ctx, &q, what)
	if err != nil {
		return nil, err
	}
	out := make([][]uint32, len(ops))
	for i, sp := range spans {
		if sp.first >= 0 {
			out[i] = append([]uint32(nil), r[sp.first:sp.first+sp.n]...)
		}
	}
	return out, nil
}
