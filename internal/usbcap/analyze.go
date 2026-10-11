package usbcap

import (
	"fmt"
	"sort"
	"strings"
)

// Transaction is one URB: a submission paired with its completion.
type Transaction struct {
	StartNs   int64  `json:"start_ns"`
	LatencyNs int64  `json:"latency_ns"` // completion time - submission time, host kernel clock
	Device    string `json:"device"`     // bus:device
	Endpoint  string `json:"endpoint"`   // "ep1 IN"
	Transfer  string `json:"transfer"`
	Class     string `json:"class,omitempty"`
	Name      string `json:"name"`
	Detail    string `json:"detail,omitempty"`
	Status    int32  `json:"status"`
	StatusMsg string `json:"status_name"`
	Bytes     uint32 `json:"bytes"`
	Hex       string `json:"hex,omitempty"` // first 16 captured bytes of the data stage
	URB       string `json:"urb"`
}

// EndpointStats aggregates the URBs of one endpoint.
type EndpointStats struct {
	Device    string  `json:"device"`
	Endpoint  string  `json:"endpoint"`
	Transfer  string  `json:"transfer"`
	Class     string  `json:"class,omitempty"`
	URBs      int     `json:"urbs"`
	Bytes     uint64  `json:"bytes"`
	Errors    int     `json:"errors"`
	MeanLatUs float64 `json:"mean_latency_us"`
	MaxLatUs  float64 `json:"max_latency_us"`
	sumLat    int64
	maxLat    int64
}

// DeviceInfo is what the capture revealed about one device.
type DeviceInfo struct {
	Device     string   `json:"device"`
	VID        string   `json:"vid,omitempty"`
	PID        string   `json:"pid,omitempty"`
	USB        string   `json:"usb_version,omitempty"`
	Class      string   `json:"class,omitempty"`
	Interfaces []string `json:"interfaces,omitempty"`
	Notes      []string `json:"notes,omitempty"`
	URBs       int      `json:"urbs"`
	Errors     int      `json:"errors"`

	desc  *DeviceDescriptor
	ifc   map[byte]*Interface // interface number -> first alternate setting seen
	epIfc map[int]*Interface  // endpoint address -> interface
}

// ErrorCount is a status code and how often it occurred.
type ErrorCount struct {
	Status int32  `json:"status"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
}

// Stats are whole-capture counters.
type Stats struct {
	Packets          int    `json:"packets"`
	Transactions     int    `json:"transactions"`
	Errors           int    `json:"errors"`
	UnpairedComplete int    `json:"unpaired_completions"` // capture began mid-URB
	StillPending     int    `json:"still_pending"`        // submitted, never completed
	DurationNs       int64  `json:"duration_ns"`
	Bytes            uint64 `json:"bytes"`
}

// Analysis is the result of Analyze.
type Analysis struct {
	Stats        Stats           `json:"stats"`
	Devices      []DeviceInfo    `json:"devices"`
	Endpoints    []EndpointStats `json:"endpoints"`
	ErrorCounts  []ErrorCount    `json:"error_counts,omitempty"`
	Transactions []Transaction   `json:"transactions"`
}

type pending struct {
	p Packet
}

// Analyze pairs URB submissions with completions, learns devices from the
// enumeration traffic it contains (device and configuration descriptors,
// SET_ADDRESS) and labels each transaction with its standard, class or vendor
// meaning. Packets must be in capture order. Anything the capture does not
// contain (a device enumerated before it began) stays unlabelled; nothing is
// guessed.
func Analyze(pkts []Packet) *Analysis {
	a := &Analysis{}
	devs := map[string]*DeviceInfo{}
	dev := func(key string) *DeviceInfo {
		d := devs[key]
		if d == nil {
			d = &DeviceInfo{Device: key, ifc: map[byte]*Interface{}, epIfc: map[int]*Interface{}}
			devs[key] = d
		}
		return d
	}
	eps := map[string]*EndpointStats{}
	errs := map[int32]int{}
	open := map[uint64]pending{}
	var first, last int64
	for i, p := range pkts {
		if i == 0 || p.TimestampNs < first {
			first = p.TimestampNs
		}
		if p.TimestampNs > last {
			last = p.TimestampNs
		}
		a.Stats.Packets++
		if p.Event == Submit {
			open[p.URBID] = pending{p}
			continue
		}
		sub, ok := open[p.URBID]
		if !ok {
			a.Stats.UnpairedComplete++
			continue
		}
		delete(open, p.URBID)
		t := buildTransaction(sub.p, p, dev(p.DeviceKey()), dev, devs)
		a.Transactions = append(a.Transactions, t)
		a.Stats.Bytes += uint64(t.Bytes)

		d := dev(p.DeviceKey())
		d.URBs++
		key := fmt.Sprintf("%s/%s/%s", t.Device, t.Endpoint, t.Transfer)
		es := eps[key]
		if es == nil {
			es = &EndpointStats{Device: t.Device, Endpoint: t.Endpoint, Transfer: t.Transfer, Class: t.Class}
			eps[key] = es
		}
		es.URBs++
		es.Bytes += uint64(t.Bytes)
		es.sumLat += t.LatencyNs
		if t.LatencyNs > es.maxLat {
			es.maxLat = t.LatencyNs
		}
		if es.Class == "" {
			es.Class = t.Class
		}
		if t.Status != 0 {
			a.Stats.Errors++
			d.Errors++
			es.Errors++
			errs[t.Status]++
		}
	}
	a.Stats.StillPending = len(open)
	a.Stats.Transactions = len(a.Transactions)
	a.Stats.DurationNs = last - first

	keys := make([]string, 0, len(devs))
	for k := range devs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := devs[k]
		if d.desc != nil && d.desc.Full {
			d.VID, d.PID = fmt.Sprintf("%04x", d.desc.VID), fmt.Sprintf("%04x", d.desc.PID)
		}
		if d.desc != nil {
			d.USB = fmt.Sprintf("%x.%02x", d.desc.USB>>8, d.desc.USB&0xff)
			d.Class = ClassName(d.desc.Class)
		}
		nums := make([]int, 0, len(d.ifc))
		for n := range d.ifc {
			nums = append(nums, int(n))
		}
		sort.Ints(nums)
		for _, n := range nums {
			d.Interfaces = append(d.Interfaces, fmt.Sprintf("if%d %s", n, ifaceClass(d.ifc[byte(n)])))
		}
		if d.URBs > 0 {
			a.Devices = append(a.Devices, *d)
		}
	}
	ekeys := make([]string, 0, len(eps))
	for k := range eps {
		ekeys = append(ekeys, k)
	}
	sort.Strings(ekeys)
	for _, k := range ekeys {
		es := eps[k]
		es.MeanLatUs = float64(es.sumLat) / float64(es.URBs) / 1000
		es.MaxLatUs = float64(es.maxLat) / 1000
		a.Endpoints = append(a.Endpoints, *es)
	}
	for s, n := range errs {
		a.ErrorCounts = append(a.ErrorCounts, ErrorCount{Status: s, Name: StatusName(s), Count: n})
	}
	sort.Slice(a.ErrorCounts, func(i, j int) bool { return a.ErrorCounts[i].Count > a.ErrorCounts[j].Count })
	return a
}

func buildTransaction(sub, done Packet, d *DeviceInfo, dev func(string) *DeviceInfo, all map[string]*DeviceInfo) Transaction {
	t := Transaction{
		StartNs: sub.TimestampNs, LatencyNs: done.TimestampNs - sub.TimestampNs, Device: done.DeviceKey(),
		Endpoint: fmt.Sprintf("ep%d %s", done.Endpoint, strings.ToUpper(done.Dir())), Transfer: done.Transfer.String(),
		Status: done.Status, StatusMsg: StatusName(done.Status), URB: fmt.Sprintf("%x", done.URBID),
	}
	if t.LatencyNs < 0 {
		t.LatencyNs = 0
	}
	// The data stage is in the completion for an IN transfer, in the submission for OUT.
	data := done.Data
	t.Bytes = done.Length
	if !done.In {
		data = sub.Data
		t.Bytes = sub.Length
		if done.Status != 0 {
			t.Bytes = 0
		}
	}
	if len(data) > 0 {
		t.Hex = previewHex(data, 16)
	}
	if ifc := d.epIfc[done.EndpointAddr()]; ifc != nil && sub.Transfer != Control {
		t.Class = ifaceClass(ifc)
	}

	switch sub.Transfer {
	case Control:
		describeControl(&t, sub, done, data, d, dev, all)
	case Bulk, Interrupt:
		describeData(&t, done, data)
	default:
		t.Name = "isochronous " + strings.ToLower(done.Dir())
	}
	if done.Status != 0 {
		t.Detail = strings.TrimSpace(t.Detail + " [" + StatusName(done.Status) + "]")
	}
	return t
}

func describeControl(t *Transaction, sub, done Packet, data []byte, d *DeviceInfo, dev func(string) *DeviceInfo, all map[string]*DeviceInfo) {
	if !sub.HasSetup {
		t.Name = "control (setup packet not captured)"
		return
	}
	s := ParseSetup(sub.Setup)
	var ifc *Interface
	switch s.Recipient() {
	case "interface":
		ifc = d.ifc[byte(s.Index)]
	case "endpoint":
		ifc = d.epIfc[int(s.Index&0xff)]
	}
	t.Name = DescribeSetup(s, ifc)
	if s.Kind() == "class" && ifc != nil {
		t.Class = ifaceClass(ifc)
	}
	ok := done.Status == 0
	switch {
	case s.Kind() == "standard" && s.Request == ReqGetDescriptor && ok:
		switch byte(s.Value >> 8) {
		case 1:
			if dd, good := ParseDevice(data); good {
				d.desc = &dd
				if dd.Full {
					t.Detail = fmt.Sprintf("%04x:%04x, USB %x.%02x, class %s", dd.VID, dd.PID, dd.USB>>8, dd.USB&0xff, ClassName(dd.Class))
				} else {
					t.Detail = fmt.Sprintf("first %d bytes: USB %x.%02x, ep0 max packet %d", len(data), dd.USB>>8, dd.USB&0xff, dd.MaxPacket0)
				}
			}
		case 2:
			learnConfig(t, d, data)
		}
	case s.Kind() == "standard" && s.Request == ReqSetAddress && ok:
		if addr := int(s.Value); addr != done.Device {
			key := fmt.Sprintf("%d:%d", done.Bus, addr)
			nd := dev(key)
			nd.desc, nd.ifc, nd.epIfc = d.desc, d.ifc, d.epIfc
			t.Detail = fmt.Sprintf("device now at %s", key)
		}
	case s.Kind() == "standard" && s.Request == ReqSetConfiguration:
		t.Detail = fmt.Sprintf("configuration %d", s.Value&0xff)
	case s.Kind() == "standard" && s.Request == ReqSetInterface:
		t.Detail = fmt.Sprintf("interface %d alternate %d", s.Index, s.Value)
	case s.Kind() == "class" && ifc != nil:
		describeClassRequest(t, s, ifc, data, ok)
	case s.Kind() == "class" && ifc == nil && s.Request == 0xfe:
		t.Detail = "interface class unknown (enumeration not in the capture)"
	}
	if s.Kind() == "vendor" {
		t.Class = "vendor"
	}
}

func learnConfig(t *Transaction, d *DeviceInfo, data []byte) {
	cfg, ok := ParseConfig(data)
	if !ok {
		return
	}
	if total := int(data[2]) | int(data[3])<<8; len(data) == 9 && total > 9 {
		// The usual first probe: the host reads the 9-byte header to learn the total length.
		t.Detail = fmt.Sprintf("configuration %d header: %d bytes in total", cfg.Value, total)
		return
	}
	for i := range cfg.Interfaces {
		ifc := &cfg.Interfaces[i]
		if _, seen := d.ifc[ifc.Number]; !seen || ifc.Alt == 0 {
			d.ifc[ifc.Number] = ifc
		}
		for _, e := range ifc.Endpoints {
			d.epIfc[int(e.Addr)] = ifc
		}
	}
	if len(cfg.Interfaces) > 0 {
		t.Detail = fmt.Sprintf("configuration %d: %d interface(s): %s", cfg.Value, len(cfg.Interfaces), strings.Join(cfg.Classes(), ", "))
	} else {
		t.Detail = fmt.Sprintf("configuration %d header only", cfg.Value)
	}
	if cfg.Truncated {
		t.Detail += " (descriptor truncated in the capture)"
		d.Notes = appendUnique(d.Notes, "configuration descriptor truncated: interface list may be incomplete")
	}
}

func describeClassRequest(t *Transaction, s Setup, ifc *Interface, data []byte, ok bool) {
	switch ifaceClass(ifc) {
	case "CDC":
		switch s.Request {
		case 0x20, 0x21:
			if lc, good := LineCoding(data); good && ok {
				t.Detail = lc
			}
		case 0x22:
			t.Detail = fmt.Sprintf("DTR=%d RTS=%d", s.Value&1, (s.Value>>1)&1)
		}
	case "DFU":
		if s.Request == 3 && ok {
			if st, good := DFUStatus(data); good {
				t.Detail = st
			}
		}
	case "mass-storage":
		if s.Request == 0xfe && ok && len(data) >= 1 {
			t.Detail = fmt.Sprintf("max LUN %d", data[0])
		}
	case "HID":
		switch s.Request {
		case 0x0a:
			t.Detail = fmt.Sprintf("idle %d ms, report %d", int(s.Value>>8)*4, s.Value&0xff)
		case 0x0b:
			t.Detail = map[uint16]string{0: "boot protocol", 1: "report protocol"}[s.Value]
		}
	}
}

func describeData(t *Transaction, done Packet, data []byte) {
	dir := strings.ToLower(done.Dir())
	switch t.Class {
	case "mass-storage":
		if !done.In {
			if d, ok := MSCCommandBlock(data); ok {
				t.Name, t.Detail = "MSC command", d
				return
			}
		} else if d, ok := MSCStatusWrapper(data); ok {
			t.Name, t.Detail = "MSC status", d
			return
		}
		t.Name = "MSC data " + dir
	case "HID":
		t.Name = "HID report " + dir
		if len(data) > 0 {
			t.Detail = previewHex(data, 16)
		}
	case "CDC-data":
		t.Name = "CDC data " + dir
		if s := printable(data, 32); s != "" {
			t.Detail = `"` + s + `"`
		}
	case "CDC":
		t.Name = "CDC notification " + dir
	default:
		t.Name = done.Transfer.String() + " " + dir
	}
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
