package esp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/riscv"
)

// SPI1 is the flash controller the CPU's own flash driver uses for commands.
// It is driven here register by register through the RISC-V system bus while
// the hart is halted: no code runs on the target, no ROM function is called.
const (
	SPI1Base = 0x60003000

	spiCmd      = 0x00
	spiAddr     = 0x04
	spiCtrl     = 0x08
	spiUser     = 0x18
	spiMOSILen  = 0x24
	spiMISOLen  = 0x28
	spiW0       = 0x58
	spiBufWords = 16

	cmdUSR = 1 << 18
	cmdPE  = 1 << 17

	userCommand = 1 << 31
	userAddr    = 1 << 30
	userDummy   = 1 << 29
	userMISO    = 1 << 28
	userMOSI    = 1 << 27

	// MaxChunk is the largest single SPI transaction (the 64-byte buffer).
	MaxChunk   = spiBufWords * 4
	PageSize   = 256
	SectorSize = 4096
	BlockSize  = 65536
)

// Flash errors.
var (
	ErrRange      = errors.New("flash: range outside the flash")
	ErrAlign      = errors.New("flash: address or length not aligned to the erase unit")
	ErrNotErased  = errors.New("flash: region is not erased")
	ErrVerify     = errors.New("flash: verify mismatch")
	ErrFlashBusy  = errors.New("flash: chip stayed busy (write-in-progress)")
	ErrController = errors.New("flash: SPI controller did not finish a transaction")
)

// Flash drives the SPI flash through SPI1.
type Flash struct {
	dm     *riscv.DM
	saved  []uint32 // ctrl, addr..., user regs as found
	size   int
	opened bool
	// Counters for diagnostics and tests.
	Transactions int
}

// OpenFlash prepares the controller: waits until the host is idle and saves
// the registers it is about to change. The caller must have halted the
// hart(s) that could be using SPI1.
func OpenFlash(ctx context.Context, dm *riscv.DM) (*Flash, error) {
	f := &Flash{dm: dm}
	if err := f.waitHostIdle(ctx); err != nil {
		return nil, err
	}
	r, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusRead, Addr: SPI1Base + spiAddr, Words: 2}, // addr, ctrl
		{Kind: riscv.BusRead, Addr: SPI1Base + spiUser, Words: 5}, // user, user1, user2, mosi, miso len
	})
	if err != nil {
		return nil, err
	}
	f.saved = append(append([]uint32{}, r[0]...), r[1]...)
	f.opened = true
	return f, nil
}

// Close puts the controller registers back as they were found.
func (f *Flash) Close(ctx context.Context) error {
	if !f.opened {
		return nil
	}
	f.opened = false
	_, err := f.dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: SPI1Base + spiAddr, Vals: f.saved[0:2]},
		{Kind: riscv.BusWrite, Addr: SPI1Base + spiUser, Vals: f.saved[2:7]},
	})
	return err
}

func (f *Flash) waitHostIdle(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := ReadWord(ctx, f.dm, SPI1Base+spiCmd)
		if err != nil {
			return err
		}
		if v == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: CMD register stayed %#x", ErrController, v)
		}
		time.Sleep(time.Millisecond)
	}
}

// txn describes one user-mode SPI transaction.
type txn struct {
	cmd   byte
	addr  uint32
	hasA  bool
	dummy int // clock cycles
	miso  int // bytes to read
	mosi  []byte
	pe    bool
}

// ops returns the register writes that configure and start the transaction.
// A read of the CMD register follows so the caller can tell it has finished.
func (t txn) ops() []riscv.BusOp {
	user := uint32(userCommand)
	var user1 uint32
	if t.hasA {
		user |= userAddr
		user1 |= 23 << 26 // 24-bit address
	}
	if t.dummy > 0 {
		user |= userDummy
		user1 |= uint32(t.dummy-1) & 0x3F
	}
	var mosiLen, misoLen uint32
	if t.miso > 0 {
		user |= userMISO
		misoLen = uint32(t.miso*8 - 1)
	}
	if len(t.mosi) > 0 {
		user |= userMOSI
		mosiLen = uint32(len(t.mosi)*8 - 1)
	}
	user2 := uint32(7)<<28 | uint32(t.cmd)
	trig := uint32(cmdUSR)
	if t.pe {
		trig |= cmdPE
	}
	ops := []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: SPI1Base + spiAddr, Vals: []uint32{t.addr & 0xFFFFFF, 0}}, // addr, ctrl=0
		{Kind: riscv.BusWrite, Addr: SPI1Base + spiUser, Vals: []uint32{user, user1, user2, mosiLen, misoLen}},
	}
	if len(t.mosi) > 0 {
		ops = append(ops, riscv.BusOp{Kind: riscv.BusWrite, Addr: SPI1Base + spiW0, Vals: packWords(t.mosi)})
	}
	return append(ops, riscv.BusOp{Kind: riscv.BusWrite, Addr: SPI1Base + spiCmd, Vals: []uint32{trig}})
}

func packWords(b []byte) []uint32 {
	w := make([]uint32, (len(b)+3)/4)
	for i := range w {
		v := uint32(0xFFFFFFFF)
		for k := 0; k < 4 && i*4+k < len(b); k++ {
			v = v&^(0xFF<<(8*k)) | uint32(b[i*4+k])<<(8*k)
		}
		w[i] = v
	}
	return w
}

func unpackWords(w []uint32, n int) []byte {
	out := make([]byte, 0, len(w)*4)
	for _, v := range w {
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return out[:n]
}

// run executes a user transaction and returns the bytes read. It waits for the
// controller to go idle first, and verifies in the same pipelined transfer
// that the command had completed before the result buffer was read.
func (f *Flash) run(ctx context.Context, t txn) ([]byte, error) {
	for try := 0; ; try++ {
		ops := t.ops()
		ops = append(ops, riscv.BusOp{Kind: riscv.BusRead, Addr: SPI1Base + spiCmd, Words: 1})
		doneAt := len(ops) - 1
		if t.miso > 0 {
			ops = append(ops, riscv.BusOp{Kind: riscv.BusRead, Addr: SPI1Base + spiW0, Words: (t.miso + 3) / 4})
		}
		r, err := f.dm.BusBatch(ctx, ops)
		if err != nil {
			return nil, err
		}
		f.Transactions++
		if r[doneAt][0] == 0 {
			if t.miso > 0 {
				return unpackWords(r[doneAt+1], t.miso), nil
			}
			return nil, nil
		}
		// Not finished when the CMD register was read: wait, then repeat the
		// whole transaction (reads and WREN are harmless to repeat).
		if try >= 3 {
			return nil, fmt.Errorf("%w (CMD=%#x)", ErrController, r[doneAt][0])
		}
		if err := f.waitHostIdle(ctx); err != nil {
			return nil, err
		}
	}
}

// JEDEC reads manufacturer, memory type and capacity code (command 0x9F).
func (f *Flash) JEDEC(ctx context.Context) ([3]byte, error) {
	b, err := f.run(ctx, txn{cmd: 0x9F, miso: 3})
	if err != nil {
		return [3]byte{}, err
	}
	return [3]byte{b[0], b[1], b[2]}, nil
}

// Status reads status register 1 (command 0x05).
func (f *Flash) Status(ctx context.Context) (byte, error) {
	b, err := f.run(ctx, txn{cmd: 0x05, miso: 1})
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// SFDP reads n bytes (max 64) of the Serial Flash Discoverable Parameters.
func (f *Flash) SFDP(ctx context.Context, addr uint32, n int) ([]byte, error) {
	return f.run(ctx, txn{cmd: 0x5A, addr: addr, hasA: true, dummy: 8, miso: n})
}

// ReadChunk reads up to 64 bytes with the fast-read command (0x0B).
func (f *Flash) ReadChunk(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if n < 1 || n > MaxChunk {
		return nil, fmt.Errorf("flash: chunk of %d bytes (want 1..%d)", n, MaxChunk)
	}
	return f.run(ctx, txn{cmd: 0x0B, addr: addr, hasA: true, dummy: 8, miso: n})
}

// WaitReady polls the write-in-progress bit until it clears.
func (f *Flash) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s, err := f.Status(ctx)
		if err != nil {
			return err
		}
		if s&1 == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s (status %#02x)", ErrFlashBusy, timeout, s)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *Flash) writeEnable(ctx context.Context) error {
	if _, err := f.run(ctx, txn{cmd: 0x06}); err != nil {
		return err
	}
	s, err := f.Status(ctx)
	if err != nil {
		return err
	}
	if s&2 == 0 {
		return fmt.Errorf("flash: write enable latch did not set (status %#02x); is the chip write protected?", s)
	}
	return nil
}

// issueUser starts a user-mode command flagged as a program/erase operation
// (CMD bits USR and PE, as the CPU's own flash driver does) and waits for the
// controller and then for the flash's write-in-progress bit. It is never
// repeated automatically: a retried erase or program is only safe when the
// caller knows it is.
func (f *Flash) issueUser(ctx context.Context, t txn, timeout time.Duration) error {
	t.pe = true
	if _, err := f.dm.BusBatch(ctx, t.ops()); err != nil {
		return err
	}
	f.Transactions++
	if err := f.waitHostIdle(ctx); err != nil {
		return err
	}
	return f.WaitReady(ctx, timeout)
}

// ProgramChunk programs up to 64 bytes that do not cross a page boundary. The
// target bytes must be erased (programming can only clear bits).
func (f *Flash) ProgramChunk(ctx context.Context, addr uint32, data []byte) error {
	if len(data) < 1 || len(data) > MaxChunk || int(addr%PageSize)+len(data) > PageSize {
		return fmt.Errorf("flash: bad program chunk addr=%#x len=%d", addr, len(data))
	}
	if err := f.writeEnable(ctx); err != nil {
		return err
	}
	return f.issueUser(ctx, txn{cmd: 0x02, addr: addr, hasA: true, mosi: data}, 50*time.Millisecond)
}

// EraseSector erases one 4 KiB sector.
func (f *Flash) EraseSector(ctx context.Context, addr uint32) error {
	if addr%SectorSize != 0 {
		return fmt.Errorf("%w: sector address %#x", ErrAlign, addr)
	}
	if err := f.writeEnable(ctx); err != nil {
		return err
	}
	return f.issueUser(ctx, txn{cmd: 0x20, addr: addr, hasA: true}, 2*time.Second)
}

// EraseBlock64 erases one 64 KiB block.
func (f *Flash) EraseBlock64(ctx context.Context, addr uint32) error {
	if addr%BlockSize != 0 {
		return fmt.Errorf("%w: block address %#x", ErrAlign, addr)
	}
	if err := f.writeEnable(ctx); err != nil {
		return err
	}
	return f.issueUser(ctx, txn{cmd: 0xD8, addr: addr, hasA: true}, 5*time.Second)
}

// EraseChip erases the whole flash. It is only reachable through the
// full-chip-erase permission tier.
func (f *Flash) EraseChip(ctx context.Context) error {
	if err := f.writeEnable(ctx); err != nil {
		return err
	}
	return f.issueUser(ctx, txn{cmd: 0xC7}, 10*time.Minute)
}
