// Package protocol holds the native Silicon Labs Wireless Starter Kit (WSTK)
// protocols hwlab speaks, independent of any transport:
//
//   - EMUCOM channel access on the kit's J-Link USB interface (emucom.go),
//   - the kit control channel: firmware version and board records (kit.go),
//   - the AEM current/voltage stream (aem.go),
//   - Debug Channel (DCH) framing (dch.go) and EFR32 Packet Trace (PTI)
//     payloads inside it (pti.go),
//   - the text admin console on TCP port 4902 (admin.go).
//
// The byte layouts were established by observing the USB traffic of Silicon
// Labs' own tools against a real BRD4001A kit and from Silicon Labs' public
// debug-channel documentation; see docs/adr/0037-wstk-native.md. No vendor
// code is used.
package protocol

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// EMUCOM channels of the kit firmware.
const (
	ChanKit  uint32 = 0x10000 // kit control: version, board list
	ChanAEM  uint32 = 0x10002 // Advanced Energy Monitor stream
	ChanUSBT uint32 = 0x10003 // socket tunnel used by Silicon Labs' silink (not used by hwlab)
)

const cmdEmucom = 0xEE

// Link is the J-Link command pipe EMUCOM runs on. plugins/jlinknative/usb.Conn
// implements it on hardware; tests use a simulated kit.
type Link interface {
	// Exchange sends cmd (nothing when empty) and reads exactly n bytes
	// (nothing when n is 0).
	Exchange(ctx context.Context, cmd []byte, n int) ([]byte, error)
	// ExchangeUpTo sends cmd and returns the first IN transfer of at most
	// maxLen bytes.
	ExchangeUpTo(ctx context.Context, cmd []byte, maxLen int) ([]byte, error)
}

// ReadCmd is the EMUCOM read request: up to maxLen bytes from channel ch.
// The probe answers with a u32 length n followed by n bytes.
func ReadCmd(ch uint32, maxLen int) []byte {
	b := []byte{cmdEmucom, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[2:], ch)
	binary.LittleEndian.PutUint32(b[6:], uint32(maxLen))
	return b
}

// WriteCmd is the EMUCOM write request header; the n data bytes follow in a
// second transfer and the probe answers with a u32 count of accepted bytes.
func WriteCmd(ch uint32, n int) []byte {
	b := []byte{cmdEmucom, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[2:], ch)
	binary.LittleEndian.PutUint32(b[6:], uint32(n))
	return b
}

// ErrProtocol marks replies that do not follow the observed protocol.
var ErrProtocol = errors.New("wstk protocol error")

// Emucom reads and writes kit channels over a Link.
type Emucom struct {
	L Link
	// Poll is the pause between empty reads while waiting for data.
	Poll time.Duration
}

// Write sends data to channel ch.
func (e *Emucom) Write(ctx context.Context, ch uint32, data []byte) error {
	if _, err := e.L.Exchange(ctx, WriteCmd(ch, len(data)), 0); err != nil {
		return err
	}
	r, err := e.L.Exchange(ctx, data, 4)
	if err != nil {
		return err
	}
	if n := binary.LittleEndian.Uint32(r); int(n) != len(data) {
		return fmt.Errorf("%w: channel %#x accepted %d of %d bytes", ErrProtocol, ch, n, len(data))
	}
	return nil
}

// Read returns what channel ch holds, at most maxLen bytes; an empty result
// means no data yet.
func (e *Emucom) Read(ctx context.Context, ch uint32, maxLen int) ([]byte, error) {
	b, err := e.L.ExchangeUpTo(ctx, ReadCmd(ch, maxLen), 4+maxLen)
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		more, err := e.L.Exchange(ctx, nil, 4-len(b))
		if err != nil {
			return nil, err
		}
		b = append(b, more...)
	}
	n := int(binary.LittleEndian.Uint32(b))
	data := b[4:]
	if n > maxLen || len(data) > n {
		return nil, fmt.Errorf("%w: channel %#x read of %d announced %d bytes and carried %d", ErrProtocol, ch, maxLen, n, len(data))
	}
	// The rest arrives in several transfers. Each read asks for one byte
	// more than is left so that the zero-length packet the probe sends after
	// a transfer of whole packets ends the read; left pending, it blocks the
	// probe's next command.
	for len(data) < n {
		rest, err := e.L.ExchangeUpTo(ctx, nil, n-len(data)+1)
		if err != nil {
			return nil, err
		}
		if len(data)+len(rest) > n {
			return nil, fmt.Errorf("%w: channel %#x read announced %d bytes and carried more", ErrProtocol, ch, n)
		}
		data = append(data, rest...)
	}
	return data, nil
}

// ReadFull reads exactly n bytes from a stream channel, polling until timeout.
func (e *Emucom) ReadFull(ctx context.Context, ch uint32, n int, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	out := make([]byte, 0, n)
	for len(out) < n {
		b, err := e.Read(ctx, ch, n-len(out))
		if err != nil {
			return out, err
		}
		if len(b) > 0 {
			out = append(out, b...)
			continue
		}
		if time.Now().After(deadline) {
			return out, fmt.Errorf("%w: channel %#x: %d of %d bytes before timeout", ErrProtocol, ch, len(out), n)
		}
		if err := sleep(ctx, e.poll()); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (e *Emucom) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return 2 * time.Millisecond
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
