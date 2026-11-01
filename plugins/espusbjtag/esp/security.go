package esp

import (
	"context"
	"fmt"

	"github.com/faronov/hwlab/internal/riscv"
)

// eFuse read registers (the shadow copy EFUSE_RD_REPEAT_DATA0..3 at
// Chip.EfuseRepeat0; read only). This package never writes an eFuse. The
// field positions below are identical on the ESP32-C6 and ESP32-H2.

// Security is the security-relevant eFuse state, as read.
type Security struct {
	FlashEncryptionCount int       `json:"flash_encryption_crypt_cnt"`
	SecureBootEnabled    bool      `json:"secure_boot_enabled"`
	USBJTAGDisabled      bool      `json:"usb_jtag_disabled"`
	PadJTAGDisabled      bool      `json:"pad_jtag_disabled"`
	SoftJTAGDisable      int       `json:"soft_jtag_disable"`
	DownloadModeDisabled bool      `json:"download_mode_disabled"`
	SecurityDownload     bool      `json:"security_download_enabled"`
	Raw                  [4]uint32 `json:"raw_repeat_words"`
}

// FlashEncrypted is true when the ROM treats flash as encrypted (an odd
// number of bits set in SPI_BOOT_CRYPT_CNT).
func (s Security) FlashEncrypted() bool {
	n, c := s.FlashEncryptionCount, 0
	for ; n != 0; n >>= 1 {
		c += n & 1
	}
	return c%2 == 1
}

// ReadSecurity reads the eFuse shadow registers.
func ReadSecurity(ctx context.Context, dm *riscv.DM, c *Chip) (Security, error) {
	if c == nil {
		return Security{}, errNoChip
	}
	r, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: c.EfuseRepeat0, Words: 4}})
	if err != nil {
		return Security{}, err
	}
	w := r[0]
	s := Security{
		FlashEncryptionCount: int(w[1] >> 18 & 7),
		SecureBootEnabled:    w[2]>>20&1 == 1,
		USBJTAGDisabled:      w[0]>>9&1 == 1,
		PadJTAGDisabled:      w[0]>>19&1 == 1,
		SoftJTAGDisable:      int(w[0] >> 16 & 7),
		DownloadModeDisabled: w[3]&1 == 1,
		SecurityDownload:     w[3]>>5&1 == 1,
	}
	copy(s.Raw[:], w)
	return s, nil
}

// CheckFlashWritable refuses flash modification on a chip with flash
// encryption or secure boot: a plaintext image would not boot, and the
// stored image cannot be read back as written.
func (s Security) CheckFlashWritable() error {
	if s.SecureBootEnabled {
		return fmt.Errorf("secure boot is enabled (eFuse SECURE_BOOT_EN): refusing to modify flash")
	}
	if s.FlashEncrypted() {
		return fmt.Errorf("flash encryption is enabled (SPI_BOOT_CRYPT_CNT=%d): refusing to modify flash", s.FlashEncryptionCount)
	}
	return nil
}
