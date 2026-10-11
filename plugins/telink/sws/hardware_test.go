//go:build hardware

package sws

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/faronov/hwlab/plugins/uart"
)

// Run with: HWLAB_TELINK_HW_PORT=/dev/ttyUSB0 go test -tags=hardware ./plugins/telink/sws -run Hardware -v
//
// READ-ONLY. Needs a Telink SWire programmer on a serial port with a target
// wired to it. Set HWLAB_TELINK_HW_FRAMES=1 to log the raw frames of each
// transaction (the source of the captured-frame vectors in frames_test.go).
// The CPU is stalled for the flash reads and always released again. Nothing
// here writes memory or flash.

type recLink struct {
	Link
	t   *testing.T
	log bool
}

func (r recLink) Write(b []byte) (int, error) {
	if r.log {
		r.t.Logf("TX %s", hex.EncodeToString(b))
	}
	return r.Link.Write(b)
}

func (r recLink) Read(b []byte) (int, error) {
	n, err := r.Link.Read(b)
	if r.log && n > 0 {
		r.t.Logf("RX %s", hex.EncodeToString(b[:n]))
	}
	return n, err
}

func hwProgrammer(t *testing.T) *Programmer {
	t.Helper()
	port := os.Getenv("HWLAB_TELINK_HW_PORT")
	if port == "" {
		t.Skip("HWLAB_TELINK_HW_PORT not set")
	}
	off := false
	m := uart.DefaultMode()
	m.Baud = DefaultBaud
	m.DTR, m.RTS = &off, &off
	sp, err := uart.OpenPort(port, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	p := New(recLink{Link: sp, t: t, log: os.Getenv("HWLAB_TELINK_HW_FRAMES") == "1"})
	p.SetTimeout(300 * time.Millisecond)
	return p
}

func TestHardwareReadOnlyIdentification(t *testing.T) {
	p := hwProgrammer(t)
	v, err := p.Version()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("programmer: chip %s id 0x%04x firmware 0x%04x swdiv %d", v.Chip, v.ChipID, v.Firmware, v.SWDiv)
	rev, id, err := p.ReadChipID()
	if err != nil {
		t.Fatalf("target does not answer over SWire: %v", err)
	}
	t.Logf("target: id 0x%04x rev 0x%02x (%s)", id, rev, ChipName(id))

	if err := p.Stall(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Go(); err != nil {
			t.Errorf("resume: %v", err)
		}
	})
	jedec, err := p.JEDEC()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("flash JEDEC 0x%06x", jedec)

	first, err := p.FlashRead(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := p.FlashRead(0, 256)
		if err != nil || !bytes.Equal(first, again) {
			t.Fatalf("repeated flash read differs (%v)", err)
		}
	}
	if _, err := p.SWRead(0x800000, 16); err != nil {
		t.Fatal(err)
	}
}
