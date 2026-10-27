package app

import (
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/usb"
)

// TestIOKitChecks covers the macOS doctor path with devices shaped like the
// IORegistry scan of an OB J-Link: the claimed interface is the last vendor
// bulk interface, the CDC siblings are listed with their drivers, a
// driver-held interface is a warning and a bootloader probe is never "usable".
func TestIOKitChecks(t *testing.T) {
	bulk := []usb.Endpoint{{Address: 0x85, Type: "Bulk", MaxPacket: 64}, {Address: 0x03, Type: "Bulk", MaxPacket: 64}}
	app := usb.Device{VID: 0x1366, PID: 0x1061, Product: "J-Link", Serial: "000000000001", Interfaces: []usb.Interface{
		{Number: 0, Class: 0x02, Driver: "AppleUSBACMControl"},
		{Number: 1, Class: 0x0a, Driver: "AppleUSBACMData", TTY: "cu.usbmodem0000000000011", Endpoints: bulk},
		{Number: 4, Class: 0xff, Name: "BULK interface", Endpoints: bulk},
	}}
	held := app
	held.Interfaces = append([]usb.Interface(nil), app.Interfaces...)
	held.Interfaces[2].Driver = "pid 42, JLinkExe"
	btl := usb.Device{VID: 0x1366, PID: 0x0101, Serial: "000000000001"}
	other := usb.Device{VID: 0x1a86, PID: 0x7523}

	find := func(cs []DoctorCheck, name string) DoctorCheck {
		t.Helper()
		for _, c := range cs {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no %q in %+v", name, cs)
		return DoctorCheck{}
	}

	cs := iokitChecks("J-Link: native backend", iokitJLink, []usb.Device{app, other})
	if c := find(cs, "J-Link: detected"); c.Level != CheckOK || !strings.Contains(c.Detail, "jlink-native:1") {
		t.Errorf("detected: %+v", c)
	}
	if c := find(cs, "J-Link: access"); c.Level != CheckOK || !strings.Contains(c.Detail, "interface 4 ") {
		t.Errorf("access: %+v", c)
	}
	if c := find(cs, "J-Link: other interfaces"); !strings.Contains(c.Detail, "1=AppleUSBACMData, /dev/cu.usbmodem0000000000011") || strings.Contains(c.Detail, "4=") {
		t.Errorf("siblings: %+v", c)
	}

	if c := find(iokitChecks("n", iokitJLink, []usb.Device{held}), "J-Link: access"); c.Level != CheckWarn || !strings.Contains(c.Detail, "JLinkExe") {
		t.Errorf("held: %+v", c)
	}

	cs = iokitChecks("n", iokitJLink, []usb.Device{btl})
	if c := find(cs, "J-Link: detected"); c.Level != CheckWarn || !strings.Contains(c.Detail, "replug") {
		t.Errorf("bootloader: %+v", c)
	}
	for _, c := range cs {
		if c.Name == "J-Link: access" {
			t.Errorf("bootloader probe got an access check: %+v", c)
		}
	}

	if c := find(iokitChecks("n", iokitJLink, []usb.Device{other}), "J-Link: detected"); c.Detail != "no J-Link connected" {
		t.Errorf("none: %+v", c)
	}

	v1 := usb.Device{VID: 0xc251, PID: 0xf001, Interfaces: []usb.Interface{{Number: 0, Class: 0x03, Name: "CMSIS-DAP"}}}
	if c := find(iokitChecks("n", iokitCMSISDAP, []usb.Device{v1}), "CMSIS-DAP: detected"); c.Level != CheckWarn || !strings.Contains(c.Detail, "not implemented on macOS") {
		t.Errorf("cmsis v1: %+v", c)
	}
}
