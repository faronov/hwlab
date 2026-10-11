package jlink

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The parsers below follow the J-Link Commander text output. SEGGER does not
// document the output format, so they are deliberately tolerant and are
// covered by tests built from sample output; they are NOT yet verified against
// a real probe (see docs/adr/0012-jlink-debugprobe.md).

var (
	reMemLine   = regexp.MustCompile(`^\s*([0-9A-Fa-f]{8,16})\s*=\s*((?:[0-9A-Fa-f]+[ \t]*)+)$`)
	reReg       = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]*(?:\([A-Za-z]+\))?)\s*=\s*([0-9A-Fa-f]{1,16})\b`)
	reSerial    = regexp.MustCompile(`(?i)serial number:\s*(\d+)`)
	reProduct   = regexp.MustCompile(`(?i)ProductName:\s*(.+?)\s*(?:,\s*Nickname:.*)?$`)
	reVTref     = regexp.MustCompile(`(?i)VTref\s*=\s*([0-9.]+)\s*V`)
	reCore      = regexp.MustCompile(`(?i)(Cortex-[A-Za-z0-9+]+|RISC-V|ARM\d+\S*)\s+identified`)
	reDP        = regexp.MustCompile(`(?i)Found (SW-DP|JTAG-DP)[^\n]*?(0x[0-9A-Fa-f]+)`)
	reFirmware  = regexp.MustCompile(`(?im)^\s*Firmware:\s*(.+?)\s*$`)
	reHardware  = regexp.MustCompile(`(?im)^\s*Hardware version:\s*(.+?)\s*$`)
	reDLL       = regexp.MustCompile(`(?im)^\s*DLL version\s*(.+?)\s*$`)
	reNickname  = regexp.MustCompile(`(?i)Nickname:\s*(.*?)\s*$`)
	reHalted    = regexp.MustCompile(`(?i)CPU is (not )?halted(?: \(PC = (0x[0-9A-Fa-f]+)\))?`)
	reCPUID     = regexp.MustCompile(`(?i)CPUID register:\s*(0x[0-9A-Fa-f]+)`)
	reCoreRev   = regexp.MustCompile(`(?i)Found (Cortex-[A-Za-z0-9+]+) (r\d+p\d+)`)
	reSN        = regexp.MustCompile(`(?im)^\s*S/N:\s*(\d+)`)
	failMarkers = []string{
		"cannot connect to target", "could not connect", "no connection to target",
		"failed to open dll", "no emulators connected", "no j-link found", "could not find supported cpu",
		"could not find core", "failed to connect", "connection to j-link failed", "unknown device",
		"error: could not", "device not found", "cannot connect to the probe",
	}
)

// failure returns the first line of out that signals a failed connection or
// command, or "".
func failure(out string) string {
	for _, line := range strings.Split(out, "\n") {
		l := strings.ToLower(line)
		// "Connecting to J-Link ...FAILED" (an unknown or busy serial number).
		if strings.Contains(l, "connecting to j-link") && strings.Contains(l, "failed") {
			return strings.TrimSpace(line)
		}
		for _, m := range failMarkers {
			if strings.Contains(l, m) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// parseEmuList extracts probes from ShowEmuList output, e.g.
// "J-Link[0]: Connection: USB, Serial number: 683123456, ProductName: J-Link OB-nRF5340-NordicSemi".
func parseEmuList(out string) []Probe {
	var ps []Probe
	for _, line := range strings.Split(out, "\n") {
		m := reSerial.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := Probe{Serial: NormalizeSerial(m[1])}
		if pm := reProduct.FindStringSubmatch(line); pm != nil {
			p.Product, p.Model = pm[1], pm[1]
		}
		if nm := reNickname.FindStringSubmatch(line); nm != nil && nm[1] != "<not set>" {
			p.Nickname = nm[1]
		}
		ps = append(ps, p)
	}
	return ps
}

// parseMem returns n bytes from MemN output lines ("20000000 = 01 02 ..."),
// with unit 1, 2 or 4 bytes per printed item (little-endian target).
func parseMem(out string, unit, n int) ([]byte, error) {
	var data []byte
	for _, line := range strings.Split(out, "\n") {
		m := reMemLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		for _, tok := range strings.Fields(m[2]) {
			if len(tok) != unit*2 {
				return nil, fmt.Errorf("unexpected memory item %q (want %d hex digits)", tok, unit*2)
			}
			v, err := strconv.ParseUint(tok, 16, 32)
			if err != nil {
				return nil, err
			}
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], uint32(v))
			data = append(data, b[:unit]...)
		}
	}
	if len(data) < n {
		if f := failure(out); f != "" {
			return nil, fmt.Errorf("%s", f)
		}
		return nil, fmt.Errorf("read %d of %d bytes: output had no memory dump", len(data), n)
	}
	return data[:n], nil
}

// parseRegs returns "NAME" -> hex value for every "NAME = HEX" pair.
func parseRegs(out string) map[string]string {
	regs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		for _, m := range reReg.FindAllStringSubmatch(line, -1) {
			if strings.EqualFold(m[1], "VTref") { // "VTref=3.319V" is not a register
				continue
			}
			regs[strings.ToUpper(m[1])] = strings.ToUpper(m[2])
		}
	}
	return regs
}

// probeInfo extracts the probe description printed when Commander connects.
func probeInfo(out string) map[string]any {
	info := map[string]any{}
	set := func(k string, re *regexp.Regexp) {
		if m := re.FindStringSubmatch(out); m != nil {
			info[k] = m[1]
		}
	}
	set("firmware", reFirmware)
	set("hardware", reHardware)
	set("dll_version", reDLL)
	set("serial", reSN)
	if m := reVTref.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			info["vtref_v"] = v
		}
	}
	return info
}

// targetInfo extracts what Commander reports when it connects to a target.
func targetInfo(out string) map[string]any {
	info := map[string]any{}
	if m := reCore.FindStringSubmatch(out); m != nil {
		info["core"] = m[1]
	}
	if m := reDP.FindStringSubmatch(out); m != nil {
		info["dp"] = m[1]
		info["dp_id"] = m[2]
	}
	if m := reCoreRev.FindStringSubmatch(out); m != nil {
		info["core_revision"] = m[2]
	}
	if m := reCPUID.FindStringSubmatch(out); m != nil {
		info["cpuid"] = "0x" + strings.ToUpper(m[1][2:])
	}
	return info
}

// tail keeps the last n non-empty lines of out for diagnostics.
func tail(out string, n int) []any {
	var lines []any
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// writeScript builds Commander write commands for data at addr: 32-bit words
// where aligned, bytes otherwise.
func writeScript(addr uint64, data []byte) []string {
	var s []string
	for i := 0; i < len(data); {
		a := addr + uint64(i)
		if a%4 == 0 && len(data)-i >= 4 {
			s = append(s, fmt.Sprintf("w4 0x%X 0x%08X", a, binary.LittleEndian.Uint32(data[i:])))
			i += 4
			continue
		}
		s = append(s, fmt.Sprintf("w1 0x%X 0x%02X", a, data[i]))
		i++
	}
	return s
}

// rttLevel maps common log level markers (Zephyr "<err>", ESP-style "E ")
// to a log level and strips terminal colour escape sequences.
var reANSI = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func classifyLine(line string) (level, msg string) {
	msg = strings.TrimSpace(reANSI.ReplaceAllString(line, ""))
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "<err>") || strings.Contains(l, "<error>"):
		return "error", msg
	case strings.Contains(l, "<wrn>") || strings.Contains(l, "<warn>"):
		return "warn", msg
	case strings.Contains(l, "<dbg>") || strings.Contains(l, "<debug>"):
		return "debug", msg
	}
	return "info", msg
}

// outputSerial returns the probe serial number the commander printed when it
// connected ("S/N: 90000001"), or "". The commander prints it once per
// connection, so it identifies the probe that actually answered.
func outputSerial(out string) string {
	if m := reSN.FindStringSubmatch(out); m != nil {
		return NormalizeSerial(m[1])
	}
	return ""
}

// emuEntry returns the ShowEmuList entry for serial, if the output lists it.
func emuEntry(out, serial string) (Probe, bool) {
	for _, p := range parseEmuList(out) {
		if p.Serial == serial {
			return p, true
		}
	}
	return Probe{}, false
}

// haltState returns the last "CPU is [not ]halted" the commander printed
// (IsHalted), the PC it reported when halted, and whether there was one.
func haltState(out string) (halted bool, pc string, ok bool) {
	for _, m := range reHalted.FindAllStringSubmatch(out, -1) {
		halted, pc, ok = m[1] == "", m[2], true
	}
	return halted, pc, ok
}
