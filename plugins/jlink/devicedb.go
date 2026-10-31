package jlink

import (
	"context"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SEGGER's DLL opens a modal "Target device settings" dialog when it is given
// a device name it does not know, even with -NoGui 1, and the commander then
// blocks until someone closes it. Device names are therefore resolved against
// the DLL's own device list (ExpDevListXML) before any target session starts;
// an unknown or ambiguous name is refused without launching a session.

// FlashBank is one flash region of a device as SEGGER's database lists it.
type FlashBank struct {
	Base uint64
	Size uint64
}

// DeviceEntry is one device of SEGGER's database.
type DeviceEntry struct {
	Name    string // canonical SEGGER name, e.g. "EFR32MG21AxxxF1024"
	Core    string
	RAMBase uint64
	RAMSize uint64
	Banks   []FlashBank
	Aliases []string
}

// DeviceDB is the parsed device list.
type DeviceDB struct {
	devices []*DeviceEntry
	byName  map[string][]*DeviceEntry // upper-case name or alias
}

type xmlDevList struct {
	Devices []struct {
		Name     string `xml:"Name,attr"`
		Core     string `xml:"Core,attr"`
		RAMStart string `xml:"WorkRAMStartAddr,attr"`
		RAMSize  string `xml:"WorkRAMSize,attr"`
		Banks    []struct {
			Start string `xml:"StartAddr,attr"`
			Size  string `xml:"Size,attr"`
		} `xml:"FlashBankInfo"`
		Aliases []struct {
			Name string `xml:"Name,attr"`
		} `xml:"AliasInfo"`
	} `xml:"VendorInfo>DeviceInfo"`
}

func hexNum(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	return v
}

// ParseDeviceDB parses the XML written by J-Link Commander's ExpDevListXML.
func ParseDeviceDB(data []byte) (*DeviceDB, error) {
	var raw xmlDevList
	if err := xml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse J-Link device list: %w", err)
	}
	db := &DeviceDB{byName: map[string][]*DeviceEntry{}}
	for _, d := range raw.Devices {
		if strings.TrimSpace(d.Name) == "" {
			continue
		}
		e := &DeviceEntry{Name: d.Name, Core: d.Core, RAMBase: hexNum(d.RAMStart), RAMSize: hexNum(d.RAMSize)}
		for _, b := range d.Banks {
			if sz := hexNum(b.Size); sz > 0 {
				e.Banks = append(e.Banks, FlashBank{Base: hexNum(b.Start), Size: sz})
			}
		}
		for _, a := range d.Aliases {
			if a.Name != "" {
				e.Aliases = append(e.Aliases, a.Name)
			}
		}
		db.devices = append(db.devices, e)
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			k := strings.ToUpper(n)
			if !containsEntry(db.byName[k], e) {
				db.byName[k] = append(db.byName[k], e)
			}
		}
	}
	if len(db.devices) == 0 {
		return nil, fmt.Errorf("the J-Link device list has no devices")
	}
	return db, nil
}

func containsEntry(es []*DeviceEntry, e *DeviceEntry) bool {
	for _, x := range es {
		if x == e {
			return true
		}
	}
	return false
}

// wildcardMatch compares name with a database name in which a run of two or
// more lower-case x stands for any characters of the ordering code
// (EFR32MG21AxxxF1024 covers EFR32MG21A020F1024).
func wildcardMatch(dbName, name string) bool {
	if len(dbName) != len(name) || !strings.Contains(dbName, "xx") {
		return false
	}
	for i := 0; i < len(dbName); i++ {
		wild := dbName[i] == 'x' && ((i > 0 && dbName[i-1] == 'x') || (i+1 < len(dbName) && dbName[i+1] == 'x'))
		if wild {
			continue
		}
		if !strings.EqualFold(dbName[i:i+1], name[i:i+1]) {
			return false
		}
	}
	return true
}

// Resolution is the outcome of resolving a user's device name.
type Resolution struct {
	Entry *DeviceEntry
	Input string
	How   string // "exact", "alias", "ordering-code pattern", "unique prefix"
}

// Resolve maps name to exactly one device, or explains why it cannot.
func (db *DeviceDB) Resolve(name string) (Resolution, error) {
	in := strings.TrimSpace(name)
	key := strings.ToUpper(in)
	if es := db.byName[key]; len(es) == 1 {
		how := "alias"
		if strings.EqualFold(es[0].Name, in) {
			how = "exact"
		}
		return Resolution{Entry: es[0], Input: in, How: how}, nil
	} else if len(es) > 1 {
		return Resolution{}, ambiguous(in, es)
	}
	var cands []*DeviceEntry
	how := "ordering-code pattern"
	for _, e := range db.devices {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if wildcardMatch(n, in) && !containsEntry(cands, e) {
				cands = append(cands, e)
			}
		}
	}
	if len(cands) == 0 && len(key) >= 6 {
		how = "unique prefix"
		for _, e := range db.devices {
			for _, n := range append([]string{e.Name}, e.Aliases...) {
				if strings.HasPrefix(strings.ToUpper(n), key) && !containsEntry(cands, e) {
					cands = append(cands, e)
				}
			}
		}
	}
	switch len(cands) {
	case 1:
		return Resolution{Entry: cands[0], Input: in, How: how}, nil
	case 0:
		return Resolution{}, status.Errorf(codes.InvalidArgument,
			"J-Link device %q is not in SEGGER's device list (J-Link would open a device selection dialog for it, so no session was started): "+
				"use a SEGGER device name such as EFR32MG21AXXXF1024 or NRF52840_XXAA (see JLinkExe's ExpDevList), or a generic core such as Cortex-M33", in)
	}
	return Resolution{}, ambiguous(in, cands)
}

func ambiguous(in string, es []*DeviceEntry) error {
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	more := ""
	if len(names) > 12 {
		more = fmt.Sprintf(" and %d more", len(names)-12)
		names = names[:12]
	}
	return status.Errorf(codes.InvalidArgument,
		"J-Link device %q matches several SEGGER devices (%s%s): no session was started; pass one of them as target", in, strings.Join(names, ", "), more)
}

// deviceDB loads SEGGER's device list once per process; a failed load is
// retried by the next call.
func (p *Plugin) deviceDB(ctx context.Context) (*DeviceDB, error) {
	p.dbMu.Lock()
	defer p.dbMu.Unlock()
	if p.db != nil {
		return p.db, nil
	}
	if _, err := p.cmd.Available(); err != nil {
		return nil, unavailable(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := p.cmd.DeviceDatabase(ctx)
	if err == nil {
		p.db, err = ParseDeviceDB(data)
	}
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot read SEGGER's device list to validate the target name (%v); refusing to start a session that could open J-Link's device selection dialog", err)
	}
	return p.db, nil
}

// resolveTarget replaces t.Device with SEGGER's canonical name.
func (p *Plugin) resolveTarget(ctx context.Context, t Target) (Target, Resolution, error) {
	db, err := p.deviceDB(ctx)
	if err != nil {
		return t, Resolution{}, err
	}
	r, err := db.Resolve(t.Device)
	if err != nil {
		return t, r, err
	}
	t.Device = r.Entry.Name
	return t, r, nil
}
