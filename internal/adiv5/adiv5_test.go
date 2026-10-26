package adiv5

import (
	"errors"
	"testing"
)

// fakeLink is a minimal ADIv5 DP plus MEM-APs backed by a word map. It is a
// test double for the generic layer, independent of any probe.
type fakeLink struct {
	dpidr    uint32
	aps      []fakeAP
	mem      map[uint32]uint32
	sel      uint32
	ctrl     uint32
	tar      uint32
	csw      uint32
	noACK    bool
	dormant  bool
	swjBits  []bool
	waits    int
	seq      [][]byte
	aborts   int
	transfer int
}

type fakeAP struct{ idr, base, cfg uint32 }

func newFake() *fakeLink {
	return &fakeLink{
		dpidr: 0x2BA01477,
		aps:   []fakeAP{{idr: 0x24770011, base: 0xE00FF003, cfg: 0}},
		mem:   map[uint32]uint32{},
		csw:   0x23000042,
	}
}

func (f *fakeLink) SWJSequence(bits int, data []byte) error {
	f.seq = append(f.seq, append([]byte(nil), data...))
	for i := 0; i < bits; i++ {
		f.swjBits = append(f.swjBits, data[i/8]>>(i%8)&1 == 1)
	}
	if f.dormant && hasSuffix(f.swjBits, DormantWakeTMS()[:148]) {
		f.dormant = false
	}
	return nil
}

func hasSuffix(h, tail []bool) bool {
	if len(h) < len(tail) {
		return false
	}
	h = h[len(h)-len(tail):]
	for i := range tail {
		if h[i] != tail[i] {
			return false
		}
	}
	return true
}

func (f *fakeLink) WriteABORT(uint32) error { f.aborts++; return nil }

func (f *fakeLink) Transfer(ops []Op) ([]uint32, error) {
	f.transfer++
	if f.noACK || f.dormant {
		return nil, &TransferError{Ack: 7, Err: ErrNoACK}
	}
	var out []uint32
	for i, op := range ops {
		if f.waits > 0 {
			f.waits--
			return out, &TransferError{Done: i, Ack: 2, Err: ErrWait}
		}
		if !op.AP {
			switch {
			case op.Read && op.Reg == dpDPIDR:
				out = append(out, f.dpidr)
			case op.Read && op.Reg == dpCtrl:
				out = append(out, f.ctrl)
			case !op.Read && op.Reg == dpCtrl:
				f.ctrl = op.Value<<0 | (op.Value&ctrlCDBGPwrReq)<<1 | (op.Value&ctrlCSYSPwrReq)<<1
			case !op.Read && op.Reg == dpSelect:
				f.sel = op.Value
			default:
				return out, &TransferError{Done: i, Ack: 4, Err: ErrFault}
			}
			continue
		}
		ap := int(f.sel >> 24)
		reg := byte(f.sel&0xF0) | op.Reg&0x0C
		if op.Reg >= 0xF0 {
			reg = byte(f.sel&0xF0) | op.Reg&0x0F
		}
		var a fakeAP
		if ap < len(f.aps) {
			a = f.aps[ap]
		}
		switch {
		case op.Read && reg == apIDR:
			out = append(out, a.idr)
		case op.Read && reg == apBase:
			out = append(out, a.base)
		case op.Read && reg == apCFG:
			out = append(out, a.cfg)
		case op.Read && reg == apCSW:
			out = append(out, f.csw)
		case !op.Read && reg == apCSW:
			f.csw = op.Value
		case !op.Read && reg == apTAR:
			f.tar = op.Value
		case op.Read && reg == apDRW:
			out = append(out, f.mem[f.tar&^3])
			f.tar += 4
		case !op.Read && reg == apDRW:
			f.mem[f.tar&^3] = op.Value
			f.tar += 4
		default:
			return out, &TransferError{Done: i, Ack: 4, Err: ErrFault}
		}
	}
	return out, nil
}

func (f *fakeLink) BlockRead(ap bool, reg byte, words int) ([]uint32, error) {
	out := make([]uint32, words)
	for i := range out {
		out[i] = f.mem[f.tar&^3]
		f.tar += 4
	}
	return out, nil
}

func (f *fakeLink) BlockWrite(ap bool, reg byte, data []uint32) error {
	for _, v := range data {
		f.mem[f.tar&^3] = v
		f.tar += 4
	}
	return nil
}

func TestConnectSequenceAndIDs(t *testing.T) {
	f := newFake()
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tg.DPIDR != 0x2BA01477 || tg.APIDR != 0x24770011 || tg.AP() != 0 {
		t.Fatalf("dpidr %#x apidr %#x", tg.DPIDR, tg.APIDR)
	}
	if len(f.seq) != 4 || len(f.seq[1]) != 2 || f.seq[1][0] != 0x9E || f.seq[1][1] != 0xE7 {
		t.Fatalf("line reset / JTAG-to-SWD sequence wrong: %x", f.seq)
	}
	if f.aborts == 0 {
		t.Error("sticky flags were not cleared")
	}
}

func TestConnectNoTargetIsExplicit(t *testing.T) {
	f := newFake()
	f.noACK = true
	_, err := Connect(f, 0)
	if !errors.Is(err, ErrNoACK) {
		t.Fatalf("want ErrNoACK, got %v", err)
	}
}

func TestConnectWakesDormantDP(t *testing.T) {
	f := newFake()
	f.dormant = true
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tg.WokeFromDormant || tg.DPIDR != 0x2BA01477 {
		t.Fatalf("woke %v dpidr %#x", tg.WokeFromDormant, tg.DPIDR)
	}
	if tg2, err := Connect(f, 0); err != nil || tg2.WokeFromDormant {
		t.Fatalf("an awake DP was woken again: %v %v", err, tg2)
	}
}

type wakerLink struct {
	*fakeLink
	wakes int
}

func (w *wakerLink) WakeDormant() error { w.wakes++; w.dormant = false; return nil }

func TestConnectUsesLinkDormantWaker(t *testing.T) {
	w := &wakerLink{fakeLink: newFake()}
	w.dormant = true
	if _, err := Connect(w, 0); err != nil {
		t.Fatal(err)
	}
	if w.wakes != 1 || len(w.swjBits) != 2*(56+16+56+8) {
		t.Fatalf("wakes %d, %d SWJ bits", w.wakes, len(w.swjBits))
	}
}

func TestConnectRejectsEmptyDPIDRAndMissingAP(t *testing.T) {
	for _, v := range []uint32{0, 0xFFFFFFFF} {
		f := newFake()
		f.dpidr = v
		if _, err := Connect(f, 0); err == nil {
			t.Errorf("DPIDR %#x accepted", v)
		}
	}
	if _, err := Connect(newFake(), 3); err == nil {
		t.Error("absent AP 3 accepted")
	}
}

func TestWaitIsRetriedAndCounted(t *testing.T) {
	f := newFake()
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.mem[0x20000000] = 0xCAFEF00D
	f.waits = 3
	v, err := tg.ReadWord(0x20000000)
	if err != nil || v != 0xCAFEF00D {
		t.Fatalf("v=%#x err=%v", v, err)
	}
}

func TestPersistentWaitFails(t *testing.T) {
	f := newFake()
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.waits = 1 << 20
	if _, err := tg.ReadWord(0x20000000); !errors.Is(err, ErrWait) {
		t.Fatalf("want ErrWait after bounded retries, got %v", err)
	}
}

func TestUnalignedWordIsRefused(t *testing.T) {
	tg, _ := Connect(newFake(), 0)
	if _, err := tg.ReadWord(0x20000002); err == nil {
		t.Error("unaligned word read accepted")
	}
	if err := tg.WriteWord(0x20000001, 1); err == nil {
		t.Error("unaligned word write accepted")
	}
}

func TestBulkRoundTripCrossesAutoIncrementWindow(t *testing.T) {
	f := newFake()
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// 0x200003F0 crosses a 1 KiB TAR window
	if err := tg.WriteMem(0x200003F0, data); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(0x200003F0, len(data))
	if err != nil || string(got) != string(data) {
		t.Fatalf("round trip differs (err=%v)", err)
	}
}

func TestReadMemRefusesAddressSpaceOverflow(t *testing.T) {
	tg, _ := Connect(newFake(), 0)
	if _, err := tg.ReadMem(0xFFFFFFF0, 32); err == nil {
		t.Error("read past the 32-bit address space accepted")
	}
	if _, err := tg.ReadMem(0, -1); err == nil {
		t.Error("negative length accepted")
	}
}

func TestScanAPsStopsAtFirstAbsent(t *testing.T) {
	f := newFake()
	f.aps = []fakeAP{{idr: 0x24770011, base: 0xE00FF003}, {idr: 0x14770001, base: 0xFFFFFFFF, cfg: 1}}
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	aps, err := tg.ScanAPs(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(aps) != 2 {
		t.Fatalf("found %d APs: %+v", len(aps), aps)
	}
	if aps[0].Class != "MEM-AP" || aps[0].Type != "AHB3-AP" || aps[0].Base != 0xE00FF003 {
		t.Errorf("AP0 %+v", aps[0])
	}
	if aps[1].Index != 1 || aps[1].CFG != 1 {
		t.Errorf("AP1 %+v", aps[1])
	}
	if tg.AP() != 0 {
		t.Errorf("scan left AP %d selected", tg.AP())
	}
	if _, err := tg.ReadWord(0x20000000); err != nil {
		t.Errorf("target unusable after a scan: %v", err)
	}
}

func TestDecodeIDRClasses(t *testing.T) {
	for _, c := range []struct {
		idr        uint32
		class, typ string
	}{
		{0x24770011, "MEM-AP", "AHB3-AP"},
		{0x84770014, "MEM-AP", "AHB5-AP"},
		{0x00002000, "COM-AP", "JTAG-AP"},
		{0x0000000F, "no class", "type 15"},
	} {
		if class, typ := decodeIDR(c.idr); class != c.class || typ != c.typ {
			t.Errorf("%#x = %q/%q, want %q/%q", c.idr, class, typ, c.class, c.typ)
		}
	}
}

// romTable lays out a Cortex-M ROM table at 0xE00FF000 with an SCS, a DWT, an
// FPB and one entry that is not a CoreSight component.
func (f *fakeLink) romTable() {
	comp := func(base uint32, class uint32, pid uint32) {
		f.mem[base+0xFF0] = 0x0D
		f.mem[base+0xFF4] = class << 4
		f.mem[base+0xFF8] = 0x05
		f.mem[base+0xFFC] = 0xB1
		f.mem[base+0xFE0] = pid & 0xFF
		f.mem[base+0xFE4] = pid >> 8 & 0xF
		f.mem[base+0xFE8] = 0
		f.mem[base+0xFEC] = 0
		f.mem[base+0xFD0] = 0
		f.mem[base+0xFD4] = 0
		f.mem[base+0xFD8] = 0
		f.mem[base+0xFDC] = 0
	}
	const rom = 0xE00FF000
	f.mem[rom+0] = (0xE000E000-rom)&0xFFFFF000 | 3
	f.mem[rom+4] = (0xE0001000-rom)&0xFFFFF000 | 3
	f.mem[rom+8] = (0xE0002000-rom)&0xFFFFF000 | 3
	f.mem[rom+12] = 0x00000002 // present bit clear: skipped
	f.mem[rom+16] = (0xE0040000-rom)&0xFFFFF000 | 3
	comp(0xE000E000, 0xE, 0x00C)
	comp(0xE0001000, 0xE, 0x002)
	comp(0xE0002000, 0xE, 0x00E)
	// 0xE0040000 has no preamble
}

func TestWalkROMNamesComponentsAndFlagsBadEntries(t *testing.T) {
	f := newFake()
	f.romTable()
	tg, err := Connect(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	comps, err := tg.WalkROM(0xE00FF003)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 4 {
		t.Fatalf("components: %+v", comps)
	}
	want := []string{"Cortex-M4 SCS", "Cortex-M3 DWT", "Cortex-M4 FPB"}
	for i, w := range want {
		if comps[i].Name != w {
			t.Errorf("component %d = %q, want %q", i, comps[i].Name, w)
		}
	}
	if comps[0].Address != 0xE000E000 {
		t.Errorf("SCS address %#x", comps[0].Address)
	}
	if comps[3].Name == "" || comps[3].Part != 0 {
		t.Errorf("a non-CoreSight entry must be reported, not named: %+v", comps[3])
	}
}

func TestReadComponentRejectsBadPreamble(t *testing.T) {
	tg, _ := Connect(newFake(), 0)
	if _, err := tg.ReadComponent(0x20000000); err == nil {
		t.Error("memory without a CoreSight preamble accepted")
	}
}
