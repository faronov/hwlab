package usbcap

import "encoding/binary"

// Synthetic returns a made-up capture: three devices on bus 1 (a HID mouse, a
// bulk-only mass-storage stick and a CDC-ACM serial port) being enumerated and
// used, with reports interrupt IN transfers on the mouse, one stalled request
// and one failed interrupt transfer. It is NOT recorded from hardware: it
// exists so the plugin, the analyzer, the replay tests and the demo run
// anywhere. VID 1209 is the pid.codes test vendor.
func Synthetic(reports int) []Packet {
	if reports < 1 {
		reports = 1
	}
	b := &synthBuilder{}
	b.hid(5, reports)
	b.msc(6)
	b.cdc(7)
	return b.pkts
}

type synthBuilder struct {
	pkts []Packet
	urb  uint64
	now  int64 // ns
}

const synthBase = 0xffff8800c0de0000

func (b *synthBuilder) advance(us int64) { b.now += us * 1000 }

// xfer emits a submission and its completion latUs later.
func (b *synthBuilder) xfer(sub, done Packet, latUs int64) {
	b.urb++
	sub.URBID, done.URBID = synthBase+b.urb*0x100, synthBase+b.urb*0x100
	sub.Event, done.Event = Submit, Complete
	sub.TimestampNs = b.now
	sub.Bus, done.Bus = 1, 1
	sub.Device = done.Device
	if sub.Status == 0 {
		sub.Status = -115
	}
	b.now += latUs * 1000
	done.TimestampNs = b.now
	b.pkts = append(b.pkts, sub, done)
}

func setup(rt, req byte, val, idx, length uint16) [8]byte {
	var s [8]byte
	s[0], s[1] = rt, req
	binary.LittleEndian.PutUint16(s[2:], val)
	binary.LittleEndian.PutUint16(s[4:], idx)
	binary.LittleEndian.PutUint16(s[6:], length)
	return s
}

// ctrl emits a control transfer; data is the data stage (IN: in the completion).
func (b *synthBuilder) ctrl(dev int, s [8]byte, data []byte, status int32, latUs int64) {
	st := ParseSetup(s)
	sub := Packet{Transfer: Control, Device: dev, In: st.In(), HasSetup: true, Setup: s, Length: uint32(st.Length)}
	done := Packet{Transfer: Control, Device: dev, In: st.In(), Status: status, Length: uint32(len(data))}
	if st.In() {
		done.Data = data
	} else {
		sub.Data = data
		done.Length = uint32(len(data))
	}
	b.xfer(sub, done, latUs)
	b.advance(150)
}

func (b *synthBuilder) data(dev int, tr Transfer, ep int, in bool, data []byte, length uint32, status int32, latUs int64) {
	sub := Packet{Transfer: tr, Device: dev, Endpoint: ep, In: in, Length: length}
	done := Packet{Transfer: tr, Device: dev, Endpoint: ep, In: in, Status: status, Length: length}
	if in {
		done.Data = data
		if status != 0 {
			done.Length = 0
		}
	} else {
		sub.Data = data
	}
	b.xfer(sub, done, latUs)
}

func devDesc(class byte, vid, pid uint16) []byte {
	d := []byte{18, 1, 0x00, 0x02, class, 0, 0, 64, 0, 0, 0, 0, 0x00, 0x01, 1, 2, 3, 1}
	binary.LittleEndian.PutUint16(d[8:], vid)
	binary.LittleEndian.PutUint16(d[10:], pid)
	return d
}

func iface(num, eps, class, sub, proto byte) []byte {
	return []byte{9, 4, num, 0, eps, class, sub, proto, 0}
}

func ep(addr, attr byte, mps uint16, interval byte) []byte {
	return []byte{7, 5, addr, attr, byte(mps), byte(mps >> 8), interval}
}

func cfgDesc(ifaces byte, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	c := []byte{9, 2, 0, 0, ifaces, 1, 0, 0x80, 50}
	binary.LittleEndian.PutUint16(c[2:], uint16(9+len(body)))
	return append(c, body...)
}

func (b *synthBuilder) enumerate(dev int, class byte, vid, pid uint16, cfg []byte) {
	d := devDesc(class, vid, pid)
	b.ctrl(dev, setup(0x80, ReqGetDescriptor, 0x0100, 0, 18), d, 0, 410)
	b.ctrl(dev, setup(0x80, ReqGetDescriptor, 0x0200, 0, 9), cfg[:9], 0, 380)
	b.ctrl(dev, setup(0x80, ReqGetDescriptor, 0x0200, 0, uint16(len(cfg))), cfg, 0, 390)
	b.ctrl(dev, setup(0x00, ReqSetConfiguration, 1, 0, 0), nil, 0, 520)
}

func (b *synthBuilder) hid(dev, reports int) {
	b.advance(1000)
	hidDesc := []byte{9, 0x21, 0x11, 0x01, 0, 1, 0x22, 52, 0}
	b.enumerate(dev, 0, 0x1209, 0x0001, cfgDesc(1, iface(0, 1, ClassHID, 1, 2), hidDesc, ep(0x81, 3, 4, 8)))
	b.ctrl(dev, setup(0x21, 0x0a, 0, 0, 0), nil, 0, 300)                                // SET_IDLE
	b.ctrl(dev, setup(0xa1, 0x03, 0, 0, 1), nil, -32, 250)                              // GET_PROTOCOL stalls
	b.ctrl(dev, setup(0x81, ReqGetDescriptor, 0x2200, 0, 52), make([]byte, 52), 0, 440) // HID report descriptor
	for i := 0; i < reports; i++ {
		b.advance(7500)
		status := int32(0)
		data := []byte{byte(i & 1), byte(i), 0xff - byte(i), 0}
		if i == reports/2 && reports > 4 {
			status, data = -71, nil // one bus error mid-capture
		}
		b.data(dev, Interrupt, 1, true, data, 4, status, 480)
	}
}

func (b *synthBuilder) msc(dev int) {
	b.advance(5000)
	b.enumerate(dev, 0, 0x1209, 0x0002, cfgDesc(1, iface(0, 2, ClassMSC, 6, 0x50), ep(0x81, 2, 512, 0), ep(0x02, 2, 512, 0)))
	b.ctrl(dev, setup(0xa1, 0xfe, 0, 0, 1), []byte{0}, 0, 200) // GET_MAX_LUN
	cbw := func(tag uint32, length uint32, in bool, cdb ...byte) []byte {
		c := make([]byte, 31)
		copy(c, "USBC")
		binary.LittleEndian.PutUint32(c[4:], tag)
		binary.LittleEndian.PutUint32(c[8:], length)
		if in {
			c[12] = 0x80
		}
		c[14] = byte(len(cdb))
		copy(c[15:], cdb)
		return c
	}
	csw := func(tag uint32, status byte) []byte {
		c := make([]byte, 13)
		copy(c, "USBS")
		binary.LittleEndian.PutUint32(c[4:], tag)
		c[12] = status
		return c
	}
	b.advance(2000)
	b.data(dev, Bulk, 2, false, cbw(1, 36, true, 0x12, 0, 0, 0, 36, 0), 31, 0, 160)
	inq := make([]byte, 36)
	copy(inq[8:], "HWLAB   SYNTH STICK     1.0 ")
	b.data(dev, Bulk, 1, true, inq, 36, 0, 210)
	b.data(dev, Bulk, 1, true, csw(1, 0), 13, 0, 140)
	b.advance(3000)
	b.data(dev, Bulk, 2, false, cbw(2, 512, true, 0x28, 0, 0, 0, 0, 0, 0, 0, 1, 0), 31, 0, 150)
	b.data(dev, Bulk, 1, true, []byte("synthetic block 0 ..............."), 512, 0, 900)
	b.data(dev, Bulk, 1, true, csw(2, 0), 13, 0, 130)
}

func (b *synthBuilder) cdc(dev int) {
	b.advance(8000)
	b.enumerate(dev, ClassCDC, 0x1209, 0x0003, cfgDesc(2,
		iface(0, 1, ClassCDC, 2, 1), ep(0x83, 3, 16, 16),
		iface(1, 2, ClassCDCData, 0, 0), ep(0x82, 2, 64, 0), ep(0x01, 2, 64, 0)))
	lc := []byte{0x00, 0xc2, 0x01, 0x00, 0, 0, 8} // 115200 baud, 1 stop, no parity, 8 bits
	b.ctrl(dev, setup(0x21, 0x20, 0, 0, 7), lc, 0, 320)
	b.ctrl(dev, setup(0x21, 0x22, 3, 0, 0), nil, 0, 280) // DTR + RTS
	b.advance(4000)
	b.data(dev, Bulk, 1, false, []byte("AT\r\n"), 4, 0, 190)
	b.data(dev, Bulk, 2, true, []byte("OK\r\n"), 64, 0, 2100)
}
