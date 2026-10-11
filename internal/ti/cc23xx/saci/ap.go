package saci

import "github.com/faronov/hwlab/internal/adiv5"

// SecAPIndex is the access port number of the CC23xx SEC-AP (TRM table 5-1).
const SecAPIndex = 2

// APMailbox reaches the SEC-AP through an adiv5 target connected to the
// MEM-AP. It works the same over every probe transport.
type APMailbox struct {
	T  *adiv5.Target
	AP uint8
}

// NewAPMailbox returns the mailbox of the SEC-AP behind t.
func NewAPMailbox(t *adiv5.Target) *APMailbox { return &APMailbox{T: t, AP: SecAPIndex} }

// ReadReg implements Mailbox.
func (m *APMailbox) ReadReg(reg byte) (uint32, error) { return m.T.ReadAPOn(m.AP, reg) }

// WriteReg implements Mailbox.
func (m *APMailbox) WriteReg(reg byte, v uint32) error { return m.T.WriteAPOn(m.AP, reg, v) }

// WriteWords implements Mailbox.
func (m *APMailbox) WriteWords(reg byte, data []uint32) error {
	return m.T.WriteAPBlockOn(m.AP, reg, data)
}
