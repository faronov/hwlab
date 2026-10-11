package modbus

import (
	"fmt"
	"math"
)

// Interpret turns registers into numbers. kind is u16, i16, u32, i32 or f32;
// the 32-bit kinds consume two registers per value. wordOrder is "big" (the
// first register holds the high word, the usual "AB CD") or "little" (low word
// first, "CD AB").
func Interpret(regs []uint16, kind, wordOrder string) ([]any, error) {
	if wordOrder == "" {
		wordOrder = "big"
	}
	if wordOrder != "big" && wordOrder != "little" {
		return nil, fmt.Errorf("word_order %q: want big or little", wordOrder)
	}
	switch kind {
	case "", "u16":
		return regsAny(regs), nil
	case "i16":
		out := make([]any, len(regs))
		for i, r := range regs {
			out[i] = int(int16(r))
		}
		return out, nil
	case "u32", "i32", "f32":
		if len(regs)%2 != 0 {
			return nil, fmt.Errorf("%s needs an even number of registers, got %d", kind, len(regs))
		}
		out := make([]any, 0, len(regs)/2)
		for i := 0; i < len(regs); i += 2 {
			hi, lo := regs[i], regs[i+1]
			if wordOrder == "little" {
				hi, lo = lo, hi
			}
			v := uint32(hi)<<16 | uint32(lo)
			switch kind {
			case "u32":
				out = append(out, float64(v))
			case "i32":
				out = append(out, float64(int32(v)))
			default:
				f := math.Float32frombits(v)
				if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
					out = append(out, nil) // JSON has no NaN or Inf
				} else {
					out = append(out, float64(f))
				}
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("type %q: want u16, i16, u32, i32 or f32", kind)
}
