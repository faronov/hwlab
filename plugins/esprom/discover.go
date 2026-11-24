package espromplugin

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
	"github.com/faronov/hwlab/plugins/esprom/rom"
)

// USB-UART bridge families wired to ESP chips with the classic circuit.
// The family names and ids match tibsl's, so both plugins name a port alike.
var bridges = []struct {
	vid, pid uint16
	family   string
}{
	{0x10c4, 0xea60, "cp210x"}, {0x10c4, 0xea70, "cp210x"},
	{0x1a86, 0x7523, "ch34x"}, {0x1a86, 0x55d4, "ch34x"}, {0x1a86, 0x55d3, "ch34x"},
	{0x0403, 0x6001, "ftdi"}, {0x0403, 0x6010, "ftdi"}, {0x0403, 0x6015, "ftdi"},
}

const (
	espressifVID = 0x303a
	usjPID       = 0x1001
)

// classify says how a USB serial port can reach an ESP ROM.
func classify(p usb.SerialPort) (Kind, string, bool) {
	for _, b := range bridges {
		if b.vid == p.VID && b.pid == p.PID {
			return KindUART, b.family, true
		}
	}
	if p.VID == espressifVID {
		if p.PID == usjPID {
			return KindUSJ, "usj", true
		}
		// The zigbee-rs-hub S3 firmware in bridge mode ("Thread BR H2 bridge").
		if strings.Contains(strings.ToLower(p.Product), "bridge") {
			return KindBridge, "bridge", true
		}
	}
	return "", "", false
}

// genericSerial reports serial numbers that do not identify a unit.
func genericSerial(s string) bool {
	if len(s) < 6 {
		return true
	}
	return strings.Count(s, s[:1]) == len(s)
}

// ttyRank prefers the vendor-neutral node when a driver publishes two
// (macOS: cu.usbserial-14730 and the SiLabs VCP's cu.SLAB_USBtoUART17).
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

// discover turns USB serial ports into candidates with stable ids. Ports of
// the same USB interface are aliases of one device (the first by ttyRank is
// opened, the others are listed as alt_ports). Only the IORegistry or sysfs
// is read: no port is opened, no line is touched.
func discover(ports []usb.SerialPort) []Entry {
	type key struct {
		loc   string
		iface int
	}
	groups := map[key][]usb.SerialPort{}
	var order []key
	for _, p := range ports {
		if _, _, ok := classify(p); !ok {
			continue
		}
		k := key{p.Location, p.Interface}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool {
			if ttyRank(g[i].TTY) != ttyRank(g[j].TTY) {
				return ttyRank(g[i].TTY) < ttyRank(g[j].TTY)
			}
			return g[i].TTY < g[j].TTY
		})
	}
	serials := map[string]int{}
	ifaces := map[string]int{}
	bridgeIf := map[string]int{} // lowest CDC interface of each bridge device
	for _, k := range order {
		p := groups[k][0]
		serials[fmt.Sprintf("%04x:%04x:%s", p.VID, p.PID, p.Serial)]++
		ifaces[k.loc]++
		if kind, _, _ := classify(p); kind == KindBridge {
			if cur, ok := bridgeIf[k.loc]; !ok || k.iface < cur {
				bridgeIf[k.loc] = k.iface
			}
		}
	}
	var out []Entry
	for _, k := range order {
		g := groups[k]
		p := g[0]
		kind, fam, _ := classify(p)
		e := Entry{Port: p.TTY, Kind: kind, Entry: kind.defaultEntry(), Baud: kind.defaultBaud(), Candidate: true,
			USB: fmt.Sprintf("%04x:%04x", p.VID, p.PID), Serial: p.Serial, Product: p.Product, Location: p.Location}
		for _, a := range g[1:] {
			e.AltPorts = append(e.AltPorts, a.TTY)
		}
		switch kind {
		case KindBridge:
			if k.iface != bridgeIf[k.loc] {
				continue // the console (CDC 1)
			}
			for _, k2 := range order {
				if k2.loc == k.loc && k2 != k {
					e.Console = groups[k2][0].TTY
				}
			}
			e.ID = "bridge-" + sanitize(p.Serial)
			if genericSerial(p.Serial) {
				e.ID = "bridge-loc" + p.Location
			}
		case KindUSJ:
			e.ID = "usj-" + strings.ToLower(strings.ReplaceAll(sanitize(p.Serial), "_", ""))
			if genericSerial(p.Serial) {
				e.ID = "usj-loc" + p.Location
			}
		default:
			e.ID = fam + "-loc" + p.Location
			if !genericSerial(p.Serial) && serials[fmt.Sprintf("%04x:%04x:%s", p.VID, p.PID, p.Serial)] == ifaces[k.loc] {
				e.ID = fam + "-" + sanitize(p.Serial)
			}
			if ifaces[k.loc] > 1 && p.Interface >= 0 {
				e.ID += fmt.Sprintf("-if%d", p.Interface)
			}
		}
		out = append(out, e)
	}
	return out
}

// fileConfig is the optional $HWLAB_HOME/esprom.yaml:
//
//	devices:
//	  cp210x-ba653de3e67bf11194bff77a9507fdd7: {baud: 921600, entry: classic}
//	ignore: [cp210x-loc20-7.4.1]
type fileConfig struct {
	Devices map[string]struct {
		Entry string `yaml:"entry"`
		Baud  *int   `yaml:"baud"`
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
func ConfigFile() string { return filepath.Join(hwlabHome(), "esprom.yaml") }

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
			if en, err := rom.ParseEntry(d.Entry); err == nil {
				e.Entry = en
			}
			if d.Baud != nil && validBaud(*d.Baud) {
				e.Baud = *d.Baud
			}
			e.Configured = true
		}
		out = append(out, e)
	}
	return out
}

// identity is what the last connection learned about the chip behind an id.
type identity struct {
	Chip       string `json:"chip"`
	Revision   string `json:"revision"`
	MAC        string `json:"mac"`
	JEDEC      string `json:"flash_jedec"`
	FlashBytes int    `json:"flash_bytes"`
	Port       string `json:"port"`
	SeenUTC    string `json:"identified_utc"`
}

type identityCache struct{ mu sync.Mutex }

func cacheFile() string { return filepath.Join(hwlabHome(), "cache", "esprom-identity.json") }

func (c *identityCache) load() map[string]identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]identity{}
	if b, err := os.ReadFile(cacheFile()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *identityCache) store(id string, v identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]identity{}
	if b, err := os.ReadFile(cacheFile()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	v.SeenUTC = time.Now().UTC().Format(time.RFC3339)
	m[id] = v
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

// tibslIdentities reads the tibsl plugin's identity cache: a port where the
// TI bootloader answered carries a TI chip, and the ESP entry sequence would
// only reset it. Ids are built the same way in both plugins.
func tibslIdentities() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(hwlabHome(), "cache", "tibsl-identity.json"))
	if err != nil {
		return out
	}
	var m map[string]struct {
		Chip string `json:"chip"`
		Seen string `json:"identified_utc"`
	}
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for id, v := range m {
		if v.Chip != "" {
			out[id] = fmt.Sprintf("%s (tibsl, %s)", v.Chip, v.Seen)
		}
	}
	return out
}
