// Package dfu programs an application into a Nordic nRF5 SDK "Open"
// bootloader (the nRF52840 dongle PCA10059 ships with one) over its USB CDC
// serial DFU transport, the same protocol as
// "nrfutil nrf5sdk-tools dfu usb-serial": SLIP-framed request/response
// objects carrying a protobuf init packet and the application image.
//
// Only application updates on an MBR-only part (no SoftDevice) are
// supported; the image must lie between AppStart and BootloaderStart so the
// MBR, the bootloader and its settings page can never be addressed.
package dfu

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// PCA10059 (nRF52840 dongle) flash layout with the Open DFU bootloader.
const (
	AppStart        = 0x00001000 // after the 4 KiB MBR
	BootloaderStart = 0x000E0000 // Open DFU bootloader, then its settings
	RAMStart        = 0x20000000
	RAMEnd          = 0x20040000
	// HWVersion is the init packet hw_version nrfutil uses for nRF52 (52).
	HWVersion = 52
)

// Image is a validated application ready for DFU.
type Image struct {
	Bin  []byte // contiguous image starting at AppStart
	Init []byte // init packet (.dat)
	// Source is "hex" or "zip".
	Source string
}

// ParseHex decodes Intel HEX and returns the contiguous image between its
// lowest and highest address, gaps filled with 0xFF (as nrfutil does), and
// its base address.
func ParseHex(data []byte) (base uint32, img []byte, err error) {
	mem := map[uint32]byte{}
	var upper uint32
	lo, hi := uint32(0xFFFFFFFF), uint32(0)
	eof := false
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if eof {
			return 0, nil, fmt.Errorf("hex line %d: data after end-of-file record", n+1)
		}
		if line[0] != ':' {
			return 0, nil, fmt.Errorf("hex line %d: missing ':'", n+1)
		}
		rec, err := hex.DecodeString(line[1:])
		if err != nil || len(rec) < 5 || len(rec) != 5+int(rec[0]) {
			return 0, nil, fmt.Errorf("hex line %d: malformed record", n+1)
		}
		var sum byte
		for _, b := range rec {
			sum += b
		}
		if sum != 0 {
			return 0, nil, fmt.Errorf("hex line %d: checksum mismatch", n+1)
		}
		count, addr, typ, payload := int(rec[0]), uint32(binary.BigEndian.Uint16(rec[1:3])), rec[3], rec[4:4+int(rec[0])]
		switch typ {
		case 0x00:
			for i := range count {
				a := upper + addr + uint32(i)
				if _, dup := mem[a]; dup {
					return 0, nil, fmt.Errorf("hex line %d: address 0x%08X written twice", n+1, a)
				}
				mem[a] = payload[i]
				lo, hi = min(lo, a), max(hi, a)
			}
		case 0x01:
			eof = true
		case 0x02:
			if count != 2 {
				return 0, nil, fmt.Errorf("hex line %d: bad segment address record", n+1)
			}
			upper = uint32(binary.BigEndian.Uint16(payload)) << 4
		case 0x04:
			if count != 2 {
				return 0, nil, fmt.Errorf("hex line %d: bad linear address record", n+1)
			}
			upper = uint32(binary.BigEndian.Uint16(payload)) << 16
		case 0x03, 0x05: // start address: irrelevant to the bootloader
		default:
			return 0, nil, fmt.Errorf("hex line %d: unknown record type %02X", n+1, typ)
		}
	}
	if !eof {
		return 0, nil, errors.New("hex: no end-of-file record (truncated file?)")
	}
	if len(mem) == 0 {
		return 0, nil, errors.New("hex: no data")
	}
	if hi-lo+1 > BootloaderStart {
		return 0, nil, fmt.Errorf("hex: spans 0x%08X..0x%08X, larger than any application", lo, hi)
	}
	img = bytes.Repeat([]byte{0xFF}, int(hi-lo+1))
	for a, b := range mem {
		img[a-lo] = b
	}
	return lo, img, nil
}

// CheckApp verifies that bin, loaded at base, is a plausible Cortex-M
// application for the dongle's application region.
func CheckApp(base uint32, bin []byte) error {
	if base != AppStart {
		return fmt.Errorf("image starts at 0x%08X; a dongle application must start at 0x%08X (after the MBR) — is it linked for the Open DFU bootloader?", base, AppStart)
	}
	end := uint64(base) + uint64(len(bin))
	if end > BootloaderStart {
		return fmt.Errorf("image ends at 0x%08X, inside the bootloader at 0x%08X", end, BootloaderStart)
	}
	if len(bin) < 8 {
		return errors.New("image too small to hold a vector table")
	}
	sp := binary.LittleEndian.Uint32(bin[0:])
	reset := binary.LittleEndian.Uint32(bin[4:])
	if sp < RAMStart || sp > RAMEnd || sp%4 != 0 {
		return fmt.Errorf("initial stack pointer 0x%08X is not in RAM: not a vector table", sp)
	}
	if reset&1 == 0 || uint64(reset&^1) < uint64(base) || uint64(reset&^1) >= end {
		return fmt.Errorf("reset vector 0x%08X is not a Thumb address inside the image", reset)
	}
	return nil
}

// LoadHex builds an Image from Intel HEX, generating the init packet.
func LoadHex(data []byte, appVersion uint32) (*Image, error) {
	base, bin, err := ParseHex(data)
	if err != nil {
		return nil, err
	}
	if err := CheckApp(base, bin); err != nil {
		return nil, err
	}
	return &Image{Bin: bin, Init: InitPacket(bin, appVersion), Source: "hex"}, nil
}

// LoadZip reads an nrfutil DFU package holding one application image and
// checks that its init packet describes that image.
func LoadZip(data []byte) (*Image, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("dfu package: %w", err)
	}
	read := func(name string) ([]byte, error) {
		f, err := zr.Open(name)
		if err != nil {
			return nil, fmt.Errorf("dfu package: %w", err)
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(io.LimitReader(f, BootloaderStart+1))
	}
	mb, err := read("manifest.json")
	if err != nil {
		return nil, err
	}
	var m struct {
		Manifest map[string]struct {
			BinFile string `json:"bin_file"`
			DatFile string `json:"dat_file"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("dfu package manifest: %w", err)
	}
	app, ok := m.Manifest["application"]
	if !ok || len(m.Manifest) != 1 {
		return nil, errors.New("dfu package must contain exactly one application image (no SoftDevice or bootloader updates)")
	}
	bin, err := read(app.BinFile)
	if err != nil {
		return nil, err
	}
	dat, err := read(app.DatFile)
	if err != nil {
		return nil, err
	}
	if err := CheckApp(AppStart, bin); err != nil {
		return nil, err
	}
	if err := CheckInit(dat, bin); err != nil {
		return nil, err
	}
	return &Image{Bin: bin, Init: dat, Source: "zip"}, nil
}

// Load reads a .hex or nrfutil .zip by file name. Raw .bin is refused: it
// carries no load address.
func Load(name string, data []byte, appVersion uint32) (*Image, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".hex", ".ihex":
		return LoadHex(data, appVersion)
	case ".zip":
		return LoadZip(data)
	}
	return nil, fmt.Errorf("%s: expected an Intel .hex or an nrfutil .zip package (a raw .bin has no load address)", name)
}
