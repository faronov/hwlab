// Package cc26xx holds what hwlab knows about the TI CC13xx/CC26xx (CC13x0,
// CC26x0, CC13x1/CC26x1, CC13x2/CC26x2 and their 7-series variants) that does
// not depend on how the chip is reached: the identity registers in FCFG1, the
// flash geometry, the factory IEEE address, the CCFG layout and the rules
// that keep a CCFG from locking the chip out. The ROM serial bootloader
// plugin (tibsl) reads these through the bootloader, the XDS110 plugin through
// the debug port.
package cc26xx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Documented registers (CC13xx/CC26xx TRM SWCU117/SWCU185).
const (
	RegICEPickDeviceID = 0x50001318 // FCFG1 ICEPICK_DEVICE_ID
	RegUserID          = 0x50001294 // FCFG1 USER_ID
	RegMAC154          = 0x500012F0 // FCFG1 MAC_15_4_0/1, MSB at the high address
	RegMACBLE          = 0x500012E8 // FCFG1 MAC_BLE_0/1
	RegFlashSize       = 0x4003002C // FLASH FLASH_SIZE: sector count in [7:0]
	FCFG1Base          = 0x50001000
	FCFG1Size          = 0x400

	// CCFGLen is the size of the CCFG at the end of flash.
	CCFGLen = 88
)

// Memory is what identification reads through: a 32-bit word read and a
// byte read of n bytes.
type Memory interface {
	Word(addr uint32) (uint32, error)
	Read(addr uint32, n int, progress func(int)) ([]byte, error)
}

// Chip is the identity of a CC13xx/CC26xx from its documented registers.
type Chip struct {
	Name      string
	Series    string
	ICEPick   uint32
	WaferID   uint32
	PGRev     uint32
	UserID    uint32
	Package   string
	Protocols []string
	PA        bool
	CC13      bool

	FlashBytes  int
	SectorBytes int
	// IEEE is the factory EUI-64 (FCFG1 MAC_15_4), most significant byte first.
	IEEE [8]byte
	// ConfigAddr is the CCFG address (the last CCFGLen bytes of flash).
	ConfigAddr uint32

	// Writable is false for parts whose flash layout hwlab does not know.
	Writable    bool
	ReadOnlyWhy string
}

// ConfigSector is the address of the flash sector holding the CCFG.
func (c *Chip) ConfigSector() uint32 { return c.ConfigAddr &^ uint32(c.SectorBytes-1) }

// IEEEString formats the factory EUI-64 as 00:12:4B:...
func (c *Chip) IEEEString() string { return FormatEUI(c.IEEE[:]) }

// Agama reports a CC13x2/CC26x2 (also the 7-series): 8 KiB sectors, the ROM
// flash API in ROM_API_FLASH_TABLE, the debug flash path of hwlab.
func (c *Chip) Agama() bool { return c.WaferID == 0xBB41 || c.WaferID == 0xBB77 }

// FormatEUI formats bytes as colon-separated upper-case hex.
func FormatEUI(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02X", x)
	}
	return strings.Join(parts, ":")
}

// Reverse returns b in reverse byte order.
func Reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

var packages = map[uint32]string{0: "4x4 mm", 1: "5x5 mm", 2: "7x7 mm", 3: "wafer", 4: "WCSP 2.7x2.7 mm", 5: "7x7 mm Q1"}

// ErrNoICEPickID means ICEPICK_DEVICE_ID could not be read.
var ErrNoICEPickID = errors.New("ICEPICK_DEVICE_ID at 0x50001318 is not readable")

// Decode names the chip from ICEPICK_DEVICE_ID and USER_ID.
func Decode(ice, user uint32) *Chip {
	ch := &Chip{ICEPick: ice, UserID: user, SectorBytes: 8192}
	ch.WaferID, ch.PGRev = (ice>>12)&0xFFFF, ice>>28
	ch.PA = user&(1<<25) != 0
	ch.CC13 = user&(1<<23) != 0
	ch.Package = packages[(user>>16)&7]
	if ch.Package == "" {
		ch.Package = fmt.Sprintf("unknown (%d)", (user>>16)&7)
	}
	proto := (user >> 12) & 0xF
	for i, n := range []string{"BLE", "RF4CE", "IEEE 802.15.4 (Zigbee/Thread)", "proprietary"} {
		if proto&(1<<i) != 0 {
			ch.Protocols = append(ch.Protocols, n)
		}
	}
	ieee := proto&4 != 0
	ble := proto&1 != 0
	pick := func(a, b string, cond bool) string {
		if cond {
			return a
		}
		return b
	}
	ch.Writable = true
	switch ch.WaferID {
	case 0xB99A:
		ch.Series, ch.SectorBytes = "CC26x0", 4096
		switch {
		case ieee && ble:
			ch.Name = "CC2650"
		case ble:
			ch.Name = "CC2640"
		default:
			ch.Name = "CC2630"
		}
	case 0xB9BE:
		ch.Series, ch.SectorBytes = "CC13x0", 4096
		ch.Name = pick("CC1350", "CC1310", ieee || ble)
	case 0xBB41, 0xBB77:
		suffix := pick("7", "", ch.WaferID == 0xBB77)
		ch.Series = "CC13x2/CC26x2" + pick("x7", "", suffix != "")
		if ch.CC13 {
			ch.Name = pick("CC1352P", pick("CC1352R", "CC1312R", ieee || ble), ch.PA) + suffix
		} else {
			ch.Name = pick("CC2652P", pick("CC2652R", "CC2642R", ieee), ch.PA) + suffix
		}
	case 0xBB7A:
		ch.Series = "CC13x1/CC26x1 (assumed)"
		ch.Name = pick(pick("CC1311P3", "CC1311R3", ch.PA), pick("CC2651P3", "CC2651R3", ch.PA), ch.CC13)
		ch.Writable, ch.ReadOnlyWhy = false, "wafer id 0xBB7A: the flash/CCFG layout was not confirmed for this family; reads only"
	default:
		ch.Name, ch.Series = fmt.Sprintf("unknown (wafer id 0x%04X)", ch.WaferID), "unknown"
		ch.Writable, ch.ReadOnlyWhy = false, fmt.Sprintf("wafer id 0x%04X is not a known CC13xx/CC26xx family; reads only", ch.WaferID)
	}
	return ch
}

// Identify reads ICEPICK_DEVICE_ID, USER_ID, FLASH_SIZE and the factory IEEE
// address. An unreadable ICEPICK_DEVICE_ID is returned wrapped in
// ErrNoICEPickID (the CC13x4/CC26x4 have another layout).
func Identify(m Memory) (*Chip, error) {
	ice, err := m.Word(RegICEPickDeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoICEPickID, err)
	}
	user, err := m.Word(RegUserID)
	if err != nil {
		return nil, fmt.Errorf("FCFG1 USER_ID: %w", err)
	}
	ch := Decode(ice, user)
	fs, err := m.Word(RegFlashSize)
	if err != nil {
		return nil, fmt.Errorf("FLASH_SIZE: %w", err)
	}
	ch.FlashBytes = int(fs&0xFF) * ch.SectorBytes
	if ch.FlashBytes == 0 {
		return nil, fmt.Errorf("FLASH_SIZE reads 0x%08x: no flash size", fs)
	}
	mac, err := m.Read(RegMAC154, 8, nil)
	if err != nil {
		return nil, fmt.Errorf("FCFG1 MAC_15_4: %w", err)
	}
	copy(ch.IEEE[:], Reverse(mac))
	ch.ConfigAddr = uint32(ch.FlashBytes - CCFGLen)
	return ch, nil
}

// CCFG field offsets from the start of the 88-byte CCFG.
const (
	CCFGIEEEMAC0     = 0x20
	CCFGIEEEMAC1     = 0x24
	CCFGBLConfig     = 0x30
	CCFGEraseConf    = 0x34
	CCFGTIOptions    = 0x38
	CCFGTapDap0      = 0x3C
	CCFGTapDap1      = 0x40
	CCFGImageValid   = 0x44
	CCFGProt         = 0x48 // four words: CCFG_PROT_31_0 .. CCFG_PROT_127_96
	ccfgSecurityLen  = 0x14 // BL_CONFIG .. CCFG_TAP_DAP_1
	enabledByteValue = 0xC5
)

var ccfgNames = []string{"EXT_LF_CLK", "MODE_CONF_1", "SIZE_AND_DIS_FLAGS", "MODE_CONF", "VOLT_LOAD_0", "VOLT_LOAD_1", "RTC_OFFSET", "FREQ_OFFSET",
	"IEEE_MAC_0", "IEEE_MAC_1", "IEEE_BLE_0", "IEEE_BLE_1", "BL_CONFIG", "ERASE_CONF", "CCFG_TI_OPTIONS", "CCFG_TAP_DAP_0", "CCFG_TAP_DAP_1",
	"IMAGE_VALID_CONF", "CCFG_PROT_31_0", "CCFG_PROT_63_32", "CCFG_PROT_95_64", "CCFG_PROT_127_96"}

func le(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// Blank reports whether every byte is 0xFF (erased).
func Blank(b []byte) bool {
	for _, x := range b {
		if x != 0xFF {
			return false
		}
	}
	return true
}

// Enabled reports whether an 8-bit CCFG enable field holds 0xC5.
func Enabled(v uint32) bool { return v&0xFF == enabledByteValue }

// Backdoor is the bootloader entry configuration of a CCFG.
type Backdoor struct {
	BootloaderEnabled bool
	BackdoorEnabled   bool
	Pin               int
	ActiveHigh        bool
}

func (b Backdoor) String() string {
	if !b.BootloaderEnabled {
		return "bootloader disabled"
	}
	if !b.BackdoorEnabled {
		return "bootloader enabled, backdoor pin disabled"
	}
	return fmt.Sprintf("backdoor on pin %d, active %s", b.Pin, map[bool]string{true: "high", false: "low"}[b.ActiveHigh])
}

// BackdoorOf decodes BL_CONFIG.
func BackdoorOf(cfg []byte) Backdoor {
	v := le(cfg, CCFGBLConfig)
	return Backdoor{BootloaderEnabled: Enabled(v >> 24), BackdoorEnabled: Enabled(v), Pin: int(v>>8) & 0xFF, ActiveHigh: v&(1<<16) != 0}
}

// DebugLocked reports whether the CCFG keeps the CPU DAP closed after a reset.
func DebugLocked(cfg []byte) bool { return !Enabled(le(cfg, CCFGTapDap0) >> 16) }

// DecodeCCFG describes a CCFG for reports. It contains configuration words
// only, no secrets.
func DecodeCCFG(cfg []byte) map[string]any {
	res := map[string]any{"blank": Blank(cfg)}
	words := map[string]any{}
	bd := BackdoorOf(cfg)
	res["bootloader"] = map[string]any{"enabled": bd.BootloaderEnabled, "backdoor_enabled": bd.BackdoorEnabled, "backdoor_pin": float64(bd.Pin),
		"backdoor_active_high": bd.ActiveHigh, "summary": bd.String()}
	for i, n := range ccfgNames {
		words[n] = fmt.Sprintf("0x%08X", le(cfg, 4*i))
	}
	res["words"] = words
	res["image_valid"] = le(cfg, CCFGImageValid) == 0
	t0, t1 := le(cfg, CCFGTapDap0), le(cfg, CCFGTapDap1)
	res["cpu_dap_enabled"] = Enabled(t0 >> 16)
	res["ti_fa_enabled"] = Enabled(le(cfg, CCFGTIOptions))
	res["taps"] = map[string]any{"CPU_DAP": Enabled(t0 >> 16), "PWRPROF_TAP": Enabled(t0 >> 8), "TEST_TAP": Enabled(t0),
		"PBIST2_TAP": Enabled(t1 >> 16), "PBIST1_TAP": Enabled(t1 >> 8), "AON_TAP": Enabled(t1)}
	ec := le(cfg, CCFGEraseConf)
	res["chip_erase_enabled"] = ec&(1<<8) != 0
	res["bank_erase_enabled"] = ec&1 != 0
	prot := 0
	for i := 0; i < 16; i++ {
		for bit := 0; bit < 8; bit++ {
			if cfg[CCFGProt+i]&(1<<bit) == 0 {
				prot++
			}
		}
	}
	res["write_protected_sectors"] = float64(prot)
	if m := cfg[CCFGIEEEMAC0 : CCFGIEEEMAC1+4]; !Blank(m) {
		res["ieee_override"] = FormatEUI(Reverse(m))
	}
	return res
}

// ProtectedSectors lists the sector numbers CCFG_PROT write-protects.
func ProtectedSectors(cfg []byte) []int {
	var out []int
	for i := 0; i < 16; i++ {
		for bit := 0; bit < 8; bit++ {
			if cfg[CCFGProt+i]&(1<<bit) == 0 {
				out = append(out, 8*i+bit)
			}
		}
	}
	return out
}

// CheckBackdoor lists the bootloader-entry changes that would lock a chip
// out of its ROM bootloader (shared with the CC2538 CCA check).
func CheckBackdoor(cb, nb Backdoor, curBlank bool) (violations []string) {
	if !nb.BootloaderEnabled {
		violations = append(violations, "the ROM bootloader would be disabled")
	} else if !nb.BackdoorEnabled {
		violations = append(violations, "the bootloader backdoor pin would be disabled: with a valid image the ROM never enters the bootloader")
	}
	if !curBlank && cb.BackdoorEnabled && nb.BackdoorEnabled && (cb.Pin != nb.Pin || cb.ActiveHigh != nb.ActiveHigh) {
		violations = append(violations, fmt.Sprintf("the backdoor would move from %s to %s: the board's BSL wiring (DTR/RTS or button) would no longer enter the bootloader", cb, nb))
	}
	return violations
}

// BlankNote is the note for a CCFG that would be left blank.
const BlankNote = "the CCFG/CCA would be blank: the ROM sees no valid image and starts the bootloader; the application will not run until a configuration is written"

// CheckCCFG compares the CCFG flash would hold after an operation (next) with
// the current one (cur) and lists what would lock the chip out of its ROM
// bootloader or its debug port. A blank next is accepted: with no valid image
// the ROM starts the bootloader. notes are informational.
func CheckCCFG(cur, next []byte) (violations, notes []string) {
	if Blank(next) {
		return nil, []string{BlankNote}
	}
	violations = CheckBackdoor(BackdoorOf(cur), BackdoorOf(next), Blank(cur))
	if DebugLocked(next) {
		violations = append(violations, "CCFG_TAP_DAP_0.CPU_DAP_ENABLE would not be 0xC5: JTAG/SWD to the CPU locked")
	}
	type field struct {
		name string
		off  int
		sh   uint
	}
	for _, fl := range []field{{"CCFG_TI_OPTIONS.TI_FA_ENABLE", CCFGTIOptions, 0}, {"PWRPROF_TAP_ENABLE", CCFGTapDap0, 8}, {"TEST_TAP_ENABLE", CCFGTapDap0, 0},
		{"PBIST2_TAP_ENABLE", CCFGTapDap1, 16}, {"PBIST1_TAP_ENABLE", CCFGTapDap1, 8}, {"AON_TAP_ENABLE", CCFGTapDap1, 0}} {
		if Enabled(le(cur, fl.off)>>fl.sh) && !Enabled(le(next, fl.off)>>fl.sh) {
			violations = append(violations, fl.name+" would change from enabled (0xC5) to disabled")
		}
	}
	ce, ne := le(cur, CCFGEraseConf), le(next, CCFGEraseConf)
	if ne&(1<<8) == 0 && (ce&(1<<8) != 0 || Blank(cur)) {
		violations = append(violations, "ERASE_CONF.CHIP_ERASE_DIS_N would be cleared: chip erase (the JTAG recovery path) disabled")
	}
	if ne&1 == 0 && (ce&1 != 0 || Blank(cur)) {
		violations = append(violations, "ERASE_CONF.BANK_ERASE_DIS_N would be cleared: bank erase disabled")
	}
	for i := 0; i < 16; i++ {
		if newly := cur[CCFGProt+i] &^ next[CCFGProt+i]; newly != 0 {
			violations = append(violations, fmt.Sprintf("CCFG_PROT would newly write-protect sectors (byte %d mask 0x%02X): they could no longer be erased through the bootloader", i, newly))
		}
	}
	if le(next, CCFGImageValid) != 0 {
		notes = append(notes, "IMAGE_VALID_CONF is not 0: the ROM will start the bootloader instead of the application")
	}
	return violations, notes
}

// DefaultSecurity is the security part of the CCFG (BL_CONFIG through
// CCFG_TAP_DAP_1, 20 bytes at CCFGBLConfig) TI's flash tools write after a
// bank erase so the erased chip keeps its debug port and ROM bootloader:
// bootloader enabled (backdoor off), erase enabled, TI FA and every TAP
// enabled.
var DefaultSecurity = []byte{
	0xFF, 0xFF, 0xFF, 0xC5,
	0xFF, 0xFF, 0xFF, 0xFF,
	0xC5, 0xFF, 0xFF, 0xFF,
	0xC5, 0xC5, 0xC5, 0xFF,
	0xC5, 0xC5, 0xC5, 0xFF,
}

// SecurityOffset and SecurityLen locate DefaultSecurity in the CCFG.
const (
	SecurityOffset = CCFGBLConfig
	SecurityLen    = ccfgSecurityLen
)
