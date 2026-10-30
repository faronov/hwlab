package jlink

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// phySim plays a PHY6252 behind J-Link Commander: 512 KiB flash, the ROM
// erase/write routines and savebin.
type phySim struct {
	flash   []byte
	ram     map[uint32]uint32
	regs    map[string]uint32
	romHex  [3]string
	noBoot  bool   // the boot breakpoint is never hit
	failPC  uint32 // ROM routine that returns an error
	erases  int
	writes  int
	corrupt bool // flip a bit on write
}

func newPhySim() *phySim {
	s := &phySim{flash: bytes.Repeat([]byte{0xFF}, 512<<10)}
	s.romHex = [3]string{"7E B5 64 21 02 91 01 02 01 22 09 0A 52 03 91 42", "10 B5 03 02 01 24 1B 0A 64 03 A3 42 01 D2 10 20", "00 F0 03 FB"}
	return s
}

func (s *phySim) reply(script []string) string {
	var out strings.Builder
	out.WriteString(connectOut)
	s.regs = map[string]uint32{}
	booting := false
	if s.ram == nil {
		s.ram = map[uint32]uint32{}
	}
	num := func(v string) uint32 {
		n, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
		return uint32(n)
	}
	for _, l := range script {
		fmt.Fprintf(&out, "J-Link>%s\n", l)
		f := strings.Fields(l)
		switch f[0] {
		case "w4":
			for i, v := range f[2:] {
				s.ram[num(f[1])+uint32(4*i)] = num(v)
			}
		case "wreg":
			s.regs[f[1]] = num(f[2])
		case "SetPC":
			pc, r0 := num(f[1]), uint32(0)
			a := s.regs["R0"] - phyFlashBase
			switch {
			case a < phyRomMinOffset:
				r0 = 0x10
			case pc == s.failPC:
				r0 = 0xFFFFFFFF
			case pc == phyRomErase:
				s.erases++
				copy(s.flash[a&^0xFFF:], bytes.Repeat([]byte{0xFF}, phySector))
			case pc == phyRomWrite:
				s.writes++
				for i := uint32(0); i < s.regs["R2"]; i += 4 {
					binary.LittleEndian.PutUint32(s.flash[a+i:], s.ram[s.regs["R1"]+i])
				}
				if s.corrupt {
					s.flash[a+7] ^= 1
				}
			}
			s.regs["R0"] = r0
		case "SetBP":
			booting = num(f[1]) == phyBootStop
		case "WaitHalt":
			if booting {
				booting = false
				if s.noBoot {
					out.WriteString("CPU not halted.\n")
				} else {
					fmt.Fprintf(&out, "CPU halted.\nPC = %08X, CycleCnt = 00000000\nR0 = 00000006, R1 = 00000000\n", phyBootStop)
				}
				continue
			}
			fmt.Fprintf(&out, "CPU halted.\nPC = %08X, CycleCnt = 00000000\nR0 = %08X, R1 = 00000000, R2 = 00000000, R3 = 00000000\n", phyStub, s.regs["R0"])
		case "mem32":
			out.WriteString("4000C8A0 = 001340C4 \n")
		case "mem8":
			for i, fp := range phyRomFingerprint {
				if num(f[1]) == fp.addr {
					fmt.Fprintf(&out, "%08X = %s \n", fp.addr, s.romHex[i])
				}
			}
		case "savebin":
			path, _ := strconv.Unquote(f[1])
			a, n := num(f[2])-phyFlashBase, num(f[3])
			_ = os.WriteFile(path, s.flash[a:a+n], 0o600)
			out.WriteString("Reading bytes into file...O.K.\n")
		}
	}
	return out.String()
}

func TestProgramPHY62xxROM(t *testing.T) {
	dir := t.TempDir()
	// sectors 0-1 blank (ROM-protected, skipped), sector 3 blank, 20 data sectors, partial tail
	img := bytes.Repeat([]byte{0xFF}, 22*phySector+100)
	for i := range img {
		if s := i / phySector; s != 0 && s != 1 && s != 3 {
			img[i] = byte(i * 7)
		}
	}
	binF := filepath.Join(dir, "fw.bin")
	if err := os.WriteFile(binF, img, 0o644); err != nil {
		t.Fatal(err)
	}
	sim := newPhySim()
	sim.flash[len(img)] = 0x55 // stale byte in the tail sector must be erased
	f := &fakeCmd{reply: sim.reply}
	p := NewWith(t.TempDir(), f)
	backup := filepath.Join(dir, "backup.bin")
	res, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"target": "Cortex-M0", "path": binF, "address": "0x11000000", "flash_loader": "phy62xx-rom", "backup": backup})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sim.flash[:len(img)], img) || sim.flash[len(img)] != 0xFF {
		t.Error("flash content differs from the image")
	}
	if sim.erases != 21 || sim.writes != 20 {
		t.Errorf("erases=%d writes=%d, want 21/20 (blank sectors below 0x2000 skipped, others only erased)", sim.erases, sim.writes)
	}
	if st, err := os.Stat(backup); err != nil || st.Size() != 512<<10 {
		t.Errorf("backup %v %v", st, err)
	}
	if res["verified"] != true || res["flash_size"] != float64(512<<10) {
		t.Errorf("result %v", res)
	}
	for _, r := range f.runs {
		for _, l := range r.script {
			if strings.HasPrefix(l, "loadfile") || strings.HasPrefix(l, "loadbin") || len(strings.Fields(l)) > 2+phyWordsPerLine {
				t.Fatalf("bad commander line %q", l)
			}
		}
	}
	if got := strings.Join(f.last().script, "|"); got != "r|g" {
		t.Errorf("final script %s", got)
	}
}

func TestProgramPHY62xxROMRefuses(t *testing.T) {
	dir := t.TempDir()
	binF := filepath.Join(dir, "fw.bin")
	if err := os.WriteFile(binF, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	args := func(kv ...any) map[string]any {
		m := map[string]any{"target": "Cortex-M0", "path": binF, "address": "0x0", "flash_loader": "phy62xx-rom"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for name, tc := range map[string]struct {
		a     map[string]any
		setup func(*phySim)
		code  codes.Code
	}{
		"FLM device":     {args("target", "PHY6222"), nil, codes.InvalidArgument},
		"unaligned":      {args("address", "0x100"), nil, codes.InvalidArgument},
		"past flash end": {args("address", "0x7F800"), nil, codes.InvalidArgument},
		"unknown loader": {args("flash_loader", "foo"), nil, codes.InvalidArgument},
		"ROM-protected":  {args("address", "0x1000"), nil, codes.InvalidArgument},
		"other ROM":      {args("address", "0x2000"), func(s *phySim) { s.romHex[1] = "00 00" }, codes.FailedPrecondition},
		"no boot stop":   {args("address", "0x2000"), func(s *phySim) { s.noBoot = true }, codes.Unavailable},
		"erase error":    {args("address", "0x2000"), func(s *phySim) { s.failPC = phyRomErase }, codes.Unavailable},
		"verify":         {args("address", "0x2000"), func(s *phySim) { s.corrupt = true }, codes.DataLoss},
	} {
		sim := newPhySim()
		if tc.setup != nil {
			tc.setup(sim)
		}
		p := NewWith(t.TempDir(), &fakeCmd{reply: sim.reply})
		if _, err := invoke(t, p, "FlashProgramming", "program", tc.a); status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", name, err, tc.code)
		}
	}
}
