//go:build hardware && (linux || darwin)

package wstk

import (
	"context"
	"fmt"
	"os"
	"time"
)

type traceConn struct {
	KitConn
	t0 time.Time
}

func short(b []byte) string {
	if len(b) > 24 {
		return fmt.Sprintf("%x...(%d)", b[:24], len(b))
	}
	return fmt.Sprintf("%x", b)
}

func (c traceConn) Exchange(ctx context.Context, cmd []byte, n int) ([]byte, error) {
	r, err := c.KitConn.Exchange(ctx, cmd, n)
	fmt.Fprintf(os.Stderr, "%8.3f X  %s want %d -> %s %v\n", time.Since(c.t0).Seconds()*1000, short(cmd), n, short(r), err)
	return r, err
}

func (c traceConn) ExchangeUpTo(ctx context.Context, cmd []byte, n int) ([]byte, error) {
	r, err := c.KitConn.ExchangeUpTo(ctx, cmd, n)
	fmt.Fprintf(os.Stderr, "%8.3f XU %s max %d -> %s %v\n", time.Since(c.t0).Seconds()*1000, short(cmd), n, short(r), err)
	return r, err
}

// hwPlugin is the real plugin; HWLAB_WSTK_TRACE=1 logs every exchange.
func hwPlugin() *Plugin {
	env := DefaultEnv(nil)
	if os.Getenv("HWLAB_WSTK_TRACE") == "1" {
		open := env.OpenUSB
		env.OpenUSB = func(k USBKit) (KitConn, error) {
			c, err := open(k)
			if err != nil {
				return nil, err
			}
			return traceConn{c, time.Now()}, nil
		}
	}
	return NewWith(env)
}
