package espromplugin

import (
	"encoding/binary"

	"github.com/faronov/hwlab/plugins/esprom/rom"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// Ids of the SYNTHETIC devices offered with HWLAB_ESPROM_SIM=1.
const (
	SimESP32  = "sim-esp32-uart"
	SimBridge = "sim-h2-bridge"
	SimS3USJ  = "sim-s3-usj"
)

// SimPartitions is the layout the simulated chips carry: the usual ESP-IDF
// OTA layout plus a custom data partition like the zigbee-rs-hub "hubdata".
var SimPartitions = []esp.Partition{
	{Label: "nvs", Type: 0x01, Subtype: 0x02, Offset: 0x9000, Size: 0x4000},
	{Label: "otadata", Type: 0x01, Subtype: 0x00, Offset: 0xD000, Size: 0x2000},
	{Label: "phy_init", Type: 0x01, Subtype: 0x01, Offset: 0xF000, Size: 0x1000},
	{Label: "ota_0", Type: 0x00, Subtype: 0x10, Offset: 0x10000, Size: 0x100000},
	{Label: "ota_1", Type: 0x00, Subtype: 0x11, Offset: 0x110000, Size: 0x100000},
	{Label: "hubdata", Type: 0x01, Subtype: 0x99, Offset: 0x210000, Size: 0x10000},
}

// SimAppDesc is an esp_app_desc_t for a SYNTHETIC application.
func SimAppDesc(project, version string) []byte {
	b := make([]byte, 256)
	binary.LittleEndian.PutUint32(b, 0xABCD5432)
	copy(b[16:48], version)
	copy(b[48:80], project)
	copy(b[80:96], "12:00:00")
	copy(b[96:112], "Nov 24 2026")
	copy(b[112:144], "v5.5.1")
	return b
}

// SimImage builds a valid ESP application image for ch.
func SimImage(ch *rom.Chip, project, version string, extra int) []byte {
	seg := append(SimAppDesc(project, version), make([]byte, extra)...)
	for i := 256; i < len(seg); i++ {
		seg[i] = byte(i * 13)
	}
	return esp.BuildImage(ch.ImageChipID, 0x40080000, map[uint32][]byte{0x3F400020: seg}, []uint32{0x3F400020})
}

// SimFlash fills a sim with a bootloader, the partition table, an app in
// ota_0 selected by otadata, nvs data and blank space elsewhere.
func SimFlash(s *rom.Sim) {
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	ch := s.Chip
	copy(s.Flash[ch.BootloaderOffset:], esp.BuildImage(ch.ImageChipID, 0x40080400, map[uint32][]byte{0x3FFF0000: make([]byte, 64)}, []uint32{0x3FFF0000}))
	copy(s.Flash[esp.PartitionTableOffset:], esp.BuildPartitionTable(SimPartitions))
	copy(s.Flash[0x10000:], SimImage(ch, "sim_app", "1.2.3", 8000))
	copy(s.Flash[0x9000:], []byte("SYNTHETIC nvs: wifi credentials, keys"))
	e := make([]byte, 32)
	binary.LittleEndian.PutUint32(e, 1)
	binary.LittleEndian.PutUint32(e[24:], 2)
	binary.LittleEndian.PutUint32(e[28:], otaCRC(e[:4]))
	copy(s.Flash[0xD000:], e)
}

// SimEntries are the simulated devices: a classic ESP32 on a USB-UART
// bridge, an ESP32-H2 behind the S3 bridge and an ESP32-S3 on its own
// USB-Serial/JTAG port.
func SimEntries() []Entry {
	mk := func(ch *rom.Chip, w rom.Entry, size int) *rom.Sim {
		s := rom.NewSim(ch, w, size)
		SimFlash(s)
		s.PowerOn()
		return s
	}
	return []Entry{
		{ID: SimESP32, Kind: KindUART, Entry: rom.EntryClassic, Baud: KindUART.defaultBaud(), Sim: mk(rom.ESP32, rom.EntryClassic, 4<<20)},
		{ID: SimBridge, Kind: KindBridge, Entry: rom.EntryClassic, Baud: KindBridge.defaultBaud(), Sim: mk(rom.ESP32H2, rom.EntryClassic, 4<<20)},
		{ID: SimS3USJ, Kind: KindUSJ, Entry: rom.EntryUSJ, Baud: 0, Sim: mk(rom.ESP32S3, rom.EntryUSJ, 4<<20)},
	}
}
