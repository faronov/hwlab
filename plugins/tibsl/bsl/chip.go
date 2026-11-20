package bsl

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/ti/cc26xx"
)

// Documented registers read by Identify (CC13xx/CC26xx TRM SWCU117/SWCU185);
// the CC13xx/CC26xx ones live in internal/ti/cc26xx.
const (
	RegICEPickDeviceID = cc26xx.RegICEPickDeviceID
	RegUserID          = cc26xx.RegUserID
	RegMAC154          = cc26xx.RegMAC154
	RegFlashSize       = cc26xx.RegFlashSize
	FCFG1Base          = cc26xx.FCFG1Base
	FCFG1Size          = cc26xx.FCFG1Size

	CC2538FlashBase = 0x00200000
	CC2538DieCfg0   = 0x400D3014 // FLASH_CTRL_DIECFG0: flash size in [6:4]
	CC2538IEEE      = 0x00280028 // primary IEEE address in the info page
	CC2538Page      = 2048
	CC2538CCALen    = 44

	// CCFGLen is the size of the CC13xx/CC26xx CCFG at the end of flash.
	CCFGLen = cc26xx.CCFGLen
)

// Chip is what Identify learned from the ROM and documented registers.
type Chip struct {
	Family    Family
	ChipID    uint32
	Name      string
	Series    string
	ICEPick   uint32
	WaferID   uint32
	PGRev     uint32
	UserID    uint32
	Package   string
	Protocols []string
	PA        bool

	FlashBase   uint32
	FlashBytes  int
	SectorBytes int
	// IEEE is the factory EUI-64, most significant byte first.
	IEEE [8]byte
	// ConfigAddr/ConfigLen locate the CCFG (CC26xx) or CCA (CC2538).
	ConfigAddr uint32
	ConfigLen  int

	// Writable is false for parts whose flash layout hwlab does not know.
	Writable    bool
	ReadOnlyWhy string
}

// ConfigSector is the address of the flash sector holding the CCFG/CCA.
func (c *Chip) ConfigSector() uint32 {
	return c.ConfigAddr &^ uint32(c.SectorBytes-1)
}

// IEEEString formats the EUI-64 as 00:12:4B:...
func (c *Chip) IEEEString() string { return FormatEUI(c.IEEE[:]) }

// FormatEUI formats bytes as colon-separated upper-case hex.
func FormatEUI(b []byte) string { return cc26xx.FormatEUI(b) }

// IsCC2538 reports whether a GET_CHIP_ID answer is a CC2538.
func IsCC2538(id uint32) bool {
	return id&0xFFFF == 0xB964 || id&0xFFFF == 0xB965
}

func reverse(b []byte) []byte { return cc26xx.Reverse(b) }

// Identify reads GET_CHIP_ID and the documented identity registers and sets
// the client's dialect.
func Identify(c *Client) (*Chip, error) {
	c.Family = FamilyCC26xx
	id, err := c.ChipID()
	if err != nil {
		return nil, fmt.Errorf("GET_CHIP_ID: %w", err)
	}
	if IsCC2538(id) {
		c.Family = FamilyCC2538
		return identify2538(c, id)
	}
	return identify26xx(c, id)
}

func identify2538(c *Client, id uint32) (*Chip, error) {
	ch := &Chip{Family: FamilyCC2538, ChipID: id, Name: "CC2538", Series: "CC2538", FlashBase: CC2538FlashBase, SectorBytes: CC2538Page, Writable: true}
	die, err := c.Word(CC2538DieCfg0)
	if err != nil {
		return nil, fmt.Errorf("DIECFG0: %w", err)
	}
	if n := (die >> 4) & 7; n >= 1 && n <= 4 {
		ch.FlashBytes = int(n) * 128 << 10
	} else {
		ch.FlashBytes = 64 << 10
	}
	raw, err := c.Read(CC2538IEEE, 8, nil)
	if err != nil {
		return nil, fmt.Errorf("IEEE address: %w", err)
	}
	r0, r1 := reverse(raw[:4]), reverse(raw[4:])
	if r0[0] == 0x00 && r0[1] == 0x12 && r0[2] == 0x4B {
		copy(ch.IEEE[:], append(r0, r1...))
	} else {
		copy(ch.IEEE[:], append(r1, r0...))
	}
	ch.ConfigLen = CC2538CCALen
	ch.ConfigAddr = ch.FlashBase + uint32(ch.FlashBytes) - CC2538CCALen
	return ch, nil
}

func identify26xx(c *Client, id uint32) (*Chip, error) {
	ch := &Chip{Family: FamilyCC26xx, ChipID: id, SectorBytes: 8192}
	cc, err := cc26xx.Identify(c)
	if err != nil {
		var se *StatusError
		if errors.Is(err, cc26xx.ErrNoICEPickID) && errors.As(err, &se) {
			ch.Name, ch.Series = "unknown", "unknown"
			ch.ReadOnlyWhy = "ICEPICK_DEVICE_ID at 0x50001318 is not readable: not a CC13x0/CC26x0/CC13x1/CC26x1/CC13x2/CC26x2 layout (CC13x4/CC26x4 keep the CCFG in a separate sector at 0x50000000 and are not supported for writes)"
			return ch, nil
		}
		if errors.Is(err, cc26xx.ErrNoICEPickID) {
			return nil, fmt.Errorf("ICEPICK_DEVICE_ID: %w", err)
		}
		return nil, err
	}
	ch.Name, ch.Series, ch.ICEPick, ch.WaferID, ch.PGRev, ch.UserID = cc.Name, cc.Series, cc.ICEPick, cc.WaferID, cc.PGRev, cc.UserID
	ch.Package, ch.Protocols, ch.PA = cc.Package, cc.Protocols, cc.PA
	ch.FlashBytes, ch.SectorBytes, ch.IEEE = cc.FlashBytes, cc.SectorBytes, cc.IEEE
	ch.Writable, ch.ReadOnlyWhy = cc.Writable, cc.ReadOnlyWhy
	ch.ConfigLen, ch.ConfigAddr = CCFGLen, cc.ConfigAddr
	return ch, nil
}

// CCFG field offsets (CC13xx/CC26xx, from the start of the 88-byte CCFG).
const (
	CCFGIEEEMAC0     = cc26xx.CCFGIEEEMAC0
	CCFGIEEEMAC1     = cc26xx.CCFGIEEEMAC1
	CCFGBLConfig     = cc26xx.CCFGBLConfig
	CCFGEraseConf    = cc26xx.CCFGEraseConf
	CCFGTIOptions    = cc26xx.CCFGTIOptions
	CCFGTapDap0      = cc26xx.CCFGTapDap0
	CCFGTapDap1      = cc26xx.CCFGTapDap1
	CCFGImageValid   = cc26xx.CCFGImageValid
	CCFGProt         = cc26xx.CCFGProt
	cc2538BootldrCfg = 0
	cc2538ImageValid = 4
	cc2538Lock       = 12
)

func le(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// Blank reports whether every byte is 0xFF (erased).
func Blank(b []byte) bool { return cc26xx.Blank(b) }

// Backdoor is the bootloader entry configuration of a CCFG/CCA.
type Backdoor = cc26xx.Backdoor

func backdoor(f Family, cfg []byte) Backdoor {
	if f == FamilyCC2538 {
		v := le(cfg, cc2538BootldrCfg)
		en := v&(1<<28) != 0
		return Backdoor{BootloaderEnabled: en, BackdoorEnabled: en, Pin: int(v>>24) & 7, ActiveHigh: v&(1<<27) != 0}
	}
	return cc26xx.BackdoorOf(cfg)
}

// DecodeConfig describes a CCFG (CC26xx) or CCA (CC2538) for reports. It
// contains configuration words only, no secrets.
func DecodeConfig(f Family, cfg []byte) map[string]any {
	if f != FamilyCC2538 {
		return cc26xx.DecodeCCFG(cfg)
	}
	res := map[string]any{"blank": Blank(cfg)}
	words := map[string]any{}
	bd := backdoor(f, cfg)
	res["bootloader"] = map[string]any{"enabled": bd.BootloaderEnabled, "backdoor_enabled": bd.BackdoorEnabled, "backdoor_pin": float64(bd.Pin),
		"backdoor_active_high": bd.ActiveHigh, "summary": bd.String()}
	words["BOOTLDR_CFG"] = fmt.Sprintf("0x%08X", le(cfg, cc2538BootldrCfg))
	words["IMAGE_VALID"] = fmt.Sprintf("0x%08X", le(cfg, cc2538ImageValid))
	words["APP_VECTOR"] = fmt.Sprintf("0x%08X", le(cfg, 8))
	res["words"] = words
	res["image_valid"] = le(cfg, cc2538ImageValid) == 0
	res["debug_locked"] = cfg[CC2538CCALen-1]&0x80 == 0
	res["locked_pages"] = float64(lockedPages2538(cfg))
	return res
}

func lockedPages2538(cfg []byte) int {
	n := 0
	for i := cc2538Lock; i < CC2538CCALen; i++ {
		for bit := 0; bit < 8; bit++ {
			if i == CC2538CCALen-1 && bit == 7 {
				continue
			}
			if cfg[i]&(1<<bit) == 0 {
				n++
			}
		}
	}
	return n
}

// CheckConfig compares the CCFG/CCA flash would hold after an operation
// (next) with the current one (cur) and lists what would lock the chip out
// of this bootloader or its debug port. A blank next is accepted: with no
// valid image the ROM starts the bootloader. notes are informational.
func CheckConfig(f Family, cur, next []byte) (violations, notes []string) {
	if f != FamilyCC2538 {
		return cc26xx.CheckCCFG(cur, next)
	}
	if Blank(next) {
		return nil, []string{cc26xx.BlankNote}
	}
	violations = cc26xx.CheckBackdoor(backdoor(f, cur), backdoor(f, next), Blank(cur))
	if next[CC2538CCALen-1]&0x80 == 0 {
		violations = append(violations, "the debug lock bit would be set (JTAG locked)")
	}
	if n, c := lockedPages2538(next), lockedPages2538(cur); n > c || (Blank(cur) && n > 0) {
		violations = append(violations, fmt.Sprintf("%d flash pages would be lock-protected (now %d)", n, c))
	}
	if le(next, cc2538ImageValid) != 0 {
		notes = append(notes, "IMAGE_VALID is not 0: the ROM will start the bootloader instead of the application")
	}
	return violations, notes
}
