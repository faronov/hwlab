package jlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/ownedtemp"
)

// FlashLoaderPHY62xx programs PHYPlus PHY6222/PHY6252 internal SPI flash over
// SWD by calling the boot ROM's own flash routines (spif_erase_sector,
// spif_write). SEGGER's PHY6222 flash loader (FLM) is not used: it erased far
// more than asked for and served verification reads from its cache.
//
// Every commander session resets the chip and stops the boot ROM with a
// hardware breakpoint right after it has set up the SPI flash controller and
// before it starts any application from flash, so the flash content does not
// matter. It then points LR at a BKPT in RAM and runs one ROM routine per call.
const FlashLoaderPHY62xx = "phy62xx-rom"

const (
	phyFlashBase   = 0x1100_0000
	phySector      = 0x1000
	phyStub        = 0x1FFF_7FF0 // BKPT #0, where every ROM call returns
	phyStack       = 0x1FFF_7FC0
	phyBuf         = 0x1FFF_8000 // one sector of data for spif_write
	phyRomErase    = 0x0001_6FA9 // spif_erase_sector(addr)
	phyRomWrite    = 0x0001_7395 // spif_write(addr, data, len)
	phyBootStop    = 0x0001_6A4A // boot ROM, just after its call to spif_init
	phyCallTimeout = 5000        // ms per ROM call
	// The ROM routines return 0x10 (invalid parameter) for flash offsets
	// below 0x2000; that area cannot be changed this way.
	phyRomMinOffset = 0x2000
	// Sectors per commander session: bounds one session well below the
	// commander timeout.
	phySectorsPerSession = 16
	phyWordsPerLine      = 32 // Commander's limit for one w4 line
)

// phyRomFingerprint holds the first bytes of the ROM routines the loader
// calls; a different ROM revision is refused instead of jumping into it.
var phyRomFingerprint = []struct {
	addr uint32
	hex  string
}{
	{0x16FA8, "7eb56421029101020122090a52039142"},
	{0x17394, "10b5030201241b0a6403a34201d21020"},
	{0x16A46, "00f003fb"}, // bl spif_init, returning to phyBootStop
}

// phyBoot resets the chip and runs the boot ROM until it has initialised the
// flash controller.
var phyBoot = []string{"r", fmt.Sprintf("SetBP 0x%X H", phyBootStop), "g", "WaitHalt 2000", "ClrBP 1"}

// phyBypass makes reads at 0x11000000 go to the flash instead of the cache
// (the SDK's HAL_CACHE_ENTER_BYPASS_SECTION).
var phyBypass = []string{"w4 0x4000C000 0x2", "w4 0x40000024 0x2", "w4 0x40000028 0x1"}

func phyCall(pc uint32, args ...uint32) []string {
	s := []string{fmt.Sprintf("wreg MSP 0x%08X", phyStack), "wreg XPSR 0x01000000", "wreg PRIMASK 1"}
	for i, a := range args {
		s = append(s, fmt.Sprintf("wreg R%d 0x%08X", i, a))
	}
	return append(s, fmt.Sprintf("wreg R14 0x%08X", phyStub|1), fmt.Sprintf("SetPC 0x%X", pc), "g",
		fmt.Sprintf("WaitHalt %d", phyCallTimeout))
}

// phySessionScript erases and writes the sectors of image (already padded to
// whole sectors) starting at flash offset off. Blank sectors are only erased.
func phySessionScript(off uint32, image []byte) (script []string, calls int) {
	script = append(append([]string{}, phyBoot...), fmt.Sprintf("w2 0x%08X 0xBE00", phyStub))
	for i := 0; i < len(image); i += phySector {
		sec := image[i : i+phySector]
		addr := phyFlashBase + off + uint32(i)
		blank := bytes.Count(sec, []byte{0xFF}) == len(sec)
		if blank && off+uint32(i) < phyRomMinOffset {
			continue // left as is; the verify still checks it is blank
		}
		script = append(script, phyCall(phyRomErase, addr)...)
		calls++
		if blank {
			continue
		}
		for w := 0; w < phySector; w += 4 * phyWordsPerLine {
			line := fmt.Sprintf("w4 0x%08X", phyBuf+w)
			for k := w; k < w+4*phyWordsPerLine; k += 4 {
				line += fmt.Sprintf(" 0x%02X%02X%02X%02X", sec[k+3], sec[k+2], sec[k+1], sec[k])
			}
			script = append(script, line)
		}
		script = append(script, phyCall(phyRomWrite, addr, phyBuf, phySector)...)
		calls++
	}
	return script, calls
}

var (
	rePhyHalt   = regexp.MustCompile(`(?m)^CPU halted\.\s*\r?\nPC = ([0-9A-Fa-f]{8}).*\r?\nR0 = ([0-9A-Fa-f]{8})`)
	rePhyMem32  = regexp.MustCompile(`(?m)^4000C8A0 = ([0-9A-Fa-f]{8})`)
	rePhyMem8   = regexp.MustCompile(`(?m)^([0-9A-Fa-f]{8}) = ((?:[0-9A-Fa-f]{2} ?)+)`)
	rePhyNoHalt = regexp.MustCompile(`(?i)cpu not halted|timeout of \d+ ms exceeded`)
)

// phyBooted checks that the session's boot stopped at phyBootStop and returns
// the halts that followed it.
func phyBooted(out string) ([][]string, error) {
	ms := rePhyHalt.FindAllStringSubmatch(out, -1)
	if len(ms) == 0 || !strings.EqualFold(ms[0][1], fmt.Sprintf("%08X", phyBootStop)) {
		return nil, status.Error(codes.Unavailable, "the boot ROM did not stop after initialising the flash controller (is this a PHY62xx?)")
	}
	return ms[1:], nil
}

// phyCheckCalls verifies that every ROM call returned to the stub with 0.
func phyCheckCalls(out string, calls int) error {
	if m := rePhyNoHalt.FindString(out); m != "" {
		return fmt.Errorf("ROM flash routine did not return: %s", m)
	}
	ms, err := phyBooted(out)
	if err != nil {
		return err
	}
	if len(ms) != calls {
		return fmt.Errorf("%d of %d ROM calls returned", len(ms), calls)
	}
	for i, m := range ms {
		if pc, _ := strconv.ParseUint(m[1], 16, 32); pc != phyStub {
			return fmt.Errorf("ROM call %d stopped at PC 0x%s instead of the return stub", i+1, m[1])
		}
		if m[2] != "00000000" {
			return fmt.Errorf("ROM call %d returned 0x%s", i+1, m[2])
		}
	}
	return nil
}

// phyProbe resets and halts the chip, checks the ROM and reads the flash JEDEC
// id (opcode 0x9F through the controller's command registers).
func (p *Plugin) phyProbe(ctx context.Context, id string, t Target) (jedec uint32, size uint32, err error) {
	script := []string{"r"}
	for _, f := range phyRomFingerprint {
		script = append(script, fmt.Sprintf("mem8 0x%X %d", f.addr, len(f.hex)/2))
	}
	script = append(append(script, phyBoot...), "w4 0x4000C890 0x9FA00001", "sleep 5", "mem32 0x4000C8A0 1")
	out, err := p.run(ctx, id, t, script)
	if err != nil {
		return 0, 0, err
	}
	got := map[string]string{}
	for _, m := range rePhyMem8.FindAllStringSubmatch(out, -1) {
		got[strings.ToUpper(m[1])] += strings.ReplaceAll(strings.ToLower(m[2]), " ", "")
	}
	for _, f := range phyRomFingerprint {
		if h := got[fmt.Sprintf("%08X", f.addr)]; !strings.HasPrefix(h, f.hex) {
			return 0, 0, status.Errorf(codes.FailedPrecondition,
				"PHY62xx ROM at 0x%X reads %q, want %q: unknown ROM revision or not a PHY62xx", f.addr, h, f.hex)
		}
	}
	if _, err := phyBooted(out); err != nil {
		return 0, 0, err
	}
	m := rePhyMem32.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, status.Error(codes.Unavailable, "no flash JEDEC id in the commander output")
	}
	v, _ := strconv.ParseUint(m[1], 16, 32)
	jedec = uint32(v) & 0xFFFFFF
	bits := (jedec >> 16) & 0xFF
	if jedec == 0 || jedec == 0xFFFFFF || bits < 16 || bits > 24 {
		return jedec, 0, status.Errorf(codes.Unavailable, "implausible flash JEDEC id %06X", jedec)
	}
	return jedec, 1 << bits, nil
}

// phyRead saves length bytes of flash at offset off into path, bypassing the
// cache.
func (p *Plugin) phyRead(ctx context.Context, id string, t Target, path string, off, length uint32) error {
	script := append(append(append([]string{}, phyBoot...), phyBypass...),
		fmt.Sprintf("savebin %q 0x%08X 0x%X", path, phyFlashBase+off, length))
	out, err := p.run(ctx, id, t, script)
	if err != nil {
		return err
	}
	if _, err := phyBooted(out); err != nil {
		return err
	}
	if st, err := os.Stat(path); err != nil || st.Size() != int64(length) {
		return status.Errorf(codes.Unavailable, "flash read back failed: %v", tail(out, 3))
	}
	return nil
}

// programPHY62xx writes a raw .bin at flash offset off. Whole sectors are
// erased: bytes after the image in its last sector become 0xFF.
func (p *Plugin) programPHY62xx(ctx context.Context, id string, t Target, path string, args map[string]any) (res map[string]any, err error) {
	if !strings.EqualFold(t.Device, "Cortex-M0") {
		return nil, status.Errorf(codes.InvalidArgument,
			"flash_loader %s needs target \"Cortex-M0\" (a PHY62xx J-Link device would load SEGGER's flash loader), got %q", FlashLoaderPHY62xx, t.Device)
	}
	a, err := toAddr(args["address"])
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "address (flash offset such as \"0x0\" or 0x11xxxxxx) is required")
	}
	if a >= phyFlashBase {
		a -= phyFlashBase
	}
	if a%phySector != 0 || a >= 1<<24 {
		return nil, status.Errorf(codes.InvalidArgument, "address 0x%X: want a 4 KiB aligned flash offset", a)
	}
	off := uint32(a)
	image, err := os.ReadFile(path)
	if err != nil || len(image) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "read %q: empty or unreadable", path)
	}
	padded := append(append([]byte{}, image...), bytes.Repeat([]byte{0xFF}, (phySector-len(image)%phySector)%phySector)...)

	if n := min(len(padded), int(max(int64(phyRomMinOffset)-int64(off), 0))); bytes.Count(padded[:n], []byte{0xFF}) != n {
		return nil, status.Errorf(codes.InvalidArgument, "the PHY62xx ROM cannot write flash below 0x%X and the image has data there", phyRomMinOffset)
	}
	jedec, size, err := p.phyProbe(ctx, id, t)
	if err != nil {
		return nil, err
	}
	if uint64(off)+uint64(len(padded)) > uint64(size) {
		return nil, status.Errorf(codes.InvalidArgument, "image 0x%X..0x%X exceeds the %d KiB flash (JEDEC %06X)",
			off, uint64(off)+uint64(len(padded)), size>>10, jedec)
	}
	dir, err := ownedtemp.New("hwlab-phy62xx-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()

	res = map[string]any{"programmed": path, "address": fmt.Sprintf("0x%X", off), "length": float64(len(image)),
		"flash_jedec_id": fmt.Sprintf("%06X", jedec), "flash_size": float64(size), "flash_loader": FlashLoaderPHY62xx}
	if b, _ := args["backup"].(string); b != "" {
		if err := p.phyRead(ctx, id, t, b, 0, size); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		res["backup"] = b
	}
	for i := 0; i < len(padded); i += phySectorsPerSession * phySector {
		end := min(i+phySectorsPerSession*phySector, len(padded))
		script, calls := phySessionScript(off+uint32(i), padded[i:end])
		out, err := p.run(ctx, id, t, script)
		if err != nil {
			return nil, err
		}
		if err := phyCheckCalls(out, calls); err != nil {
			return nil, status.Errorf(codes.Unavailable, "flash 0x%X..0x%X: %v", off+uint32(i), off+uint32(end), err)
		}
	}
	rb := dir.File("readback.bin")
	if err := p.phyRead(ctx, id, t, rb, off, uint32(len(padded))); err != nil {
		return nil, err
	}
	got, err := os.ReadFile(rb)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(got, padded) {
		i := 0
		for i < len(got) && got[i] == padded[i] {
			i++
		}
		return nil, status.Errorf(codes.DataLoss, "verify failed at flash offset 0x%X", off+uint32(i))
	}
	res["verified"] = true
	script := []string{"r"}
	if run, ok := args["run"].(bool); !ok || run {
		script = append(script, "g")
	}
	if _, err := p.run(ctx, id, t, script); err != nil {
		return nil, err
	}
	return res, nil
}
