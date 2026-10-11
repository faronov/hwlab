// Package policy decides whether an operation that changes the physical lab
// state may proceed. The MVP policy only requires an explicit confirmation
// flag, but callers depend on the Policy interface so richer rules (roles,
// allow-lists, audit) can be added without touching services.
package policy

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies an action.
type Kind string

const (
	// KindConfig is device configuration that changes electrical state
	// (supply voltage, DUT power) - see PluginInfo.dangerous_config_keys.
	KindConfig Kind = "config"
	// KindInvoke is a capability method call marked DANGER_CONFIRM
	// (flash, memory write, raw SCPI, power control).
	KindInvoke Kind = "invoke"
)

// Action describes what is about to happen.
type Action struct {
	Kind       Kind
	Plugin     string
	Device     string
	Capability string
	Method     string
	ConfigKeys []string
}

func (a Action) String() string {
	switch a.Kind {
	case KindInvoke:
		return fmt.Sprintf("%s.%s on %s/%s", a.Capability, a.Method, a.Plugin, a.Device)
	default:
		return fmt.Sprintf("configure %s/%s with %s", a.Plugin, a.Device, strings.Join(a.ConfigKeys, ", "))
	}
}

// ConfirmationRequired is returned when an action needs explicit approval.
type ConfirmationRequired struct{ Action Action }

func (e *ConfirmationRequired) Error() string {
	return fmt.Sprintf("dangerous operation requires confirmation: %s (pass --yes / confirm=true)", e.Action)
}

// IsConfirmationRequired reports whether err is a ConfirmationRequired.
func IsConfirmationRequired(err error) bool {
	var c *ConfirmationRequired
	return errors.As(err, &c)
}

// Policy authorizes actions.
type Policy interface {
	// Authorize returns nil, or *ConfirmationRequired when confirmed is false
	// and the action is dangerous, or another error to deny outright.
	Authorize(a Action, confirmed bool) error
}

// RequireConfirmation is the default policy: every dangerous action needs
// confirmed == true.
type RequireConfirmation struct{}

func (RequireConfirmation) Authorize(a Action, confirmed bool) error {
	if confirmed {
		return nil
	}
	return &ConfirmationRequired{Action: a}
}
