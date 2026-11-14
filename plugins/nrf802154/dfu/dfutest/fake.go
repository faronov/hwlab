// Package dfutest provides a simulated nRF5 SDK serial DFU bootloader for
// tests of the dfu client and the plugin's dfu_program method.
package dfutest

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"sync"
	"time"

	"github.com/faronov/hwlab/plugins/nrf802154/dfu"
)

// Bootloader emulates the request handler closely enough to check the
// client's sequence, chunk sizes, offsets and CRCs. It implements
// usbserial.Port.
type Bootloader struct {
	MTU     int
	MaxData int
	// Reject, if set, may fail any request with a result (and extended) code.
	Reject func(op byte) (res, ext byte, reject bool)
	// Mute, if set, may swallow a request without any response (a stalled
	// transfer as seen on hardware).
	Mute func(op byte) bool
	// ResetOnLastExecute drops the response to the final execute, as a
	// device that resets immediately would.
	ResetOnLastExecute bool

	mu       sync.Mutex
	dec      dfu.SlipDecoder
	out      []byte
	obj      []byte
	objType  byte
	objSize  int
	fwSize   int
	Init     []byte
	Firmware []byte
	Executed []byte // object types in execution order
	Ops      []byte // request op codes except writes
	Closed   bool
}

func (f *Bootloader) respond(op, res byte, data ...byte) {
	f.out = append(f.out, dfu.SlipEncode(append([]byte{dfu.OpResponse, op, res}, data...))...)
}

// Write implements io.Writer.
func (f *Bootloader) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dec.Feed(p, func(m []byte) { f.handle(bytes.Clone(m)) })
	return len(p), nil
}

// Read implements io.Reader; it returns (0, nil) when idle (read timeout).
func (f *Bootloader) Read(p []byte) (int, error) {
	f.mu.Lock()
	n := copy(p, f.out)
	f.out = f.out[n:]
	f.mu.Unlock()
	if n == 0 {
		time.Sleep(time.Millisecond)
	}
	return n, nil
}

// SetReadTimeout implements usbserial.Port.
func (f *Bootloader) SetReadTimeout(time.Duration) error { return nil }

// Close implements io.Closer.
func (f *Bootloader) Close() error {
	f.mu.Lock()
	f.Closed = true
	f.mu.Unlock()
	return nil
}

func (f *Bootloader) crcState() (uint32, uint32) {
	if f.objType == dfu.ObjCommand {
		return uint32(len(f.obj)), crc32.ChecksumIEEE(f.obj)
	}
	all := append(bytes.Clone(f.Firmware), f.obj...)
	return uint32(len(all)), crc32.ChecksumIEEE(all)
}

func (f *Bootloader) handle(m []byte) {
	op := m[0]
	if op != dfu.OpWrite {
		f.Ops = append(f.Ops, op)
	}
	if f.Mute != nil && f.Mute(op) {
		return
	}
	if f.Reject != nil {
		if res, ext, ok := f.Reject(op); ok {
			f.respond(op, res, ext)
			return
		}
	}
	le := binary.LittleEndian
	u32 := func(v uint32) []byte { return le.AppendUint32(nil, v) }
	switch op {
	case dfu.OpPing:
		f.respond(op, dfu.ResSuccess, m[1])
	case dfu.OpSetPRN:
		f.respond(op, dfu.ResSuccess)
	case dfu.OpMTUGet:
		f.respond(op, dfu.ResSuccess, byte(f.MTU), byte(f.MTU>>8))
	case dfu.OpSelect:
		maxSize := uint32(256)
		if m[1] == dfu.ObjData {
			maxSize = uint32(f.MaxData)
		}
		f.respond(op, dfu.ResSuccess, append(append(u32(maxSize), u32(0)...), u32(0)...)...)
	case dfu.OpCreate:
		f.objType, f.objSize, f.obj = m[1], int(le.Uint32(m[2:])), nil
		f.respond(op, dfu.ResSuccess)
	case dfu.OpWrite:
		// A SLIP-escaped write must fit the MTU even if every byte is escaped.
		if 2*len(m)+1 > f.MTU+2 {
			f.respond(op, 0x03)
			return
		}
		f.obj = append(f.obj, m[1:]...) // no response (PRN 0)
	case dfu.OpCRCGet:
		off, crc := f.crcState()
		f.respond(op, dfu.ResSuccess, append(u32(off), u32(crc)...)...)
	case dfu.OpExecute:
		if len(f.obj) != f.objSize {
			f.respond(op, 0x05) // invalid object
			return
		}
		f.Executed = append(f.Executed, f.objType)
		if f.objType == dfu.ObjCommand {
			f.Init = f.obj
			n, err := dfu.InitAppSize(f.Init)
			if err != nil {
				f.respond(op, dfu.ResExtendedError, 0x04)
				return
			}
			f.fwSize = n
		} else {
			f.Firmware = append(f.Firmware, f.obj...)
			if len(f.Firmware) == f.fwSize && f.ResetOnLastExecute {
				f.obj = nil
				return
			}
		}
		f.obj = nil
		f.respond(op, dfu.ResSuccess)
	default:
		f.respond(op, 0x02)
	}
}
