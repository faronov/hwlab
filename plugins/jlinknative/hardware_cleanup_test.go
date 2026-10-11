//go:build hardware && linux

package jlinknativeplugin

import (
	"context"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestHWWatchpointCleanup writes only the explicitly approved SRAM scratch
// window. It restores memory/registers and leaves the firmware running.
func TestHWWatchpointCleanup(t *testing.T) {
	if os.Getenv("HWLAB_JLINKNATIVE_MAINT") != "1" {
		t.Skip("set HWLAB_JLINKNATIVE_MAINT=1, SERIAL and SCRATCH for the owned EFR32")
	}
	if os.Getenv("HWLAB_JLINKNATIVE_SERIAL") == "" {
		t.Fatal("explicit serial selection required")
	}
	n, err := strconv.ParseUint(os.Getenv("HWLAB_JLINKNATIVE_SCRATCH"), 0, 32)
	if err != nil || n < 0x20000000 || n+0x104 > 0x20008000 || n%4 != 0 {
		t.Fatal("SCRATCH must be an approved aligned window in EFR32MG1 32 KiB SRAM")
	}
	code, data := uint32(n), uint32(n)+0x100
	p := New()
	id := hwID(t, p)
	ctx := context.Background()
	if err := p.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.CloseDevice(ctx, id); err != nil {
			t.Errorf("close cleanup: %v", err)
		}
	})
	call := func(m string, a map[string]any) map[string]any {
		t.Helper()
		r, err := hwInvoke(p, id, "DebugProbe", m, a)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		return r
	}
	call("connect", nil)
	call("halt", nil)
	d := p.devs[id]
	c := d.core
	t.Cleanup(func() {
		if _, err := c.Resume(ctx); err != nil {
			t.Errorf("leave target running: %v", err)
		}
	})
	savedRegs := call("read_registers", nil)["registers"].(map[string]any)
	savedCode, err := c.Read(ctx, code, 4)
	if err != nil {
		t.Fatal(err)
	}
	savedData, err := c.Read(ctx, data, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		check := func(name string, err error) {
			if err != nil {
				t.Errorf("restore %s: %v", name, err)
			}
		}
		_, err := c.Halt(ctx)
		check("halt", err)
		check("watchpoints", c.CleanupDebug(ctx))
		check("SRAM code", c.Write(ctx, code, savedCode))
		check("SRAM data", c.Write(ctx, data, savedData))
		for _, name := range []string{"r0", "r1", "pc"} {
			id, _ := cortexm.ParseRegister(name)
			v, err := strconv.ParseUint(savedRegs[name].(string), 0, 32)
			check(name, err)
			if err == nil {
				check(name, c.WriteRegister(ctx, id, uint32(v)))
			}
		}
		_, err = c.Resume(ctx)
		check("resume", err)
		state, err := c.Status(ctx)
		check("final status", err)
		if err == nil && state.Halted {
			t.Error("target left halted")
		}
	})
	stub, _ := hex.DecodeString("0160fee7") // str r1,[r0]; b .
	if err := c.Write(ctx, code, stub); err != nil {
		t.Fatal(err)
	}
	prepare := func() {
		t.Helper()
		if _, err := c.Halt(ctx); err != nil {
			t.Fatal(err)
		}
		for reg, v := range map[cortexm.RegisterID]uint32{0: data, 1: 0xA5A5A5A5, cortexm.RegPC: code} {
			if err := c.WriteRegister(ctx, reg, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Bus.Write32(ctx, data, 0); err != nil {
			t.Fatal(err)
		}
		if err := c.ClearCause(ctx); err != nil {
			t.Fatal(err)
		}
	}
	assertDisabled := func(slot int) {
		t.Helper()
		// Read with trace enabled: a powered-down DWT reading zero is not proof.
		demcr, err := c.Bus.Read32(ctx, cortexm.AddrDEMCR)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Bus.Write32(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRTraceEn); err != nil {
			t.Fatal(err)
		}
		fn, rerr := c.Bus.Read32(ctx, cortexm.AddrDWTComp+16*uint32(slot)+8)
		werr := c.Bus.Write32(ctx, cortexm.AddrDEMCR, demcr)
		if rerr != nil || werr != nil || fn&0xF != 0 {
			t.Fatalf("FUNCTION%d after cleanup: 0x%x, read=%v restore=%v", slot, fn, rerr, werr)
		}
	}
	args := map[string]any{"address": float64(data), "kind": "write", "mask_bits": 2.0}
	for round := 0; round < 20; round++ {
		prepare()
		wp := call("set_watchpoint", args)
		slot := int(wp["slot"].(float64))
		if err := c.RunMasked(ctx); err != nil {
			t.Fatal(err)
		}
		hit := call("wait_halt", map[string]any{"wait_ms": 1000.0})
		if hit["watchpoint_hit"] != true {
			t.Fatalf("round %d did not hit: %v", round, hit)
		}
		written, err := c.Bus.Read32(ctx, data)
		if err != nil || written != 0xA5A5A5A5 {
			t.Fatalf("round %d SRAM write: 0x%x, %v", round, written, err)
		}
		call("clear_watchpoint", map[string]any{"all": true})
		assertDisabled(slot)
		// Repeat the exact core-side write WITHOUT any watchpoint.
		prepare()
		if err := c.RunMasked(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		st, err := c.Status(ctx)
		if err != nil || st.Halted {
			t.Fatalf("round %d stale comparator halted the core: %v, %v", round, st, err)
		}
		if _, err := c.Halt(ctx); err != nil {
			t.Fatal(err)
		}
		cause, err := c.HaltCause(ctx)
		if err != nil || cause.Has("watchpoint") {
			t.Fatalf("round %d stale DWT cause: %v, %v", round, cause, err)
		}
		written, err = c.Bus.Read32(ctx, data)
		if err != nil || written != 0xA5A5A5A5 {
			t.Fatalf("negative test did not execute the store: 0x%x, %v", written, err)
		}
		t.Logf("round %02d: hit, clear, FUNCTION disabled, no stale hit", round+1)
	}
	prepare()
	wp := call("set_watchpoint", args)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	a, _ := structpb.NewStruct(map[string]any{"wait_ms": 1000.0})
	_, err = p.Invoke(cctx, &pluginv1.InvokeRequest{DeviceId: id, Capability: "DebugProbe", Method: "wait_halt", Args: a})
	if status.Code(err) != codes.DeadlineExceeded && status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled operation: %v", err)
	}
	assertDisabled(int(wp["slot"].(float64)))
	t.Log("cancelled watchpoint operation: comparator disabled using fresh cleanup context")
}
