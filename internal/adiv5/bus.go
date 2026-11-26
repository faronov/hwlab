package adiv5

import (
	"context"
	"fmt"
)

// Bus adapts a Target to the 32-bit bus the Cortex-M layer drives
// (cortexm.Bus, BlockBus and ByteWriter). Context-aware links receive the
// current operation's context, including a fresh context for cleanup.
type Bus struct{ T *Target }

// Read32 reads one aligned word.
func (b Bus) Read32(ctx context.Context, addr uint32) (uint32, error) {
	b.setContext(ctx)
	return b.T.ReadWord(addr)
}

// Write32 writes one aligned word.
func (b Bus) Write32(ctx context.Context, addr, v uint32) error {
	b.setContext(ctx)
	return b.T.WriteWord(addr, v)
}

func (b Bus) setContext(ctx context.Context) {
	if link, ok := b.T.p.(interface{ SetContext(context.Context) }); ok {
		link.SetContext(ctx)
	}
}

// ReadBlock reads len(p) bytes with DAP block transfers.
func (b Bus) ReadBlock(ctx context.Context, addr uint32, p []byte) error {
	b.setContext(ctx)
	data, err := b.T.ReadMem(addr, len(p))
	copy(p, data)
	return err
}

// WriteBlock writes data with DAP block transfers.
func (b Bus) WriteBlock(ctx context.Context, addr uint32, data []byte) error {
	b.setContext(ctx)
	return b.T.WriteMem(addr, data)
}

// WriteBytes writes data of any length and alignment; the MEM-AP does sub-word
// accesses on the right byte lane, so neighbouring bytes are not touched.
func (b Bus) WriteBytes(ctx context.Context, addr uint32, data []byte) error {
	b.setContext(ctx)
	return b.T.WriteMem(addr, data)
}

// APInfo is what an access port reports about itself.
type APInfo struct {
	Index   uint8  `json:"index"`
	IDR     uint32 `json:"idr"`
	Base    uint32 `json:"base"`
	CFG     uint32 `json:"cfg"`
	Class   string `json:"class"`
	Type    string `json:"type"`
	Present bool   `json:"present"`
	// Error is set when the AP did not answer; the scan stops there.
	Error string `json:"error,omitempty"`
}

var apTypes = map[uint32]string{0: "JTAG-AP", 1: "AHB3-AP", 2: "APB2/3-AP", 3: "AXI3-AP", 4: "AHB5-AP", 5: "APB4/5-AP", 6: "AXI5-AP", 7: "AHB5-AP(HPROT)"}

func decodeIDR(idr uint32) (class, typ string) {
	switch (idr >> 13) & 0xF {
	case 0:
		class = "no class"
	case 1:
		class = "COM-AP"
	case 8:
		class = "MEM-AP"
	default:
		class = fmt.Sprintf("class %d", (idr>>13)&0xF)
	}
	typ = apTypes[idr&0xF]
	if typ == "" {
		typ = fmt.Sprintf("type %d", idr&0xF)
	}
	return
}

// ScanAPs reads IDR of access ports 0..max-1, and BASE and CFG of the
// MEM-APs among them (those registers are not defined for a JTAG-AP or a
// vendor COM-AP). It stops at the first absent AP (IDR reads 0) and at the
// first one that does not answer: some devices have APs behind a power or
// security domain whose access never completes, which leaves the DP
// answering WAIT (or nothing). Such an AP is listed with Error, the stuck
// transaction is aborted (DAPABORT) and the debug port is brought up again,
// so the scan result is returned without an error. An error is returned only
// when the debug port itself cannot be recovered. It only reads and leaves
// the selected AP as it was.
func (t *Target) ScanAPs(max int) ([]APInfo, error) {
	saved := t.ap
	defer func() {
		t.ap = saved
		t.invalidate()
	}()
	var out []APInfo
	for i := 0; i < max && i < 256; i++ {
		t.ap = uint8(i)
		t.invalidate()
		info := APInfo{Index: uint8(i)}
		idr, err := t.ReadAP(apIDR)
		if err == nil && idr == 0 {
			break
		}
		if err == nil {
			info.IDR, info.Present = idr, true
			info.Class, info.Type = decodeIDR(idr)
			if isMemAP(idr) {
				if info.Base, err = t.ReadAP(apBase); err == nil {
					info.CFG, err = t.ReadAP(apCFG)
				}
			}
		}
		if err != nil {
			info.Error = err.Error()
			out = append(out, info)
			t.ap = saved
			if rerr := t.Reinit(); rerr != nil {
				return out, fmt.Errorf("AP %d did not answer (%w) and the debug port could not be recovered: %w", i, err, rerr)
			}
			break
		}
		out = append(out, info)
	}
	return out, nil
}

// ScanReport is the ap_scan result shared by the SWD probe plugins: the APs
// (with errors for those that did not answer), the ROM table behind the
// selected MEM-AP and how that AP is accessed.
func (t *Target) ScanReport(max int) (map[string]any, error) {
	aps, err := t.ScanAPs(max)
	if err != nil {
		return nil, err
	}
	hex32 := func(v uint32) string { return fmt.Sprintf("0x%08x", v) }
	list := []any{}
	var rom []any
	for _, ap := range aps {
		e := map[string]any{"index": float64(ap.Index), "present": ap.Present}
		if ap.Error != "" {
			e["error"] = ap.Error
			list = append(list, e)
			continue
		}
		e["idr"], e["class"], e["type"] = hex32(ap.IDR), ap.Class, ap.Type
		if ap.Class == "MEM-AP" {
			e["base"], e["cfg"] = hex32(ap.Base), hex32(ap.CFG)
		}
		list = append(list, e)
		if ap.Index == t.ap && ap.Class == "MEM-AP" && ap.Base&1 != 0 && ap.Base != 0xFFFFFFFF {
			comps, werr := t.WalkROM(ap.Base)
			for _, c := range comps {
				rom = append(rom, map[string]any{"address": hex32(c.Address), "class": float64(c.Class), "part": fmt.Sprintf("0x%03x", c.Part), "name": c.Name, "pid": fmt.Sprintf("0x%x", c.PID)})
			}
			if werr != nil {
				rom = append(rom, map[string]any{"error": werr.Error()})
			}
		}
	}
	res := map[string]any{"dpidr": hex32(t.DPIDR), "aps": list, "rom_table": rom, "selected_ap": float64(t.ap), "csw": hex32(t.CSW)}
	if t.Security != "" {
		res["access"] = t.Security
	}
	if t.WaitAborts > 0 {
		res["dap_aborts"] = float64(t.WaitAborts)
	}
	return res, nil
}

// Component is a CoreSight component found in a ROM table.
type Component struct {
	Address uint32 `json:"address"`
	Class   uint32 `json:"class"`
	PID     uint64 `json:"pid"`
	Part    uint16 `json:"part"`
	Name    string `json:"name,omitempty"`
}

var partNames = map[uint16]string{
	0x000: "Cortex-M3 SCS", 0x001: "Cortex-M3 ITM", 0x002: "Cortex-M3 DWT", 0x003: "Cortex-M3 FPB",
	0x008: "Cortex-M0 SCS", 0x00A: "Cortex-M0 DWT", 0x00B: "Cortex-M0 BPU",
	0x00C: "Cortex-M4 SCS", 0x00D: "CoreSight ETM-M4", 0x00E: "Cortex-M4 FPB",
	0x4C0: "Cortex-M0+ ROM table", 0x4C3: "Cortex-M3 ROM table", 0x4C4: "Cortex-M4 ROM table",
	0x471: "Cortex-M0 ROM table", 0x9A1: "Cortex-M4 TPIU", 0x9A9: "Cortex-M7 TPIU",
	0x920: "ETM", 0x907: "ETB", 0x906: "CoreSight CTI",
}

// ReadComponent reads the CoreSight identification registers at base.
func (t *Target) ReadComponent(base uint32) (Component, error) {
	base &^= 0xFFF
	var c [4]uint32
	for i := range c {
		v, err := t.ReadWord(base + 0xFF0 + 4*uint32(i))
		if err != nil {
			return Component{}, err
		}
		c[i] = v & 0xFF
	}
	var p [8]uint32
	offs := [8]uint32{0xFE0, 0xFE4, 0xFE8, 0xFEC, 0xFD0, 0xFD4, 0xFD8, 0xFDC}
	for i, o := range offs {
		v, err := t.ReadWord(base + o)
		if err != nil {
			return Component{}, err
		}
		p[i] = v & 0xFF
	}
	comp := Component{Address: base}
	if c[0] != 0x0D || c[2] != 0x05 || c[3] != 0xB1 {
		return comp, fmt.Errorf("0x%08X: component ID preamble is %02X %02X %02X %02X, not a CoreSight component", base, c[0], c[1], c[2], c[3])
	}
	comp.Class = c[1] >> 4
	for i, v := range p {
		comp.PID |= uint64(v) << (8 * uint(i))
	}
	comp.Part = uint16(comp.PID & 0xFFF)
	comp.Name = partNames[comp.Part]
	return comp, nil
}

// WalkROM lists the components below the ROM table at base, one level deep
// (nested tables are reported but not entered, which keeps a bad table from
// causing unbounded reads).
func (t *Target) WalkROM(base uint32) ([]Component, error) {
	base &^= 0xFFF
	var out []Component
	for i := uint32(0); i < 960; i++ {
		e, err := t.ReadWord(base + 4*i)
		if err != nil {
			return out, err
		}
		if e == 0 {
			break
		}
		if e&1 == 0 {
			continue
		}
		off := int32(e & 0xFFFFF000) // signed offset in 4 KiB units
		c, err := t.ReadComponent(uint32(int64(base) + int64(off)))
		if err != nil {
			c.Address = uint32(int64(base) + int64(off))
			c.Name = "unreadable: " + err.Error()
		}
		out = append(out, c)
	}
	return out, nil
}
