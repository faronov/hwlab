package nrf802154

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/decode"
)

const defaultDecodeLimit = 200

// decodePcap runs tshark over a capture artifact and returns the layer
// histogram plus the first `limit` per-packet summaries.
func (p *Plugin) decodePcap(ctx context.Context, args map[string]any) (map[string]any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "path is required")
	}
	limit := defaultDecodeLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	sums, err := p.dec.Summaries(ctx, path)
	if err != nil {
		if errors.Is(err, decode.ErrUnavailable) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, err
	}
	layers := map[string]any{}
	var rows []any
	for i, s := range sums {
		top := s.Protocols[strings.LastIndexByte(s.Protocols, ':')+1:]
		n, _ := layers[top].(float64)
		layers[top] = n + 1
		if i < limit {
			rows = append(rows, map[string]any{"frame": float64(s.Frame), "layers": s.Protocols, "info": s.Info})
		}
	}
	return map[string]any{"frames": float64(len(sums)), "top_layer_counts": layers, "packets": rows, "truncated": len(sums) > limit}, nil
}
