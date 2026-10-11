package app

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/faronov/hwlab/internal/modbus"
	"github.com/faronov/hwlab/internal/timeline"
)

const (
	defaultModbusLimit = 100
	maxModbusLimit     = 1000
)

// ModbusQuery asks for the Modbus transactions in the raw serial traffic a
// session recorded with the uart plugin.
type ModbusQuery struct {
	Session string
	Source  string // uart device alias; empty = every source that has serial packets
	Mode    string // master (default), slave or sniff: what the capturing host was
	GapNs   int64  // idle gap that ends a frame (default 50 ms)
	FromNs  int64
	ToNs    int64 // <= FromNs: to the end
	Limit   int   // transactions listed per source (default 100, max 1000)
}

// ModbusStream is the analysis of one serial source.
type ModbusStream struct {
	Source       string               `json:"source"`
	Mode         string               `json:"mode"`
	Chunks       int                  `json:"chunks"`
	Stats        modbus.Stats         `json:"stats"`
	Transactions []modbus.Transaction `json:"transactions"`
	// Truncated is set when more transactions exist than were listed.
	Truncated bool `json:"truncated,omitempty"`
}

// ModbusReport is the result of DecodeModbus.
type ModbusReport struct {
	Session string         `json:"session"`
	Streams []ModbusStream `json:"streams"`
	Note    string         `json:"note"`
}

const modbusNote = "Decoded offline from recorded serial bytes. Latencies are differences of host timestamps (a tx chunk is stamped before the write call, an rx chunk when the read returned), not wire times; bytes that fit no frame are counted as junk, and a frame with a bad CRC is reported, never repaired."

// DecodeModbus decodes Modbus RTU from the serial packets of a session. It
// works on recorded data and on replays, with no hardware.
func (a *App) DecodeModbus(q ModbusQuery) (*ModbusReport, error) {
	switch q.Mode {
	case "":
		q.Mode = modbus.ModeMaster
	case modbus.ModeMaster, modbus.ModeSlave, modbus.ModeSniff:
	default:
		return nil, fmt.Errorf("mode %q: want master, slave or sniff", q.Mode)
	}
	if q.Limit <= 0 {
		q.Limit = defaultModbusLimit
	}
	if q.Limit > maxModbusLimit {
		q.Limit = maxModbusLimit
	}
	tq := TimelineQuery{Session: q.Session, FromNs: q.FromNs, ToNs: q.ToNs, Types: []string{timeline.TypePacket}, Limit: 1 << 30}
	if q.Source != "" {
		tq.Sources = []string{q.Source}
	}
	evs, err := a.QueryTimeline(tq)
	if err != nil {
		return nil, err
	}
	bySource := map[string][]modbus.Chunk{}
	for _, e := range evs {
		if e.Data["protocol"] != "uart" {
			continue
		}
		dir, _ := e.Data["attr.dir"].(string)
		h, _ := e.Data["hex"].(string)
		b, err := hex.DecodeString(h)
		if err != nil || len(b) == 0 || (dir != "tx" && dir != "rx") {
			continue
		}
		bySource[e.SourceID] = append(bySource[e.SourceID], modbus.Chunk{TimeNs: e.TimestampNs, Dir: dir, Data: b})
	}
	if len(bySource) == 0 {
		return nil, fmt.Errorf("session has no serial (uart) packets to decode; record the bus with the uart plugin, or read the modbus plugin's own transactions (timeline type %q)", timeline.TypeDecoded)
	}
	names := make([]string, 0, len(bySource))
	for s := range bySource {
		names = append(names, s)
	}
	sort.Strings(names)
	rep := &ModbusReport{Session: q.Session, Note: modbusNote}
	for _, s := range names {
		an := modbus.Analyze(bySource[s], modbus.Options{Mode: q.Mode, GapNs: q.GapNs})
		st := ModbusStream{Source: s, Mode: an.Mode, Chunks: len(bySource[s]), Stats: an.Stats, Transactions: an.Transactions}
		if len(st.Transactions) > q.Limit {
			st.Transactions, st.Truncated = st.Transactions[:q.Limit], true
		}
		rep.Streams = append(rep.Streams, st)
	}
	return rep, nil
}
