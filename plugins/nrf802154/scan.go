package nrf802154

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

const (
	defaultDwell      = time.Second
	defaultMinPackets = 3
	maxDwell          = 60 * time.Second
)

// ParseChannels parses "11-26", "15,20,25" or "11-14,20"; empty means all.
func ParseChannels(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "all" {
		out := make([]int, 0, MaxChannel-MinChannel+1)
		for c := MinChannel; c <= MaxChannel; c++ {
			out = append(out, c)
		}
		return out, nil
	}
	seen := map[int]bool{}
	var out []int
	add := func(c int) error {
		if c < MinChannel || c > MaxChannel {
			return fmt.Errorf("channel %d out of range [%d,%d]", c, MinChannel, MaxChannel)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad channel %q", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || b < a {
				return nil, fmt.Errorf("bad channel range %q", part)
			}
		}
		for c := a; c <= b; c++ {
			if err := add(c); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

type chanStats struct {
	packets          int
	rssiMin, rssiMax int
	rssiSum, lqiSum  int
	elapsed          time.Duration
}

// scan dwells on each channel and reports activity. It listens only; no
// frames are transmitted.
func (p *Plugin) scan(ctx context.Context, deviceID string, args map[string]any) (_ map[string]any, retErr error) {
	spec, _ := args["channels"].(string)
	if f, ok := args["channels"].(float64); ok {
		spec = strconv.Itoa(int(f))
	}
	chans, err := ParseChannels(spec)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	dwell := defaultDwell
	if v, ok := args["dwell_ms"].(float64); ok && v > 0 {
		dwell = time.Duration(v * float64(time.Millisecond))
	}
	if dwell > maxDwell {
		return nil, status.Errorf(codes.InvalidArgument, "dwell_ms must be at most %d", maxDwell.Milliseconds())
	}
	minPk := defaultMinPackets
	if v, ok := args["min_packets"].(float64); ok && v >= 1 {
		minPk = int(v)
	}
	port, err := p.resolvePort(deviceID, args)
	if err != nil {
		return nil, err
	}
	release, err := p.acquire(port)
	if err != nil {
		return nil, err
	}
	defer release()
	dev, err := p.openSniffer(port)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, dev.Close()) }()

	started := time.Now()
	results := make([]any, 0, len(chans))
	var active []any
	for i, ch := range chans {
		if i == 0 {
			err = dev.Start(ctx, ch)
		} else {
			err = dev.Retune(ctx, ch)
		}
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "scan channel %d: %v", ch, err)
		}
		st := chanStats{rssiMin: math.MaxInt, rssiMax: math.MinInt}
		dctx, cancel := context.WithTimeout(ctx, dwell)
		t0 := time.Now()
		rerr := dev.Run(dctx, func(f protocol.Frame, _ time.Time) {
			st.packets++
			st.rssiSum += f.RSSI
			st.lqiSum += f.LQI
			st.rssiMin = min(st.rssiMin, f.RSSI)
			st.rssiMax = max(st.rssiMax, f.RSSI)
		}, nil)
		cancel()
		st.elapsed = time.Since(t0)
		if rerr != nil {
			return nil, status.Errorf(codes.Unavailable, "scan channel %d: %v", ch, rerr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := map[string]any{"channel": float64(ch), "packets": float64(st.packets),
			"packets_per_s": round(float64(st.packets)/st.elapsed.Seconds(), 2)}
		if st.packets > 0 {
			row["rssi_min_dbm"] = float64(st.rssiMin)
			row["rssi_max_dbm"] = float64(st.rssiMax)
			row["rssi_mean_dbm"] = round(float64(st.rssiSum)/float64(st.packets), 1)
			row["lqi_mean"] = round(float64(st.lqiSum)/float64(st.packets), 1)
		}
		likely := st.packets >= minPk
		row["likely_active"] = likely
		if likely {
			active = append(active, float64(ch))
		}
		results = append(results, row)
	}
	return map[string]any{
		"channels": results, "likely_active": active, "dwell_ms": float64(dwell.Milliseconds()),
		"min_packets": float64(minPk), "total_ms": float64(time.Since(started).Milliseconds()),
		"note": "likely_active means at least min_packets frames were heard while dwelling; it says nothing about network identity",
	}, nil
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
