package can

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []Frame{
		{ID: 0x123, Data: []byte{1, 2, 3}},
		{ID: 0x1ABCDEF0, Extended: true, Data: []byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 1, 2, 3}},
		{ID: 0x7FF, RTR: true, DLC: 4},
		{ID: 0x1FFFFFFF, Extended: true, FD: true, BRS: true, ESI: true, Data: bytes.Repeat([]byte{0xA5}, 64)},
		{ID: 0x100, FD: true, Data: bytes.Repeat([]byte{1}, 12)},
		{ID: 0x4, Error: true, Data: []byte{0, 0, 0x40, 0, 0, 0, 0, 0}},
		{ID: 0x0},
	}
	for _, f := range cases {
		if err := f.Validate(); err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		got, err := Decode(f.Encode())
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		assertSame(t, "pcap layout", f, got)
		got, err = FromKernel(f.KernelBytes())
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		assertSame(t, "kernel layout", f, got)
	}
}

func assertSame(t *testing.T, what string, want, got Frame) {
	t.Helper()
	if got.ID != want.ID || got.Extended != want.Extended || got.RTR != want.RTR || got.Error != want.Error ||
		got.FD != want.FD || got.BRS != want.BRS || got.ESI != want.ESI || got.DLC != want.DLC || !bytes.Equal(got.Data, want.Data) {
		t.Errorf("%s: got %+v want %+v", what, got, want)
	}
}

func TestEncodeMatchesSocketCANPcapLayout(t *testing.T) {
	// Wireshark's LINKTYPE_CAN_SOCKETCAN: big-endian id word with EFF in bit 31,
	// length, flags, two reserved bytes, data, padded to 16 bytes for classic frames.
	f := Frame{ID: 0x12345678, Extended: true, Data: []byte{0xAA, 0xBB}}
	want := []byte{0x92, 0x34, 0x56, 0x78, 2, 0, 0, 0, 0xAA, 0xBB, 0, 0, 0, 0, 0, 0}
	if got := f.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("encode = % X, want % X", got, want)
	}
	fd := Frame{ID: 0x1, FD: true, BRS: true, Data: make([]byte, 12)}
	if got := fd.Encode(); len(got) != 20 || got[5] != 0x05 {
		t.Fatalf("fd encode = % X", got)
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]Frame{
		"std id over 11 bits": {ID: 0x800},
		"ext id over 29 bits": {ID: 0x20000000, Extended: true},
		"classic 9 bytes":     {ID: 1, Data: make([]byte, 9)},
		"fd 9 bytes":          {ID: 1, FD: true, Data: make([]byte, 9)},
		"fd 65 bytes":         {ID: 1, FD: true, Data: make([]byte, 65)},
		"fd rtr":              {ID: 1, FD: true, RTR: true},
		"brs without fd":      {ID: 1, BRS: true},
		"rtr with data":       {ID: 1, RTR: true, Data: []byte{1}},
		"rtr dlc 9":           {ID: 1, RTR: true, DLC: 9},
	}
	for name, f := range bad {
		if f.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, n := range []int{0, 8, 12, 16, 20, 24, 32, 48, 64} {
		if err := (Frame{ID: 1, FD: true, Data: make([]byte, n)}).Validate(); err != nil {
			t.Errorf("fd length %d: %v", n, err)
		}
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	if _, err := Decode([]byte{0, 0, 0}); err == nil {
		t.Error("short header accepted")
	}
	if _, err := Decode([]byte{0, 0, 0, 1, 5, 0, 0, 0, 1, 2}); err == nil {
		t.Error("truncated payload accepted")
	}
	if _, err := FromKernel(make([]byte, 20)); err == nil {
		t.Error("odd kernel size accepted")
	}
	k := make([]byte, KernelClassicSize)
	k[4] = 9
	if _, err := FromKernel(k); err == nil {
		t.Error("classic length 9 accepted")
	}
}

func TestSummaryAttributesAndPacket(t *testing.T) {
	f := Frame{Interface: "can0", ID: 0x1F, Data: []byte{0xDE, 0xAD}, Own: true}
	if got, want := f.Summary(), "tx can0 01F [2] DE AD"; got != want {
		t.Errorf("summary %q want %q", got, want)
	}
	a := f.Attributes()
	if a["id"] != "0x01F" || a["dir"] != "tx" || a["kind"] != "can" || a["payload"] != "dead" || a["dlc"] != "2" {
		t.Errorf("attributes %v", a)
	}
	back, err := FromPacket(f.Encode(), a)
	if err != nil || back.Interface != "can0" || !back.Own || back.ID != 0x1F {
		t.Errorf("FromPacket: %+v err=%v", back, err)
	}
	if got := (Frame{Interface: "can0", ID: 0x1ABCDE, Extended: true, RTR: true, DLC: 3}).Summary(); got != "rx can0 001ABCDE [3] remote" {
		t.Errorf("rtr summary %q", got)
	}
}

func TestFilters(t *testing.T) {
	f, err := ParseFilter("120:7F0")
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(Frame{ID: 0x125}) || f.Match(Frame{ID: 0x135}) || f.Match(Frame{ID: 0x125, Extended: true}) {
		t.Error("id:mask matching wrong")
	}
	inv, _ := ParseFilter("120~7F0")
	if inv.Match(Frame{ID: 0x125}) || !inv.Match(Frame{ID: 0x135}) {
		t.Error("inverted filter wrong")
	}
	ext, err := ParseFilter("12345678")
	if err != nil || !ext.Extended || ext.Mask != 0x1FFFFFFF || !ext.Match(Frame{ID: 0x12345678, Extended: true}) {
		t.Errorf("extended filter: %+v err=%v", ext, err)
	}
	id, mask := f.KernelPair()
	if id != 0x120 || mask != 0x7F0|0x80000000 {
		t.Errorf("kernel pair %#x %#x", id, mask)
	}
	for _, s := range []string{"", "xyz", "800", "123456789", "123:zz"} {
		if _, err := ParseFilter(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if !MatchAny(nil, Frame{ID: 1}) || MatchAny([]Filter{f}, Frame{ID: 0x500}) {
		t.Error("MatchAny wrong")
	}
}
