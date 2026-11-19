package bsl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Documented registers read by Identify (CC13xx/CC26xx TRM SWCU117/SWCU185).
const (
	RegICEPickDeviceID = 0x50001318 // FCFG1 ICEPICK_DEVICE_ID
	RegUserID          = 0x50001294 // FCFG1 USER_ID
	RegMAC154          = 0x500012F0 // FCFG1 MAC_15_4_0/1, MSB at the high address
	RegFlashSize       = 0x4003002C // FLASH FLASH_SIZE: sector count in [7:0]
	FCFG1Base          = 0x50001000
	FCFG1Size          = 0x400

	CC2538FlashBase = 0x00200000
	CC2538DieCfg0   = 0x400D3014 // FLASH_CTRL_DIECFG0: flash size in [6:4]
	CC2538IEEE      = 0x00280028 // primary IEEE address in the info page
	CC2538Page      = 2048
	CC2538CCALen    = 44

	// CCFGLen is the size of the CC13xx/CC26xx CCFG at the end of flash.
	CCFGLen = 88
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
func FormatEUI(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02X", x)
	}
	return strings.Join(parts, ":")
}

// IsCC2538 reports whether a GET_CHIP_ID answer is a CC2538.
func IsCC2538(id uint32) bool {
	return id&0xFFFF == 0xB964 || id&0xFFFF == 0xB965
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

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

var packages = map[uint32]string{0: "4x4 mm", 1: "5x5 mm", 2: "7x7 mm", 3: "wafer", 4: "WCSP 2.7x2.7 mm", 5: "7x7 mm Q1"}

func identify26xx(c *Client, id uint32) (*Chip, error) {
	ch := &Chip{Family: FamilyCC26xx, ChipID: id, SectorBytes: 8192}
	ice, err := c.Word(RegICEPickDeviceID)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			ch.Name, ch.Series = "unknown", "unknown"
			ch.ReadOnlyWhy = "ICEPICK_DEVICE_ID at 0x50001318 is not readable: not a CC13x0/CC26x0/CC13x1/CC26x1/CC13x2/CC26x2 layout (CC13x4/CC26x4 keep the CCFG in a separate sector at 0x50000000 and are not supported for writes)"
			return ch, nil
		}
		return nil, fmt.Errorf("ICEPICK_DEVICE_ID: %w", err)
	}
	user, err := c.Word(RegUserID)
	if err != nil {
		return nil, fmt.Errorf("FCFG1 USER_ID: %w", err)
	}
	ch.ICEPick, ch.UserID = ice, user
	ch.WaferID, ch.PGRev = (ice>>12)&0xFFFF, ice>>28
	ch.PA = user&(1<<25) != 0
	cc13 := user&(1<<23) != 0
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
		if cc13 {
			ch.Name = pick("CC1352P", pick("CC1352R", "CC1312R", ieee || ble), ch.PA) + suffix
		} else {
			ch.Name = pick("CC2652P", pick("CC2652R", "CC2642R", ieee), ch.PA) + suffix
		}
	case 0xBB7A:
		ch.Series = "CC13x1/CC26x1 (assumed)"
		ch.Name = pick(pick("CC1311P3", "CC1311R3", ch.PA), pick("CC2651P3", "CC2651R3", ch.PA), cc13)
		ch.Writable, ch.ReadOnlyWhy = false, "wafer id 0xBB7A: the flash/CCFG layout was not confirmed for this family; reads only"
	default:
		ch.Name, ch.Series = fmt.Sprintf("unknown (wafer id 0x%04X)", ch.WaferID), "unknown"
		ch.Writable, ch.ReadOnlyWhy = false, fmt.Sprintf("wafer id 0x%04X is not a known CC13xx/CC26xx family; reads only", ch.WaferID)
	}
	fs, err := c.Word(RegFlashSize)
	if err != nil {
		return nil, fmt.Errorf("FLASH_SIZE: %w", err)
	}
	ch.FlashBytes = int(fs&0xFF) * ch.SectorBytes
	if ch.FlashBytes == 0 {
		return nil, fmt.Errorf("FLASH_SIZE reads 0x%08x: no flash size", fs)
	}
	mac, err := c.Read(RegMAC154, 8, nil)
	if err != nil {
		return nil, fmt.Errorf("FCFG1 MAC_15_4: %w", err)
	}
	copy(ch.IEEE[:], reverse(mac))
	ch.ConfigLen = CCFGLen
	ch.ConfigAddr = uint32(ch.FlashBytes - CCFGLen)
	return ch, nil
}

// CCFG field offsets (CC13xx/CC26xx, from the start of the 88-byte CCFG).
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
	cc2538BootldrCfg = 0
	cc2538ImageValid = 4
	cc2538Lock       = 12
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

func enabled(v uint32) bool { return v&0xFF == 0xC5 }

// Backdoor is the bootloader entry configuration of a CCFG/CCA.
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

func backdoor(f Family, cfg []byte) Backdoor {
	if f == FamilyCC2538 {
		v := le(cfg, cc2538BootldrCfg)
		en := v&(1<<28) != 0
		return Backdoor{BootloaderEnabled: en, BackdoorEnabled: en, Pin: int(v>>24) & 7, ActiveHigh: v&(1<<27) != 0}
	}
	v := le(cfg, CCFGBLConfig)
	return Backdoor{BootloaderEnabled: enabled(v >> 24), BackdoorEnabled: enabled(v), Pin: int(v>>8) & 0xFF, ActiveHigh: v&(1<<16) != 0}
}

// DecodeConfig describes a CCFG (CC26xx) or CCA (CC2538) for reports. It
// contains configuration words only, no secrets.
func DecodeConfig(f Family, cfg []byte) map[string]any {
	res := map[string]any{"blank": Blank(cfg)}
	words := map[string]any{}
	bd := backdoor(f, cfg)
	res["bootloader"] = map[string]any{"enabled": bd.BootloaderEnabled, "backdoor_enabled": bd.BackdoorEnabled, "backdoor_pin": float64(bd.Pin),
		"backdoor_active_high": bd.ActiveHigh, "summary": bd.String()}
	if f == FamilyCC2538 {
		words["BOOTLDR_CFG"] = fmt.Sprintf("0x%08X", le(cfg, cc2538BootldrCfg))
		words["IMAGE_VALID"] = fmt.Sprintf("0x%08X", le(cfg, cc2538ImageValid))
		words["APP_VECTOR"] = fmt.Sprintf("0x%08X", le(cfg, 8))
		res["words"] = words
		res["image_valid"] = le(cfg, cc2538ImageValid) == 0
		res["debug_locked"] = cfg[CC2538CCALen-1]&0x80 == 0
		res["locked_pages"] = float64(lockedPages2538(cfg))
		return res
	}
	for i, n := range ccfgNames {
		words[n] = fmt.Sprintf("0x%08X", le(cfg, 4*i))
	}
	res["words"] = words
	res["image_valid"] = le(cfg, CCFGImageValid) == 0
	t0, t1 := le(cfg, CCFGTapDap0), le(cfg, CCFGTapDap1)
	res["cpu_dap_enabled"] = enabled(t0 >> 16)
	res["ti_fa_enabled"] = enabled(le(cfg, CCFGTIOptions))
	res["taps"] = map[string]any{"CPU_DAP": enabled(t0 >> 16), "PWRPROF_TAP": enabled(t0 >> 8), "TEST_TAP": enabled(t0),
		"PBIST2_TAP": enabled(t1 >> 16), "PBIST1_TAP": enabled(t1 >> 8), "AON_TAP": enabled(t1)}
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
		res["ieee_override"] = FormatEUI(reverse(m))
	}
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
	if Blank(next) {
		return nil, []string{"the CCFG/CCA would be blank: the ROM sees no valid image and starts the bootloader; the application will not run until a configuration is written"}
	}
	nb, cb := backdoor(f, next), backdoor(f, cur)
	if !nb.BootloaderEnabled {
		violations = append(violations, "the ROM bootloader would be disabled")
	} else if !nb.BackdoorEnabled {
		violations = append(violations, "the bootloader backdoor pin would be disabled: with a valid image the ROM never enters the bootloader")
	}
	if !Blank(cur) && cb.BackdoorEnabled && nb.BackdoorEnabled && (cb.Pin != nb.Pin || cb.ActiveHigh != nb.ActiveHigh) {
		violations = append(violations, fmt.Sprintf("the backdoor would move from %s to %s: the board's BSL wiring (DTR/RTS or button) would no longer enter the bootloader", cb, nb))
	}
	if f == FamilyCC2538 {
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
	t0 := le(next, CCFGTapDap0)
	if !enabled(t0 >> 16) {
		violations = append(violations, "CCFG_TAP_DAP_0.CPU_DAP_ENABLE would not be 0xC5: JTAG/SWD to the CPU locked")
	}
	type field struct {
		name string
		off  int
		sh   uint
	}
	for _, fl := range []field{{"CCFG_TI_OPTIONS.TI_FA_ENABLE", CCFGTIOptions, 0}, {"PWRPROF_TAP_ENABLE", CCFGTapDap0, 8}, {"TEST_TAP_ENABLE", CCFGTapDap0, 0},
		{"PBIST2_TAP_ENABLE", CCFGTapDap1, 16}, {"PBIST1_TAP_ENABLE", CCFGTapDap1, 8}, {"AON_TAP_ENABLE", CCFGTapDap1, 0}} {
		if enabled(le(cur, fl.off)>>fl.sh) && !enabled(le(next, fl.off)>>fl.sh) {
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
