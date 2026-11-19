package usb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSysfsSerialPorts(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "bus", "usb", "devices")
	write := func(p, v string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []struct{ name, vid, pid, serial, tty string }{
		{"1-2", "10c4", "ea60", "0001", "1-2:1.0/ttyUSB0"},
		{"1-3.1", "2fe3", "0100", "", "1-3.1:1.0/tty/ttyACM0"},
	} {
		dir := filepath.Join(dev, d.name)
		write(filepath.Join(dir, "busnum"), "1")
		write(filepath.Join(dir, "devnum"), "5")
		write(filepath.Join(dir, "idVendor"), d.vid)
		write(filepath.Join(dir, "idProduct"), d.pid)
		write(filepath.Join(dir, "serial"), d.serial)
		write(filepath.Join(dir, d.name+":1.0", "bInterfaceNumber"), "00")
		if err := os.MkdirAll(filepath.Join(dir, d.tty), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ps := sysfsSerialPorts(root, nil)
	if len(ps) != 2 || ps[0].TTY != "/dev/ttyUSB0" || ps[0].Location != "1-2" || ps[0].VID != 0x10c4 || ps[0].Serial != "0001" ||
		ps[1].TTY != "/dev/ttyACM0" || ps[1].Location != "1-3.1" || ps[1].Interface != 0 {
		t.Fatalf("%+v", ps)
	}
}
