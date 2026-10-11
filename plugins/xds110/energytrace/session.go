package energytrace

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/plugins/xds110/xds"
)

// Command codes on the vendor command channel (the same framing as the debug
// protocol). Only the sequence below was observed; names for commands whose
// meaning is UNKNOWN are descriptive of their position, not of their function.
const (
	CmdPrepare      = 0x28 // UNKNOWN meaning, first command of every EnergyTrace run
	CmdUnknown46    = 0x46 // UNKNOWN meaning, reply 02000000 07
	CmdUnknown30    = 0x30 // UNKNOWN meaning, argument 00
	CmdUnknown31    = 0x31 // UNKNOWN meaning, argument 01
	CmdStreamConfig = 0x1d // 7 constant argument bytes, meaning UNKNOWN
	CmdCalibration  = 0x1e // argument u16 LE range, reply u32 sum + u32 count
	CmdStreamStart  = 0x1f // data starts on the second bulk interface
	CmdStreamStop   = 0x20
	CmdStreamFree   = 0x21

	// StatusAlreadyStopped is the status the probe returns to a repeated
	// stop; the oracle tool provokes it on every run and ignores it.
	StatusAlreadyStopped xds.Status = -644
)

// Commander is the command channel; *xds.Probe implements it.
type Commander interface {
	Do(cmd byte, args []byte) ([]byte, error)
}

// Calibrations are the zero-current offsets measured by the probe at power
// up. Only range 0 is decoded; ranges 4 and 1 are reported for completeness.
type Calibrations struct {
	Range0 Calibration
	Range4 Calibration
	Range1 Calibration
}

var (
	argSetTCK  = []byte{0x02, 0, 0, 0}
	argConfig  = []byte{0x01, 0x00, 0x80, 0xbb, 0x00, 0x00, 0x00}
	argUnk30   = []byte{0x00}
	argUnk31   = []byte{0x01}
	calRanges  = [3]uint16{0, 4, 1}
	wantHWID   = xds.HWLPXDS110ET
	errUnknown = errors.New("energytrace: unsupported probe")
)

// Prepare replays, command for command, the sequence the vendor tool sends
// before streaming, up to but excluding the stream start. Nothing outside
// that sequence is ever sent. It returns the calibrations.
//
// The 0x04 command is the debug protocol's set-TCK command with the value 2;
// the vendor tool sends it unchanged, so it is sent unchanged here.
func Prepare(c Commander) (Calibrations, error) {
	var cal Calibrations
	step := func(name string, cmd byte, args []byte) ([]byte, error) {
		d, err := c.Do(cmd, args)
		if err != nil {
			return nil, fmt.Errorf("energytrace %s (cmd %#02x): %w", name, cmd, err)
		}
		return d, nil
	}
	// A previous owner (the debug path) may have left the session connected.
	if _, err := c.Do(xds.CmdDisconnect, nil); err != nil {
		var xe *xds.Error
		if !errors.As(err, &xe) {
			return cal, fmt.Errorf("energytrace disconnect: %w", err)
		}
	}
	if _, err := step("prepare", CmdPrepare, nil); err != nil {
		return cal, err
	}
	ver, err := step("version", xds.CmdVersion, nil)
	if err != nil {
		return cal, err
	}
	if len(ver) < 6 {
		return cal, fmt.Errorf("%w: version reply of %d bytes", errUnknown, len(ver))
	}
	if hw := binary.LittleEndian.Uint16(ver[4:]); hw != wantHWID {
		return cal, fmt.Errorf("%w: hardware id %#x, only %#x (LP-XDS110ET) was observed", errUnknown, hw, wantHWID)
	}
	if _, err := step("set-tck", xds.CmdSetTCK, argSetTCK); err != nil {
		return cal, err
	}
	if _, err := step("cmd46", CmdUnknown46, nil); err != nil {
		return cal, err
	}
	if _, err := step("version", xds.CmdVersion, nil); err != nil {
		return cal, err
	}
	if _, err := step("cmd30", CmdUnknown30, argUnk30); err != nil {
		return cal, err
	}
	if _, err := step("cmd31", CmdUnknown31, argUnk31); err != nil {
		return cal, err
	}
	if _, err := step("config", CmdStreamConfig, argConfig); err != nil {
		return cal, err
	}
	dst := [3]*Calibration{&cal.Range0, &cal.Range4, &cal.Range1}
	for i, r := range calRanges {
		var a [2]byte
		binary.LittleEndian.PutUint16(a[:], r)
		d, err := step(fmt.Sprintf("calibration range %d", r), CmdCalibration, a[:])
		if err != nil {
			return cal, err
		}
		if *dst[i], err = ParseCalibration(d); err != nil {
			return cal, err
		}
	}
	return cal, nil
}

// Begin sends the command that starts the stream. It is the last command of
// the sequence and the point from which sample time is counted.
func Begin(c Commander) error {
	if _, err := c.Do(CmdStreamStart, nil); err != nil {
		return fmt.Errorf("energytrace stream-start (cmd %#02x): %w", CmdStreamStart, err)
	}
	return nil
}

// Start runs [Prepare] and [Begin].
func Start(c Commander) (Calibrations, error) {
	cal, err := Prepare(c)
	if err != nil {
		return cal, err
	}
	return cal, Begin(c)
}

func tolerant(c Commander, cmd byte, what string) error {
	_, err := c.Do(cmd, nil)
	var xe *xds.Error
	if errors.As(err, &xe) && xe.Status == StatusAlreadyStopped {
		return nil
	}
	if err != nil {
		return fmt.Errorf("energytrace %s (cmd %#02x): %w", what, cmd, err)
	}
	return nil
}

// StopStream asks the probe to stop sampling. A repeated stop is not an error.
func StopStream(c Commander) error { return tolerant(c, CmdStreamStop, "stop") }

// ReleaseStream is the second half of the stop pair. The vendor tool sends it
// about a second after StopStream, after draining the data endpoint.
func ReleaseStream(c Commander) error { return tolerant(c, CmdStreamFree, "release") }

// Disconnect ends the command session, as the vendor tool does last.
func Disconnect(c Commander) error {
	if _, err := c.Do(xds.CmdDisconnect, nil); err != nil {
		return fmt.Errorf("energytrace disconnect: %w", err)
	}
	return nil
}
