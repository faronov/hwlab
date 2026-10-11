package dap

import (
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/adiv5"
)

// AttachOptions configure [Attach].
type AttachOptions struct {
	ClockHz uint32 // SWD clock; default 1 MHz
	AP      uint8  // MEM-AP index; default 0
}

// Attach connects the probe to the target over SWD (DAP_Connect, clock,
// transfer configuration) and brings the ADIv5 debug port up. It does not
// halt, reset or write the target. Release the pins with [Probe.Disconnect].
func Attach(p *Probe, o AttachOptions) (*adiv5.Target, error) {
	if err := AttachLink(p, o); err != nil {
		return nil, err
	}
	t, err := adiv5.Connect(p, o.AP)
	if err != nil {
		_ = p.Disconnect()
		return nil, err
	}
	return t, nil
}

// AttachLink does the probe side of Attach: DAP_Connect(SWD), the clock and
// the transfer configuration. The caller brings the debug port up on the probe
// itself (a target that needs a different bring-up) and releases the pins with
// [Probe.Disconnect].
func AttachLink(p *Probe, o AttachOptions) error {
	if !p.info.SupportsSWD() {
		return errors.New("the probe does not report SWD support (JTAG-only probes are not supported)")
	}
	if o.ClockHz == 0 {
		o.ClockHz = 1_000_000
	}
	port, err := p.Connect(PortSWD)
	if err != nil {
		return err
	}
	if port != PortSWD {
		_ = p.Disconnect()
		return fmt.Errorf("the probe connected port %d, not SWD", port)
	}
	ok := false
	defer func() {
		if !ok {
			_ = p.Disconnect()
		}
	}()
	if err := p.SetClock(o.ClockHz); err != nil {
		return err
	}
	if err := p.Configure(0, 100, 0); err != nil {
		return err
	}
	if err := p.SWDConfigure(0); err != nil {
		return err
	}
	ok = true
	return nil
}
