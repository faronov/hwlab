// Package device defines the vendor-neutral device model aggregated from all
// plugins.
package device

import (
	"sort"
	"strings"

	"github.com/faronov/hwlab/internal/apperr"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Device is a physical or virtual instrument reported by a plugin.
type Device struct {
	// ID is globally unique: "<plugin>/<local id>".
	ID           string            `json:"id"`
	PluginID     string            `json:"plugin"`
	LocalID      string            `json:"local_id"`
	Name         string            `json:"name"`
	Serial       string            `json:"serial,omitempty"`
	Transport    string            `json:"transport,omitempty"`
	Capabilities []string          `json:"capabilities"`
	Properties   map[string]string `json:"properties,omitempty"`
}

// FromProto converts a plugin device.
func FromProto(pluginID string, d *pluginv1.Device) Device {
	caps := make([]string, 0, len(d.GetCapabilities()))
	for _, c := range d.GetCapabilities() {
		caps = append(caps, c.GetId())
	}
	sort.Strings(caps)
	return Device{
		ID: pluginID + "/" + d.GetId(), PluginID: pluginID, LocalID: d.GetId(),
		Name: d.GetName(), Serial: d.GetSerial(), Transport: d.GetTransport(),
		Capabilities: caps, Properties: d.GetProperties(),
	}
}

// Has reports whether the device has capability id.
func (d Device) Has(id string) bool {
	for _, c := range d.Capabilities {
		if c == id {
			return true
		}
	}
	return false
}

// Select picks a device matching selector from a plugin's devices.
// An empty selector matches only when exactly one device is present.
// A selector matches the full id, local id, serial or, when unique, name.
func Select(devs []Device, pluginID, selector string) (Device, error) {
	var cands []Device
	for _, d := range devs {
		if d.PluginID == pluginID {
			cands = append(cands, d)
		}
	}
	if selector == "" {
		switch len(cands) {
		case 1:
			return cands[0], nil
		case 0:
			return Device{}, apperr.Newf(apperr.NotFound, "plugin %q reports no devices", pluginID)
		}
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.LocalID
		}
		return Device{}, apperr.Newf(apperr.Ambiguous, "plugin %q has %d devices (%s): specify one with `device:`", pluginID, len(cands), strings.Join(ids, ", "))
	}
	for _, d := range cands {
		if d.ID == selector || d.LocalID == selector || (d.Serial != "" && d.Serial == selector) {
			return d, nil
		}
	}
	// A name is a convenience and several probes of one model share it, so it
	// must never silently pick the first.
	var named []Device
	for _, d := range cands {
		if d.Name == selector {
			named = append(named, d)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
		return Device{}, apperr.Newf(apperr.NotFound, "device %q not found in plugin %q", selector, pluginID)
	}
	ids := make([]string, len(named))
	for i, c := range named {
		ids[i] = c.LocalID
	}
	return Device{}, apperr.Newf(apperr.Ambiguous, "device name %q is ambiguous in plugin %q (%s): select by serial/id", selector, pluginID, strings.Join(ids, ", "))
}
