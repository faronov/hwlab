//go:build !linux && !darwin

package uart

import "errors"

func applyFlow(any, string) error {
	return errors.New("flow control is not supported on this operating system")
}

func flowSupported() bool { return false }
