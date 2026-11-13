package dfu_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/plugins/nrf802154/dfu"
	"github.com/faronov/hwlab/plugins/nrf802154/dfu/dfutest"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tiny.zip was made from tiny.hex by nrfutil 8.1.1 / nrf5sdk-tools 1.1.0:
//
//	nrfutil nrf5sdk-tools pkg generate --hw-version 52 --sd-req 0x00 \
//	    --application-version 3 --application tiny.hex tiny.zip
func TestInitPacketMatchesNrfutil(t *testing.T) {
	fromZip, err := dfu.LoadZip(fixture(t, "tiny.zip"))
	if err != nil {
		t.Fatal(err)
	}
	fromHex, err := dfu.LoadHex(fixture(t, "tiny.hex"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromHex.Bin, fromZip.Bin) {
		t.Fatalf("hex image (%d B) differs from nrfutil's bin (%d B)", len(fromHex.Bin), len(fromZip.Bin))
	}
	if !bytes.Equal(fromHex.Init, fromZip.Init) {
		t.Fatalf("init packet differs from nrfutil's:\n got % X\nwant % X", fromHex.Init, fromZip.Init)
	}
	if _, err := dfu.Load("x.bin", nil, 1); err == nil {
		t.Fatal("raw .bin accepted")
	}
}

func TestCheckInitRejectsMismatch(t *testing.T) {
	img, err := dfu.LoadZip(fixture(t, "tiny.zip"))
	if err != nil {
		t.Fatal(err)
	}
	other := bytes.Clone(img.Bin)
	other[200] ^= 1
	if err := dfu.CheckInit(img.Init, other); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("hash mismatch not caught: %v", err)
	}
	if err := dfu.CheckInit(img.Init, other[:100]); err == nil || !strings.Contains(err.Error(), "app_size") {
		t.Fatalf("size mismatch not caught: %v", err)
	}
}

func hexLine(typ byte, addr uint16, payload []byte) string {
	b := append([]byte{byte(len(payload)), byte(addr >> 8), byte(addr), typ}, payload...)
	var sum byte
	for _, c := range b {
		sum += c
	}
	b = append(b, -sum)
	return ":" + strings.ToUpper(fmtHex(b))
}

func fmtHex(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}

func hexImage(base uint32, data []byte) []byte {
	var sb strings.Builder
	upper := uint32(0xFFFFFFFF)
	for i := 0; i < len(data); i += 16 {
		a := base + uint32(i)
		if a>>16 != upper {
			upper = a >> 16
			sb.WriteString(hexLine(4, 0, []byte{byte(upper >> 8), byte(upper)}) + "\n")
		}
		sb.WriteString(hexLine(0, uint16(a), data[i:min(i+16, len(data))]) + "\n")
	}
	sb.WriteString(hexLine(1, 0, nil) + "\n")
	return []byte(sb.String())
}

func vectors(sp, reset uint32, n int) []byte {
	b := make([]byte, n)
	binary.LittleEndian.PutUint32(b, sp)
	binary.LittleEndian.PutUint32(b[4:], reset)
	return b
}

func TestImageGuards(t *testing.T) {
	good := vectors(0x20040000, 0x1009, 64)
	if _, err := dfu.LoadHex(hexImage(0x1000, good), 1); err != nil {
		t.Fatalf("good image rejected: %v", err)
	}
	cases := []struct {
		name, want string
		hex        []byte
	}{
		{"over the MBR", "must start at 0x00001000", hexImage(0x0000, good)},
		{"in the bootloader", "must start at 0x00001000", hexImage(0xDFFF0, good)},
		{"no stack pointer", "not in RAM", hexImage(0x1000, vectors(0x1000, 0x1009, 64))},
		{"ARM reset vector", "not a Thumb address", hexImage(0x1000, vectors(0x20040000, 0x1008, 64))},
		{"reset outside", "not a Thumb address", hexImage(0x1000, vectors(0x20040000, 0x9001, 64))},
	}
	for _, tc := range cases {
		if _, err := dfu.LoadHex(tc.hex, 1); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}
	big := vectors(0x20040000, 0x1009, dfu.BootloaderStart-0x1000+16)
	if _, err := dfu.LoadHex(hexImage(0x1000, big), 1); err == nil || !strings.Contains(err.Error(), "bootloader") {
		t.Errorf("image over the bootloader accepted: %v", err)
	}
	bad := []byte(strings.Replace(string(hexImage(0x1000, good)), ":10", ":11", 1))
	if _, _, err := dfu.ParseHex(bad); err == nil {
		t.Error("malformed record accepted")
	}
	trunc := hexImage(0x1000, good)
	if _, _, err := dfu.ParseHex(trunc[:len(trunc)-12]); err == nil || !strings.Contains(err.Error(), "end-of-file") {
		t.Errorf("truncated hex accepted: %v", err)
	}
}

func TestSlipRoundTrip(t *testing.T) {
	msg := []byte{0x08, 0xC0, 0x01, 0xDB, 0xDC, 0xDD}
	enc := dfu.SlipEncode(msg)
	if !bytes.Equal(enc, []byte{0x08, 0xDB, 0xDC, 0x01, 0xDB, 0xDD, 0xDC, 0xDD, 0xC0}) {
		t.Fatalf("encode % X", enc)
	}
	var got [][]byte
	var d dfu.SlipDecoder
	d.Feed(append([]byte{0xC0, 0xC0}, enc[:3]...), func(f []byte) { got = append(got, bytes.Clone(f)) })
	d.Feed(enc[3:], func(f []byte) { got = append(got, bytes.Clone(f)) })
	if len(got) != 1 || !bytes.Equal(got[0], msg) {
		t.Fatalf("decode %v", got)
	}
}

func TestProgram(t *testing.T) {
	img, err := dfu.LoadZip(fixture(t, "tiny.zip"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reset := range []bool{false, true} {
		fb := &dfutest.Bootloader{MTU: 131, MaxData: 4096, ResetOnLastExecute: reset}
		var prog []int
		c := &dfu.Client{Port: fb, Timeout: 200 * time.Millisecond, Progress: func(d, _ int) { prog = append(prog, d) }}
		res, err := c.Program(context.Background(), img)
		if err != nil {
			t.Fatalf("reset=%v: %v", reset, err)
		}
		if !bytes.Equal(fb.Init, img.Init) || !bytes.Equal(fb.Firmware, img.Bin) {
			t.Fatalf("bootloader received a different init/image")
		}
		if string(fb.Executed) != "\x01\x02\x02" || res.Objects != 2 || res.MTU != 131 || res.Activated == reset {
			t.Fatalf("reset=%v: executed % X, result %+v", reset, fb.Executed, res)
		}
		if !reset && (len(prog) != 2 || prog[1] != len(img.Bin)) {
			t.Fatalf("progress %v", prog)
		}
		if fb.Ops[0] != dfu.OpPing || fb.Ops[1] != dfu.OpSetPRN || fb.Ops[2] != dfu.OpMTUGet {
			t.Fatalf("open sequence % X", fb.Ops[:3])
		}
	}
}

func TestProgramReportsBootloaderErrors(t *testing.T) {
	img, err := dfu.LoadZip(fixture(t, "tiny.zip"))
	if err != nil {
		t.Fatal(err)
	}
	execs := 0
	fb := &dfutest.Bootloader{MTU: 131, MaxData: 4096, Reject: func(op byte) (byte, byte, bool) {
		if op == dfu.OpExecute {
			execs++
			if execs == 1 {
				return dfu.ResExtendedError, 0x05, true
			}
		}
		return 0, 0, false
	}}
	c := &dfu.Client{Port: fb, Timeout: 200 * time.Millisecond}
	_, err = c.Program(context.Background(), img)
	if err == nil || !strings.Contains(err.Error(), "firmware version failure") {
		t.Fatalf("got %v", err)
	}
}

func TestProgramTimesOutWithoutBootloader(t *testing.T) {
	img, _ := dfu.LoadZip(fixture(t, "tiny.zip"))
	c := &dfu.Client{Port: silentPort{}, Timeout: 50 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.Program(ctx, img); err == nil || !strings.Contains(err.Error(), "ping") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("ping retry not bounded")
	}
}

type silentPort struct{}

func (silentPort) Write(p []byte) (int, error) { return len(p), nil }
func (silentPort) Read([]byte) (int, error)    { time.Sleep(time.Millisecond); return 0, nil }
