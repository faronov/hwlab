// Package swd turns the probe's raw bit sequences into an ADIv5 SWD link
// (adiv5.Link). It frames SWD requests (start, APnDP, RnW, A[3:2], parity,
// stop, park), appends the turnaround and ACK bits, checks ACK and parity of
// every transaction, batches many transactions into one USB command, and
// recovers the line after a WAIT, FAULT or missing ACK.
//
// It contains no DP/AP or Cortex-M logic: the DP bring-up, MEM-AP access and
// ROM walk are internal/adiv5 and the debug logic is internal/cortexm.
//
// Sampling rule (observed on a real probe, see ADR 0034): for bits the probe
// does not drive, reply bit i holds the level of wire cycle i+1. A read
// transaction therefore finds its ACK in the three reply bits that start at
// the first turnaround cycle, followed by the 32 data bits and the parity.
package swd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
)

// Runner executes one bit sequence; *protocol.Client implements it.
type Runner interface {
	Run(ctx context.Context, s protocol.Sequence) (protocol.SequenceResult, error)
}

// Power reports the target voltage in millivolts, to tell a powered-down
// target from an unwired line when nothing acknowledges.
type Power interface {
	HWStatus(ctx context.Context) (protocol.HWStatus, error)
}

// ACK values, as the three ACK bits read LSB first.
const (
	ackOK    = 0b001
	ackWait  = 0b010
	ackFault = 0b100
)

const (
	dpABORT  = 0x0
	dpDPIDR  = 0x0
	dpCTRL   = 0x4
	dpSELECT = 0x8
	dpRDBUFF = 0xC

	abortClearAll = 0x1E // STKCMPCLR|STKERRCLR|WDERRCLR|ORUNERRCLR
	ctrlPwrReq    = 1<<28 | 1<<30
	ctrlPwrAck    = 1<<29 | 1<<31

	// requestBits is the request header; the read data transaction after it
	// is TA, ACK(3), data(32), parity, TA.
	requestBits = 8
	readTail    = 1 + 3 + 32 + 1 + 1
	writeMid    = 1 + 3 + 1
	writeData   = 32 + 1
)

// DefaultIdle is the number of idle (SWDIO low) cycles after each transaction.
const DefaultIdle = 2

// waitRetries bounds the automatic retries of a WAIT answer.
const waitRetries = 12

// Link implements adiv5.Link on a probe.
type Link struct {
	r     Runner
	power Power

	mu   sync.Mutex
	ctx  context.Context
	idle int
	max  int

	// Last DP SELECT and CTRL/STAT values the target acknowledged, restored
	// after a line reset taken for error recovery.
	selVal, ctrlVal   uint32
	haveSel, haveCtrl bool

	stats Stats
}

// Stats counts what happened on the wire, for diagnostics and tests.
type Stats struct {
	Commands  int // probe sequence commands sent
	Txns      int // SWD transactions executed
	Waits     int // WAIT acknowledgements seen
	Faults    int
	NoACKs    int
	Resyncs   int
	ParityErr int
}

var _ adiv5.Link = (*Link)(nil)

// New returns a Link on r. power may be nil.
func New(r Runner, power Power) *Link {
	return &Link{r: r, power: power, ctx: context.Background(), idle: DefaultIdle, max: protocol.MaxSequenceBits}
}

// SetContext sets the context used for probe commands (cancellation).
func (l *Link) SetContext(ctx context.Context) {
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
}

// SetIdle sets the idle cycles after each transaction (0..32).
func (l *Link) SetIdle(n int) error {
	if n < 0 || n > 32 {
		return fmt.Errorf("idle cycles %d: want 0..32", n)
	}
	l.mu.Lock()
	l.idle = n
	l.mu.Unlock()
	return nil
}

// Stats returns a copy of the counters.
func (l *Link) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// builder accumulates a bit vector with a per-bit direction.
type builder struct {
	dir, out []byte
	n        int
}

func (b *builder) grow() {
	if b.n%8 == 0 {
		b.dir = append(b.dir, 0)
		b.out = append(b.out, 0)
	}
}

// put appends one cycle. drive=false releases SWDIO (the value is ignored).
func (b *builder) put(drive bool, v byte) {
	b.grow()
	if drive {
		b.dir[b.n/8] |= 1 << (b.n % 8)
		if v != 0 {
			b.out[b.n/8] |= 1 << (b.n % 8)
		}
	}
	b.n++
}

func (b *builder) drive(val uint64, n int) {
	for i := 0; i < n; i++ {
		b.put(true, byte(val>>i)&1)
	}
}

func (b *builder) release(n int) {
	for i := 0; i < n; i++ {
		b.put(false, 0)
	}
}

func parity32(v uint32) byte {
	v ^= v >> 16
	v ^= v >> 8
	v ^= v >> 4
	v ^= v >> 2
	v ^= v >> 1
	return byte(v & 1)
}

// request encodes the 8-bit SWD request header.
func request(ap, read bool, reg byte) byte {
	r := byte(1 | 1<<7) // start, park
	var p byte
	if ap {
		r |= 1 << 1
		p ^= 1
	}
	if read {
		r |= 1 << 2
		p ^= 1
	}
	a2, a3 := (reg>>2)&1, (reg>>3)&1
	r |= a2<<3 | a3<<4
	p ^= a2 ^ a3
	r |= p << 5
	return r
}

// txn is one SWD transaction.
type txn struct {
	ap, read bool
	reg      byte
	val      uint32 // write data
}

func (t txn) bits(idle int) int {
	if t.read {
		return requestBits + readTail + idle
	}
	return requestBits + writeMid + writeData + idle
}

func (t txn) emit(b *builder, idle int) (ackAt, dataAt int) {
	b.drive(uint64(request(t.ap, t.read, t.reg)), requestBits)
	ackAt = b.n
	if t.read {
		b.release(readTail)
	} else {
		b.release(writeMid)
		b.drive(uint64(t.val)|uint64(parity32(t.val))<<32, writeData)
	}
	dataAt = ackAt + 3
	for i := 0; i < idle; i++ {
		b.put(true, 0)
	}
	return
}

func bit(in []byte, i int) byte { return (in[i/8] >> (i % 8)) & 1 }

func field(in []byte, at, n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v |= uint32(bit(in, at+i)) << i
	}
	return v
}

// failure describes the first transaction that was not acknowledged.
type failure struct {
	idx    int
	ack    uint32
	parity bool
}

// exec runs txns in as many probe commands as the bit budget needs. It
// returns the raw read values by transaction index and the first failure; the
// transactions before the failure completed. Everything after a failure was
// clocked out anyway and its effect on the target is undefined.
func (l *Link) exec(txns []txn) (vals []uint32, fail *failure, err error) {
	vals = make([]uint32, len(txns))
	for start := 0; start < len(txns); {
		var b builder
		type at struct{ ack, data int }
		var marks []at
		end := start
		for end < len(txns) && (end == start || b.n+txns[end].bits(l.idle) <= l.max) {
			a, d := txns[end].emit(&b, l.idle)
			marks = append(marks, at{a, d})
			end++
		}
		res, rerr := l.r.Run(l.ctx, protocol.Sequence{Bits: b.n, A: b.dir, B: b.out})
		l.stats.Commands++
		if rerr != nil {
			return vals, nil, rerr
		}
		if want := (b.n + 7) / 8; len(res.In) != want {
			return vals, nil, jlerr.New(jlerr.ProtocolError, "SWD sequence", fmt.Errorf("short reply: %d of %d sampled bytes", len(res.In), want))
		}
		for k, m := range marks {
			i := start + k
			l.stats.Txns++
			ack := field(res.In, m.ack, 3)
			if ack != ackOK {
				return vals, &failure{idx: i, ack: ack}, nil
			}
			if txns[i].read {
				v := field(res.In, m.data, 32)
				if parity32(v) != bit(res.In, m.data+32) {
					l.stats.ParityErr++
					return vals, &failure{idx: i, ack: ack, parity: true}, nil
				}
				vals[i] = v
			}
		}
		start = end
	}
	return vals, nil, nil
}

// failErr maps a failure to the adiv5 error vocabulary and the jlink classes.
func (l *Link) failErr(f *failure, done int) *adiv5.TransferError {
	te := &adiv5.TransferError{Done: done, Ack: byte(f.ack)}
	switch {
	case f.parity:
		te.Err = jlerr.New(jlerr.ProtocolError, "SWD read parity", fmt.Errorf("%w: data parity mismatch", adiv5.ErrProtocol))
	case f.ack == ackWait:
		te.Err = jlerr.New(jlerr.SWDWait, "SWD", adiv5.ErrWait)
	case f.ack == ackFault:
		te.Err = jlerr.New(jlerr.SWDFault, "SWD", adiv5.ErrFault)
	case f.ack == 0b111 || f.ack == 0:
		te.Err = l.noACK(f.ack)
	default:
		te.Err = jlerr.New(jlerr.ProtocolError, "SWD ACK", fmt.Errorf("%w: invalid ACK 0b%03b", adiv5.ErrProtocol, f.ack))
	}
	return te
}

func (l *Link) noACK(ack uint32) error {
	if l.power != nil {
		if st, err := l.power.HWStatus(l.ctx); err == nil && st.VTargetMV < 1000 {
			return jlerr.New(jlerr.TargetPowerOff, "SWD", fmt.Errorf("%w: no ACK (0b%03b) and the target voltage reads %d mV", adiv5.ErrNoACK, ack, st.VTargetMV))
		}
	}
	return jlerr.New(jlerr.SWDNoACK, "SWD", fmt.Errorf("%w (ACK bits 0b%03b)", adiv5.ErrNoACK, ack))
}

// resync brings the DP back after a failed transaction: a line reset, the
// mandatory DPIDR read, an ABORT that clears the sticky flags, and the SELECT
// and CTRL/STAT values the target had acknowledged before.
func (l *Link) resync() error {
	l.stats.Resyncs++
	var b builder
	b.drive(^uint64(0)>>8, 56)
	for i := 0; i < 8; i++ {
		b.put(true, 0)
	}
	res, err := l.r.Run(l.ctx, protocol.Sequence{Bits: b.n, A: b.dir, B: b.out})
	l.stats.Commands++
	_ = res
	if err != nil {
		return err
	}
	seq := []txn{{read: true, reg: dpDPIDR}, {reg: dpABORT, val: abortClearAll}}
	if l.haveSel {
		seq = append(seq, txn{reg: dpSELECT, val: l.selVal})
	}
	if l.haveCtrl {
		seq = append(seq, txn{reg: dpCTRL, val: l.ctrlVal})
	}
	if _, f, err := l.exec(seq); err != nil {
		return err
	} else if f != nil {
		return l.failErr(f, 0)
	}
	if l.haveCtrl && l.ctrlVal&ctrlPwrReq != 0 {
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			v, f, err := l.exec([]txn{{read: true, reg: dpCTRL}})
			if err != nil {
				return err
			}
			if f != nil {
				return l.failErr(f, 0)
			}
			if v[0]&ctrlPwrAck == l.ctrlVal&ctrlPwrReq<<1 {
				return nil
			}
			if time.Now().After(deadline) {
				return jlerr.New(jlerr.TargetUnavailable, "resync", fmt.Errorf("debug power-up not acknowledged after recovery (CTRL/STAT 0x%08x)", v[0]))
			}
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// track records DP state changes acknowledged by the target.
func (l *Link) track(t txn) {
	if t.ap || t.read {
		return
	}
	switch t.reg & 0xC {
	case dpSELECT:
		l.selVal, l.haveSel = t.val, true
	case dpCTRL:
		l.ctrlVal, l.haveCtrl = t.val, true
	}
}

// run executes txns, retrying WAIT answers after a resync. It returns the raw
// read values and, on failure, the index of the failing transaction.
func (l *Link) run(txns []txn) ([]uint32, int, error) {
	vals := make([]uint32, len(txns))
	from := 0
	waits := 0
	for from < len(txns) {
		v, f, err := l.exec(txns[from:])
		if err != nil {
			return vals, from, err
		}
		if f == nil {
			copy(vals[from:], v)
			for _, t := range txns[from:] {
				l.track(t)
			}
			return vals, len(txns), nil
		}
		copy(vals[from:from+f.idx], v[:f.idx])
		for _, t := range txns[from : from+f.idx] {
			l.track(t)
		}
		switch f.ack {
		case ackWait:
			l.stats.Waits++
		case ackFault:
			l.stats.Faults++
		default:
			l.stats.NoACKs++
		}
		failIdx := from + f.idx
		if f.ack == 0b111 || f.ack == 0 {
			// Nothing answered: do not hammer the line with a recovery that
			// cannot work; report it with the power state.
			return vals, failIdx, l.failErr(f, failIdx)
		}
		if rerr := l.resync(); rerr != nil {
			return vals, failIdx, rerr
		}
		if f.ack == ackWait && waits < waitRetries {
			waits++
			time.Sleep(time.Duration(waits) * 500 * time.Microsecond)
			from = failIdx
			continue
		}
		return vals, failIdx, l.failErr(f, failIdx)
	}
	return vals, len(txns), nil
}

// plan expands ops into transactions. AP reads are posted: the value of an AP
// read comes back with the next AP read or with a DP RDBUFF read that the link
// inserts before anything else touches the DP.
type plan struct {
	txns []txn
	// slot[i] is the transaction whose raw result answers op i (-1: no value).
	slot []int
	// last[i] is the transaction that completes op i.
	last []int
}

func buildPlan(ops []adiv5.Op) plan {
	var p plan
	pending := -1 // op index of an AP read awaiting its value
	flush := func() {
		if pending >= 0 {
			p.txns = append(p.txns, txn{read: true, reg: dpRDBUFF})
			p.slot[pending] = len(p.txns) - 1
			p.last[pending] = len(p.txns) - 1
			pending = -1
		}
	}
	for i, op := range ops {
		p.slot = append(p.slot, -1)
		p.last = append(p.last, -1)
		switch {
		case op.Read && op.AP:
			p.txns = append(p.txns, txn{ap: true, read: true, reg: op.Reg})
			if pending >= 0 {
				p.slot[pending] = len(p.txns) - 1
				p.last[pending] = len(p.txns) - 1
			}
			pending = i
			p.last[i] = len(p.txns) - 1
		case op.Read:
			flush()
			p.txns = append(p.txns, txn{read: true, reg: op.Reg})
			p.slot[i], p.last[i] = len(p.txns)-1, len(p.txns)-1
		default:
			flush()
			p.txns = append(p.txns, txn{ap: op.AP, reg: op.Reg, val: op.Value})
			p.last[i] = len(p.txns) - 1
		}
	}
	flush()
	return p
}

// Transfer implements adiv5.Link.
func (l *Link) Transfer(ops []adiv5.Op) ([]uint32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(ops) == 0 {
		return nil, nil
	}
	p := buildPlan(ops)
	raw, failIdx, err := l.run(p.txns)
	var out []uint32
	done := 0
	for i, op := range ops {
		if err != nil && p.last[i] >= failIdx {
			break
		}
		done = i + 1
		if op.Read {
			out = append(out, raw[p.slot[i]])
		}
	}
	if err != nil {
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = done
		}
		return out, err
	}
	return out, nil
}

// BlockRead implements adiv5.Link: words reads of one register. The posted
// AP read pipeline continues across probe commands, so there is one extra
// RDBUFF read per block, not per command.
func (l *Link) BlockRead(ap bool, reg byte, words int) ([]uint32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if words <= 0 {
		return nil, nil
	}
	txns := make([]txn, 0, words+1)
	for i := 0; i < words; i++ {
		txns = append(txns, txn{ap: ap, read: true, reg: reg})
	}
	if ap {
		txns = append(txns, txn{read: true, reg: dpRDBUFF})
	}
	raw, failIdx, err := l.run(txns)
	first := 0
	if ap {
		first = 1
	}
	if err != nil {
		have := failIdx - first
		if have < 0 {
			have = 0
		}
		out := append([]uint32(nil), raw[first:first+have]...)
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = len(out)
		}
		return out, err
	}
	return append([]uint32(nil), raw[first:first+words]...), nil
}

// BlockWrite implements adiv5.Link.
func (l *Link) BlockWrite(ap bool, reg byte, data []uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	txns := make([]txn, len(data))
	for i, v := range data {
		txns[i] = txn{ap: ap, reg: reg, val: v}
	}
	_, failIdx, err := l.run(txns)
	if err != nil {
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = failIdx
		}
		return err
	}
	return nil
}

// WriteABORT implements adiv5.Link.
func (l *Link) WriteABORT(v uint32) error {
	_, err := l.Transfer([]adiv5.Op{{Reg: dpABORT, Value: v}})
	return err
}

// SWJSequence implements adiv5.Link: the bits are driven as given. A run of
// at least 50 ones is a line reset, which also invalidates the DP state this
// link remembers for recovery.
func (l *Link) SWJSequence(bits int, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bits <= 0 {
		return nil
	}
	if len(data)*8 < bits {
		return fmt.Errorf("SWJ sequence of %d bits needs %d data bytes, got %d", bits, (bits+7)/8, len(data))
	}
	ones := 0
	for i := 0; i < bits; i++ {
		if bit(data, i) == 1 {
			ones++
			if ones >= 50 {
				l.haveSel, l.haveCtrl = false, false
			}
		} else {
			ones = 0
		}
	}
	for done := 0; done < bits; {
		n := bits - done
		if n > l.max {
			n = l.max
		}
		var b builder
		for i := 0; i < n; i++ {
			b.put(true, bit(data, done+i))
		}
		if _, err := l.r.Run(l.ctx, protocol.Sequence{Bits: b.n, A: b.dir, B: b.out}); err != nil {
			return err
		}
		l.stats.Commands++
		done += n
	}
	return nil
}
