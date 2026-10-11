package dap

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

func fakeDev(t *testing.T, root, name, vid, pid, serial, busnum, devnum string) string {
	dir := filepath.Join(root, "bus", "usb", "devices", name)
	put(t, filepath.Join(dir, "idVendor"), vid)
	put(t, filepath.Join(dir, "idProduct"), pid)
	put(t, filepath.Join(dir, "busnum"), busnum)
	put(t, filepath.Join(dir, "devnum"), devnum)
	put(t, filepath.Join(dir, "product"), "Fake probe")
	if serial != "" {
		put(t, filepath.Join(dir, "serial"), serial)
	}
	return dir
}

func fakeIface(t *testing.T, dir, name, num, class, iface string) string {
	idir := filepath.Join(dir, name)
	put(t, filepath.Join(idir, "bInterfaceNumber"), num)
	put(t, filepath.Join(idir, "bInterfaceClass"), class)
	put(t, filepath.Join(idir, "interface"), iface)
	return idir
}

func fakeEP(t *testing.T, idir, addr, typ, mps string) {
	put(t, filepath.Join(idir, "ep_"+addr, "bEndpointAddress"), addr)
	put(t, filepath.Join(idir, "ep_"+addr, "type"), typ)
	put(t, filepath.Join(idir, "ep_"+addr, "wMaxPacketSize"), mps)
}

func TestScanFakeSysfs(t *testing.T) {
	root := t.TempDir()

	// v2 bulk probe with a serial
	d1 := fakeDev(t, root, "1-2", "0D28", "0204", "ABC123", "1", "7")
	i1 := fakeIface(t, d1, "1-2:1.0", "00", "ff", "CMSIS-DAP v2")
	fakeEP(t, i1, "81", "Bulk", "0040")
	fakeEP(t, i1, "02", "Bulk", "0040")
	fakeIface(t, d1, "1-2:1.1", "01", "02", "CDC Control") // not a probe interface

	// HID-only v1 probe without a serial
	d2 := fakeDev(t, root, "1-3.4", "c251", "f002", "", "1", "9")
	i2 := fakeIface(t, d2, "1-3.4:1.3", "03", "03", "CMSIS-DAP")
	fakeEP(t, i2, "84", "Interrupt", "0040")
	put(t, filepath.Join(i2, "0003:C251:F002.0005", "hidraw", "hidraw4", "dev"), "244:4")

	// probe offering both: the v2 interface wins
	d3 := fakeDev(t, root, "2-1", "2e8a", "000c", "E66", "2", "3")
	i3a := fakeIface(t, d3, "2-1:1.0", "00", "03", "CMSIS-DAP HID")
	fakeEP(t, i3a, "81", "Interrupt", "0040")
	i3b := fakeIface(t, d3, "2-1:1.1", "01", "ff", "CMSIS-DAP v2 interface")
	fakeEP(t, i3b, "83", "Bulk", "0200")
	fakeEP(t, i3b, "04", "Bulk", "0200")

	// unrelated device and a v2 interface without bulk endpoints
	fakeDev(t, root, "1-1", "1d6b", "0002", "", "1", "1")
	d5 := fakeDev(t, root, "3-1", "1234", "5678", "X", "3", "2")
	fakeIface(t, d5, "3-1:1.0", "00", "ff", "CMSIS-DAP v2")

	// sysfs root hubs and interface entries at the top level are ignored
	put(t, filepath.Join(root, "bus", "usb", "devices", "usb1", "idVendor"), "1d6b")

	got := Scan(root)
	if len(got) != 3 {
		t.Fatalf("want 3 probes, got %d: %+v", len(got), got)
	}
	by := map[string]UsbDevice{}
	for _, d := range got {
		by[d.VID] = d
	}
	a := by["0d28"]
	if a.Version != V2 || a.EPIn != 0x81 || a.EPOut != 0x02 || a.MaxPacket != 64 || a.Bus != 1 || a.Address != 7 || a.Interface != 0 || a.ID() != "0d28:0204:ABC123" {
		t.Fatalf("v2 probe: %+v", a)
	}
	b := by["c251"]
	if b.Version != V1 || b.HIDRaw != "/dev/hidraw4" || b.MaxPacket != 64 || b.ID() != "c251:f002@1-3.4" {
		t.Fatalf("v1 probe: %+v", b)
	}
	c := by["2e8a"]
	if c.Version != V2 || c.Interface != 1 || c.EPIn != 0x83 || c.EPOut != 0x04 || c.MaxPacket != 0x200 {
		t.Fatalf("dual probe: %+v", c)
	}
}

func TestScanMissingSysfs(t *testing.T) {
	if got := Scan(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// TestScanCompositeClaimsOnlyDAPInterface is the regression for composite
// probes such as the BL702-DAPLink (d6e7:3507): the descriptor has a vendor
// CMSIS-DAP bulk interface and a CDC ACM pair. Scan must select interface 0
// with its own endpoints and never offer the CDC interfaces, so OpenUSB (which
// claims exactly Interface) leaves cdc_acm and /dev/ttyACM* alone.
func TestScanCompositeClaimsOnlyDAPInterface(t *testing.T) {
	root := t.TempDir()
	d := fakeDev(t, root, "2-1", "d6e7", "3507", "012345ABCDEF", "2", "88")
	i0 := fakeIface(t, d, "2-1:1.0", "00", "ff", "RV CMSIS-DAP")
	fakeEP(t, i0, "01", "Bulk", "0040")
	fakeEP(t, i0, "82", "Bulk", "0040")
	i1 := fakeIface(t, d, "2-1:1.1", "01", "02", "")
	fakeEP(t, i1, "83", "Interrupt", "0010")
	i2 := fakeIface(t, d, "2-1:1.2", "02", "0a", "")
	fakeEP(t, i2, "04", "Bulk", "0040")
	fakeEP(t, i2, "85", "Bulk", "0040")

	got := Scan(root)
	if len(got) != 1 {
		t.Fatalf("want one probe, got %+v", got)
	}
	p := got[0]
	if p.Version != V2 || p.Interface != 0 || p.EPOut != 0x01 || p.EPIn != 0x82 || p.MaxPacket != 64 || p.ID() != "d6e7:3507:012345ABCDEF" {
		t.Fatalf("composite probe: %+v", p)
	}
	if p.HIDRaw != "" {
		t.Fatalf("no HID interface exists: %+v", p)
	}
}
