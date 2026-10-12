package telinkplugin

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/telink/target"
)

// flashBytes is the flash size from the JEDEC id. It is only readable while
// the target CPU is captured (DebugProbe.halt); with the CPU running the id
// comes back as garbage and is rejected.
func (d *device) flashBytes() (size, jedec uint32, err error) {
	jedec, err = d.prog.JEDEC()
	if err != nil {
		return 0, 0, err
	}
	if size = target.FlashSize(jedec); size == 0 {
		return 0, jedec, status.Errorf(codes.FailedPrecondition, "flash id 0x%06x is not plausible: halt the CPU (DebugProbe.halt) and retry", jedec)
	}
	return size, jedec, nil
}

// family is the driver for the connected target, nil if the chip id is unknown.
func (d *device) family() (*target.Family, error) {
	_, id, err := d.prog.ReadChipID()
	if err != nil {
		return nil, err
	}
	return target.Lookup(id), nil
}

// protection resolves the flash protection map for this target. It is
// recomputed on every use: it reads flash and must never be stale.
func (d *device) protection() (target.Protection, error) {
	size, _, err := d.flashBytes()
	if err != nil {
		return target.Protection{}, err
	}
	f, err := d.family()
	if err != nil {
		return target.Protection{}, err
	}
	return f.Protection(d.o.layout, size, func(addr uint32, n int) ([]byte, error) {
		return d.prog.FlashRead(addr, n)
	}), nil
}

// modifiable returns the verified layout, or a PermissionDenied error that
// says why erase/program is disabled for the connected target.
func (d *device) modifiable(op string) (target.Protection, error) {
	prot, err := d.protection()
	if err != nil {
		return prot, status.Errorf(codes.FailedPrecondition, "flash_%s disabled: cannot establish the flash protection map: %v", op, err)
	}
	if !prot.Verified {
		return prot, status.Errorf(codes.PermissionDenied, "flash_%s disabled for this target (implemented, allowed_on_current_target: false): %s", op, prot.Reason)
	}
	return prot, nil
}

// capabilityState reports, per operation, whether it is implemented and
// whether it is allowed on the connected target.
func capabilityState(prot target.Protection, protErr error) map[string]any {
	allowed, reason := false, ""
	switch {
	case protErr != nil:
		reason = "cannot establish the flash protection map: " + protErr.Error()
	case !prot.Verified:
		reason = prot.Reason
	default:
		allowed, reason = true, prot.Reason
	}
	op := func() map[string]any {
		return map[string]any{"implemented": true, "allowed_on_current_target": allowed, "reason": reason}
	}
	return map[string]any{
		"identify": "yes", "memory_read": "yes", "flash_read": "yes", "halt": "yes", "step": "yes",
		"breakpoint": "yes (1)", "memory_write": "yes (SRAM; registers need allow_registers)",
		"flash_erase": op(), "flash_program": op(), "flash_verify": "yes",
	}
}

func (d *device) sramWindow() (lo, hi uint32, err error) {
	f, err := d.family()
	if err != nil {
		return 0, 0, err
	}
	if f == nil {
		return 0, 0, status.Error(codes.PermissionDenied, "target chip family unknown: SRAM window not known, memory writes are disabled")
	}
	return f.SRAMStart, f.SRAMEnd, nil
}
