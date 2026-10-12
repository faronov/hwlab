package xds

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/adiv5"
)

// SWD request byte as the XDS110 queue expects it: start, APnDP, RnW, A[3:2],
// parity, stop (0), park.
const (
	swdStart = 0x01
	swdAP    = 0x02
	swdRead  = 0x04
	swdPark  = 0x80
	dpRDBUFF = 0x0c
	dpCTRL   = 0x04
	// ctrlCORUNDETECT is refused: the probe does not support sticky overrun
	// detection through its queue, so it is never forwarded.
	ctrlCORUNDETECT = 1 << 0
)

func swdRequest(ap, read bool, reg byte) byte {
	b := byte(swdStart | swdPark)
	if ap {
		b |= swdAP
	}
	if read {
		b |= swdRead
	}
	b |= (reg & 0x0c) << 1 // A[3:2] at bits 4:3
	par := 0
	for _, v := range []bool{ap, read, reg&0x04 != 0, reg&0x08 != 0} {
		if v {
			par ^= 1
		}
	}
	b |= byte(par) << 5
	return b
}

// Queue limits derived from the 4 KiB data block of the protocol.
const (
	maxQueueBytes = 4000
	maxQueueReads = 1000
)

// SWDLink adapts a connected probe to adiv5.Link using the probe's own SWD
// engine. The firmware reports one status per queue, so a failing queue
// carries no index of the failing transfer (TransferError.Done is 0) and the
// caller replays the whole queue on WAIT.
type SWDLink struct {
	P *Probe
}

// NewSWDLink returns a Link on p, which must already be connected (XDS_CONNECT)
// with the clock set. The SWD line is brought up by the JTAG-to-SWD sequence
// that adiv5.Connect sends through SWJSequence.
func NewSWDLink(p *Probe) *SWDLink { return &SWDLink{P: p} }

var _ adiv5.Link = (*SWDLink)(nil)

// SWJSequence recognises the standard SWJ switch sequences and lets the probe
// perform them; the probe cannot clock arbitrary bit patterns.
func (l *SWDLink) SWJSequence(bits int, data []byte) error {
	switch {
	case bits >= 50 && allOnes(data, bits):
		return nil // line reset is part of the probe's SWD connect
	case bits == 16 && len(data) >= 2 && data[0] == 0x9e && data[1] == 0xe7:
		return l.attachSWD()
	case bits <= 8 && isZero(data, bits):
		return nil
	}
	return fmt.Errorf("the XDS110 cannot clock an arbitrary %d-bit SWJ sequence", bits)
}

func (l *SWDLink) attachSWD() error {
	if l.P.Info.Firmware != 0 && l.P.Info.Firmware < FWOptimizedDAP {
		return fmt.Errorf("XDS110 firmware %s is older than %s, which introduced the SWD queue", l.P.Info.FirmwareText, FirmwareString(FWOptimizedDAP))
	}
	if err := l.P.SWDConnect(); err != nil {
		return err
	}
	if _, err := l.P.Do(CmdCMAPIConnect, nil); err != nil {
		return err
	}
	_, err := l.P.Do(CmdCMAPIAcquire, nil)
	return err
}

func allOnes(b []byte, bits int) bool {
	for i := 0; i < bits; i++ {
		if i/8 >= len(b) || b[i/8]>>(i%8)&1 == 0 {
			return false
		}
	}
	return true
}

func isZero(b []byte, bits int) bool {
	for i := 0; i < bits; i++ {
		if i/8 < len(b) && b[i/8]>>(i%8)&1 != 0 {
			return false
		}
	}
	return true
}

// queue builds an OCD_DAP_REQUEST body and remembers where each result goes.
type queue struct {
	buf   bytes.Buffer
	reads int
}

func (q *queue) read(ap bool, reg byte) {
	q.buf.WriteByte(swdRequest(ap, true, reg))
	q.reads++
}

func (q *queue) write(ap bool, reg byte, v uint32) {
	q.buf.WriteByte(swdRequest(ap, false, reg))
	var w [4]byte
	binary.LittleEndian.PutUint32(w[:], v)
	q.buf.Write(w[:])
}

func (q *queue) size() int { return q.buf.Len() + 1 }

// run sends the queue and returns the raw read results.
func (l *SWDLink) run(q *queue) ([]uint32, error) {
	if q.reads == 0 && q.buf.Len() == 0 {
		return nil, nil
	}
	body := append(q.buf.Bytes(), 0) // zero byte ends the queue
	d, err := l.P.Do(CmdDapRequest, body)
	if err != nil {
		var xe *Error
		if errors.As(err, &xe) {
			return nil, transferError(xe)
		}
		return nil, err
	}
	if err := want(d, 4*q.reads, "OCD_DAP_REQUEST results"); err != nil {
		return nil, err
	}
	out := make([]uint32, q.reads)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(d[4*i:])
	}
	return out, nil
}

func transferError(e *Error) error {
	te := &adiv5.TransferError{Err: fmt.Errorf("%w: %w", adiv5.ErrProtocol, e)}
	switch e.Status {
	case StatusSWDWait:
		te.Err, te.Ack = adiv5.ErrWait, 2
	case StatusSWDFault:
		te.Err, te.Ack = adiv5.ErrFault, 4
	case StatusSWDDevID:
		te.Err = fmt.Errorf("%w (%w)", adiv5.ErrNoACK, e)
	case StatusSWDProto, StatusSWDParity:
		te.Err = fmt.Errorf("%w (%w)", adiv5.ErrProtocol, e)
	default:
		return e
	}
	return te
}

// Transfer runs ops in order. AP reads are posted by the SWD protocol, so the
// value of each AP read is collected from the next AP read or from a DP
// RDBUFF read the link inserts; callers see ordinary per-op results.
func (l *SWDLink) Transfer(ops []adiv5.Op) ([]uint32, error) {
	var results []uint32
	for len(ops) > 0 {
		n, vals, err := l.chunk(ops)
		results = append(results, vals...)
		if err != nil {
			if te := asTransfer(err); te != nil {
				te.Done = len(results)
			}
			return results, err
		}
		ops = ops[n:]
	}
	return results, nil
}

// chunk executes as many leading ops as fit one queue and returns how many
// ops it consumed and the read values for them.
func (l *SWDLink) chunk(ops []adiv5.Op) (int, []uint32, error) {
	q := &queue{}
	type slot struct {
		raw int
		ap  bool
	}
	var slots []slot
	pendingAP := false
	consumed := 0
	flush := func() {
		if pendingAP {
			q.read(false, dpRDBUFF)
			pendingAP = false
		}
	}
	for _, op := range ops {
		need := 5
		if op.Read {
			need = 1
		}
		if q.size()+need+1 > maxQueueBytes || q.reads+2 > maxQueueReads {
			break
		}
		switch {
		case op.Read && op.AP:
			q.read(true, op.Reg)
			slots = append(slots, slot{q.reads - 1, true})
			pendingAP = true
		case op.Read:
			flush()
			q.read(false, op.Reg)
			slots = append(slots, slot{q.reads - 1, false})
		default:
			flush()
			v := op.Value
			if !op.AP && op.Reg == dpCTRL {
				v &^= ctrlCORUNDETECT
			}
			q.write(op.AP, op.Reg, v)
		}
		consumed++
	}
	flush()
	raw, err := l.run(q)
	if err != nil {
		return 0, nil, err
	}
	// An AP read returns the previous posted result, so the value of an AP
	// read is the raw result of the read request that follows it (the next
	// AP read or the inserted RDBUFF).
	vals := make([]uint32, len(slots))
	for k, s := range slots {
		if s.ap {
			vals[k] = raw[s.raw+1]
		} else {
			vals[k] = raw[s.raw]
		}
	}
	return consumed, vals, nil
}

// BlockRead reads words registers from the same address (auto-incrementing
// TAR makes AP DRW reads walk memory).
func (l *SWDLink) BlockRead(ap bool, reg byte, words int) ([]uint32, error) {
	out := make([]uint32, 0, words)
	for words > 0 {
		n := words
		if n > maxQueueReads-2 {
			n = maxQueueReads - 2
		}
		q := &queue{}
		for i := 0; i < n; i++ {
			q.read(ap, reg)
		}
		if ap {
			q.read(false, dpRDBUFF)
		}
		raw, err := l.run(q)
		if err != nil {
			if te := asTransfer(err); te != nil {
				te.Done = len(out)
			}
			return out, err
		}
		if ap {
			raw = raw[1:]
		}
		out = append(out, raw...)
		words -= n
	}
	return out, nil
}

// BlockWrite writes data words to the same register.
func (l *SWDLink) BlockWrite(ap bool, reg byte, data []uint32) error {
	const perQueue = (maxQueueBytes - 8) / 5
	for done := 0; done < len(data); {
		n := len(data) - done
		if n > perQueue {
			n = perQueue
		}
		q := &queue{}
		for _, v := range data[done : done+n] {
			q.write(ap, reg, v)
		}
		if _, err := l.run(q); err != nil {
			if te := asTransfer(err); te != nil {
				te.Done = done
			}
			return err
		}
		done += n
	}
	return nil
}

// WriteABORT clears sticky errors through the DP ABORT register.
func (l *SWDLink) WriteABORT(v uint32) error {
	q := &queue{}
	q.write(false, 0x00, v)
	_, err := l.run(q)
	return err
}

func asTransfer(err error) *adiv5.TransferError {
	var te *adiv5.TransferError
	if errors.As(err, &te) {
		return te
	}
	return nil
}
