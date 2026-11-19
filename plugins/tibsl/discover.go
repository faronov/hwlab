package tibslplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/tibsl/bsl"
)

// bridge is a USB-UART bridge family worth offering as a BSL candidate.
type bridge struct {
	vid, pid uint16
	family   string
}

var bridges = []bridge{
	{0x10c4, 0xea60, "cp210x"}, {0x10c4, 0xea70, "cp210x"},
	{0x1a86, 0x7523, "ch34x"}, {0x1a86, 0x55d4, "ch34x"}, {0x1a86, 0x55d3, "ch34x"},
	{0x0403, 0x6001, "ftdi"}, {0x0403, 0x6010, "ftdi"}, {0x0403, 0x6015, "ftdi"},
	{0x0451, 0xbef3, "xds110"}, {0x0451, 0xbef4, "xds110"},
}

// productHints mark boards with a TI radio by their USB product string.
var productHints = []string{"zigbee", "slzb", "xds110", "launchpad", "cc26", "cc13", "cc2538", "zbdongle"}

func bridgeFamily(vid, pid uint16, product string) (string, bool) {
	for _, b := range bridges {
		if b.vid == vid && b.pid == pid {
			return b.family, true
		}
	}
	p := strings.ToLower(product)
	for _, h := range productHints {
		if strings.Contains(p, h) {
			return "usbserial", true
		}
	}
	return "", false
}

// genericSerial reports serial numbers that do not identify a unit: empty,
// short (CP2102 clones ship "0001") or one repeated character.
func genericSerial(s string) bool {
	if len(s) < 6 {
		return true
	}
	return strings.Count(s, s[:1]) == len(s)
}

// ttyRank prefers the vendor-neutral node when a driver publishes two
// (macOS: cu.usbserial-0001 and the SiLabs VCP's cu.SLAB_USBtoUART).
func ttyRank(p string) int {
	b := filepath.Base(p)
	switch {
	case strings.HasPrefix(b, "cu.usbserial"), strings.HasPrefix(b, "cu.usbmodem"), strings.HasPrefix(b, "ttyUSB"), strings.HasPrefix(b, "ttyACM"):
		return 0
	case strings.HasPrefix(b, "cu."):
		return 1
	}
	return 2
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// discover turns the USB serial ports into candidates with stable ids:
// <family>-<serial> when the USB serial number identifies the unit, else
// <family>-loc<port path> (the physical USB port). Only the IORegistry or
// sysfs is read: no port is opened, no line is touched.
func discover(ports []usb.SerialPort) []Entry {
	type key struct {
		loc   string
		iface int
	}
	groups := map[key][]usb.SerialPort{}
	var order []key
	for _, p := range ports {
		if _, ok := bridgeFamily(p.VID, p.PID, p.Product); !ok {
			continue
		}
		k := key{p.Location, p.Interface}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	serials := map[string]int{}
	ifaces := map[string]int{}
	for _, k := range order {
		p := groups[k][0]
		serials[fmt.Sprintf("%04x:%04x:%s", p.VID, p.PID, p.Serial)]++
		ifaces[k.loc]++
	}
	var out []Entry
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g, func(i, j int) bool {
			if ttyRank(g[i].TTY) != ttyRank(g[j].TTY) {
				return ttyRank(g[i].TTY) < ttyRank(g[j].TTY)
			}
			return g[i].TTY < g[j].TTY
		})
		p := g[0]
		fam, _ := bridgeFamily(p.VID, p.PID, p.Product)
		id := fam + "-loc" + p.Location
		if !genericSerial(p.Serial) && serials[fmt.Sprintf("%04x:%04x:%s", p.VID, p.PID, p.Serial)] == ifaces[k.loc] {
			id = fam + "-" + sanitize(p.Serial)
		}
		if ifaces[k.loc] > 1 && p.Interface >= 0 {
			id += fmt.Sprintf("-if%d", p.Interface)
		}
		e := Entry{ID: id, Port: p.TTY, Baud: defaultBaud, Entry: bsl.EntryDTRRTS, Candidate: true,
			USB: fmt.Sprintf("%04x:%04x", p.VID, p.PID), Serial: p.Serial, Product: p.Product, Location: p.Location}
		for _, a := range g[1:] {
			e.AltPorts = append(e.AltPorts, a.TTY)
		}
		out = append(out, e)
	}
	return out
}

// fileConfig is the optional $HWLAB_HOME/tibsl.yaml:
//
//	devices:
//	  cp210x-loc20-7.2: {bsl_entry: dtr-rts, baud: 460800}
//	ignore: [ch34x-loc20-7.4.4]
type fileConfig struct {
	Devices map[string]struct {
		BSLEntry string `yaml:"bsl_entry"`
		Baud     int    `yaml:"baud"`
	} `yaml:"devices"`
	Ignore []string `yaml:"ignore"`
}

func hwlabHome() string {
	if h := os.Getenv("HWLAB_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hwlab")
}

// ConfigFile is the optional per-device configuration below HWLAB_HOME.
func ConfigFile() string { return filepath.Join(hwlabHome(), "tibsl.yaml") }

func loadFileConfig() (fileConfig, error) {
	var c fileConfig
	b, err := os.ReadFile(ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigFile(), err)
	}
	return c, nil
}

// apply sets per-device overrides and drops ignored entries.
func (c fileConfig) apply(es []Entry) []Entry {
	ignore := map[string]bool{}
	for _, id := range c.Ignore {
		ignore[id] = true
	}
	var out []Entry
	for _, e := range es {
		if ignore[e.ID] {
			continue
		}
		if d, ok := c.Devices[e.ID]; ok {
			if en, err := bsl.ParseEntry(d.BSLEntry); err == nil {
				e.Entry = en
			}
			if d.Baud >= 9600 && d.Baud <= 1_500_000 {
				e.Baud = d.Baud
			}
			e.Configured = true
		}
		out = append(out, e)
	}
	return out
}

// identity is what the last successful connection learned about the chip
// behind a stable id, so device_list can show it without touching the port.
type identity struct {
	Chip       string `json:"chip"`
	IEEE       string `json:"ieee"`
	ChipID     string `json:"chip_id"`
	FlashBytes int    `json:"flash_bytes"`
	Port       string `json:"port"`
	Entry      string `json:"bsl_entry"`
	SeenUTC    string `json:"identified_utc"`
}

type identityCache struct {
	mu sync.Mutex
}

func cacheFile() string { return filepath.Join(hwlabHome(), "cache", "tibsl-identity.json") }

func (c *identityCache) load() map[string]identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return readCache()
}

func readCache() map[string]identity {
	m := map[string]identity{}
	if b, err := os.ReadFile(cacheFile()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *identityCache) store(id string, d *device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := readCache()
	m[id] = identity{Chip: d.chip.Name, IEEE: d.chip.IEEEString(), ChipID: hex32(d.chip.ChipID), FlashBytes: d.chip.FlashBytes,
		Port: d.entry.Port, Entry: string(d.o.entry), SeenUTC: time.Now().UTC().Format(time.RFC3339)}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	path := cacheFile()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := path + ".new"
	if os.WriteFile(tmp, append(b, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
