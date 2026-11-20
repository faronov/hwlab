// Package jtagdp is an ADIv5 JTAG-DP transport: it implements adiv5.Link with
// whole IR/DR scans (jtag.Scanner), so the shared ADIv5 MEM-AP and Cortex-M
// layers run unchanged over JTAG, including IEEE 1149.7 two-pin cJTAG and a
// DAP that sits behind a TI ICEPick router.
//
// Every DPACC/APACC scan captures the acknowledge of the previous
// transaction and the result of the previous read, so reads are collected
// from the following scan (a DP RDBUFF read closes a sequence). WAIT is
// handled inside the link: a WAIT followed only by further WAITs means the DP
// ignored those requests, and they are issued again in order. A WAIT followed
// by an accepted request inside one batch would have reordered the accesses;
// that is reported as an error, never retried blindly.
package jtagdp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/jtag"
)

// JTAG-DP instructions (IR length 4).
const (
	IRLen    = 4
	IRAbort  = 0x8
	IRDPACC  = 0xA
	IRAPACC  = 0xB
	IRIDCode = 0xE
	IRBypass = 0xF
)

const (
	drBits     = 35
	ackOKFault = 0x2
	ackWait    = 0x1

	dpCtrl   = 0x4
	dpRdbuff = 0xC

	// CTRL/STAT sticky flags (write 1 to clear on a JTAG-DP).
	ctrlStickyOrun = 1 << 1
	ctrlStickyCmp  = 1 << 4
	ctrlStickyErr  = 1 << 5
	ctrlPowerReqs  = 1<<28 | 1<<30

	waitRetries = 50
	waitPause   = time.Millisecond
)

// Link is a JTAG-DP reached through a scanner. Other TAPs on the chain are in
// BYPASS: Pre counts the bits between the DAP and TDO, Post those between TDI
// and the DAP (for a DAP linked behind an ICEPick: IRPost 6, DRPost 1).
type Link struct {
	S             jtag.Scanner
	IRPre, IRPost int
	DRPre, DRPost int
	// IDCode is the DAP TAP IDCODE. A JTAG-DP without DPIDR (DPv0) reads 0
	// at DP address 0; the IDCODE (same version/part/designer layout) is
	// reported there instead and DPIDRFromIDCode is set.
	IDCode          uint32
	DPIDRFromIDCode bool
	// IdleAfterAP is the number of Run-Test/Idle clocks after every AP
	// access, giving the bus time to complete it before the next scan.
	IdleAfterAP int
	// Ctx bounds the scans; nil means context.Background.
	Ctx context.Context

	ir    uint32
	irSet bool
}

var (
	_ adiv5.Link         = (*Link)(nil)
	_ adiv5.DormantWaker = (*Link)(nil)
)

// ErrReordered means a WAIT was followed by an accepted request in the same
// batch, so the requests did not execute in order.
var ErrReordered = errors.New("JTAG-DP answered WAIT and then accepted a later request of the same batch: the access order is not guaranteed")

// SetContext sets the context of the following scans (adiv5.Bus calls it).
func (l *Link) SetContext(ctx context.Context) { l.Ctx = ctx }

func (l *Link) ctx() context.Context {
	if l.Ctx != nil {
		return l.Ctx
	}
	return context.Background()
}

// item is one DPACC/APACC/ABORT DR scan.
type item struct {
	ir  uint32
	req uint64
}

func request(read bool, reg byte, v uint32) uint64 {
	r := uint64(v)<<3 | uint64(reg>>2&3)<<1
	if read {
		r |= 1
	}
	return r
}

// pack places the n low bits of v at bit offset pre of a total-bit vector.
func pack(v uint64, pre, n, total int) []byte {
	out := make([]byte, (total+7)/8)
	for i := 0; i < n; i++ {
		if v>>i&1 == 1 {
			out[(pre+i)/8] |= 1 << ((pre + i) % 8)
		}
	}
	return out
}

func (l *Link) irScan(instr uint32) jtag.ScanOp {
	total := l.IRPre + IRLen + l.IRPost
	out := make([]byte, (total+7)/8)
	for i := 0; i < total; i++ {
		bit := true // BYPASS for the other TAPs
		if i >= l.IRPre && i < l.IRPre+IRLen {
			bit = instr>>(i-l.IRPre)&1 == 1
		}
		if bit {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return jtag.ScanOp{IR: true, Bits: total, Out: out, End: jtag.RunTestIdle}
}

func (l *Link) drScan(req uint64) jtag.ScanOp {
	total := l.DRPre + drBits + l.DRPost
	return jtag.ScanOp{Bits: total, Out: pack(req, l.DRPre, drBits, total), End: jtag.RunTestIdle}
}

func (l *Link) capture(b []byte) (ack byte, data uint32) {
	var v uint64
	for i := 0; i < drBits; i++ {
		j := l.DRPre + i
		if j/8 < len(b) && b[j/8]>>(j%8)&1 == 1 {
			v |= 1 << i
		}
	}
	return byte(v & 7), uint32(v >> 3)
}

// exec runs the items in order and returns one capture per item: the
// acknowledge of the preceding transaction and the result of the preceding
// read. WAITs are retried as described in the package comment.
func (l *Link) exec(items []item) ([]uint32, error) {
	out := make([]uint32, 0, len(items))
	retries := 0
	for len(items) > 0 {
		var ops []jtag.ScanOp
		dr := make([]int, 0, len(items))
		for _, it := range items {
			if !l.irSet || l.ir != it.ir {
				ops = append(ops, l.irScan(it.ir))
				l.ir, l.irSet = it.ir, true
			}
			dr = append(dr, len(ops))
			ops = append(ops, l.drScan(it.req))
			if it.ir == IRAPACC && l.IdleAfterAP > 0 {
				ops = append(ops, jtag.ScanOp{Idle: l.IdleAfterAP, End: jtag.RunTestIdle})
			}
		}
		caps, err := l.S.Scans(l.ctx(), ops)
		if err != nil {
			l.irSet = false
			return out, err
		}
		// caps holds one entry per op with Bits > 0, i.e. IR and DR scans.
		idx := make([]int, len(ops))
		for i, n := 0, 0; i < len(ops); i++ {
			idx[i] = n
			if ops[i].Bits > 0 {
				n++
			}
		}
		wait := -1
		for k, op := range dr {
			ack, data := l.capture(caps[idx[op]])
			switch {
			case ack == ackOKFault && wait < 0:
				out = append(out, data)
				continue
			case ack == ackOKFault:
				_ = l.abort()
				return out, &adiv5.TransferError{Done: len(out), Err: fmt.Errorf("%w: %w", adiv5.ErrProtocol, ErrReordered)}
			case ack == ackWait:
				if wait < 0 {
					wait = k
				}
				continue
			}
			l.irSet = false
			return out, &adiv5.TransferError{Done: len(out), Ack: ack, Err: fmt.Errorf("%w: JTAG-DP acknowledge 0b%03b (TDO/TMSC stuck or the DAP fell off the chain)", adiv5.ErrNoACK, ack)}
		}
		if wait < 0 {
			return out, nil
		}
		retries++
		if retries > waitRetries {
			_ = l.abort()
			return out, &adiv5.TransferError{Done: len(out), Ack: ackWait, Err: fmt.Errorf("%w: still WAIT after %d retries (DAPABORT sent)", adiv5.ErrProtocol, waitRetries)}
		}
		time.Sleep(waitPause)
		items = items[wait:]
		l.irSet = false
	}
	return out, nil
}

// abort cancels a stalled transaction (DAPABORT).
func (l *Link) abort() error {
	l.irSet = false
	_, err := l.S.Scans(l.ctx(), []jtag.ScanOp{l.irScan(IRAbort), l.drScan(request(false, 0, 1))})
	return err
}

func instrFor(ap bool) uint32 {
	if ap {
		return IRAPACC
	}
	return IRDPACC
}

// Transfer implements adiv5.Link.
func (l *Link) Transfer(ops []adiv5.Op) ([]uint32, error) {
	items := make([]item, 0, len(ops)+1)
	for _, op := range ops {
		items = append(items, item{ir: instrFor(op.AP), req: request(op.Read, op.Reg, op.Value)})
	}
	if len(ops) > 0 && ops[len(ops)-1].Read {
		items = append(items, item{ir: IRDPACC, req: request(true, dpRdbuff, 0)})
	}
	caps, err := l.exec(items)
	// The value of op i is captured by the scan of item i+1.
	var vals []uint32
	for i, op := range ops {
		if i+1 >= len(caps) {
			break
		}
		if op.Read {
			v := caps[i+1]
			if !op.AP && op.Reg == 0 && v == 0 && l.IDCode != 0 {
				v, l.DPIDRFromIDCode = l.IDCode, true
			}
			vals = append(vals, v)
		}
	}
	if err != nil {
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = max(len(caps)-1, 0)
		}
		return vals, err
	}
	return vals, nil
}

// BlockRead implements adiv5.Link.
func (l *Link) BlockRead(ap bool, reg byte, words int) ([]uint32, error) {
	items := make([]item, 0, words+1)
	for i := 0; i < words; i++ {
		items = append(items, item{ir: instrFor(ap), req: request(true, reg, 0)})
	}
	items = append(items, item{ir: IRDPACC, req: request(true, dpRdbuff, 0)})
	caps, err := l.exec(items)
	var out []uint32
	if len(caps) > 1 {
		out = caps[1:]
	}
	if len(out) > words {
		out = out[:words]
	}
	if err != nil {
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = len(out)
		}
	}
	return out, err
}

// BlockWrite implements adiv5.Link.
func (l *Link) BlockWrite(ap bool, reg byte, data []uint32) error {
	items := make([]item, 0, len(data))
	for _, v := range data {
		items = append(items, item{ir: instrFor(ap), req: request(false, reg, v)})
	}
	caps, err := l.exec(items)
	if err != nil {
		var te *adiv5.TransferError
		if errors.As(err, &te) {
			te.Done = len(caps)
		}
	}
	return err
}

// WriteABORT implements adiv5.Link. The SW-DP style clear bits are mapped to
// the JTAG-DP way of clearing sticky flags: writing them back as ones to
// CTRL/STAT, keeping the power-up requests. DAPABORT goes to ABORT.
func (l *Link) WriteABORT(v uint32) error {
	if v&1 != 0 {
		if err := l.abort(); err != nil {
			return err
		}
	}
	var clear uint32
	if v&(1<<1) != 0 {
		clear |= ctrlStickyCmp
	}
	if v&(1<<2) != 0 {
		clear |= ctrlStickyErr
	}
	if v&(1<<4) != 0 {
		clear |= ctrlStickyOrun
	}
	if clear == 0 {
		return nil
	}
	vals, err := l.Transfer([]adiv5.Op{{Read: true, Reg: dpCtrl}})
	if err != nil {
		return err
	}
	_, err = l.Transfer([]adiv5.Op{{Reg: dpCtrl, Value: vals[0]&ctrlPowerReqs | clear}})
	return err
}

// SWJSequence implements adiv5.Link. A JTAG-DP has no SWD line: the line
// resets and the JTAG-to-SWD switch adiv5 sends are not applicable and do
// nothing.
func (l *Link) SWJSequence(int, []byte) error { return nil }

// WakeDormant implements adiv5.DormantWaker: there is no dormant state to
// leave on a JTAG-DP reached through an active scan format.
func (l *Link) WakeDormant() error {
	return errors.New("not applicable to a JTAG-DP")
}
