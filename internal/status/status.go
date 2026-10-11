// Package status is the machine-readable support matrix of hwlab plugins and
// backends. It separates what is implemented from how far it has been
// validated, so "code exists and tests pass" is never reported as "works on
// hardware". The registry is owned by the core, not by plugins: a plugin must
// not grade itself.
package status

import (
	"fmt"
	"sort"
	"strings"
)

// Implementation is how much of a backend's intended scope is written.
type Implementation string

const (
	ImplComplete Implementation = "complete" // every method of the intended scope exists
	ImplPartial  Implementation = "partial"  // some methods or operations are not written
	ImplSkeleton Implementation = "skeleton" // interfaces only, no real transport
	ImplMissing  Implementation = "missing"  // backend not written at all
)

// Validation is the highest level of evidence that the backend works.
type Validation string

const (
	ValUntested        Validation = "untested"
	ValSynthetic       Validation = "synthetic" // only generated/fake inputs
	ValReplay          Validation = "replay"    // recorded real session replays, no live device in this run
	ValHardwarePartial Validation = "real-hardware-partial"
	ValHardware        Validation = "real-hardware"
)

// Dependency is how the backend reaches the device.
type Dependency string

const (
	DepNative           Dependency = "native"                 // pure Go (+ OS drivers)
	DepVendorSDK        Dependency = "vendor-sdk"             // needs a vendor library at run time
	DepExternalTool     Dependency = "external-tool"          // needs an external program
	DepOptionalExternal Dependency = "optional-external-tool" // works natively; a tool is an optional fallback
)

// Fixture says what recorded data backs the replay tests.
type Fixture string

const (
	FixtureNone      Fixture = "none"
	FixtureSynthetic Fixture = "synthetic" // generated, not recorded from a device
	FixtureReal      Fixture = "real"      // recorded from real hardware
)

// CapStatus grades one capability separately, so working neighbours never
// upgrade a capability that was not itself exercised.
type CapStatus string

const (
	CapRealHW         CapStatus = "REAL_HW_VERIFIED"     // ran on a physical device, evidence in docs/notes
	CapUntested       CapStatus = "IMPLEMENTED_UNTESTED" // code exists, never ran on the device or a model of it
	CapSimulatedOnly  CapStatus = "SIMULATED_ONLY"       // exercised only against a software model
	CapNotImplemented CapStatus = "NOT_IMPLEMENTED"
	// CapUnsupportedHW: implemented and simulated, but the attached target cannot
	// exercise it at all (for example STM32 flash with an nRF52840 attached).
	CapUnsupportedHW CapStatus = "UNSUPPORTED_BY_ATTACHED_HW"
	// Reverse-engineered protocols: how much of the wire format is understood,
	// separately from whether a capability ran on hardware.
	CapReplayVerified  CapStatus = "REPLAY_VERIFIED"  // decoded from a real recording, matches an oracle
	CapProtocolPartial CapStatus = "PROTOCOL_PARTIAL" // works for the observed case; parts of the protocol are unknown
	CapHypothesisOnly  CapStatus = "HYPOTHESIS_ONLY"  // inferred, not confirmed by any measurement
)

// Capability is the status of one named capability of a backend.
type Capability struct {
	Name     string    `json:"name"`
	Status   CapStatus `json:"status"`
	Evidence string    `json:"evidence,omitempty"`
}

// Backend is one way of driving a device family.
type Backend struct {
	Name           string         `json:"name"`
	Implementation Implementation `json:"implementation"`
	Validation     Validation     `json:"validation"`
	Fixture        Fixture        `json:"replay_fixture"`
	Dependency     Dependency     `json:"dependency"`
	Dependencies   string         `json:"dependencies,omitempty"` // exact external pieces
	Cgo            string         `json:"cgo"`
	Verified       []string       `json:"verified,omitempty"`
	Unverified     []string       `json:"unverified,omitempty"`
	Gaps           []string       `json:"known_gaps,omitempty"`
	Capabilities   []Capability   `json:"capabilities,omitempty"`
}

// Entry is the support status of one plugin id. The first backend is the
// primary one.
type Entry struct {
	Plugin   string    `json:"plugin"`
	Title    string    `json:"title"`
	Backends []Backend `json:"backends"`
}

// Summary is the one-line status of an entry across its backends:
// the weakest implementation, the highest validation, the strongest runtime
// dependency (optional fallbacks excluded) and the backends not written at all.
type Summary struct {
	Implementation Implementation `json:"implementation"`
	Validation     Validation     `json:"validation"`
	Dependency     Dependency     `json:"dependency"`
	Missing        []string       `json:"missing_backends,omitempty"`
}

var (
	implRank = map[Implementation]int{ImplComplete: 3, ImplPartial: 2, ImplSkeleton: 1, ImplMissing: 0}
	valRank  = map[Validation]int{ValUntested: 0, ValSynthetic: 1, ValReplay: 2, ValHardwarePartial: 3, ValHardware: 4}
	depRank  = map[Dependency]int{DepNative: 0, DepOptionalExternal: 0, DepExternalTool: 2, DepVendorSDK: 3}
)

// Summary condenses the backends of an entry. A missing backend makes the
// implementation partial (or missing when nothing is written); it never
// raises the validation.
func (e Entry) Summary() Summary {
	s := Summary{Implementation: ImplComplete, Validation: ValUntested, Dependency: DepNative}
	written := 0
	for _, b := range e.Backends {
		if b.Implementation == ImplMissing {
			s.Missing = append(s.Missing, b.Name)
			continue
		}
		written++
		if implRank[b.Implementation] < implRank[s.Implementation] {
			s.Implementation = b.Implementation
		}
		if valRank[b.Validation] > valRank[s.Validation] {
			s.Validation = b.Validation
		}
		if depRank[b.Dependency] > depRank[s.Dependency] {
			s.Dependency = b.Dependency
		}
	}
	switch {
	case written == 0:
		s.Implementation = ImplMissing
	case len(s.Missing) > 0 && s.Implementation == ImplComplete:
		s.Implementation = ImplPartial
	}
	return s
}

// Lookup returns the entry for a plugin id.
func Lookup(plugin string) (Entry, bool) {
	for _, e := range Registry() {
		if e.Plugin == plugin {
			return e, true
		}
	}
	return Entry{}, false
}

// Validate checks a registry for internal consistency.
func Validate(entries []Entry) error {
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Plugin == "" || e.Title == "" || len(e.Backends) == 0 {
			return fmt.Errorf("status: entry %q incomplete", e.Plugin)
		}
		if seen[e.Plugin] {
			return fmt.Errorf("status: duplicate plugin %q", e.Plugin)
		}
		seen[e.Plugin] = true
		for _, b := range e.Backends {
			id := e.Plugin + "/" + b.Name
			switch b.Implementation {
			case ImplComplete, ImplPartial, ImplSkeleton, ImplMissing:
			default:
				return fmt.Errorf("status: %s: bad implementation %q", id, b.Implementation)
			}
			switch b.Validation {
			case ValUntested, ValSynthetic, ValReplay, ValHardwarePartial, ValHardware:
			default:
				return fmt.Errorf("status: %s: bad validation %q", id, b.Validation)
			}
			switch b.Dependency {
			case DepNative, DepVendorSDK, DepExternalTool, DepOptionalExternal:
			default:
				return fmt.Errorf("status: %s: bad dependency %q", id, b.Dependency)
			}
			switch b.Fixture {
			case FixtureNone, FixtureSynthetic, FixtureReal:
			default:
				return fmt.Errorf("status: %s: bad fixture %q", id, b.Fixture)
			}
			real := b.Validation == ValHardware || b.Validation == ValHardwarePartial
			if real && len(b.Verified) == 0 {
				return fmt.Errorf("status: %s: real-hardware validation needs a verified list", id)
			}
			if b.Implementation == ImplMissing && b.Validation != ValUntested {
				return fmt.Errorf("status: %s: a missing backend cannot be validated", id)
			}
			if b.Validation == ValHardware && len(b.Unverified) > 0 {
				return fmt.Errorf("status: %s: real-hardware lists nothing unverified (use real-hardware-partial)", id)
			}
			if b.Validation == ValReplay && b.Fixture != FixtureReal {
				return fmt.Errorf("status: %s: replay validation needs a real fixture", id)
			}
			for _, c := range b.Capabilities {
				switch c.Status {
				case CapRealHW, CapUntested, CapSimulatedOnly, CapNotImplemented, CapUnsupportedHW, CapReplayVerified, CapProtocolPartial, CapHypothesisOnly:
				default:
					return fmt.Errorf("status: %s: capability %q has bad status %q", id, c.Name, c.Status)
				}
				if c.Status == CapRealHW && c.Evidence == "" {
					return fmt.Errorf("status: %s: capability %q is REAL_HW_VERIFIED without evidence", id, c.Name)
				}
				if c.Status == CapRealHW && !real {
					return fmt.Errorf("status: %s: capability %q claims real hardware but the backend does not", id, c.Name)
				}
			}
			if b.Cgo == "" {
				return fmt.Errorf("status: %s: cgo statement missing", id)
			}
		}
	}
	return nil
}

// Markdown renders the support matrix. docs/support-matrix.md is generated
// from it and a test keeps the two identical.
func Markdown(entries []Entry) string {
	es := append([]Entry(nil), entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].Plugin < es[j].Plugin })
	var b strings.Builder
	b.WriteString("# Support matrix\n\n")
	b.WriteString("<!-- Generated: HWLAB_UPDATE_DOCS=1 go test ./internal/status. Do not edit by hand. -->\n\n")
	b.WriteString("Implementation says what is written. Validation says the highest evidence that it works.\n")
	b.WriteString("Code, fixtures and passing tests are not hardware validation.\n\n")
	b.WriteString("- implementation: `complete` | `partial` | `skeleton` | `missing`\n")
	b.WriteString("- validation: `untested` | `synthetic` | `replay` | `real-hardware-partial` | `real-hardware`\n")
	b.WriteString("- dependency: `native` | `vendor-sdk` | `external-tool` | `optional-external-tool`\n")
	b.WriteString("- replay fixture: `real` (recorded from hardware) | `synthetic` (generated) | `none`\n\n")
	b.WriteString("| Plugin: backend | Implementation | Validation | Replay fixture | Dependency | CGO |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, e := range es {
		for _, be := range e.Backends {
			fmt.Fprintf(&b, "| %s: %s | %s | %s | %s | %s | %s |\n", e.Plugin, be.Name, be.Implementation,
				be.Validation, be.Fixture, be.Dependency, be.Cgo)
		}
	}
	b.WriteString("\n## Details\n")
	for _, e := range es {
		fmt.Fprintf(&b, "\n### %s — %s\n", e.Plugin, e.Title)
		for _, be := range e.Backends {
			fmt.Fprintf(&b, "\n**%s**\n\n", be.Name)
			fmt.Fprintf(&b, "- implementation: %s; validation: %s; dependency: %s", be.Implementation, be.Validation, be.Dependency)
			if be.Dependencies != "" {
				fmt.Fprintf(&b, " (%s)", be.Dependencies)
			}
			b.WriteString("\n")
			list := func(label string, items []string) {
				if len(items) == 0 {
					return
				}
				fmt.Fprintf(&b, "- %s:\n", label)
				for _, it := range items {
					fmt.Fprintf(&b, "  - %s\n", it)
				}
			}
			if len(be.Capabilities) > 0 {
				b.WriteString("- capabilities:\n\n  | Capability | Status | Evidence |\n  |---|---|---|\n")
				for _, c := range be.Capabilities {
					fmt.Fprintf(&b, "  | %s | %s | %s |\n", c.Name, c.Status, c.Evidence)
				}
				b.WriteString("\n")
			}
			list("verified", be.Verified)
			list("unverified", be.Unverified)
			list("known gaps", be.Gaps)
		}
	}
	return b.String()
}
