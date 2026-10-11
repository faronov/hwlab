package modbusplugin

import (
	"context"
	"sync"

	"github.com/faronov/hwlab/internal/modbus"
)

// Fake is a SYNTHETIC Modbus RTU slave (unit 1). Its tables are deterministic
// and its behaviour is the simulator's reading of the specification, not
// evidence about any real device:
//
//	holding 0..4     100 200 300 400 500        (writable)
//	holding 10..11   IEEE-754 float32 23.5, big word order
//	input   0        a counter that grows by one per request served
//	input   1        2315  (a "temperature" in 0.01 degC)
//	coil    0..3     true false true false      (writable)
//	discrete 0..3    false true false true
type Fake struct{ Slave *modbus.Slave }

// NewFake builds the slave.
func NewFake() *Fake {
	s := modbus.NewSlave(1, 256)
	s.SetHolding(0, 100, 200, 300, 400, 500)
	s.SetHolding(10, 0x41BC, 0x0000) // 23.5
	s.SetInput(1, 2315)
	s.SetCoil(0, true)
	s.SetCoil(2, true)
	s.SetDiscrete(1, true)
	s.SetDiscrete(3, true)
	s.OnRequest = func(s *modbus.Slave, r *modbus.Request) { s.AddInput(0, 1) }
	return &Fake{Slave: s}
}

// Dial connects a master to the slave through an in-memory serial link.
func (f *Fake) Dial() modbus.Transport {
	master, slave := modbus.NewLink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Slave.ServeRTU(ctx, slave)
	}()
	t := modbus.NewRTU(master, 19200, "fake (in-memory RTU link)")
	return &fakeTransport{RTU: t, stop: func() {
		cancel()
		_ = slave.Close()
		<-done
	}}
}

type fakeTransport struct {
	*modbus.RTU
	once sync.Once
	stop func()
}

func (t *fakeTransport) Close() error {
	err := t.RTU.Close()
	t.once.Do(t.stop)
	return err
}
