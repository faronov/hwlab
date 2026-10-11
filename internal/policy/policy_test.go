package policy

import (
	"errors"
	"testing"
)

func TestRequireConfirmation(t *testing.T) {
	act := Action{Kind: KindConfig, Plugin: "ppk2", Device: "ABC", ConfigKeys: []string{"supply_mv"}}
	err := RequireConfirmation{}.Authorize(act, false)
	if !IsConfirmationRequired(err) {
		t.Fatalf("unconfirmed dangerous action must require confirmation, got %v", err)
	}
	var cr *ConfirmationRequired
	if !errors.As(err, &cr) || cr.Action.Plugin != "ppk2" {
		t.Errorf("error does not carry the action: %v", err)
	}
	if err := (RequireConfirmation{}).Authorize(act, true); err != nil {
		t.Errorf("confirmed action rejected: %v", err)
	}
	if IsConfirmationRequired(errors.New("other")) || IsConfirmationRequired(nil) {
		t.Error("IsConfirmationRequired must be false for unrelated errors")
	}
	if s := act.String(); s == "" {
		t.Error("Action.String must describe the action")
	}
}
