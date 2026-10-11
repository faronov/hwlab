package target

import "github.com/faronov/hwlab/plugins/telink/sws"

// tlsr825x covers the TLSR825x family (chip id 0x5562 on SWire): TLSR8250,
// 8251, 8253, 8258 and relatives. The chip id does not tell them apart.
var tlsr825x = &Family{
	Name:        "TLSR825x",
	ChipIDs:     []uint16{0x5562},
	SRAMStart:   0x840000,
	SRAMEnd:     0x850000,
	VariantNote: "chip id 0x5562 is shared by all TLSR825x; the variant is not determined",
	layouts: []layoutDef{{
		Layout: sws.Layout{Name: "tb04-512k", Size: 0x80000, Regions: []sws.Region{
			{Name: "application", Start: 0x00000, End: 0x70000, Class: sws.ClassApp},
			{Name: "aps-journal", Start: 0x70000, End: 0x72000, Class: sws.ClassJournal},
			{Name: "child-journal", Start: 0x72000, End: 0x74000, Class: sws.ClassJournal},
			{Name: "security-journal", Start: 0x74000, End: 0x76000, Class: sws.ClassJournal},
			{Name: "factory-eui64", Start: 0x76000, End: 0x77000, Class: sws.ClassFactory},
			{Name: "factory-config-adc", Start: 0x77000, End: 0x78000, Class: sws.ClassFactory},
		}},
		Source:   "zigbee-rs docs/book/src/platform-guides/telink.md, TB-04 module",
		Evidence: []string{"factory-eui64"},
	}},
}
