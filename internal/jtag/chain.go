package jtag

import (
	"context"
	"errors"
	"fmt"
)

// IDCode is a decoded IEEE 1149.1 identification register.
type IDCode struct {
	Raw uint32 `json:"raw"`
	// Version is bits 31:28.
	Version uint8 `json:"version"`
	// Part is bits 27:12.
	Part uint16 `json:"part"`
	// Manufacturer is bits 11:1, the JEDEC JEP106 code (continuation count in
	// bits 10:7, identity in bits 6:0).
	Manufacturer uint16 `json:"manufacturer"`
	// ManufacturerName is informational and empty for unknown codes.
	ManufacturerName string `json:"manufacturer_name,omitempty"`
}

// ErrNotIDCode is returned for a word whose bit 0 is clear: the 1149.1
// standard reserves bit 0 = 1 for IDCODE, so such a word is something else.
var ErrNotIDCode = errors.New("jtag: bit 0 of an IDCODE must be 1")

// jedec maps 11-bit manufacturer codes to names. It is informational only and
// deliberately small; unknown codes simply have no name.
var jedec = map[uint16]string{
	0x23B: "ARM",
	0x049: "Xilinx",
	0x06E: "Intel/Altera",
	0x043: "Lattice",
	0x017: "Texas Instruments",
	0x020: "STMicroelectronics",
	0x489: "SiFive",
	0x01F: "Microchip/Atmel",
	0x00E: "Freescale/NXP",
}

// DecodeIDCode splits an IDCODE word into its fields.
func DecodeIDCode(raw uint32) (IDCode, error) {
	if raw&1 == 0 {
		return IDCode{Raw: raw}, ErrNotIDCode
	}
	id := IDCode{
		Raw:          raw,
		Version:      uint8(raw >> 28),
		Part:         uint16(raw >> 12),
		Manufacturer: uint16(raw >> 1 & 0x7FF),
	}
	id.ManufacturerName = jedec[id.Manufacturer]
	return id, nil
}

func (i IDCode) String() string {
	name := i.ManufacturerName
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("0x%08x (mfg 0x%03x %s, part 0x%04x, ver %d)", i.Raw, i.Manufacturer, name, i.Part, i.Version)
}

// Device is one TAP in the chain. Index 0 is the TAP nearest TDO (the first
// whose bits are shifted out).
type Device struct {
	Index int `json:"index"`
	// Bypass is true when the TAP has no IDCODE register and presented a
	// single 0 bit.
	Bypass   bool    `json:"bypass,omitempty"`
	IDCode   *IDCode `json:"idcode,omitempty"`
	IRLength int     `json:"ir_length,omitempty"`
}

// Chain status values.
const (
	// StatusOK means at least one TAP answered.
	StatusOK = "ok"
	// StatusNoDevice means TDO read all ones: nothing drives TDO (or it is
	// pulled high).
	StatusNoDevice = "no-device"
	// StatusStuckLow means TDO read all zeros: shorted low or an unconnected
	// line pulled down.
	StatusStuckLow = "stuck-low"
	// StatusAmbiguous means the data could not be parsed as a chain.
	StatusAmbiguous = "ambiguous"
)

// Chain is the result of a scan-chain enumeration.
type Chain struct {
	Status  string   `json:"status"`
	Devices []Device `json:"devices"`
	// BitsRead is how many DR bits were captured while enumerating.
	BitsRead int `json:"bits_read"`
	// TotalIR is the measured total instruction register length (0 when not
	// measured).
	TotalIR int `json:"total_ir_length,omitempty"`
	// BypassBits is the measured number of TAPs counted through BYPASS (-1
	// when not measured).
	BypassBits int      `json:"bypass_count"`
	Notes      []string `json:"notes,omitempty"`
}

// ScanOptions tune ScanChain.
type ScanOptions struct {
	// MaxDevices bounds the chain length (default 32).
	MaxDevices int
	// MeasureIR also measures the total IR length and the TAP count through
	// BYPASS. This loads the all-ones instruction (BYPASS on every compliant
	// TAP) and ends with a TAP reset.
	MeasureIR bool
	// IRLengths optionally gives each device's IR length, nearest TDO first;
	// it is checked against the measured total.
	IRLengths []int
}

// ScanChain enumerates the chain: it resets the TAPs, shifts out the DR
// (IDCODE or BYPASS, whichever each TAP selects after reset) while feeding
// ones, and parses the stream.
func ScanChain(ctx context.Context, d *Driver, opt ScanOptions) (Chain, error) {
	if opt.MaxDevices <= 0 {
		opt.MaxDevices = 32
	}
	n := 32*opt.MaxDevices + 64
	if err := d.Reset(ctx); err != nil {
		return Chain{}, err
	}
	raw, err := d.ShiftDR(ctx, Ones(n), n, true)
	if err != nil {
		return Chain{}, err
	}
	c := ParseChain(raw, n, opt.MaxDevices)
	if c.Status == StatusOK && opt.MeasureIR {
		measureLengths(ctx, d, &c, opt)
	} else {
		c.BypassBits = -1
	}
	if err := d.Reset(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// ParseChain interprets the DR bits captured after TAP reset with all ones fed
// to TDI (see ScanChain). It is pure so it can be tested on fixtures.
func ParseChain(raw []byte, nbits, maxDevices int) Chain {
	c := Chain{BitsRead: nbits, BypassBits: -1}
	ones, zeros := true, true
	for i := 0; i < nbits; i++ {
		if GetBit(raw, i) {
			zeros = false
		} else {
			ones = false
		}
	}
	switch {
	case ones:
		c.Status = StatusNoDevice
		c.Notes = append(c.Notes, "TDO read all ones: no TAP answered (nothing connected, or TDO pulled high)")
		return c
	case zeros:
		c.Status = StatusStuckLow
		c.Notes = append(c.Notes, "TDO read all zeros: stuck low or shorted")
		return c
	}
	i := 0
	for i < nbits {
		if !GetBit(raw, i) {
			c.Devices = append(c.Devices, Device{Index: len(c.Devices), Bypass: true})
			i++
		} else {
			if i+32 > nbits {
				c.Notes = append(c.Notes, "truncated IDCODE at end of capture")
				c.Status = StatusAmbiguous
				return c
			}
			w := uint32(Field(raw, i, 32))
			if w == 0xFFFFFFFF {
				break
			}
			id, _ := DecodeIDCode(w)
			c.Devices = append(c.Devices, Device{Index: len(c.Devices), IDCode: &id})
			i += 32
		}
		if len(c.Devices) > maxDevices {
			c.Devices = nil
			c.Status = StatusAmbiguous
			c.Notes = append(c.Notes, fmt.Sprintf("more than %d TAPs parsed: TDO does not look like a real chain", maxDevices))
			return c
		}
	}
	if len(c.Devices) == 0 {
		c.Status = StatusAmbiguous
		c.Notes = append(c.Notes, "no TAP could be parsed from the capture")
		return c
	}
	c.Status = StatusOK
	return c
}

// measureLengths finds the total IR length and the TAP count by flushing the
// register with ones and timing the appearance of a single zero.
func measureLengths(ctx context.Context, d *Driver, c *Chain, opt ScanOptions) {
	const limit = 256
	ir, err := measure(ctx, d, true, limit)
	if err != nil {
		c.Notes = append(c.Notes, "IR length measurement failed: "+err.Error())
		return
	}
	c.TotalIR = ir
	if ir == 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("IR length not found within %d bits", limit))
	}
	bp, err := measure(ctx, d, false, limit)
	if err != nil {
		c.Notes = append(c.Notes, "BYPASS count measurement failed: "+err.Error())
	} else if bp == 0 {
		c.Notes = append(c.Notes, "BYPASS count not found")
	} else {
		c.BypassBits = bp
		if bp != len(c.Devices) {
			c.Notes = append(c.Notes, fmt.Sprintf("BYPASS path is %d bit(s) but %d TAP(s) were enumerated", bp, len(c.Devices)))
		}
	}
	if len(opt.IRLengths) > 0 {
		if len(opt.IRLengths) != len(c.Devices) {
			c.Notes = append(c.Notes, fmt.Sprintf("%d IR length(s) configured for %d TAP(s)", len(opt.IRLengths), len(c.Devices)))
			return
		}
		sum := 0
		for i, l := range opt.IRLengths {
			c.Devices[i].IRLength = l
			sum += l
		}
		if c.TotalIR != 0 && sum != c.TotalIR {
			c.Notes = append(c.Notes, fmt.Sprintf("configured IR lengths sum to %d but %d was measured", sum, c.TotalIR))
		}
	} else if len(c.Devices) == 1 && c.TotalIR > 0 {
		c.Devices[0].IRLength = c.TotalIR
	}
}

// measure flushes the IR or DR with ones, then shifts a zero followed by ones
// and returns the number of clocks until the zero emerges (the register
// length), or 0 if it does not within limit.
func measure(ctx context.Context, d *Driver, ir bool, limit int) (int, error) {
	shift, pause := d.ShiftDRTo, PauseDR
	if ir {
		shift, pause = d.ShiftIRTo, PauseIR
	}
	if _, err := shift(ctx, Ones(limit), limit, false, pause); err != nil {
		return 0, err
	}
	pat := Ones(limit + 1)
	SetBit(pat, 0, false)
	got, err := shift(ctx, pat, limit+1, true, RunTestIdle)
	if err != nil {
		return 0, err
	}
	for i := 0; i <= limit; i++ {
		if !GetBit(got, i) {
			return i, nil
		}
	}
	return 0, nil
}
