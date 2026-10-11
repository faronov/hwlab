package riscv

import (
	"context"
	"errors"
	"fmt"
)

// Errors for memory access.
var (
	ErrAlignment   = errors.New("riscv: misaligned access")
	ErrWidth       = errors.New("riscv: access width not supported by the system bus")
	ErrRangeAccess = errors.New("riscv: address range wraps the 32-bit address space")
)

// sbcs bits.
const (
	sbBusyError  = 1 << 22
	sbBusy       = 1 << 21
	sbReadOnAddr = 1 << 20
	sbAutoIncr   = 1 << 16
	sbReadOnData = 1 << 15
	sbClearErr   = 7<<12 | sbBusyError
)

// SBAInfo is what sbcs says about System Bus Access.
type SBAInfo struct {
	Version  int  `json:"version"`
	ASize    int  `json:"address_bits"`
	Access8  bool `json:"access8"`
	Access16 bool `json:"access16"`
	Access32 bool `json:"access32"`
	Access64 bool `json:"access64"`
}

// Supports reports whether an access of w bytes (1, 2, 4) is supported.
func (s SBAInfo) Supports(w int) bool {
	if s.ASize == 0 {
		return false
	}
	switch w {
	case 1:
		return s.Access8
	case 2:
		return s.Access16
	case 4:
		return s.Access32
	}
	return false
}

func decodeSBA(v uint32) SBAInfo {
	return SBAInfo{Version: int(v >> 29), ASize: int(v >> 5 & 0x7F), Access8: v&1 == 1, Access16: v>>1&1 == 1,
		Access32: v>>2&1 == 1, Access64: v>>3&1 == 1}
}

// SBA reads the System Bus Access capabilities (cached after the first call).
func (m *DM) SBA(ctx context.Context) (SBAInfo, error) {
	if m.sbaOK {
		return m.sba, nil
	}
	if err := m.Activate(ctx); err != nil {
		return SBAInfo{}, err
	}
	v, err := m.dtm.Read(ctx, regSBCS)
	if err != nil {
		return SBAInfo{}, err
	}
	m.sba, m.sbaOK = decodeSBA(v), true
	return m.sba, nil
}

func sbSizeCode(w int) uint32 {
	switch w {
	case 1:
		return 0
	case 2:
		return 1
	}
	return 2
}

func checkRange(addr uint32, length int) error {
	if length < 0 {
		return fmt.Errorf("riscv: negative length %d", length)
	}
	if uint64(addr)+uint64(length) > 1<<32 {
		return ErrRangeAccess
	}
	return nil
}

// maxSBAChunk bounds one pipelined System Bus Access batch in 32-bit words.
const maxSBAChunk = 1024

// ReadMem reads length bytes from addr through System Bus Access, 32 bits at a
// time with address auto-increment, pipelined. It works while the hart runs and
// writes nothing to the target. Unaligned ranges are widened to whole words and
// trimmed.
func (m *DM) ReadMem(ctx context.Context, addr uint32, length int) ([]byte, error) {
	if length < 0 || length > 1<<26 {
		return nil, fmt.Errorf("riscv: read length %d out of range (0..%d)", length, 1<<26)
	}
	if length == 0 {
		return nil, nil
	}
	if err := checkRange(addr, length); err != nil {
		return nil, err
	}
	sba, err := m.SBA(ctx)
	if err != nil {
		return nil, err
	}
	if !sba.Supports(4) {
		return nil, ErrNoSBA
	}
	start := addr &^ 3
	words := int((uint64(addr)+uint64(length)+3)/4 - uint64(start)/4)
	buf := make([]byte, 0, words*4)
	for done := 0; done < words; {
		n := min(words-done, maxSBAChunk)
		chunk, err := m.sbaReadWords(ctx, start+uint32(done)*4, n)
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		done += n
	}
	off := int(addr - start)
	return buf[off : off+length], nil
}

var errSBABusy = fmt.Errorf("%w: bus busy", ErrBusAccess)

func (m *DM) sbaReadWords(ctx context.Context, start uint32, words int) ([]byte, error) {
	var lastErr error
	for try := 0; try < 5; try++ {
		out, err := m.sbaReadOnce(ctx, start, words)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !errors.Is(err, errSBABusy) {
			return nil, err
		}
		m.dtm.idle = min(m.dtm.idle+4, 64)
	}
	return nil, lastErr
}

func (m *DM) sbaReadOnce(ctx context.Context, start uint32, words int) ([]byte, error) {
	cfg := uint32(sbReadOnAddr | 2<<17 | sbAutoIncr | sbClearErr)
	if words > 1 {
		cfg |= sbReadOnData
	}
	ops := []DMIOp{
		{Write: true, Addr: regSBCS, Data: cfg},
		{Write: true, Addr: regSBAddress0, Data: start},
	}
	for i := 0; i < words; i++ {
		if i == words-1 && words > 1 {
			// Stop the read-on-data chain so the last data read does not start
			// one more access beyond the requested range.
			ops = append(ops, DMIOp{Write: true, Addr: regSBCS, Data: 2<<17 | sbAutoIncr})
		}
		ops = append(ops, DMIOp{Addr: regSBData0})
	}
	ops = append(ops, DMIOp{Addr: regSBCS})
	res, err := m.dtm.Pipeline(ctx, ops)
	if err != nil {
		return nil, err
	}
	if err := m.sbaCheck(ctx, res[len(res)-1], "reading", start); err != nil {
		return nil, err
	}
	out := make([]byte, 0, words*4)
	k := 2
	for i := 0; i < words; i++ {
		if i == words-1 && words > 1 {
			k++
		}
		v := res[k]
		k++
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return out, nil
}

// sbaCheck decodes a final sbcs read and clears any error it reports.
func (m *DM) sbaCheck(ctx context.Context, sbcs uint32, what string, addr uint32) error {
	if e := sbcs >> 12 & 7; e != 0 {
		_ = m.dtm.Write(ctx, regSBCS, 7<<12|sbBusyError)
		return fmt.Errorf("%w: sberror %d (%s) %s %#x", ErrBusAccess, e, sbErrName(e), what, addr)
	}
	if sbcs&sbBusyError != 0 {
		_ = m.dtm.Write(ctx, regSBCS, sbBusyError)
		return errSBABusy
	}
	return nil
}

// WriteMemWidth writes data with accesses of exactly width bytes (1, 2 or 4).
// addr and len(data) must be multiples of width. Nothing is split or widened:
// this is the precise primitive for device registers.
func (m *DM) WriteMemWidth(ctx context.Context, addr uint32, width int, data []byte) error {
	if width != 1 && width != 2 && width != 4 {
		return fmt.Errorf("riscv: access width %d is not 1, 2 or 4", width)
	}
	if addr%uint32(width) != 0 || len(data)%width != 0 {
		return fmt.Errorf("%w: %d-byte access at %#x, length %d", ErrAlignment, width, addr, len(data))
	}
	if len(data) == 0 {
		return nil
	}
	if err := checkRange(addr, len(data)); err != nil {
		return err
	}
	sba, err := m.SBA(ctx)
	if err != nil {
		return err
	}
	if !sba.Supports(4) && !sba.Supports(width) {
		return ErrNoSBA
	}
	if !sba.Supports(width) {
		return fmt.Errorf("%w: %d-byte accesses", ErrWidth, width)
	}
	per := maxSBAChunk * 4 / width
	for off := 0; off < len(data); {
		n := min((len(data)-off)/width, per)
		if err := m.sbaWriteRun(ctx, addr+uint32(off), width, data[off:off+n*width]); err != nil {
			return err
		}
		off += n * width
	}
	return nil
}

func (m *DM) sbaWriteRun(ctx context.Context, addr uint32, width int, data []byte) error {
	var lastErr error
	for try := 0; try < 5; try++ {
		ops := []DMIOp{
			{Write: true, Addr: regSBCS, Data: sbSizeCode(width)<<17 | sbAutoIncr | sbClearErr},
			{Write: true, Addr: regSBAddress0, Data: addr},
		}
		for i := 0; i < len(data); i += width {
			var v uint32
			for b := 0; b < width; b++ {
				v |= uint32(data[i+b]) << (8 * b)
			}
			ops = append(ops, DMIOp{Write: true, Addr: regSBData0, Data: v})
		}
		ops = append(ops, DMIOp{Addr: regSBCS})
		res, err := m.dtm.Pipeline(ctx, ops)
		if err != nil {
			return err
		}
		err = m.sbaCheck(ctx, res[len(res)-1], "writing", addr)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, errSBABusy) {
			return err
		}
		m.dtm.idle = min(m.dtm.idle+4, 64)
	}
	return lastErr
}

// WriteMem writes data at any alignment through System Bus Access. It picks the
// widest supported access that the address alignment allows; where the bus has
// no 8/16-bit access it does an explicit read-modify-write of the containing
// 32-bit word. It writes nothing but the requested bytes. Policy about which
// addresses may be written belongs to the caller.
func (m *DM) WriteMem(ctx context.Context, addr uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := checkRange(addr, len(data)); err != nil {
		return err
	}
	sba, err := m.SBA(ctx)
	if err != nil {
		return err
	}
	if !sba.Supports(4) {
		return ErrNoSBA
	}
	pos := addr
	rem := data
	for len(rem) > 0 {
		switch {
		case pos%4 == 0 && len(rem) >= 4:
			n := len(rem) / 4 * 4
			if err := m.WriteMemWidth(ctx, pos, 4, rem[:n]); err != nil {
				return err
			}
			pos, rem = pos+uint32(n), rem[n:]
		case pos%2 == 0 && len(rem) >= 2 && sba.Supports(2):
			if err := m.WriteMemWidth(ctx, pos, 2, rem[:2]); err != nil {
				return err
			}
			pos, rem = pos+2, rem[2:]
		case sba.Supports(1):
			if err := m.WriteMemWidth(ctx, pos, 1, rem[:1]); err != nil {
				return err
			}
			pos, rem = pos+1, rem[1:]
		default:
			// Read-modify-write the word holding pos.
			base := pos &^ 3
			cur, err := m.ReadMem(ctx, base, 4)
			if err != nil {
				return err
			}
			n := min(int(4-(pos-base)), len(rem))
			copy(cur[pos-base:], rem[:n])
			if err := m.WriteMemWidth(ctx, base, 4, cur); err != nil {
				return err
			}
			pos, rem = pos+uint32(n), rem[n:]
		}
	}
	return nil
}

// BusKind selects what one BusOp does.
type BusKind int

// Bus operation kinds. All accesses are 32 bits wide.
const (
	// BusWrite stores Vals starting at Addr (consecutive words).
	BusWrite BusKind = iota
	// BusRead loads Words consecutive words starting at Addr.
	BusRead
)

// BusOp is one step of a BusBatch.
type BusOp struct {
	Kind  BusKind
	Addr  uint32
	Vals  []uint32 // BusWrite
	Words int      // BusRead
}

// BusBatch runs a sequence of 32-bit system-bus operations as one pipelined
// transfer, so a whole device transaction (program registers, trigger, poll,
// fetch the result) costs a single USB round trip. The i-th result holds the
// words read by the i-th op (nil for writes).
//
// If the debug module reports busy the WHOLE sequence is run again, so a batch
// must be safe to repeat as a unit: it has to set up every register it relies
// on itself. A bus error aborts with ErrBusAccess.
func (m *DM) BusBatch(ctx context.Context, ops []BusOp) ([][]uint32, error) {
	sba, err := m.SBA(ctx)
	if err != nil {
		return nil, err
	}
	if !sba.Supports(4) {
		return nil, ErrNoSBA
	}
	for _, op := range ops {
		n := op.Words
		if op.Kind == BusWrite {
			n = len(op.Vals)
		}
		if n < 1 || n > maxSBAChunk {
			return nil, fmt.Errorf("riscv: bus operation of %d words (want 1..%d)", n, maxSBAChunk)
		}
		if op.Addr%4 != 0 {
			return nil, fmt.Errorf("%w: word access at %#x", ErrAlignment, op.Addr)
		}
		if err := checkRange(op.Addr, n*4); err != nil {
			return nil, err
		}
	}
	var lastErr error
	for try := 0; try < 5; try++ {
		res, err := m.busBatchOnce(ctx, ops)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !errors.Is(err, errSBABusy) {
			return nil, err
		}
		m.dtm.idle = min(m.dtm.idle+4, 64)
	}
	return nil, lastErr
}

func (m *DM) busBatchOnce(ctx context.Context, ops []BusOp) ([][]uint32, error) {
	var dmi []DMIOp
	cur := uint32(0xFFFFFFFF) // sbcs value last written; impossible initially
	setCfg := func(v uint32) {
		if cur != v {
			dmi = append(dmi, DMIOp{Write: true, Addr: regSBCS, Data: v})
			cur = v
		}
	}
	type slot struct{ first, n int } // index of the first read result and word count
	slots := make([]slot, len(ops))
	// Every read in the sequence produces exactly one DMI result at a known
	// position; positions are resolved after the sequence is built.
	var readAt []int
	for i, op := range ops {
		switch op.Kind {
		case BusWrite:
			setCfg(2<<17 | sbAutoIncr | sbClearErr)
			dmi = append(dmi, DMIOp{Write: true, Addr: regSBAddress0, Data: op.Addr})
			for _, v := range op.Vals {
				dmi = append(dmi, DMIOp{Write: true, Addr: regSBData0, Data: v})
			}
		case BusRead:
			cfg := uint32(sbReadOnAddr | 2<<17 | sbAutoIncr | sbClearErr)
			if op.Words > 1 {
				cfg |= sbReadOnData
			}
			setCfg(cfg)
			dmi = append(dmi, DMIOp{Write: true, Addr: regSBAddress0, Data: op.Addr})
			slots[i] = slot{len(readAt), op.Words}
			for w := 0; w < op.Words; w++ {
				if w == op.Words-1 && op.Words > 1 {
					dmi = append(dmi, DMIOp{Write: true, Addr: regSBCS, Data: 2<<17 | sbAutoIncr})
					cur = 2<<17 | sbAutoIncr
				}
				readAt = append(readAt, len(dmi))
				dmi = append(dmi, DMIOp{Addr: regSBData0})
			}
		}
	}
	sbcsAt := len(dmi)
	dmi = append(dmi, DMIOp{Addr: regSBCS})
	res, err := m.dtm.Pipeline(ctx, dmi)
	if err != nil {
		return nil, err
	}
	if err := m.sbaCheck(ctx, res[sbcsAt], "batch", 0); err != nil {
		return nil, err
	}
	out := make([][]uint32, len(ops))
	for i, op := range ops {
		if op.Kind != BusRead {
			continue
		}
		out[i] = make([]uint32, op.Words)
		for w := 0; w < op.Words; w++ {
			out[i][w] = res[readAt[slots[i].first+w]]
		}
	}
	return out, nil
}
