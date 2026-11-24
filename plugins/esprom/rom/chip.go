package rom

import (
	"errors"
	"fmt"
	"math/bits"
)

// ChipDetectAddr is the ROM word used as a chip magic by chips that do not
// report a chip id in GET_SECURITY_INFO.
const ChipDetectAddr = 0x40001000

// SPIRegs is the SPI1 user-command register map used to send flash
// commands (JEDEC ID) through READ_REG/WRITE_REG.
type SPIRegs struct {
	Base, User, User1, User2, MOSILen, MISOLen, W0 uint32
}

// Bits of the SPI user registers (same on the ESP32 SPI and the newer SPI_MEM).
const (
	spiCmdUSR      = 1 << 18
	spiUserCommand = 1 << 31
	spiUserMISO    = 1 << 28
)

// Chip is what the plugin knows about one chip type. Register addresses come
// from ESP-IDF v5.5.1 headers (soc/<chip>/register/soc/*.h, Apache-2.0) and
// the eFuse tables (components/efuse/<chip>/esp_efuse_table.csv).
type Chip struct {
	Name string
	// Key names the stub file (third_party/esp-flasher-stub/<key>.json).
	Key string
	// ImageChipID is the chip id in the application image header.
	ImageChipID uint16
	// SecChipID is the chip id GET_SECURITY_INFO reports (0: none).
	SecChipID uint32
	// Magic are ROM words at ChipDetectAddr that identify the chip.
	Magic []uint32
	// HasStub marks a chip with a vendored flasher stub: only those get
	// flash read/backup/program.
	HasStub bool
	SPI     SPIRegs
	// EfuseBase is DR_REG_EFUSE_BASE.
	EfuseBase uint32
	// BootloaderOffset is where the ROM loads the second-stage bootloader.
	BootloaderOffset uint32
	// USJ: the chip has a USB-Serial/JTAG controller.
	USJ bool
}

var spiMem = func(base uint32) SPIRegs {
	return SPIRegs{Base: base, User: 0x18, User1: 0x1C, User2: 0x20, MOSILen: 0x24, MISOLen: 0x28, W0: 0x58}
}

// Chips known to the plugin. Only those with HasStub get flash access.
var (
	ESP32 = &Chip{Name: "ESP32", Key: "esp32", ImageChipID: 0x0000, Magic: []uint32{0x00F01D83}, HasStub: true,
		SPI:       SPIRegs{Base: 0x3FF42000, User: 0x1C, User1: 0x20, User2: 0x24, MOSILen: 0x28, MISOLen: 0x2C, W0: 0x80},
		EfuseBase: 0x3FF5A000, BootloaderOffset: 0x1000}
	ESP32S3 = &Chip{Name: "ESP32-S3", Key: "esp32s3", ImageChipID: 0x0009, SecChipID: 9, Magic: []uint32{0x00000009}, HasStub: true,
		SPI: spiMem(0x60002000), EfuseBase: 0x60007000, USJ: true}
	ESP32C6 = &Chip{Name: "ESP32-C6", Key: "esp32c6", ImageChipID: 0x000D, SecChipID: 13, Magic: []uint32{0x2CE0806F}, HasStub: true,
		SPI: spiMem(0x60003000), EfuseBase: 0x600B0800, USJ: true}
	ESP32H2 = &Chip{Name: "ESP32-H2", Key: "esp32h2", ImageChipID: 0x0010, SecChipID: 16, HasStub: true,
		SPI: spiMem(0x60003000), EfuseBase: 0x600B0800, USJ: true}
	// Recognized but identify-only (no stub vendored).
	ESP32S2 = &Chip{Name: "ESP32-S2", Key: "esp32s2", ImageChipID: 0x0002, Magic: []uint32{0x000007C6}, BootloaderOffset: 0x1000}
	ESP32C3 = &Chip{Name: "ESP32-C3", Key: "esp32c3", ImageChipID: 0x0005, SecChipID: 5, USJ: true}
	ESP32C2 = &Chip{Name: "ESP32-C2", Key: "esp32c2", ImageChipID: 0x000C, SecChipID: 12}
	ESP32C5 = &Chip{Name: "ESP32-C5", Key: "esp32c5", ImageChipID: 0x0017, SecChipID: 23, USJ: true, BootloaderOffset: 0x2000}
	ESP32P4 = &Chip{Name: "ESP32-P4", Key: "esp32p4", ImageChipID: 0x0012, SecChipID: 18, USJ: true, BootloaderOffset: 0x2000}

	Chips = []*Chip{ESP32, ESP32S3, ESP32C6, ESP32H2, ESP32S2, ESP32C3, ESP32C2, ESP32C5, ESP32P4}
)

// ChipByKey finds a chip by its key.
func ChipByKey(k string) *Chip {
	for _, c := range Chips {
		if c.Key == k {
			return c
		}
	}
	return nil
}

// Detection is the evidence behind a chip identification.
type Detection struct {
	Chip     *Chip
	Magic    uint32
	Security *SecurityInfo
	// SecurityErr is why GET_SECURITY_INFO gave nothing (ESP32: unsupported).
	SecurityErr error
	Basis       string
}

// ErrUnknownChip means the evidence matches no known chip.
var ErrUnknownChip = errors.New("unknown chip")

// Detect identifies the chip: the ROM word at ChipDetectAddr first (the
// ESP32 and ESP32-S2 are known by it and the ESP32 does not implement
// GET_SECURITY_INFO), else the chip id of GET_SECURITY_INFO, else the magic
// tables of the other chips.
func (c *Client) Detect() (*Detection, error) {
	d := &Detection{}
	var err error
	if d.Magic, err = c.ReadReg(ChipDetectAddr); err != nil {
		return d, fmt.Errorf("read the chip magic at 0x%08X: %w", ChipDetectAddr, err)
	}
	for _, ch := range []*Chip{ESP32, ESP32S2} {
		for _, m := range ch.Magic {
			if m == d.Magic {
				d.Chip, d.Basis = ch, fmt.Sprintf("ROM word 0x%08X at 0x%08X", d.Magic, ChipDetectAddr)
				if ch == ESP32S2 {
					d.Security, d.SecurityErr = c.GetSecurityInfo()
				}
				return d, nil
			}
		}
	}
	d.Security, d.SecurityErr = c.GetSecurityInfo()
	if d.Security != nil && d.Security.HasChipID {
		for _, ch := range Chips {
			if ch.SecChipID != 0 && ch.SecChipID == d.Security.ChipID {
				d.Chip, d.Basis = ch, fmt.Sprintf("GET_SECURITY_INFO chip id %d", d.Security.ChipID)
				return d, nil
			}
		}
		return d, fmt.Errorf("%w: GET_SECURITY_INFO chip id %d, ROM word 0x%08X", ErrUnknownChip, d.Security.ChipID, d.Magic)
	}
	for _, ch := range Chips {
		for _, m := range ch.Magic {
			if m == d.Magic {
				d.Chip, d.Basis = ch, fmt.Sprintf("ROM word 0x%08X at 0x%08X (no chip id: %v)", d.Magic, ChipDetectAddr, d.SecurityErr)
				return d, nil
			}
		}
	}
	why := d.SecurityErr
	if why == nil {
		why = errors.New("no chip id")
	}
	return d, fmt.Errorf("%w: ROM word 0x%08X, GET_SECURITY_INFO: %w", ErrUnknownChip, d.Magic, why)
}

// efuseWord reads 32-bit word i of an eFuse block: on the ESP32 BLK0 is
// EFUSE_BLK0_RDATA0..6 at +0x00; on the newer chips BLK0 word 0 is
// EFUSE_RD_WR_DIS (+0x2C), words 1..5 RD_REPEAT_DATA0..4 (+0x30..), BLK1
// EFUSE_RD_MAC_(SPI_)SYS_0..5 (+0x44..).
func (c *Client) efuseWord(ch *Chip, blk, i int) (uint32, error) {
	var off uint32
	switch {
	case ch == ESP32 && blk == 0:
		off = uint32(4 * i)
	case blk == 0:
		off = 0x2C + uint32(4*i)
	case blk == 1:
		off = 0x44 + uint32(4*i)
	default:
		return 0, fmt.Errorf("eFuse block %d is not read", blk)
	}
	return c.ReadReg(ch.EfuseBase + off)
}

// field extracts bits [bit, bit+n) of a block given as words.
func field(w []uint32, bit, n int) uint32 {
	v := uint64(w[bit/32])
	if bit/32+1 < len(w) {
		v |= uint64(w[bit/32+1]) << 32
	}
	return uint32(v>>(bit%32)) & (1<<n - 1)
}

func (c *Client) efuseBlock(ch *Chip, blk, words int) ([]uint32, error) {
	w := make([]uint32, words)
	for i := range w {
		v, err := c.efuseWord(ch, blk, i)
		if err != nil {
			return nil, err
		}
		w[i] = v
	}
	return w, nil
}

// Facts is the identity read from eFuses (all read-only).
type Facts struct {
	MAC      [6]byte
	MACExt   []byte // ESP32-C6/H2: the 2 bytes that make the EUI-64
	MACCRC   *uint8 // ESP32
	Major    int
	Minor    int
	Package  string
	PkgCode  int
	Features []string
	// Security state (eFuse fields, or GET_SECURITY_INFO).
	FlashEncrypted bool
	SecureBoot     bool
	SecureDownload bool
	Security       map[string]any
	Basis          string
}

// Revision is "v<major>.<minor>".
func (f *Facts) Revision() string { return fmt.Sprintf("v%d.%d", f.Major, f.Minor) }

// MACString is the base MAC as aa:bb:cc:dd:ee:ff.
func (f *Facts) MACString() string {
	m := f.MAC
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])
}

// EUI64 is the IEEE 802.15.4 address of the C6/H2 (MAC with MAC_EXT inserted).
func (f *Facts) EUI64() string {
	if len(f.MACExt) != 2 {
		return ""
	}
	m := f.MAC
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], f.MACExt[0], f.MACExt[1], m[3], m[4], m[5])
}

func macFrom(lo, hi uint32) [6]byte {
	return [6]byte{byte(hi >> 8), byte(hi), byte(lo >> 24), byte(lo >> 16), byte(lo >> 8), byte(lo)}
}

var esp32Packages = map[int]string{0: "ESP32-D0WDQ6", 1: "ESP32-D0WD", 2: "ESP32-D2WD", 4: "ESP32-U4WDH", 5: "ESP32-PICO-D4", 6: "ESP32-PICO-V3-02", 7: "ESP32-D0WDR2-V3"}

// ReadFacts reads MAC, revision, package and security state from eFuses.
// Only the ESP32 decode has been checked on hardware; the others follow the
// ESP-IDF v5.5.1 eFuse tables.
func (c *Client) ReadFacts(d *Detection) (*Facts, error) {
	ch := d.Chip
	if ch.EfuseBase == 0 {
		return nil, fmt.Errorf("%s: eFuse layout not known to hwlab", ch.Name)
	}
	if ch == ESP32 {
		return c.esp32Facts()
	}
	b0, err := c.efuseBlock(ch, 0, 6)
	if err != nil {
		return nil, err
	}
	b1, err := c.efuseBlock(ch, 1, 6)
	if err != nil {
		return nil, err
	}
	f := &Facts{MAC: macFrom(b1[0], b1[1]), Basis: "ESP-IDF v5.5.1 eFuse table of the " + ch.Name}
	switch ch {
	case ESP32S3:
		f.Minor = int(field(b1, 183, 1)<<3 | field(b1, 114, 3))
		f.Major = int(field(b1, 184, 2))
		f.PkgCode = int(field(b1, 117, 3))
		flash := map[uint32]string{0: "none", 1: "8 MB", 2: "4 MB"}[field(b1, 123, 3)]
		psram := map[uint32]string{0: "none", 1: "8 MB", 2: "2 MB", 3: "16 MB", 4: "4 MB"}[field(b1, 179, 1)<<2|field(b1, 131, 2)]
		f.Features = []string{"Wi-Fi", "BLE", "dual core", "embedded flash: " + orUnknown(flash), "embedded PSRAM: " + orUnknown(psram)}
	case ESP32C6:
		f.Minor, f.Major, f.PkgCode = int(field(b1, 114, 4)), int(field(b1, 118, 2)), int(field(b1, 120, 3))
		f.MACExt = []byte{byte(b1[1] >> 24), byte(b1[1] >> 16)}
		f.Features = []string{"Wi-Fi 6", "BLE", "IEEE 802.15.4"}
	case ESP32H2:
		f.Minor, f.Major, f.PkgCode = int(field(b1, 114, 3)), int(field(b1, 117, 2)), int(field(b1, 128, 3))
		f.MACExt = []byte{byte(b1[1] >> 24), byte(b1[1] >> 16)}
		f.Features = []string{"BLE", "IEEE 802.15.4"}
	}
	f.Package = fmt.Sprintf("package code %d", f.PkgCode)
	sec := map[string]any{}
	if s := d.Security; s != nil {
		var on []any
		for _, fl := range SecurityFlags {
			if s.Flags&fl.Bit != 0 {
				on = append(on, fl.Name)
			}
		}
		if on == nil {
			on = []any{}
		}
		sec["flags"] = fmt.Sprintf("0x%08X", s.Flags)
		sec["flags_set"] = on
		sec["flash_crypt_cnt"] = float64(s.FlashCryptCnt)
		if s.HasChipID {
			sec["chip_id"], sec["eco_version"] = float64(s.ChipID), float64(s.ECO)
		}
		f.FlashEncrypted = bits.OnesCount8(s.FlashCryptCnt&7)%2 == 1
		f.SecureBoot = s.Flags&1 != 0
		f.SecureDownload = s.Flags&4 != 0
		sec["source"] = "GET_SECURITY_INFO"
	} else {
		cnt := field(b0, 82, 3)
		f.FlashEncrypted = bits.OnesCount32(cnt)%2 == 1
		f.SecureBoot = field(b0, 116, 1) == 1
		sec["spi_boot_crypt_cnt"], sec["source"] = float64(cnt), "eFuse BLK0"
	}
	sec["dis_download_mode"] = field(b0, 128, 1) == 1
	sec["dis_pad_jtag"] = field(b0, 51, 1) == 1
	sec["soft_dis_jtag"] = float64(field(b0, 48, 3))
	f.Security = sec
	return f, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown code"
	}
	return s
}

// esp32Facts decodes the classic ESP32 BLK0 (EFUSE_BLK0_RDATA0..6) and the
// major revision bit in APB_CTRL_DATE (SYSCON_DATE_REG, 0x3FF6607C, bit 31).
func (c *Client) esp32Facts() (*Facts, error) {
	w, err := c.efuseBlock(ESP32, 0, 7)
	if err != nil {
		return nil, err
	}
	date, err := c.ReadReg(0x3FF6607C)
	if err != nil {
		return nil, err
	}
	f := &Facts{MAC: macFrom(w[1], w[2]), Basis: "ESP32 eFuse BLK0 (ESP-IDF v5.5.1 esp_efuse_table.csv) and APB_CTRL_DATE bit 31"}
	crc := uint8(w[2] >> 16)
	f.MACCRC = &crc
	rev1, rev2, rev3 := field(w, 111, 1), field(w, 180, 1), date>>31
	switch rev3<<2 | rev2<<1 | rev1 {
	case 1:
		f.Major = 1
	case 3:
		f.Major = 2
	case 7:
		f.Major = 3
	}
	f.Minor = int(field(w, 184, 2))
	f.PkgCode = int(field(w, 98, 1)<<3 | field(w, 105, 3))
	f.Package = esp32Packages[f.PkgCode]
	if f.Package == "" {
		f.Package = fmt.Sprintf("unknown package %d", f.PkgCode)
	} else if f.Major == 3 && (f.PkgCode == 0 || f.PkgCode == 1) {
		f.Package += "-V3"
	}
	cores := "dual core"
	if field(w, 96, 1) == 1 {
		cores = "single core"
	}
	f.Features = []string{"Wi-Fi", cores}
	if field(w, 97, 1) == 0 {
		f.Features = append(f.Features, "BT/BLE")
	}
	mhz := "240 MHz"
	if field(w, 109, 1) == 1 && field(w, 108, 1) == 1 {
		mhz = "160 MHz"
	}
	f.Features = append(f.Features, mhz)
	switch f.PkgCode {
	case 2, 4, 5, 6:
		f.Features = append(f.Features, "embedded flash")
	}
	if f.PkgCode == 6 || f.PkgCode == 7 {
		f.Features = append(f.Features, "embedded PSRAM")
	}
	cnt := field(w, 20, 7)
	f.FlashEncrypted = bits.OnesCount32(cnt)%2 == 1
	f.SecureBoot = field(w, 196, 1) == 1 || field(w, 197, 1) == 1
	f.Security = map[string]any{
		"source": "eFuse BLK0", "flash_crypt_cnt": float64(cnt), "uart_download_dis": field(w, 27, 1) == 1,
		"secure_boot_v1": field(w, 196, 1) == 1, "secure_boot_v2": field(w, 197, 1) == 1, "jtag_disable": field(w, 198, 1) == 1,
		"disable_dl_encrypt": field(w, 199, 1) == 1, "disable_dl_decrypt": field(w, 200, 1) == 1, "disable_dl_cache": field(w, 201, 1) == 1,
		"coding_scheme": float64(field(w, 192, 2)), "console_debug_disable": field(w, 194, 1) == 1,
		"vdd_sdio_force": field(w, 144, 1) == 1, "vdd_sdio_reg": field(w, 142, 1) == 1, "vdd_sdio_tieh": field(w, 143, 1) == 1,
	}
	return f, nil
}

// FlashID sends JEDEC READ ID (0x9F) through the SPI1 user command registers
// and returns the 3 id bytes (manufacturer, memory type, capacity) as
// manufacturer | type<<8 | capacity<<16. The registers it changes are put
// back. The flash must be attached first (SPI_ATTACH).
func (c *Client) FlashID(ch *Chip) (uint32, error) {
	r := ch.SPI
	if r.Base == 0 {
		return 0, fmt.Errorf("%s: SPI1 registers not known to hwlab", ch.Name)
	}
	regs := []uint32{r.User, r.User1, r.User2, r.MOSILen, r.MISOLen}
	saved := make([]uint32, len(regs))
	for i, o := range regs {
		v, err := c.ReadReg(r.Base + o)
		if err != nil {
			return 0, err
		}
		saved[i] = v
	}
	restore := func() error {
		for i, o := range regs {
			if err := c.WriteReg(r.Base+o, saved[i], 0xFFFFFFFF, 0); err != nil {
				return err
			}
		}
		return nil
	}
	steps := [][2]uint32{
		{r.User, spiUserCommand | spiUserMISO},
		{r.User2, 7<<28 | 0x9F},
		{r.MOSILen, 0},
		{r.MISOLen, 24 - 1},
		{r.W0, 0},
		{0, spiCmdUSR},
	}
	for _, s := range steps {
		if err := c.WriteReg(r.Base+s[0], s[1], 0xFFFFFFFF, 0); err != nil {
			return 0, errors.Join(err, restore())
		}
	}
	done := false
	for i := 0; i < 10 && !done; i++ {
		v, err := c.ReadReg(r.Base)
		if err != nil {
			return 0, errors.Join(err, restore())
		}
		done = v&spiCmdUSR == 0
	}
	if !done {
		return 0, errors.Join(errors.New("SPI1 user command did not finish"), restore())
	}
	id, err := c.ReadReg(r.Base + r.W0)
	if err != nil {
		return 0, errors.Join(err, restore())
	}
	return id & 0xFFFFFF, restore()
}

// FlashSize decodes the JEDEC capacity byte (the usual 2^n encoding, and
// the 0x20.. and 0x30.. ranges some vendors use); 0 when unknown.
func FlashSize(jedec uint32) int {
	cp := int(jedec>>16) & 0xFF
	switch {
	case cp >= 0x12 && cp <= 0x1C:
		return 1 << cp
	case cp >= 0x20 && cp <= 0x22:
		return 64 << 20 << (cp - 0x20)
	case cp >= 0x32 && cp <= 0x3A:
		return 1 << (cp - 0x20)
	}
	return 0
}

// JEDECString formats an id as manufacturer/type/capacity hex.
func JEDECString(id uint32) string {
	return fmt.Sprintf("%02x%02x%02x", id&0xFF, id>>8&0xFF, id>>16&0xFF)
}

var manufacturers = map[uint32]string{0xC8: "GigaDevice", 0xEF: "Winbond", 0x20: "XMC/Micron", 0xC2: "Macronix", 0x68: "Boya",
	0x5E: "Zbit", 0x0B: "XTX", 0x85: "Puya", 0x1C: "EON", 0xA1: "Fudan", 0x9D: "ISSI", 0xBA: "Zetta"}

// Manufacturer names the JEDEC manufacturer byte when known.
func Manufacturer(id uint32) string {
	if n, ok := manufacturers[id&0xFF]; ok {
		return n
	}
	return "unknown"
}
