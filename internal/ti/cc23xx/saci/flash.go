package saci

import (
	"context"
	"fmt"
	"time"
)

// Flash geometry the SACI flash commands work in.
const (
	SectorSize      = 2048
	SectorWords     = SectorSize / 4
	CCFGSectorWords = SectorWords
	UserRecordWords = 32
	// ProgramGranule is the flash programming granularity (128 bit).
	ProgramGranule = 16
)

// Long-running flash command timeouts.
const (
	EraseTimeout   = 30 * time.Second
	ProgramTimeout = 5 * time.Second
	VerifyTimeout  = 10 * time.Second
)

// Pack turns bytes into little-endian words, padding the tail with pad.
func Pack(b []byte, pad byte) []uint32 {
	n := (len(b) + 3) / 4
	w := make([]uint32, n)
	for i := 0; i < n*4; i++ {
		v := pad
		if i < len(b) {
			v = b[i]
		}
		w[i/4] |= uint32(v) << (8 * (i % 4))
	}
	return w
}

// EraseChip sends FLASH_ERASE_CHIP. retainSectors is the retain mask the
// command takes in bits 31:16 of word 0 (0 retains nothing). After it the
// CCFG is invalid: EXIT_SACI_HALT/RUN are refused until a reset.
func (c *Client) EraseChip(ctx context.Context, retainSectors uint16) error {
	_, err := c.Exec(ctx, EraseTimeout, CmdFlashEraseChip, retainSectors, FlashKey)
	return err
}

// ProgramMain programs bytes at addr with FLASH_PROG_MAIN_SECTOR. The range
// must not cross a 2 KiB sector. The tail is padded with 0xFF to a word.
func (c *Client) ProgramMain(ctx context.Context, addr uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) >= 1<<16 {
		return fmt.Errorf("SACI program range of %d bytes is too large", len(data))
	}
	if addr/SectorSize != (addr+uint32(len(data))-1)/SectorSize {
		return fmt.Errorf("SACI program range %#x+%d crosses a sector boundary", addr, len(data))
	}
	payload := append([]uint32{FlashKey, addr}, Pack(data, 0xFF)...)
	_, err := c.Exec(ctx, ProgramTimeout, CmdFlashProgMain, uint16(len(data)), payload...)
	return err
}

// ProgramCCFG programs the 2 KiB CCFG sector (512 words), which must be
// erased. skipUserRec leaves the user record alone. Not allowed to be
// followed by EXIT_SACI_HALT/RUN in the same session.
func (c *Client) ProgramCCFG(ctx context.Context, sector []byte, skipUserRec bool) error {
	if len(sector) != SectorSize {
		return fmt.Errorf("CCFG sector must be %d bytes, got %d", SectorSize, len(sector))
	}
	var spec uint16
	if skipUserRec {
		spec = 1
	}
	_, err := c.Exec(ctx, ProgramTimeout, CmdFlashProgCCFG, spec, append([]uint32{FlashKey}, Pack(sector, 0xFF)...)...)
	return err
}

// ProgramUserRecord programs the 128 byte CCFG user record.
func (c *Client) ProgramUserRecord(ctx context.Context, rec []byte) error {
	if len(rec) != UserRecordWords*4 {
		return fmt.Errorf("user record must be %d bytes, got %d", UserRecordWords*4, len(rec))
	}
	_, err := c.Exec(ctx, ProgramTimeout, CmdFlashProgUserRec, 0, append([]uint32{FlashKey}, Pack(rec, 0xFF)...)...)
	return err
}

// VerifyMain checks whole sectors from addr: blank (all 0xFF) or CRC32
// against crc (standard CRC-32). length is a whole number of sectors, or
// whole sectors minus 4 bytes.
func (c *Client) VerifyMain(ctx context.Context, addr, length, crc uint32, blank bool) error {
	spec := uint16(0)
	if blank {
		spec = 0x8000 // bit 31 of word 0
	}
	_, err := c.Exec(ctx, VerifyTimeout, CmdFlashVerifyMain, spec, addr, length, crc)
	return err
}

// CCFGCRCs are the expected CRC32 values for VERIFY_CCFG_SECTOR.
type CCFGCRCs struct {
	BootCfg, Central, UserRec, DebugCfg uint32
}

// VerifyCCFG checks the CCFG sector: blank, or CRC32 of its sections.
// skipUserRec leaves the user record out of the check.
func (c *Client) VerifyCCFG(ctx context.Context, blank bool, crcs CCFGCRCs, skipUserRec bool) error {
	spec := uint16(0)
	if blank {
		spec = 0x8000
	} else {
		spec = 1
		if skipUserRec {
			spec |= 2
		}
	}
	_, err := c.Exec(ctx, VerifyTimeout, CmdFlashVerifyCCFG, spec, crcs.BootCfg, crcs.Central, crcs.UserRec, crcs.DebugCfg)
	return err
}
