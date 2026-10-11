package app_test

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/status"
)

func TestDoctorReportsSupportStatus(t *testing.T) {
	a := newApp(t)
	r := a.Doctor(context.Background())
	if r.Failed() {
		t.Fatalf("doctor failed: %+v", r.Checks)
	}
	var fake *app.DoctorPlugin
	var pico *app.DoctorPlugin
	for i := range r.Plugins {
		switch r.Plugins[i].ID {
		case "fake":
			fake = &r.Plugins[i]
		case "picoscope":
			pico = &r.Plugins[i]
		}
	}
	if fake == nil || !fake.Installed || !fake.Classified || fake.Validation != status.ValSynthetic {
		t.Fatalf("fake plugin status wrong: %+v", fake)
	}
	if pico == nil || pico.Installed || pico.Implementation != status.ImplPartial || len(pico.Missing) == 0 {
		t.Fatalf("picoscope must be listed as not installed here, partial with a missing backend: %+v", pico)
	}
	for _, p := range a.ListPlugins(context.Background()) {
		if p.ID == "fake" && (p.Support == nil || p.Support.Validation != status.ValSynthetic) {
			t.Fatalf("plugin list lacks support status: %+v", p)
		}
	}
}
