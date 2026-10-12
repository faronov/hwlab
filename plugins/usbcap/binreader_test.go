package usbcapplugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/testutil"
	"github.com/faronov/hwlab/internal/usbcap"
)

func binPacket(id uint64, ev byte, ep int, in bool, data []byte, ts int64) usbcap.Packet {
	return usbcap.Packet{URBID: id, Event: ev, Transfer: usbcap.Bulk, Bus: 2, Device: 106, Endpoint: ep, In: in, Length: uint32(len(data)), Data: data, TimestampNs: ts}
}

// wire is what read() on /dev/usbmonN returns: the 48-byte header, then the payload.
func wire(p usbcap.Packet) []byte {
	enc := p.Encode()
	return append(append([]byte(nil), enc[:wireHeaderLen]...), enc[usbcap.HeaderLen:]...)
}

func TestBinReaderKeepsFullPayloadAndSplitsEvents(t *testing.T) {
	big := bytes.Repeat([]byte{0xA5}, 512)
	pk := []usbcap.Packet{
		binPacket(1, usbcap.Submit, 5, false, []byte{0x01, 0x02}, 1_000_000),
		binPacket(2, usbcap.Complete, 7, true, big, 2_000_000),
	}
	var stream []byte
	for _, p := range pk {
		stream = append(stream, wire(p)...)
	}
	path := filepath.Join(t.TempDir(), "usbmon2")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := newBinReader(f)
	defer func() { testutil.CheckError(t, r.Close()) }()
	// A regular file returns both events in one read, like a large kernel read.
	for i, want := range pk {
		got, err := r.Next(context.Background())
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if got.URBID != want.URBID || got.EndpointAddr() != want.EndpointAddr() || !bytes.Equal(got.Data, want.Data) {
			t.Errorf("packet %d: %+v", i, got)
		}
	}
	if _, err := r.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("want EOF, got %v", err)
	}
	if n, first := r.Skipped(); n != 0 {
		t.Errorf("skipped %d: %s", n, first)
	}
}

func TestBinReaderSkipsGarbageAndHonoursCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usbmon9")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { testutil.CheckError(t, w.Close()) }()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := newBinReader(f)
	defer func() { testutil.CheckError(t, r.Close()) }()
	bad := make([]byte, wireHeaderLen)
	bad[8] = 'X'
	good := wire(binPacket(7, usbcap.Submit, 2, false, []byte{9}, 5_000_000))
	if _, err := w.Write(append(bad, good...)); err != nil {
		t.Fatal(err)
	}
	p, err := r.Next(context.Background())
	if err != nil || p.URBID != 7 {
		t.Fatalf("%+v %v", p, err)
	}
	if n, _ := r.Skipped(); n != 1 {
		t.Errorf("skipped %d", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.Next(ctx); err == nil {
		t.Error("a blocked read ignored the context")
	}
}
