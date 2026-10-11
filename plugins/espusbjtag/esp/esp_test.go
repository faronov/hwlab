package esp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

var ctx = context.Background()

func regfile(base, size uint32) (rvsim.Device, map[uint32]uint32) {
	m := map[uint32]uint32{}
	return rvsim.Device{Base: base, Size: size, Read: func(o uint32) uint32 { return m[o] }, Write: func(o, v uint32) { m[o] = v }}, m
}

type rig struct {
	dm    *riscv.DM
	fl    *esp.SimFlash
	tgt   *rvsim.Target
	efuse map[uint32]uint32
}

func newRig(t *testing.T, size int, batch bool) *rig {
	t.Helper()
	fl := esp.NewSimFlash(size)
	r := &rig{fl: fl}
	efuseDev, efuse := regfile(0x600B0800, 0x100)
	r.efuse = efuse
	tg0, _ := regfile(0x60008000, 0x100)
	tg1, _ := regfile(0x60009000, 0x100)
	lp, _ := regfile(0x600B1C00, 0x100)
	cfg := rvsim.Config{
		Harts:   1,
		Memory:  []rvsim.Region{{Base: esp.HPSRAMBase, Data: make([]byte, 4096)}, {Base: esp.ChipMagicAddr &^ 0xFFF, Data: make([]byte, 4096)}},
		Devices: []rvsim.Device{fl.Device(), efuseDev, tg0, tg1, lp},
	}
	r.tgt = rvsim.New(cfg)
	var port jtag.Port = rvsim.Port{T: r.tgt}
	if batch {
		port = &rvsim.BatchPort{Port: rvsim.Port{T: r.tgt}}
	}
	dtm, err := riscv.NewDTM(ctx, jtag.NewDriver(port), 5)
	if err != nil {
		t.Fatal(err)
	}
	r.dm = riscv.NewDM(dtm)
	r.dm.Timeout = 500 * time.Millisecond
	if _, err := r.dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) flash(t *testing.T) (*esp.Flash, esp.Info) {
	t.Helper()
	f, err := esp.OpenFlash(ctx, r.dm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close(ctx) })
	info, err := f.ReadInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return f, info
}

func fill(b []byte, seed int64) {
	rand.New(rand.NewSource(seed)).Read(b)
}

func TestInfoAndSize(t *testing.T) {
	r := newRig(t, 1<<20, false)
	_, info := r.flash(t)
	if info.JEDEC != "ef4014" || info.SizeBytes != 1<<20 || !strings.Contains(info.SizeBasis, "SFDP") || info.SFDPSize != 1<<20 || info.JEDECSize != 1<<20 {
		t.Fatalf("%+v", info)
	}
	if info.Manufacturer != "Winbond" {
		t.Fatal(info.Manufacturer)
	}
}

func TestInfoNoChip(t *testing.T) {
	r := newRig(t, 1<<20, false)
	r.fl.ID = [3]byte{0xFF, 0xFF, 0xFF}
	f, _ := esp.OpenFlash(ctx, r.dm)
	defer f.Close(ctx)
	if _, err := f.ReadInfo(ctx); err == nil || !strings.Contains(err.Error(), "no flash") {
		t.Fatal(err)
	}
}

func TestReadAnyRange(t *testing.T) {
	for _, batch := range []bool{false, true} {
		r := newRig(t, 1<<20, batch)
		fill(r.fl.Mem, 1)
		f, _ := r.flash(t)
		for _, c := range []struct {
			a uint32
			n int
		}{{0, 4096}, {0, 1}, {1, 3}, {63, 130}, {4095, 2}, {0x1234, 777}, {1<<20 - 5, 5}, {100, 6 * 64 * 2}} {
			got, err := f.Read(ctx, c.a, c.n)
			if err != nil {
				t.Fatalf("%+v: %v", c, err)
			}
			if !bytes.Equal(got, r.fl.Mem[c.a:int(c.a)+c.n]) {
				t.Fatalf("batch=%v %+v: mismatch", batch, c)
			}
		}
	}
}

func TestReadErrors(t *testing.T) {
	r := newRig(t, 1<<20, false)
	f, _ := r.flash(t)
	for _, c := range []struct {
		a uint32
		n int
	}{{0, 0}, {0, -1}, {1 << 20, 1}, {1<<20 - 1, 2}, {0xFFFFFFFF, 2}} {
		if _, err := f.Read(ctx, c.a, c.n); !errors.Is(err, esp.ErrRange) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

func TestEraseProgramVerify(t *testing.T) {
	r := newRig(t, 1<<20, false)
	fill(r.fl.Mem, 2)
	f, _ := r.flash(t)
	if err := f.Erase(ctx, 100, 4096, nil); !errors.Is(err, esp.ErrAlign) {
		t.Fatal(err)
	}
	if err := f.Erase(ctx, 0x1000, 100, nil); !errors.Is(err, esp.ErrAlign) {
		t.Fatal(err)
	}
	if err := f.Erase(ctx, 0x10000, 0x20000, nil); err != nil { // two aligned blocks
		t.Fatal(err)
	}
	if err := f.Erase(ctx, 0x30000, 0x3000, nil); err != nil { // sectors
		t.Fatal(err)
	}
	if err := f.VerifyErased(ctx, 0x10000, 0x20000); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3000)
	fill(data, 3)
	if err := f.Program(ctx, 0x10010, data, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Verify(ctx, 0x10010, data); err != nil {
		t.Fatal(err)
	}
	data[1000] ^= 0x10
	if err := f.Verify(ctx, 0x10010, data); !errors.Is(err, esp.ErrVerify) {
		t.Fatalf("corrupted verify gave %v", err)
	}
	if err := f.VerifyErased(ctx, 0x10000, 0x1000); !errors.Is(err, esp.ErrNotErased) {
		t.Fatal(err)
	}
	// Programming over unerased data cannot give the requested bytes.
	fill(r.fl.Mem[0x50000:0x50100], 4)
	if err := f.Program(ctx, 0x50000, bytes.Repeat([]byte{0xF0}, 256), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Verify(ctx, 0x50000, bytes.Repeat([]byte{0xF0}, 256)); !errors.Is(err, esp.ErrVerify) {
		t.Fatalf("want verify failure on a non-erased target, got %v", err)
	}
}

func TestWriteRegionPreservesNeighbours(t *testing.T) {
	r := newRig(t, 1<<20, false)
	fill(r.fl.Mem, 5)
	orig := append([]byte(nil), r.fl.Mem...)
	f, _ := r.flash(t)
	data := make([]byte, 5000)
	fill(data, 6)
	rep, err := f.WriteRegion(ctx, 0x20F00, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Erased != 3 || rep.Sectors != 3 || rep.BytesPreserved != 3*4096-5000 {
		t.Fatalf("%+v", rep)
	}
	want := append([]byte(nil), orig...)
	copy(want[0x20F00:], data)
	if !bytes.Equal(r.fl.Mem, want) {
		t.Fatal("flash differs from expected, or a neighbour was modified")
	}
	// Same data again: nothing happens.
	e0, p0 := r.fl.Erases, r.fl.Programs
	rep, err = f.WriteRegion(ctx, 0x20F00, data, nil)
	if err != nil || rep.Identical != 3 || r.fl.Erases != e0 || r.fl.Programs != p0 {
		t.Fatalf("%+v %v", rep, err)
	}
	// Clearing bits only: programmed without an erase.
	data[10] &= 0x0F
	rep, err = f.WriteRegion(ctx, 0x20F00, data, nil)
	if err != nil || rep.ProgrammedOnly != 1 || rep.Erased != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestWriteRegionErrors(t *testing.T) {
	r := newRig(t, 1<<20, false)
	f, _ := r.flash(t)
	if _, err := f.WriteRegion(ctx, 0, nil, nil); !errors.Is(err, esp.ErrRange) {
		t.Fatal(err)
	}
	if _, err := f.WriteRegion(ctx, 1<<20-2, []byte{1, 2, 3}, nil); !errors.Is(err, esp.ErrRange) {
		t.Fatal(err)
	}
	r.fl.CorruptProgram = true
	if _, err := f.WriteRegion(ctx, 0x1000, bytes.Repeat([]byte{0x5A}, 300), nil); !errors.Is(err, esp.ErrVerify) {
		t.Fatalf("a flash that corrupts writes must fail verification, got %v", err)
	}
	r.fl.CorruptProgram = false
	r.fl.Protected = true
	if _, err := f.WriteRegion(ctx, 0x3000, []byte{0}, nil); err == nil || !strings.Contains(err.Error(), "write enable") {
		t.Fatalf("write-protected flash: %v", err)
	}
}

func TestStuckBusy(t *testing.T) {
	r := newRig(t, 1<<20, false)
	f, _ := r.flash(t)
	r.fl.StuckBusy = true
	ctx2, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := f.WaitReady(ctx2, 100*time.Millisecond); !errors.Is(err, esp.ErrFlashBusy) {
		t.Fatal(err)
	}
}

func TestFlashRestoresSPIRegisters(t *testing.T) {
	r := newRig(t, 1<<20, false)
	dev := r.fl.Device()
	dev.Write(0x04, 0x123456)
	dev.Write(0x18, 0xAAAA5555)
	f, err := esp.OpenFlash(ctx, r.dm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadInfo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if dev.Read(0x04) != 0x123456 || dev.Read(0x18) != 0xAAAA5555 {
		t.Fatalf("SPI registers not restored: addr=%#x user=%#x", dev.Read(0x04), dev.Read(0x18))
	}
}

func TestBackupTwiceAndMismatch(t *testing.T) {
	r := newRig(t, 256<<10, false)
	fill(r.fl.Mem, 7)
	copy(r.fl.Mem[esp.PartitionTableOffset:], esp.BuildPartitionTable([]esp.Partition{{Label: "factory", Type: 0, Subtype: 0, Offset: 0x10000, Size: 0x20000}, {Label: "nvs", Type: 1, Subtype: 2, Offset: 0x9000, Size: 0x6000}}))
	copy(r.fl.Mem[0:], esp.BuildImage(esp.ChipIDESP32C6, 0x40800000, map[uint32][]byte{0x40800000: {1, 2, 3, 4}}, []uint32{0x40800000}))
	f, info := r.flash(t)
	dir := t.TempDir()
	m, path, err := f.Backup(ctx, dir, "SERIAL", info, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	sum := sha256.Sum256(b)
	if !bytes.Equal(b, r.fl.Mem) || m.SHA256 != m.SecondRead || len(m.Partitions) != 2 || m.Bootloader == nil {
		t.Fatalf("%+v", m)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions %v", st.Mode())
	}
	if _, mm, err := esp.LoadBackup(path); err != nil || mm.SHA256 != m.SHA256 || m.SHA256 != hexs(sum[:]) {
		t.Fatal(err)
	}
	// Tampered file is refused.
	b[10] ^= 1
	os.WriteFile(path, b, 0o600)
	if _, _, err := esp.LoadBackup(path); err == nil {
		t.Fatal("a modified backup must be refused")
	}
	// A flash that changes between the two reads must fail loudly.
	dir2 := t.TempDir()
	_, bad, err := f.Backup(ctx, dir2, "S", info, func(done, total int) {
		if done == total {
			r.fl.Mem[100] ^= 0x40
		}
	})
	if !errors.Is(err, esp.ErrBackupMismatch) || !strings.HasSuffix(bad, ".mismatch") {
		t.Fatalf("%v %v", bad, err)
	}
	if ents, _ := filepath.Glob(filepath.Join(dir2, "*.bin")); len(ents) != 0 {
		t.Fatal("a mismatching backup must not be left as a usable .bin")
	}
}

func hexs(b []byte) string {
	const d = "0123456789abcdef"
	o := make([]byte, 0, len(b)*2)
	for _, v := range b {
		o = append(o, d[v>>4], d[v&15])
	}
	return string(o)
}

func TestSecurity(t *testing.T) {
	r := newRig(t, 1<<20, false)
	s, err := esp.ReadSecurity(ctx, r.dm)
	if err != nil || s.CheckFlashWritable() != nil {
		t.Fatal(s, err)
	}
	r.efuse[0x30] = 1 << 9 // not used
	r.efuse[0x34] = 1 << 18
	s, _ = esp.ReadSecurity(ctx, r.dm)
	if !s.FlashEncrypted() || s.CheckFlashWritable() == nil {
		t.Fatalf("%+v", s)
	}
	r.efuse[0x34] = 0
	r.efuse[0x38] = 1 << 20
	s, _ = esp.ReadSecurity(ctx, r.dm)
	if !s.SecureBootEnabled || s.CheckFlashWritable() == nil {
		t.Fatalf("%+v", s)
	}
}

func TestQuiesceRestore(t *testing.T) {
	r := newRig(t, 1<<20, false)
	if _, err := r.dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusWrite, Addr: 0x60008048, Vals: []uint32{1<<31 | 5}}}); err != nil {
		t.Fatal(err)
	}
	q, err := esp.Quiesce(ctx, r.dm)
	if err != nil || q.TIMG0 != 1<<31|5 {
		t.Fatal(q, err)
	}
	v, _ := esp.ReadWord(ctx, r.dm, 0x60008048)
	if v&(1<<31) != 0 {
		t.Fatal("watchdog still enabled")
	}
	if err := q.Restore(ctx, r.dm); err != nil {
		t.Fatal(err)
	}
	v, _ = esp.ReadWord(ctx, r.dm, 0x60008048)
	if v != 1<<31|5 {
		t.Fatalf("%#x", v)
	}
}

func TestClassify(t *testing.T) {
	cases := map[uint32]esp.Class{
		0x40800000: esp.ClassSRAM, 0x4087FFFF: esp.ClassSRAM, 0x50000000: esp.ClassSRAM, 0x50003FFF: esp.ClassSRAM,
		0x40000000: esp.ClassROM, 0x42000000: esp.ClassFlashMap, 0x60008000: esp.ClassPeripheral, 0x10: esp.ClassUnmapped, 0x40880000: esp.ClassUnmapped,
	}
	for a, c := range cases {
		if got := esp.Classify(a); got != c {
			t.Errorf("%#x: %s want %s", a, got, c)
		}
	}
	if !esp.WritableSRAM(0x40800000, 16) || esp.WritableSRAM(0x4087FFF0, 32) || esp.WritableSRAM(0x40000000, 4) ||
		esp.WritableSRAM(0x42000000, 4) || esp.WritableSRAM(0x60000000, 4) || esp.WritableSRAM(0x40800000, 0) ||
		esp.WritableSRAM(0x50003FF0, 0x20) || esp.WritableSRAM(0xFFFFFFF0, 0x20) {
		t.Fatal("WritableSRAM boundaries")
	}
}

func TestEvidence(t *testing.T) {
	r := newRig(t, 1<<20, false)
	ev, err := esp.ReadEvidence(ctx, r.dm)
	if err != nil || ev.MagicMatchesC6 {
		t.Fatal(ev, err)
	}
	if err := r.dm.WriteMem(ctx, esp.ChipMagicAddr, []byte{0x6F, 0x80, 0xE0, 0x2C}); err != nil {
		t.Fatal(err)
	}
	if ev, _ = esp.ReadEvidence(ctx, r.dm); !ev.MagicMatchesC6 {
		t.Fatal(ev)
	}
}

func TestPartitionTable(t *testing.T) {
	ps := []esp.Partition{{Label: "nvs", Type: 1, Subtype: 2, Offset: 0x9000, Size: 0x6000}, {Label: "phy_init", Type: 1, Subtype: 1, Offset: 0xf000, Size: 0x1000}, {Label: "factory", Type: 0, Subtype: 0, Offset: 0x10000, Size: 0x100000}, {Label: "otadata", Type: 1, Subtype: 0, Offset: 0xe000, Size: 0x1000}}
	b := esp.BuildPartitionTable(ps)
	b = append(b, bytes.Repeat([]byte{0xFF}, 0xC00-len(b))...)
	got, err := esp.ParsePartitionTable(b)
	if err != nil || len(got) != 4 || got[2].Kind() != "app/factory" || !got[0].Protected() || !got[1].Protected() || got[2].Protected() || got[3].Protected() {
		t.Fatalf("%+v %v", got, err)
	}
	if o := esp.Overlap(got, 0x9FFF, 2); len(o) != 1 || o[0].Label != "nvs" {
		t.Fatalf("%+v", o)
	}
	if o := esp.Overlap(got, 0x8000, 0x1000); len(o) != 0 {
		t.Fatalf("%+v", o)
	}
	b[40] ^= 1 // inside the second entry: the MD5 entry must catch it
	if _, err := esp.ParsePartitionTable(b); err == nil {
		t.Fatal("corrupt table accepted")
	}
	if _, err := esp.ParsePartitionTable(bytes.Repeat([]byte{0xFF}, 64)); err == nil {
		t.Fatal("empty table accepted")
	}
	if _, err := esp.ParsePartitionTable(bytes.Repeat([]byte{0x00}, 64)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestImageParse(t *testing.T) {
	img := esp.BuildImage(esp.ChipIDESP32C6, 0x40850000, map[uint32][]byte{0x40850000: bytes.Repeat([]byte{7}, 100), 0x40860000: {1, 2, 3}}, []uint32{0x40850000, 0x40860000})
	im, err := esp.ParseImage(img)
	if err != nil || len(im.Segments) != 2 || im.ChipID != esp.ChipIDESP32C6 || im.Entry != 0x40850000 || im.SizeBytes != len(img) {
		t.Fatalf("%+v %v", im, err)
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"truncated", img[:len(img)-30]}, {"truncated header", img[:10]}, {"bad magic", append([]byte{0x00}, img[1:]...)}, {"empty", nil},
		{"truncated checksum", img[:len(img)-1]},
	} {
		if _, err := esp.ParseImage(c.b); !errors.Is(err, esp.ErrImage) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	bad := append([]byte(nil), img...)
	bad[40] ^= 0xFF
	if _, err := esp.ParseImage(bad); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal(err)
	}
}
