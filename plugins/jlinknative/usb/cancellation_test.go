package usb

import (
	"context"
	"errors"
	"testing"
)

func TestCancellationIsNotProtocolError(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := mapUSB("bulk in", err); !errors.Is(got, err) || errors.Unwrap(got) != nil {
			t.Fatalf("cancellation reclassified: %v", got)
		}
		c := &Conn{}
		c.afterError(err)
		if !c.dirty {
			t.Fatal("cancelled exchange may leave a reply pending; next exchange must drain")
		}
	}
}
