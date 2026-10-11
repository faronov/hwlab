package targetaccept

import "encoding/binary"

// Firmware is the small hwlab-owned image the destructive flash acceptance
// programs. It is hand assembled Thumb code (ARMv6-M), reproducible byte for
// byte without a toolchain, and contains no security configuration: the CCFG
// is never part of it.
//
//	0x00  vector table: SP 0x20009000, reset, NMI/HardFault/... -> hang
//	0x40  reset: r1 = &counter; *r1 = 0; loop { (*r1)++; bp_func(); }
//	0x5C  bp_func: r4++ ; return           (breakpoint friendly, 16-bit code)
//	0x60  marker "HWLAB-CC2340-ACCEPT-1"
//	0x80  hang: b .
type Firmware struct {
	Image          []byte
	ResetHandler   uint32 // Thumb address of reset, bit 0 clear
	BreakpointFunc uint32 // address of bp_func, bit 0 clear
	CounterAddr    uint32 // SRAM word incremented once per loop iteration
	Marker         string
	MarkerOffset   int
}

// AcceptanceFirmware returns the image to be programmed at flash address 0.
func AcceptanceFirmware() Firmware {
	const (
		sp       = 0x20009000
		counter  = 0x20008F00
		resetOff = 0x40
		bpOff    = 0x5C
		markOff  = 0x60
		hangOff  = 0x80
		marker   = "HWLAB-CC2340-ACCEPT-1"
	)
	img := make([]byte, 0xA0)
	w32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(img[off:], v) }
	w16 := func(off int, v uint16) { binary.LittleEndian.PutUint16(img[off:], v) }
	w32(0, sp)
	w32(4, resetOff|1)
	for off := 8; off < 0x40; off += 4 {
		w32(off, hangOff|1)
	}
	code := []uint16{
		0x4904, // 0x40 ldr  r1, [pc, #16]   ; counter address (0x54)
		0x2200, // 0x42 movs r2, #0
		0x600A, // 0x44 str  r2, [r1]
		0x6808, // 0x46 loop: ldr r0, [r1]
		0x3001, // 0x48 adds r0, #1
		0x6008, // 0x4A str  r0, [r1]
		0x4B02, // 0x4C ldr  r3, [pc, #8]    ; bp_func (0x58)
		0x479B, // 0x4E blx  r3
		0xE7F9, // 0x50 b    loop
		0x46C0, // 0x52 nop
	}
	for i, c := range code {
		w16(resetOff+2*i, c)
	}
	w32(0x54, counter)
	w32(0x58, bpOff|1)
	w16(bpOff, 0x3401)   // adds r4, #1
	w16(bpOff+2, 0x4770) // bx lr
	copy(img[markOff:], marker)
	w16(hangOff, 0xE7FE) // b .
	return Firmware{Image: img, ResetHandler: resetOff, BreakpointFunc: bpOff, CounterAddr: counter, Marker: marker, MarkerOffset: markOff}
}
