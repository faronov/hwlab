package can

import (
	"encoding/binary"
	"fmt"
)

// Kernel frame sizes: struct can_frame and struct canfd_frame.
const (
	KernelClassicSize = 16
	KernelFDSize      = 72
)

// KernelBytes returns the SocketCAN struct can_frame / canfd_frame of f. The
// identifier word is in host byte order, unlike the network-order pcap layout of
// [Frame.Encode].
func (f Frame) KernelBytes() []byte {
	id := f.ID
	if f.Extended {
		id |= flagEFF
	}
	if f.RTR {
		id |= flagRTR
	}
	if f.Error {
		id |= flagERR
	}
	size := KernelClassicSize
	if f.FD {
		size = KernelFDSize
	}
	b := make([]byte, size)
	binary.NativeEndian.PutUint32(b, id)
	b[4] = byte(f.Len())
	if f.FD {
		b[5] = fdFDF
		if f.BRS {
			b[5] |= fdBRS
		}
		if f.ESI {
			b[5] |= fdESI
		}
	}
	copy(b[8:], f.Data)
	return b
}

// FromKernel parses a struct can_frame (16 bytes) or canfd_frame (72 bytes) as
// returned by a SocketCAN raw socket. Interface, Own and TimestampNs are left
// for the caller.
func FromKernel(b []byte) (Frame, error) {
	switch len(b) {
	case KernelClassicSize, KernelFDSize:
	default:
		return Frame{}, fmt.Errorf("kernel CAN frame of %d bytes (want %d or %d)", len(b), KernelClassicSize, KernelFDSize)
	}
	id := binary.NativeEndian.Uint32(b)
	n := int(b[4])
	f := Frame{
		Extended: id&flagEFF != 0,
		RTR:      id&flagRTR != 0,
		Error:    id&flagERR != 0,
		FD:       len(b) == KernelFDSize,
	}
	if f.Extended || f.Error {
		f.ID = id & maskEFF
	} else {
		f.ID = id & maskSFF
	}
	if f.FD {
		f.BRS = b[5]&fdBRS != 0
		f.ESI = b[5]&fdESI != 0
		f.RTR = false
	}
	maxLen := MaxClassic
	if f.FD {
		maxLen = MaxFD
	}
	if f.RTR {
		if n > MaxClassic {
			return Frame{}, fmt.Errorf("remote frame with DLC %d", n)
		}
		f.DLC = uint8(n)
		return f, nil
	}
	if n > maxLen {
		return Frame{}, fmt.Errorf("kernel CAN frame announces %d payload bytes, at most %d fit", n, maxLen)
	}
	f.Data = append([]byte(nil), b[8:8+n]...)
	return f, nil
}
