package usb

import (
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeFT2232 lays out a sysfs tree shaped like the attached Sipeed adapter
// (two vendor-class interfaces, each bound to ftdi_sio, one tty each).
func fakeFT2232(t *testing.T, root string) {
	dir := filepath.Join(root, "bus", "usb", "devices", "2-2")
	for k, v := range map[string]string{
		"idVendor": "0403", "idProduct": "6010", "bcdDevice": "0500", "busnum": "2", "devnum": "34",
		"manufacturer": "SIPEED", "product": "JTAG Debugger", "serial": "FactoryAIOT Pro",
		"bConfigurationValue": "1", "speed": "12",
	} {
		put(t, filepath.Join(dir, k), v)
	}
	for n, spec := range []struct{ in, out, tty, name string }{{"81", "02", "ttyUSB1", "JTAG Debugger"}, {"83", "04", "ttyUSB2", ""}} {
		idir := filepath.Join(dir, "2-2:1."+string(rune('0'+n)))
		put(t, filepath.Join(idir, "bInterfaceNumber"), "0"+string(rune('0'+n)))
		put(t, filepath.Join(idir, "bInterfaceClass"), "ff")
		if spec.name != "" {
			put(t, filepath.Join(idir, "interface"), spec.name)
		}
		put(t, filepath.Join(idir, "ep_"+spec.in, "bEndpointAddress"), spec.in)
		put(t, filepath.Join(idir, "ep_"+spec.in, "type"), "Bulk")
		put(t, filepath.Join(idir, "ep_"+spec.in, "wMaxPacketSize"), "0040")
		put(t, filepath.Join(idir, "ep_"+spec.out, "bEndpointAddress"), spec.out)
		put(t, filepath.Join(idir, "ep_"+spec.out, "type"), "Bulk")
		put(t, filepath.Join(idir, "ep_"+spec.out, "wMaxPacketSize"), "0040")
		put(t, filepath.Join(idir, spec.tty, "dev"), "188:1")
		drv := filepath.Join(root, "bus", "usb", "drivers", "ftdi_sio")
		_ = os.MkdirAll(drv, 0o755)
		if err := os.Symlink(drv, filepath.Join(idir, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	// an unrelated device and a root hub that must be ignored
	other := filepath.Join(root, "bus", "usb", "devices", "1-1")
	put(t, filepath.Join(other, "idVendor"), "1a86")
	put(t, filepath.Join(other, "idProduct"), "7523")
	put(t, filepath.Join(other, "busnum"), "1")
	put(t, filepath.Join(other, "devnum"), "5")
	hub := filepath.Join(root, "bus", "usb", "devices", "usb2")
	put(t, filepath.Join(hub, "idVendor"), "1d6b")
	put(t, filepath.Join(hub, "idProduct"), "0002")
	put(t, filepath.Join(hub, "busnum"), "2")
	put(t, filepath.Join(hub, "devnum"), "1")
}

func TestScanFakeSysfs(t *testing.T) {
	root := t.TempDir()
	fakeFT2232(t, root)
	devs := Scan(root, func(v, p uint16) bool { return v == 0x0403 && p == 0x6010 })
	if len(devs) != 1 {
		t.Fatalf("%d devices", len(devs))
	}
	d := devs[0]
	if d.Manufacturer != "SIPEED" || d.Product != "JTAG Debugger" || d.Serial != "FactoryAIOT Pro" || d.BCD != 0x0500 || d.Configuration != 1 || d.SpeedMbps != 12 {
		t.Fatalf("%+v", d)
	}
	if d.Path() != "/dev/bus/usb/002/034" {
		t.Fatal(d.Path())
	}
	if len(d.Interfaces) != 2 {
		t.Fatalf("%d interfaces", len(d.Interfaces))
	}
	a, _ := d.Iface(0)
	b, _ := d.Iface(1)
	if a.Driver != "ftdi_sio" || a.TTY != "ttyUSB1" || a.Name != "JTAG Debugger" || b.TTY != "ttyUSB2" || b.Name != "" {
		t.Fatalf("%+v %+v", a, b)
	}
	in, out, mp := a.BulkEndpoints()
	if in != 0x81 || out != 0x02 || mp != 64 {
		t.Fatalf("channel A endpoints %#x %#x %d", in, out, mp)
	}
	in, out, _ = b.BulkEndpoints()
	if in != 0x83 || out != 0x04 {
		t.Fatalf("channel B endpoints %#x %#x", in, out)
	}
	if _, ok := d.Iface(2); ok {
		t.Fatal("there is no interface 2")
	}
	// nil matcher returns every real device, still skipping hubs
	if all := Scan(root, nil); len(all) != 2 {
		t.Fatalf("%d devices without a matcher", len(all))
	}
	if devs := Scan(filepath.Join(root, "missing"), nil); devs != nil {
		t.Fatal("a missing sysfs is an empty scan")
	}
}

// A CDC-ACM interface keeps its tty node one level down (tty/ttyACM1).
func TestScanCDCACMTTY(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bus", "usb", "devices", "2-2")
	for k, v := range map[string]string{"idVendor": "303a", "idProduct": "1001", "busnum": "2", "devnum": "44"} {
		put(t, filepath.Join(dir, k), v)
	}
	idir := filepath.Join(dir, "2-2:1.0")
	put(t, filepath.Join(idir, "bInterfaceNumber"), "00")
	put(t, filepath.Join(idir, "bInterfaceClass"), "02")
	put(t, filepath.Join(idir, "tty", "ttyACM1", "dev"), "166:1")
	d := Scan(root, nil)
	if len(d) != 1 || len(d[0].Interfaces) != 1 || d[0].Interfaces[0].TTY != "ttyACM1" {
		t.Fatalf("%+v", d)
	}
}
