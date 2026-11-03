package esp

import (
	"context"
	"fmt"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/xtensa"
)

// Bus is the 32-bit memory access this package needs from a debug transport.
// *riscv.DM implements it through the RISC-V system bus; an Xtensa target
// implements it through its debug module (instructions executed while the
// core is halted). BusOp keeps the riscv shape so the ESP32-C6/H2 paths are
// unchanged.
type Bus interface {
	ReadMem(ctx context.Context, addr uint32, n int) ([]byte, error)
	BusBatch(ctx context.Context, ops []riscv.BusOp) ([][]uint32, error)
}

var _ Bus = (*riscv.DM)(nil)

// XtensaBus is a Bus through a halted Xtensa core: every access is a load or
// store the core executes in debug mode. Unlike the RISC-V system bus it
// needs the core halted. A BusBatch is one pipelined debug-module queue
// (xtensa.Core.MemBatch) with a single exception check at its end.
type XtensaBus struct{ C *xtensa.Core }

var _ Bus = XtensaBus{}

// ReadMem reads n bytes.
func (b XtensaBus) ReadMem(ctx context.Context, addr uint32, n int) ([]byte, error) {
	return b.C.ReadMem(ctx, addr, n)
}

// BusBatch runs ops in order in one queue; a fault fails the whole batch.
func (b XtensaBus) BusBatch(ctx context.Context, ops []riscv.BusOp) ([][]uint32, error) {
	mo := make([]xtensa.MemOp, len(ops))
	for i, op := range ops {
		switch op.Kind {
		case riscv.BusRead:
			mo[i] = xtensa.MemOp{Addr: op.Addr, Words: op.Words}
		case riscv.BusWrite:
			mo[i] = xtensa.MemOp{Write: true, Addr: op.Addr, Vals: op.Vals}
		default:
			return nil, fmt.Errorf("esp: unknown bus operation %d", op.Kind)
		}
	}
	return b.C.MemBatch(ctx, mo)
}
