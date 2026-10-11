package cortexm_test

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	cmsis "github.com/faronov/hwlab/plugins/cmsisdap"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
	jlink "github.com/faronov/hwlab/plugins/jlinknative"
	jlsim "github.com/faronov/hwlab/plugins/jlinknative/sim"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
	stlink "github.com/faronov/hwlab/plugins/stlink"
	stsim "github.com/faronov/hwlab/plugins/stlink/sim"
	xdsplugin "github.com/faronov/hwlab/plugins/xds110"
	"github.com/faronov/hwlab/plugins/xds110/xds"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type adapterRig struct {
	p         pluginsdk.Plugin
	id, reset string
	core      *cortexm.Sim
	cfg       map[string]any
}

func watchpointRigs() map[string]func() adapterRig {
	return map[string]func() adapterRig{
		"jlink-native": func() adapterRig {
			s := jlsim.New()
			p := jlink.NewWith(func() []jlink.Entry { return []jlink.Entry{{ID: jlink.SimID, Sim: s}} },
				func(e jlink.Entry) (jusb.Transport, error) { return e.Sim, nil })
			return adapterRig{p: p, id: jlink.SimID, reset: "sysreset", core: s.Core()}
		},
		"cmsisdap": func() adapterRig {
			s := dap.NewSim()
			p := cmsis.NewWith(func() []cmsis.Entry { return []cmsis.Entry{{ID: cmsis.SimID, Sim: s}} },
				func(e cmsis.Entry) (dap.Transport, error) { return e.Sim, nil })
			return adapterRig{p: p, id: cmsis.SimID, reset: "sysreset", core: s.Core()}
		},
		"xds110": func() adapterRig {
			s := xds.NewSim()
			p := xdsplugin.NewWith(func() []xdsplugin.Entry { return []xdsplugin.Entry{{ID: xdsplugin.SimID, Sim: s}} },
				func(e xdsplugin.Entry) (xds.Transport, error) { return e.Sim, nil })
			return adapterRig{p: p, id: xdsplugin.SimID, reset: "sysreset", core: s.Core()}
		},
		"stlink": func() adapterRig {
			s := cortexm.NewSim()
			e := stlink.Entry{ID: "0483:3748:WATCH", Dev: stlink.SimDevice("WATCH"), Serial: "WATCH", Sim: stsim.NewConfig("WATCH", s)}
			p := stlink.NewWith(func() []stlink.Entry { return []stlink.Entry{e} },
				func(e stlink.Entry, _ int, _ usb.OpenOptions) (usb.Port, error) { return e.Sim.Port(), nil })
			return adapterRig{p: p, id: "0483:3748:WATCH", reset: "system", core: s, cfg: map[string]any{"allow_reset": true}}
		},
	}
}

func TestWatchpointLifecycleAcrossAdapters(t *testing.T) {
	for name, makeRig := range watchpointRigs() {
		t.Run(name, func(t *testing.T) {
			for _, scenario := range []string{"reuse-reset-reopen", "verify-failure", "clear-failure", "cancel", "operation-error", "foreign-owner"} {
				t.Run(scenario, func(t *testing.T) {
					r := makeRig()
					ctx := context.Background()
					if err := r.p.OpenDevice(ctx, r.id, r.cfg); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						r.core.Hooks = nil
						if err := r.p.CloseDevice(ctx, r.id); err != nil {
							t.Error(err)
						}
					})
					call := func(ctx context.Context, m string, args map[string]any) (map[string]any, error) {
						st, err := structpb.NewStruct(args)
						if err != nil {
							t.Fatal(err)
						}
						res, err := r.p.Invoke(ctx, &pluginv1.InvokeRequest{DeviceId: r.id, Capability: "DebugProbe", Method: m, Args: st})
						if err != nil {
							return nil, err
						}
						return res.Result.AsMap(), nil
					}
					must := func(m string, args map[string]any) map[string]any {
						t.Helper()
						res, err := call(ctx, m, args)
						if err != nil {
							t.Fatalf("%s: %v", m, err)
						}
						return res
					}
					assertDisabled := func(slot int) {
						t.Helper()
						fn, err := r.core.Read32(ctx, cortexm.AddrDWTComp+16*uint32(slot)+8)
						if err != nil || fn&0xF != 0 {
							t.Fatalf("FUNCTION%d leaked: 0x%x, %v", slot, fn, err)
						}
					}
					args := map[string]any{"address": "0x20000010", "kind": "write", "mask_bits": 2.0}
					if scenario == "verify-failure" {
						armed, injected := false, false
						r.core.Hooks = append(r.core.Hooks, func(a uint32, write bool, v uint32) (uint32, bool, error) {
							if a == cortexm.AddrDWTComp+8 && write && v == 6 {
								armed = true
							}
							if a == cortexm.AddrDWTComp+8 && !write && armed && !injected {
								injected = true
								return 7, true, nil
							}
							return 0, false, nil
						})
						if _, err := call(ctx, "set_watchpoint", args); err == nil {
							t.Fatal("failed verification claimed success")
						}
						r.core.Hooks = nil
						assertDisabled(0)
						must("set_watchpoint", args)
						must("clear_watchpoint", map[string]any{"all": true})
						return
					}
					if scenario == "foreign-owner" {
						for a, v := range map[uint32]uint32{cortexm.AddrDWTComp: 0x20000080, cortexm.AddrDWTComp + 4: 2, cortexm.AddrDWTComp + 8: 6, cortexm.AddrDEMCR: cortexm.DEMCRTraceEn} {
							if err := r.core.Write32(ctx, a, v); err != nil {
								t.Fatal(err)
							}
						}
					}
					must("halt", nil)
					wp := must("set_watchpoint", args)
					slot := int(wp["slot"].(float64))
					if _, err := call(ctx, "set_watchpoint", map[string]any{"address": "0x20000010", "kind": "access", "mask_bits": 2.0}); status.Code(err) != codes.AlreadyExists {
						t.Fatalf("conflicting comparator settings: %v", err)
					}
					switch scenario {
					case "reuse-reset-reopen":
						for i := 0; i < 20; i++ {
							must("clear_watchpoint", map[string]any{"all": true})
							assertDisabled(slot)
							next := must("set_watchpoint", args)
							if next["slot"] != wp["slot"] {
								t.Fatal("comparator was not reused")
							}
						}
						must("reset", map[string]any{"kind": r.reset, "halt": true})
						assertDisabled(slot)
						must("set_watchpoint", args)
						if err := r.p.CloseDevice(ctx, r.id); err != nil {
							t.Fatal(err)
						}
						assertDisabled(slot)
						if err := r.p.OpenDevice(ctx, r.id, r.cfg); err != nil {
							t.Fatal(err)
						}
						must("set_watchpoint", args)
					case "clear-failure":
						r.core.Hooks = append(r.core.Hooks, func(a uint32, write bool, v uint32) (uint32, bool, error) {
							return 0, write && a == cortexm.AddrDWTComp+16*uint32(slot)+8 && v == 0, nil
						})
						if _, err := call(ctx, "clear_watchpoint", map[string]any{"all": true}); err == nil {
							t.Fatal("failed clear reported success")
						}
						live := must("list_watchpoints", nil)["watchpoints"].([]any)
						if len(live) != 1 {
							t.Fatal("failed clear lost hardware ownership")
						}
						r.core.Hooks = nil
					case "cancel":
						cctx, cancel := context.WithCancel(ctx)
						cancel()
						if _, err := call(cctx, "wait_halt", nil); status.Code(err) != codes.DeadlineExceeded && status.Code(err) != codes.Canceled {
							t.Fatalf("lost cancellation classification: %v", err)
						}
						assertDisabled(slot)
					case "operation-error":
						must("resume", nil)
						if _, err := call(ctx, "wait_halt", map[string]any{"wait_ms": 1.0}); status.Code(err) != codes.DeadlineExceeded {
							t.Fatalf("lost timeout classification: %v", err)
						}
						assertDisabled(slot)
					}
					if scenario != "cancel" && scenario != "operation-error" {
						must("clear_watchpoint", map[string]any{"all": true})
					}
					assertDisabled(slot)
					if scenario == "foreign-owner" {
						fn, err := r.core.Read32(ctx, cortexm.AddrDWTComp+8)
						if err != nil || fn&0xF != 6 {
							t.Fatal("foreign comparator was cleared")
						}
					}
				})
			}
		})
	}
}
