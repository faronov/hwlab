package xtensa

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Instruction encodings (little-endian Xtensa, 24-bit formats RSR and RRI8),
// from the Xtensa Instruction Set Architecture Reference Manual. The debug
// module executes the low 24 bits of DIR0.

// RSR returns "RSR at, sr" (read special register into at).
func RSR(sr, t uint8) uint32 { return 0x030000 | uint32(sr)<<8 | uint32(t&15)<<4 }

// WSR returns "WSR at, sr" (write at to special register).
func WSR(sr, t uint8) uint32 { return 0x130000 | uint32(sr)<<8 | uint32(t&15)<<4 }

// XSR returns "XSR at, sr" (exchange).
func XSR(sr, t uint8) uint32 { return 0x610000 | uint32(sr)<<8 | uint32(t&15)<<4 }

func rri8(op uint32, s, t uint8, imm uint8) uint32 {
	return op | uint32(imm)<<16 | uint32(s&15)<<8 | uint32(t&15)<<4
}

// ADDI returns "ADDI at, as, imm" (imm sign-extended, -128..127).
func ADDI(s, t uint8, imm int8) uint32 { return rri8(0x00C002, s, t, uint8(imm)) }

// L32I returns "L32I at, as, imm*4".
func L32I(s, t, imm uint8) uint32 { return rri8(0x002002, s, t, imm) }

// S32I returns "S32I at, as, imm*4".
func S32I(s, t, imm uint8) uint32 { return rri8(0x006002, s, t, imm) }

// L16UI returns "L16UI at, as, imm*2".
func L16UI(s, t, imm uint8) uint32 { return rri8(0x001002, s, t, imm) }

// S16I returns "S16I at, as, imm*2".
func S16I(s, t, imm uint8) uint32 { return rri8(0x005002, s, t, imm) }

// L8UI returns "L8UI at, as, imm".
func L8UI(s, t, imm uint8) uint32 { return rri8(0x000002, s, t, imm) }

// S8I returns "S8I at, as, imm".
func S8I(s, t, imm uint8) uint32 { return rri8(0x004002, s, t, imm) }

// The OCD-only instructions (LDDR32.P, SDDR32.P, RFDO) are not in the ISA
// manual; their encodings are the ones OpenOCD's xtensa.c uses (read for
// reference) and were confirmed by executing them on an ESP32-S3.

// LDDR32P returns "LDDR32.P as": DDR = mem32[as]; as += 4 (OCD only).
func LDDR32P(s uint8) uint32 { return 0x0070E0 | uint32(s&15)<<8 }

// SDDR32P returns "SDDR32.P as": mem32[as] = DDR; as += 4 (OCD only).
func SDDR32P(s uint8) uint32 { return 0x0070F0 | uint32(s&15)<<8 }

// Fixed instructions.
const (
	InsnRFDO  = 0xF1E000 // return from debug operation
	InsnISYNC = 0x002000
	InsnRSYNC = 0x002010
	InsnDSYNC = 0x002030
)

// Special register numbers (Xtensa ISA). EPC[n] = 176+n, EPS[n] = 192+n.
const (
	SRLBEG         = 0
	SRLEND         = 1
	SRLCOUNT       = 2
	SRSAR          = 3
	SRBR           = 4
	SRLITBASE      = 5
	SRSCOMPARE1    = 12
	SRWINDOWBASE   = 72
	SRWINDOWSTART  = 73
	SRIBREAKENABLE = 96
	SRATOMCTL      = 99
	SRDDR          = 104
	SRIBREAKA0     = 128
	SRDBREAKA0     = 144
	SRDBREAKC0     = 160
	SRCONFIGID0    = 176
	SREPC1         = 177
	SRDEPC         = 192
	SREPS2         = 194
	SRCONFIGID1    = 208
	SREXCSAVE1     = 209
	SRCPENABLE     = 224
	SRINTERRUPT    = 226 // INTSET on write
	SRINTCLEAR     = 227
	SRINTENABLE    = 228
	SRPS           = 230
	SRVECBASE      = 231
	SREXCCAUSE     = 232
	SRDEBUGCAUSE   = 233
	SRCCOUNT       = 234
	SRPRID         = 235
	SRICOUNT       = 236
	SRICOUNTLEVEL  = 237
	SREXCVADDR     = 238
	SRCCOMPARE0    = 240
	SRMISC0        = 244
)

// EPC returns the EPC[level] special register number (level 1..7).
func EPC(level int) uint8 { return uint8(SRCONFIGID0 + level) }

// EPS returns the EPS[level] special register number (level 2..7).
func EPS(level int) uint8 { return uint8(SRDEPC + level) }

// DEBUGCAUSE bits.
const (
	CauseICount      = 1 << 0
	CauseIBreak      = 1 << 1
	CauseDBreak      = 1 << 2
	CauseBreak       = 1 << 3
	CauseBreakN      = 1 << 4
	CauseDebugInt    = 1 << 5
	CauseDBreakNumSh = 8
)

// CauseNames lists the DEBUGCAUSE reasons set in v.
func CauseNames(v uint32) []string {
	var r []string
	for _, c := range []struct {
		bit  uint32
		name string
	}{
		{CauseICount, "icount"}, {CauseIBreak, "ibreak"}, {CauseDBreak, "dbreak"},
		{CauseBreak, "break"}, {CauseBreakN, "break.n"}, {CauseDebugInt, "debug-interrupt"},
	} {
		if v&c.bit != 0 {
			r = append(r, c.name)
		}
	}
	return r
}

// srNames maps the special registers this package reads by name. The list
// is the LX7 baseline used by ESP32-S3-class cores; a register the core does
// not implement reads back as an exec exception and is reported as such.
var srNames = map[string]uint8{
	"lbeg": SRLBEG, "lend": SRLEND, "lcount": SRLCOUNT, "sar": SRSAR,
	"windowbase": SRWINDOWBASE, "windowstart": SRWINDOWSTART,
	"ibreakenable": SRIBREAKENABLE, "ibreaka0": SRIBREAKA0, "ibreaka1": SRIBREAKA0 + 1,
	"dbreaka0": SRDBREAKA0, "dbreaka1": SRDBREAKA0 + 1, "dbreakc0": SRDBREAKC0, "dbreakc1": SRDBREAKC0 + 1,
	"configid0": SRCONFIGID0, "configid1": SRCONFIGID1, "epc1": SREPC1, "depc": SRDEPC,
	"excsave1": SREXCSAVE1, "cpenable": SRCPENABLE, "interrupt": SRINTERRUPT, "intenable": SRINTENABLE,
	"vecbase": SRVECBASE, "exccause": SREXCCAUSE, "debugcause": SRDEBUGCAUSE, "ccount": SRCCOUNT,
	"prid": SRPRID, "icount": SRICOUNT, "icountlevel": SRICOUNTLEVEL, "excvaddr": SREXCVADDR,
	"atomctl": SRATOMCTL, "ccompare0": SRCCOMPARE0, "misc0": SRMISC0,
	"ps_debug": SRPS,
}

// SRNames returns the named special registers in a stable order.
func SRNames() []string {
	n := make([]string, 0, len(srNames))
	for k := range srNames {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// regKind classifies a register name.
type regKind int

const (
	kindAR regKind = iota
	kindPC
	kindPS
	kindSR
)

// ParseReg resolves a register name: a0..a15 (current window), pc, ps (the
// interrupted program's PS, i.e. EPS[debug level]), a named special register
// (SRNames), or "sr<n>" for any special register number.
func parseReg(name string) (regKind, uint8, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case n == "pc":
		return kindPC, 0, nil
	case n == "ps":
		return kindPS, 0, nil
	case len(n) >= 2 && n[0] == 'a' && n[1] >= '0' && n[1] <= '9':
		v, err := strconv.Atoi(n[1:])
		if err != nil || v < 0 || v > 15 {
			return 0, 0, fmt.Errorf("xtensa: register %q: only a0..a15 (the current window) are addressable", name)
		}
		return kindAR, uint8(v), nil
	case strings.HasPrefix(n, "sr"):
		if v, err := strconv.ParseUint(n[2:], 0, 8); err == nil {
			return kindSR, uint8(v), nil
		}
	}
	if sr, ok := srNames[n]; ok {
		return kindSR, sr, nil
	}
	return 0, 0, fmt.Errorf("xtensa: unknown register %q", name)
}
