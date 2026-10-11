package native

import (
	"io"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// Geometry returns the geometry identified by ReadChipInfo in this session.
func (d *Debugger) Geometry() (target.Geometry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.geom == nil {
		return target.Geometry{}, false
	}
	return *d.geom, true
}

// flashLimit is the exclusive physical flash range ReadFlashCode accepts: the
// identified flash size, or the legacy hardware-validated CC2530F256 window
// that excludes the NV and lock pages until the chip has been identified.
func (d *Debugger) flashLimit() int {
	if g, ok := d.Geometry(); ok {
		return g.FlashSize
	}
	return FlashSize
}

// geometry returns the identified geometry, identifying the chip first when
// needed. It must not be called from inside an operation.
func (d *Debugger) geometry() (target.Geometry, error) {
	if g, ok := d.Geometry(); ok {
		return g, nil
	}
	info, err := d.ReadChipInfo()
	return info.Geometry, err
}

// ReadChipInfo identifies the chip from its CHIPID, CHVER, CHIPINFO0 and
// CHIPINFO1 registers (read-only identity registers, XDATA 0x6249/0x624A/
// 0x6276/0x6277) on the halted CPU, restoring the CPU registers. The decoded
// geometry is cached for flash access and programming. A requested chip name
// is never trusted over this.
func (d *Debugger) ReadChipInfo() (info ChipInfo, err error) {
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			var regs [4]byte
			for i, a := range []uint16{target.CHIPIDAddr, target.CHVERAddr, target.CHIPINFO0Addr, target.CHIPINFO1Addr} {
				b, err := d.xget(a, 1, false, dl)
				if err != nil {
					return err
				}
				regs[i] = b[0]
			}
			g, err := target.Decode(regs[0], regs[1], regs[2], regs[3])
			if err != nil {
				return transportf("target identity: %v", err)
			}
			st, err := d.adapterState(dl)
			if err != nil {
				return err
			}
			info = ChipInfo{Geometry: g, AdapterTargetID: st.TargetID, CHIPINFO0: regs[2], CHIPINFO1: regs[3]}
			d.geom = &g
			return nil
		})
	})
	return
}

// ReadChipIDCommand issues the debug interface's GET_CHIP_ID command, which
// also works on a debug-locked target. It returns CHIPID and CHVER.
func (d *Debugger) ReadChipIDCommand() (chipID, revision uint8, err error) {
	err = d.target(need{}, func(dl *deadline) error {
		b, e := d.exchange([]byte{0x3F, cmdGetChipID}, 2, dl)
		if e != nil {
			return e
		}
		chipID, revision = b[0], b[1]
		return nil
	})
	return
}

// readFlashOp reads a physical flash range of at most flashChunk bytes inside
// one bank as one bounded operation.
func (d *Debugger) readFlashOp(addr, n int) (out []byte, err error) {
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			var e error
			out, e = d.flashRead(addr, n, dl)
			return e
		})
	})
	return
}

// ReadFlash streams length bytes of physical flash starting at addr to w, in
// bounded chunks that never cross a 32 KiB bank. The range must lie inside the
// identified flash, including the NV and lock pages. Reading is non-destructive.
func (d *Debugger) ReadFlash(addr, length int, w io.Writer, progress Progress) (int, error) {
	g, err := d.geometry()
	if err != nil {
		return 0, err
	}
	if addr < 0 || length < 1 || addr+length > g.FlashSize {
		return 0, invalidf("flash read 0x%X+0x%X outside the %d-byte flash", addr, length, g.FlashSize)
	}
	done := 0
	for done < length {
		a := addr + done
		_, off := target.Bank(a)
		n := min(length-done, flashChunk, target.BankSize-off)
		b, err := d.readFlashOp(a, n)
		if err != nil {
			return done, err
		}
		if _, err := w.Write(b); err != nil {
			return done, err
		}
		done += n
		if progress != nil {
			progress("read", done, length)
		}
	}
	return done, nil
}

// ReadFlashBytes reads a small physical range (at most 64 KiB) into memory.
func (d *Debugger) ReadFlashBytes(addr, length int) ([]byte, error) {
	if length < 1 || length > 1<<16 {
		return nil, invalidf("in-memory flash read must be 1..65536 bytes")
	}
	var buf sliceWriter
	if _, err := d.ReadFlash(addr, length, &buf, nil); err != nil {
		return nil, err
	}
	return buf.b, nil
}

type sliceWriter struct{ b []byte }

func (s *sliceWriter) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }

// ReadLock reads and decodes the 16-byte lock-bit structure at the end of
// flash. It works only while the debug interface is unlocked.
func (d *Debugger) ReadLock() (target.Lock, error) {
	g, err := d.geometry()
	if err != nil {
		return target.Lock{}, err
	}
	raw, err := d.readFlashOp(g.LockAddr(), target.LockSize)
	if err != nil {
		return target.Lock{}, err
	}
	return target.DecodeLock(g, raw)
}

// ReadInfoPage reads the 2 KB read-only information page (XDATA 0x7800).
func (d *Debugger) ReadInfoPage() (page []byte, err error) {
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			var e error
			page, e = d.xget(target.InfoPageXDATA, target.InfoPageSize, false, dl)
			return e
		})
	})
	return
}

// ReadIEEE reads the primary IEEE address from the information page and the
// secondary one from flash, just below the lock-bit structure. An all-0x00 or
// all-0xFF address is reported as absent.
func (d *Debugger) ReadIEEE() (IEEEAddresses, error) {
	g, err := d.geometry()
	if err != nil {
		return IEEEAddresses{}, err
	}
	var res IEEEAddresses
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			var e error
			if res.Primary, e = d.xget(uint16(g.PrimaryIEEE), target.IEEESize, false, dl); e != nil {
				return e
			}
			res.Secondary, e = d.flashRead(g.SecondaryIEEEAddr(), target.IEEESize, dl)
			return e
		})
	})
	if err != nil {
		return res, err
	}
	res.PrimarySource = "information page, XDATA 0x" + hexStr(g.PrimaryIEEE)
	res.SecondarySource = "flash 0x" + hexStr(g.SecondaryIEEEAddr()) + " (just below the lock-bit structure)"
	res.PrimaryPresent = present(res.Primary)
	res.SecondaryPresent = present(res.Secondary)
	return res, nil
}

func present(b []byte) bool {
	zero, ff := true, true
	for _, v := range b {
		zero = zero && v == 0
		ff = ff && v == 0xFF
	}
	return !zero && !ff
}

func hexStr(v int) string {
	const digits = "0123456789ABCDEF"
	if v == 0 {
		return "0"
	}
	var b []byte
	for ; v > 0; v >>= 4 {
		b = append([]byte{digits[v&15]}, b...)
	}
	return string(b)
}
