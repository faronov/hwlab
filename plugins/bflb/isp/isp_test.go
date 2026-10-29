package isp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
	"time"
)

// testHelper builds a RAM boot image with one segment at HelperEntry.
func testHelper(size int) []byte {
	b := SimHelperImage()
	img := append([]byte(nil), b[:bootHeaderLen+segHeaderLen]...)
	seg := img[bootHeaderLen:]
	binary.LittleEndian.PutUint32(seg[4:], uint32(size))
	binary.LittleEndian.PutUint32(seg[12:], crc32.ChecksumIEEE(seg[:12]))
	return append(img, bytes.Repeat([]byte{0x13}, size)...)
}

func helperClient(t *testing.T) (*Client, *Sim) {
	t.Helper()
	s := NewSim()
	c := New(s)
	bi, err := c.Connect()
	if err != nil {
		t.Fatal(err)
	}
	img, err := ParseImage(testHelper(9000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadHelper(img, bi); err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestParseImage(t *testing.T) {
	img, err := ParseImage(testHelper(5000))
	if err != nil || len(img.Segments) != 1 || img.Segments[0].Dest != HelperEntry || len(img.Segments[0].Data) != 5000 {
		t.Fatalf("parse: %v %+v", err, img)
	}
	bad := testHelper(100)
	copy(bad, "XXXX")
	if _, err := ParseImage(bad); err == nil {
		t.Fatal("accepted an image without BFNP magic")
	}
	if _, err := ParseImage(testHelper(100)[:250]); err == nil {
		t.Fatal("accepted a truncated image")
	}
	if _, err := ParseImage(append(testHelper(100), 0)); err == nil {
		t.Fatal("accepted trailing bytes")
	}
}

func TestConnectLoadHelperAndIdentify(t *testing.T) {
	c, s := helperClient(t)
	if !s.HelperRunning() {
		t.Fatal("helper not running in the sim")
	}
	mac, err := c.MAC()
	if err != nil || !bytes.Equal(mac, s.MAC[:]) {
		t.Fatalf("mac %x %v", mac, err)
	}
	id, size, err := c.JEDEC()
	if err != nil || id != 0xc22014 || size != 1<<20 {
		t.Fatalf("jedec %06x %d %v", id, size, err)
	}
}

// After a port reopen the resident helper waits for a handshake again; a
// probe must find it without desynchronising a helper in command mode.
func TestConnectProbesBeforeHandshake(t *testing.T) {
	c, s := helperClient(t)
	hs := c.Stats().Handshakes
	bi, err := c.Connect()
	if err != nil || !bi.HelperRunning() {
		t.Fatalf("connect in command mode: %v %+v", err, bi)
	}
	if c.Stats().Handshakes != hs {
		t.Fatal("connect handshook although the helper was in command mode")
	}
	s.Reopen()
	bi, err = c.Connect()
	if err != nil || !bi.HelperRunning() || c.Stats().Handshakes != hs+1 {
		t.Fatalf("connect after reopen: %v %+v handshakes %d", err, bi, c.Stats().Handshakes)
	}
	if _, _, err := c.JEDEC(); err != nil {
		t.Fatal(err)
	}
}

func TestEraseWriteReadSHA(t *testing.T) {
	c, s := helperClient(t)
	data := make([]byte, 3*IOChunk+17)
	for i := range data {
		data[i] = byte(i * 7)
	}
	const addr = 0x2000
	if err := c.Erase(addr, addr+2*Sector-1); err != nil {
		t.Fatal(err)
	}
	// Over USB the helper loses frames of a full packet or more.
	c.Timeout = 50 * time.Millisecond
	if err := c.Write(addr, data); !errors.Is(err, ErrNoAnswer) || s.Lost != 1 {
		t.Fatalf("full-size write over USB: %v (lost %d)", err, s.Lost)
	}
	c.WriteChunk = USBWriteChunk
	if err := c.Write(addr, data); err != nil || s.Lost != 1 {
		t.Fatalf("write: %v (lost %d)", err, s.Lost)
	}
	if err := c.WriteCheck(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(addr, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back differs: %v", err)
	}
	sum, err := c.SHA256(addr, len(data))
	if err != nil || sum != sha256.Sum256(data) {
		t.Fatalf("sha: %v", err)
	}
	if !bytes.Equal(s.Flash[addr:addr+len(data)], data) {
		t.Fatal("sim flash differs")
	}
}

func TestChipErrorIsReported(t *testing.T) {
	c, _ := helperClient(t)
	_, err := c.Read(1<<20-4, 8)
	var ce *ChipError
	if !errors.As(err, &ce) || ce.Code != 0x0504 {
		t.Fatalf("want FL0504, got %v", err)
	}
	// The link stays usable after a refused command.
	if _, _, err := c.JEDEC(); err != nil {
		t.Fatal(err)
	}
}

func TestSignedChipIsRefused(t *testing.T) {
	s := NewSim()
	s.SignRequired = true
	c := New(s)
	bi, err := c.Connect()
	if err != nil {
		t.Fatal(err)
	}
	img, _ := ParseImage(testHelper(100))
	if _, err := c.LoadHelper(img, bi); err == nil {
		t.Fatal("loaded an unsigned helper on a chip that requires signed images")
	}
}

func TestResetReturnsToROM(t *testing.T) {
	c, s := helperClient(t)
	if err := c.Reset(); err != nil || s.HelperRunning() || s.Resets != 1 {
		t.Fatalf("reset: %v helper=%v resets=%d", err, s.HelperRunning(), s.Resets)
	}
}

func TestFrameChecksum(t *testing.T) {
	f := frame(0x32, []byte{0x00, 0x10, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00})
	want := []byte{0x32, 0x20, 0x08, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00}
	if !bytes.Equal(f, want) {
		t.Fatalf("frame % x, want % x", f, want)
	}
}

func TestSyncBytesFor(t *testing.T) {
	if SyncBytesFor(115200) != 34 || SyncBytesFor(9600) != DefaultSyncBytes {
		t.Fatal(SyncBytesFor(115200), SyncBytesFor(9600))
	}
}
