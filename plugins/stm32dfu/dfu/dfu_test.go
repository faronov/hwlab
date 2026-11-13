package dfu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/plugins/stm32dfu/sim"
)

func TestParseMemory(t *testing.T) {
	m, err := ParseMemory("@Internal Flash  /0x08000000/256*04Kg")
	if err != nil {
		t.Fatal(err)
	}
	sg := m.Segments[0]
	if m.Name != "Internal Flash" || m.Start() != 0x08000000 || m.End() != 0x08100000 || sg.PageSize != 4096 || !sg.Readable() || !sg.Erasable() || !sg.Writable() {
		t.Fatalf("flash: %+v", m)
	}
	ob, err := ParseMemory("@Option Bytes  /0x1FFF8000/01*128 e")
	if err != nil {
		t.Fatal(err)
	}
	if s := ob.Segments[0]; ob.End() != 0x1FFF8080 || s.PageSize != 128 || !s.Readable() || s.Erasable() || !s.Writable() || s.AttrString() != "r-w" {
		t.Fatalf("option bytes: %+v %s", ob.Segments, s.AttrString())
	}
	multi, err := ParseMemory("@Internal Flash  /0x08000000/04*016Kg,01*064Kg,07*128Kg")
	if err != nil || len(multi.Segments) != 3 || multi.End() != 0x08100000 || multi.Segments[1].Start != 0x08010000 {
		t.Fatalf("multi: %+v %v", multi, err)
	}
	if sgm, ok := multi.Find(0x08020000); !ok || sgm.PageSize != 128<<10 {
		t.Fatalf("find: %+v %v", sgm, ok)
	}
	for _, bad := range []string{"Internal Flash/0x08000000/4*1Kg", "@x/0x08000000/4*1Kz", "@x/0xZZ/4*1Kg", "@x/0x08000000/4*1Qg", "@x/0xFFFFF000/2*4Kg"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseConfig(t *testing.T) {
	d := sim.New(sim.Config{SFSA: 0xCB, Xfer: 2048})
	b, err := d.Port().ControlIn(context.Background(), 0x80, 6, 0x0200, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ci.Alts) != 3 || ci.Functional == nil || ci.Functional.TransferSize != 2048 || !ci.Functional.Has(AttrCanUpload) || !ci.Functional.Has(AttrWillDetach) || ci.Alts[1].IString != 5 {
		t.Fatalf("%+v %+v", ci, ci.Functional)
	}
}

func newClient(t *testing.T, c sim.Config) (*Client, *sim.Device) {
	t.Helper()
	d := sim.New(c)
	cl := New(d.Port(), 0, 1024)
	cl.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := cl.SelectAlt(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	return cl, d
}

func TestGuardRefusesWithoutSending(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	if err := cl.SetAddress(ctx, 0x08000000); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		block uint16
		data  []byte
	}{
		{"mass erase", 0, []byte{CmdErase}},
		{"read unprotect", 0, []byte{CmdReadUnprotect}},
		{"get commands via download", 0, []byte{CmdGetCommands}},
		{"page erase, window closed", 0, []byte{CmdErase, 0, 0, 0, 0x08}},
		{"data, window closed", 2, make([]byte, 8)},
		{"block 1", 1, make([]byte, 8)},
		{"zero-length outside leave", 2, nil},
	}
	for _, c := range cases {
		if err := cl.dnload(ctx, c.block, c.data, time.Second); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	cl.SetWriteWindow(0, 0x08000000, 0x08000000+0xCB*4096)
	for _, c := range []struct {
		name  string
		block uint16
		data  []byte
	}{
		{"page erase at the secure start", 0, []byte{CmdErase, 0x00, 0xB0, 0x0C, 0x08}},
		{"page erase below flash", 0, []byte{CmdErase, 0, 0xF0, 0xFF, 0x07}},
		{"mass erase with window open", 0, []byte{CmdErase}},
	} {
		if err := cl.dnload(ctx, c.block, c.data, time.Second); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Data crossing the secure start.
	if err := cl.SetAddress(ctx, 0x08000000+0xCB*4096-8); err != nil {
		t.Fatal(err)
	}
	if err := cl.dnload(ctx, 2, make([]byte, 16), time.Second); !errors.Is(err, ErrRefused) {
		t.Errorf("crossing: %v", err)
	}
	// The option-bytes alternate setting: even a pointer inside the window
	// on another alt is refused.
	if err := cl.SelectAlt(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := cl.SetAddress(ctx, 0x08000000); err != nil {
		t.Fatal(err)
	}
	if err := cl.dnload(ctx, 2, make([]byte, 8), time.Second); !errors.Is(err, ErrRefused) {
		t.Errorf("option-bytes alt: %v", err)
	}
	if err := cl.dnload(ctx, 0, []byte{CmdErase, 0, 0, 0, 0x08}, time.Second); !errors.Is(err, ErrRefused) {
		t.Errorf("erase on option-bytes alt: %v", err)
	}
	if cl.Downloads != 3 || d.UnsafeCount() != 0 || d.Erases != 0 || d.Programs != 0 {
		t.Fatalf("downloads %d (want the 3 set-address commands), unsafe %d, erases %d, programs %d", cl.Downloads, d.UnsafeCount(), d.Erases, d.Programs)
	}
	for _, l := range d.Log {
		if strings.HasPrefix(l, "DNLOAD") && !strings.HasPrefix(l, "DNLOAD 0 21") {
			t.Errorf("sent %s", l)
		}
	}
}

func TestReadWriteErase(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xF4})
	d.ExtraBusy = 2
	cl.SetWriteWindow(0, 0x08000000, 0x08000000+0xF4*4096)
	if err := cl.ErasePage(ctx, 0x08001000); err != nil {
		t.Fatal(err)
	}
	img := make([]byte, 4096)
	for i := range img {
		img[i] = byte(i*3 + 1)
	}
	if err := cl.Write(ctx, 0x08000000, img); !errors.Is(err, ErrDevice) {
		t.Fatal("programmed a non-erased page")
	}
	if err := cl.EnsureIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cl.Write(ctx, 0x08001000, img); err != nil {
		t.Fatal(err)
	}
	got, err := cl.Read(ctx, 0x08001000, 4096)
	if err != nil || string(got) != string(img) {
		t.Fatalf("read back: %v", err)
	}
	// A read across 32 KiB windows and a failing upload that recovers.
	if _, err := cl.Read(ctx, 0x08000000, 70000); err != nil {
		t.Fatal(err)
	}
	d.FailUploadAt = 0x08000400
	if _, err := cl.Read(ctx, 0x08000000, 4096); err == nil {
		t.Fatal("failed upload succeeded")
	}
	d.FailUploadAt = 0
	if st, err := cl.GetState(ctx); err != nil || st != StateIdle {
		t.Fatalf("after failure: %v %v", st, err)
	}
	// Uploads from the secure area fail on the device too.
	if _, err := cl.Read(ctx, 0x08000000+0xF4*4096, 16); err == nil {
		t.Fatal("secure read")
	}
	if d.UnsafeCount() != 0 || d.Erases != 1 {
		t.Fatalf("unsafe %d erases %d", d.UnsafeCount(), d.Erases)
	}
}

func TestLeave(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	d.SetSRAM(0x20004000, []byte{0x00, 0x00, 0x03, 0x20, 0x01, 0x41, 0x00, 0x20})
	res, err := cl.Leave(ctx, 0x20004000)
	if err != nil || !res.Manifest || res.Note != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if !d.Gone() || d.Leaves != 1 || d.UnsafeCount() != 0 || d.JumpBase != 0x20004000 || d.JumpSP != 0x20030000 || d.JumpPC != 0x20004101 {
		t.Fatalf("gone %v leaves %d jump 0x%08x sp 0x%08x pc 0x%08x", d.Gone(), d.Leaves, d.JumpBase, d.JumpSP, d.JumpPC)
	}
	// Set-address, its two GETSTATUS, the zero-length download (wValue 2)
	// and the manifestation GETSTATUS.
	tail := strings.Join(d.Log[len(d.Log)-4:], "|")
	if tail != "GETSTATUS|GETSTATUS|DNLOAD 2 |GETSTATUS" {
		t.Fatalf("log tail %q", tail)
	}
}

func TestLeaveOutcomes(t *testing.T) {
	ctx := context.Background()
	// Dropping off the bus instead of answering is the expected
	// non-manifestation-tolerant outcome, reported as unconfirmed.
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	d.LeaveDrop = true
	res, err := cl.Leave(ctx, 0x08000000)
	if err != nil || res.Manifest || !strings.Contains(res.Note, "dfuMANIFEST was not observed") || !d.Gone() {
		t.Fatalf("drop: %+v %v", res, err)
	}
	// Any state but dfuMANIFEST is a protocol error and the device stays.
	cl, d = newClient(t, sim.Config{SFSA: 0xCB})
	d.LeaveState = uint8(StateDnloadIdle)
	if _, err := cl.Leave(ctx, 0x08000000); !errors.Is(err, ErrProtocol) || d.Gone() || d.Leaves != 0 {
		t.Fatalf("wrong state: %v gone %v", err, d.Gone())
	}
	// Leave without a selected alternate setting is refused before sending.
	d = sim.New(sim.Config{SFSA: 0xCB})
	cl = New(d.Port(), 0, 1024)
	if _, err := cl.Leave(ctx, 0x08000000); !errors.Is(err, ErrRefused) || d.Leaves != 0 {
		t.Fatalf("no alt: %v", err)
	}
}

// Every command is a DNLOAD followed by two GETSTATUS: the first starts it
// (dfuDNBUSY), the second reports dfuDNLOAD-IDLE.
func TestCommandTakesTwoGetStatus(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	n := len(d.Log)
	if err := cl.SetAddress(ctx, 0x08001000); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.Log[n:], "|"); got != "DNLOAD 0 2100100008|GETSTATUS|GETSTATUS" {
		t.Fatalf("log %q", got)
	}
	if st, err := cl.GetState(ctx); err != nil || st != StateDnloadIdle {
		t.Fatalf("state %v %v", st, err)
	}
}

func TestErrorClasses(t *testing.T) {
	ctx := context.Background()
	cl, _ := newClient(t, sim.Config{SFSA: 0xCB})
	// errTARGET: an address the bootloader does not serve.
	if _, err := cl.Read(ctx, 0x30000000, 16); !errors.Is(err, ErrTarget) || !errors.Is(err, ErrDevice) {
		t.Fatalf("bad address: %v", err)
	}
	if st, err := cl.GetState(ctx); err != nil || st != StateIdle {
		t.Fatalf("after errTARGET: %v %v", st, err)
	}
	// errVENDOR: read protection.
	cl, d := newClient(t, sim.Config{SFSA: 0xCB, RDPActive: true})
	if _, err := cl.Read(ctx, 0x08000000, 16); !errors.Is(err, ErrProtected) || errors.Is(err, ErrTarget) {
		t.Fatalf("rdp read: %v", err)
	}
	cl.SetWriteWindow(0, 0x08000000, 0x08000000+0xCB*4096)
	if err := cl.ErasePage(ctx, 0x08001000); !errors.Is(err, ErrProtected) {
		t.Fatalf("rdp erase: %v", err)
	}
	if st, err := cl.GetState(ctx); err != nil || st != StateIdle || d.Erases != 0 {
		t.Fatalf("after errVENDOR: %v %v", st, err)
	}
	// The command list is still available under RDP.
	cmds, err := cl.GetCommands(ctx)
	if err != nil || string(cmds) != "\x00\x21\x41\x92" {
		t.Fatalf("commands %x %v", cmds, err)
	}
	if CommandName(0x21) != "set-address-pointer" || CommandName(0x92) != "read-unprotect" || CommandName(0x55) != "0x55" {
		t.Fatal("command names")
	}
}

func TestReadSizes(t *testing.T) {
	ctx := context.Background()
	for _, xfer := range []int{1024, 4096, 6} {
		cl, d := newClient(t, sim.Config{SFSA: 0xCB, Xfer: xfer})
		cl.TransferSize = xfer
		want := d.Flash()[:5000]
		for _, n := range []int{2, 3, 1025, 2049, 5000} {
			got, err := cl.Read(ctx, 0x08000000, n)
			if err != nil || string(got) != string(want[:n]) {
				t.Fatalf("xfer %d n %d: %v", xfer, n, err)
			}
		}
		if _, err := cl.Read(ctx, 0x08000000, 1); !errors.Is(err, ErrProtocol) {
			t.Fatalf("1-byte read: %v", err)
		}
		for _, l := range d.Log {
			var v, n int
			if _, err := fmt.Sscanf(l, "UPLOAD %d %d", &v, &n); err == nil && v != 0 && (n < 2 || n > 2048) {
				t.Fatalf("xfer %d: %s", xfer, l)
			}
		}
	}
}

func TestRAMWindow(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	// SetRAMWindow never reaches the bootloader's RAM.
	cl.SetRAMWindow(0, 0x20000000, 0x20030000)
	data := []byte{1, 2, 3, 4, 5}
	if err := cl.Write(ctx, 0x20003FFE, data); !errors.Is(err, ErrRefused) {
		t.Fatalf("boot RAM: %v", err)
	}
	if err := cl.Write(ctx, 0x2002FFFE, data); !errors.Is(err, ErrRefused) {
		t.Fatalf("past the window: %v", err)
	}
	if err := cl.Write(ctx, 0x20004000, data[:1]); !errors.Is(err, ErrRefused) {
		t.Fatalf("1 byte: %v", err)
	}
	if err := cl.SetAddress(ctx, 0x20004000); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		block uint16
		data  []byte
	}{{3, make([]byte, 8)}, {2, make([]byte, 1)}, {2, make([]byte, 2049)}} {
		if err := cl.dnload(ctx, c.block, c.data, time.Second); !errors.Is(err, ErrRefused) {
			t.Fatalf("block %d len %d: %v", c.block, len(c.data), err)
		}
	}
	// Inside the window, including a 1-byte tail.
	big := make([]byte, 2049)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := cl.Write(ctx, 0x20004000, big); err != nil {
		t.Fatal(err)
	}
	if string(d.SRAM(0x20004000, len(big))) != string(big) || d.RAMWrites != 3 {
		t.Fatalf("ram writes %d", d.RAMWrites)
	}
	// The window belongs to one alternate setting.
	if err := cl.SelectAlt(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := cl.Write(ctx, 0x20004000, data); !errors.Is(err, ErrRefused) {
		t.Fatalf("other alt: %v", err)
	}
	cl.CloseWriteWindow()
	if err := cl.SelectAlt(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := cl.Write(ctx, 0x20004000, data); !errors.Is(err, ErrRefused) {
		t.Fatalf("closed: %v", err)
	}
	if d.UnsafeCount() != 0 || d.RAMWrites != 3 {
		t.Fatalf("unsafe %d ram writes %d", d.UnsafeCount(), d.RAMWrites)
	}
}

func TestDeviceErrorIsCleared(t *testing.T) {
	ctx := context.Background()
	cl, d := newClient(t, sim.Config{SFSA: 0xCB})
	cl.SetWriteWindow(0, 0x08000000, 0x08000000+0xCB*4096)
	d.FailProgram = true
	if err := cl.Write(ctx, 0x08002000, make([]byte, 64)); !errors.Is(err, ErrDevice) {
		t.Fatalf("want ErrDevice, got %v", err)
	}
	if st, err := cl.GetState(ctx); err != nil || st != StateIdle {
		t.Fatalf("state %v %v", st, err)
	}
}
