package adiv5

import "errors"

// ReadAPOn reads register reg (0x00..0xFC) of access port ap and then returns
// to the MEM-AP the Target was connected with. It is for vendor APs that sit
// next to the MEM-AP (for example a configuration or mailbox AP).
func (t *Target) ReadAPOn(ap uint8, reg byte) (uint32, error) {
	saved := t.ap
	t.ap = ap
	t.invalidate()
	defer func() {
		t.ap = saved
		t.invalidate()
	}()
	return t.ReadAP(reg)
}

// WriteAPOn writes register reg of access port ap; see ReadAPOn.
func (t *Target) WriteAPOn(ap uint8, reg byte, val uint32) error {
	saved := t.ap
	t.ap = ap
	t.invalidate()
	defer func() {
		t.ap = saved
		t.invalidate()
	}()
	return t.WriteAP(reg, val)
}

// WriteAPBlockOn writes data words to the same register of access port ap
// (for a FIFO-like vendor mailbox); see ReadAPOn.
func (t *Target) WriteAPBlockOn(ap uint8, reg byte, data []uint32) error {
	saved := t.ap
	t.ap = ap
	t.invalidate()
	defer func() {
		t.ap = saved
		t.invalidate()
	}()
	ops := t.prefix(reg)
	if len(ops) > 0 {
		if _, err := t.run(ops); err != nil {
			t.selOK = false
			return err
		}
	}
	if err := t.p.BlockWrite(true, reg, data); err != nil {
		if errors.Is(err, ErrFault) {
			t.recover()
		} else {
			t.invalidate()
		}
		return err
	}
	return nil
}

// ReadAPBlockOn reads words from the same register of access port ap.
func (t *Target) ReadAPBlockOn(ap uint8, reg byte, words int) ([]uint32, error) {
	saved := t.ap
	t.ap = ap
	t.invalidate()
	defer func() {
		t.ap = saved
		t.invalidate()
	}()
	ops := t.prefix(reg)
	if len(ops) > 0 {
		if _, err := t.run(ops); err != nil {
			t.selOK = false
			return nil, err
		}
	}
	out, err := t.p.BlockRead(true, reg, words)
	if err != nil {
		if errors.Is(err, ErrFault) {
			t.recover()
		} else {
			t.invalidate()
		}
		return nil, err
	}
	return out, nil
}
