package usbcapplugin

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/faronov/hwlab/internal/usbcap"
)

// DevDir holds the usbmon binary character devices (/dev/usbmonN).
const DevDir = "/dev"

// binReadBuf holds one header plus the largest payload the kernel hands out
// with the default ring buffer.
const binReadBuf = 1 << 20

// wireHeaderLen is the struct mon_bin_hdr that read() returns. The 64-byte
// "mmapped" layout of pcap link type 220 appends the isochronous/interval
// fields, which are zero for everything but isochronous URBs.
const wireHeaderLen = 48

// widen turns a read() record (48-byte header + payload) into the 64-byte
// mmapped layout that usbcap.Decode and Wireshark understand.
func widen(rec []byte) []byte {
	out := make([]byte, usbcap.HeaderLen+len(rec)-wireHeaderLen)
	copy(out, rec[:wireHeaderLen])
	copy(out[usbcap.HeaderLen:], rec[wireHeaderLen:])
	return out
}

// binReader reads the usbmon binary API: every read() returns one URB event as
// a 48-byte header followed by len_cap payload bytes. Unlike the text API it is
// not limited to 32 payload bytes.
type binReader struct {
	f       *os.File
	ch      chan textResult
	done    chan struct{}
	once    sync.Once
	skipped atomic.Uint64
	mu      sync.Mutex
	first   string
}

func newBinReader(f *os.File) *binReader {
	r := &binReader{f: f, ch: make(chan textResult, 1024), done: make(chan struct{})}
	go r.run()
	return r
}

func (r *binReader) fail(msg string) {
	r.skipped.Add(1)
	r.mu.Lock()
	if r.first == "" {
		r.first = msg
	}
	r.mu.Unlock()
}

func (r *binReader) run() {
	defer close(r.ch)
	buf := make([]byte, binReadBuf)
	for {
		n, err := r.f.Read(buf)
		for off := 0; n-off >= wireHeaderLen; {
			capLen := int(binary.LittleEndian.Uint32(buf[off+36:]))
			if capLen < 0 || off+wireHeaderLen+capLen > n {
				capLen = n - off - wireHeaderLen
			}
			end := off + wireHeaderLen + capLen
			p, derr := usbcap.Decode(widen(buf[off:end]))
			off = end
			if derr != nil {
				r.fail(derr.Error())
				continue
			}
			select {
			case r.ch <- textResult{p: p}:
			case <-r.done:
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				select {
				case r.ch <- textResult{err: err}:
				case <-r.done:
				}
			}
			return
		}
	}
}

func (r *binReader) Next(ctx context.Context) (usbcap.Packet, error) {
	select {
	case <-ctx.Done():
		return usbcap.Packet{}, ctx.Err()
	case res, ok := <-r.ch:
		if !ok {
			return usbcap.Packet{}, io.EOF
		}
		return res.p, res.err
	}
}

func (r *binReader) Close() error {
	r.once.Do(func() { close(r.done) })
	return r.f.Close()
}

func (r *binReader) Skipped() (uint64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped.Load(), r.first
}
