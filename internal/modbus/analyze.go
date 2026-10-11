package modbus

import (
	"fmt"
	"sort"
)

// Chunk is a timestamped run of bytes seen on a serial line, as the uart
// plugin records them. Dir is "tx" (written by the capturing host) or "rx".
type Chunk struct {
	TimeNs int64
	Dir    string
	Data   []byte
}

// Analysis modes: what the capturing host was on the bus.
const (
	// ModeMaster: tx bytes are requests, rx bytes are responses.
	ModeMaster = "master"
	// ModeSlave: rx bytes are requests, tx bytes are responses.
	ModeSlave = "slave"
	// ModeSniff: both directions carry both kinds of frames (a tap on the bus),
	// told apart by pairing.
	ModeSniff = "sniff"
)

// Transaction statuses.
const (
	StatusOK         = "ok"
	StatusException  = "exception"
	StatusNoResponse = "no_response"
	StatusOrphan     = "orphan_response"
	StatusBroadcast  = "broadcast"
	StatusBadFrame   = "bad_frame"
	StatusBadCRC     = "crc_error"
)

// Transaction is one request with its answer, or a lone frame.
type Transaction struct {
	StartNs int64 `json:"t_ns"`
	// LatencyNs is response chunk time minus request chunk time. It is a
	// difference of host chunk stamps, not a wire time: a tx stamp is taken
	// before the write and an rx stamp after the read returns.
	LatencyNs int64     `json:"latency_ns,omitempty"`
	Status    string    `json:"status"`
	Unit      int       `json:"unit"`
	Function  string    `json:"function"`
	Request   *Request  `json:"-"`
	Response  *Response `json:"-"`
	ReqHex    string    `json:"request_hex,omitempty"`
	RespHex   string    `json:"response_hex,omitempty"`
	Summary   string    `json:"summary"`
	Note      string    `json:"note,omitempty"`
}

// Stats counts what the analysis saw.
type Stats struct {
	Frames       int `json:"frames"`
	Transactions int `json:"transactions"`
	OK           int `json:"ok"`
	Exceptions   int `json:"exceptions"`
	NoResponse   int `json:"no_response"`
	Orphans      int `json:"orphan_responses"`
	CRCErrors    int `json:"crc_errors"`
	BadFrames    int `json:"bad_frames"`
	JunkBytes    int `json:"junk_bytes"`
}

// Analysis is the decoded view of a recorded serial stream.
type Analysis struct {
	Mode         string        `json:"mode"`
	Transactions []Transaction `json:"-"`
	Stats        Stats         `json:"stats"`
}

// Options tune Analyze.
type Options struct {
	Mode string // default ModeMaster
	// GapNs: bytes of one direction separated by more than this are different
	// frames; a partial frame left behind is junk. Default 50 ms.
	GapNs int64
}

type seen struct {
	t      int64
	frame  RTUFrame
	dir    string
	forced Role // RoleAny when the frame may be either
}

// Analyze cuts recorded chunks into RTU frames and pairs requests with
// responses. It never invents data: bytes that fit no frame are counted as
// junk and frames with a bad CRC are reported as such.
func Analyze(chunks []Chunk, o Options) *Analysis {
	if o.Mode == "" {
		o.Mode = ModeMaster
	}
	if o.GapNs <= 0 {
		o.GapNs = 50_000_000
	}
	a := &Analysis{Mode: o.Mode}

	roleOf := func(dir string) Role {
		switch o.Mode {
		case ModeMaster:
			if dir == "tx" {
				return RoleRequest
			}
			return RoleResponse
		case ModeSlave:
			if dir == "rx" {
				return RoleRequest
			}
			return RoleResponse
		}
		return RoleAny
	}

	sorted := append([]Chunk(nil), chunks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TimeNs < sorted[j].TimeNs })

	type stream struct {
		buf     []byte
		startNs []int64 // time of the chunk each byte of buf came from
		lastNs  int64
	}
	streams := map[string]*stream{}
	var frames []seen

	flush := func(dir string, st *stream, final bool) {
		role := roleOf(dir)
		fs, rest, junk := ExtractRTU(st.buf, role, final)
		a.Stats.JunkBytes += junk
		for _, f := range fs {
			frames = append(frames, seen{t: st.startNs[f.Offset], frame: f, dir: dir, forced: role})
		}
		consumed := len(st.buf) - len(rest)
		st.buf = append([]byte(nil), rest...)
		st.startNs = append([]int64(nil), st.startNs[consumed:]...)
	}

	for _, c := range sorted {
		if len(c.Data) == 0 {
			continue
		}
		st := streams[c.Dir]
		if st == nil {
			st = &stream{}
			streams[c.Dir] = st
		}
		if len(st.buf) > 0 && c.TimeNs-st.lastNs > o.GapNs {
			flush(c.Dir, st, true)
		}
		for range c.Data {
			st.startNs = append(st.startNs, c.TimeNs)
		}
		st.buf = append(st.buf, c.Data...)
		st.lastNs = c.TimeNs
		flush(c.Dir, st, false)
	}
	for dir, st := range streams {
		if len(st.buf) > 0 {
			flush(dir, st, true)
		}
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].t < frames[j].t })
	a.Stats.Frames = len(frames)

	var pending *Transaction
	closePending := func() {
		if pending == nil {
			return
		}
		if pending.Status == "" {
			pending.Status = StatusNoResponse
			if pending.Unit == 0 {
				pending.Status = StatusBroadcast
			}
		}
		a.add(*pending)
		pending = nil
	}
	for _, s := range frames {
		f := s.frame
		if f.BadCRC {
			closePending()
			a.add(Transaction{StartNs: s.t, Status: StatusBadCRC, Unit: int(f.Unit), Function: funcOfPDU(f.PDU),
				ReqHex: Hex(f.Raw), Summary: fmt.Sprintf("frame with bad CRC (%d bytes)", len(f.Raw)),
				Note: "length taken from the function code; the CRC does not match"})
			continue
		}
		// A response to the pending request?
		if pending != nil && pending.Status == "" && s.forced != RoleRequest && f.Unit == byte(pending.Unit) {
			if resp, err := ParseResponse(f.Unit, f.PDU, pending.Request); err == nil {
				pending.Response, pending.RespHex = resp, Hex(f.Raw)
				pending.LatencyNs = s.t - pending.StartNs
				pending.Summary = pending.Request.Summary() + " -> " + resp.Summary()
				if resp.Exception != 0 {
					pending.Status = StatusException
				} else {
					pending.Status = StatusOK
				}
				closePending()
				continue
			}
		}
		if s.forced == RoleResponse || (s.forced == RoleAny && f.Role == RoleResponse) {
			closePending()
			resp, err := ParseResponse(f.Unit, f.PDU, nil)
			if err != nil {
				a.add(Transaction{StartNs: s.t, Status: StatusBadFrame, Unit: int(f.Unit), Function: funcOfPDU(f.PDU), RespHex: Hex(f.Raw),
					Summary: "undecodable response", Note: err.Error()})
				continue
			}
			a.add(Transaction{StartNs: s.t, Status: StatusOrphan, Unit: int(f.Unit), Function: FunctionName(resp.Function), Response: resp,
				RespHex: Hex(f.Raw), Summary: resp.Summary(), Note: "no matching request was seen"})
			continue
		}
		// Otherwise a request.
		closePending()
		req, err := ParseRequest(f.Unit, f.PDU)
		if err != nil {
			a.add(Transaction{StartNs: s.t, Status: StatusBadFrame, Unit: int(f.Unit), Function: funcOfPDU(f.PDU), ReqHex: Hex(f.Raw),
				Summary: "undecodable request", Note: err.Error()})
			continue
		}
		pending = &Transaction{StartNs: s.t, Unit: int(req.Unit), Function: FunctionName(req.Function), Request: req,
			ReqHex: Hex(f.Raw), Summary: req.Summary()}
	}
	closePending()
	return a
}

func funcOfPDU(pdu []byte) string {
	if len(pdu) == 0 {
		return ""
	}
	return FunctionName(pdu[0])
}

func (a *Analysis) add(t Transaction) {
	a.Transactions = append(a.Transactions, t)
	a.Stats.Transactions++
	switch t.Status {
	case StatusOK:
		a.Stats.OK++
	case StatusException:
		a.Stats.Exceptions++
	case StatusNoResponse:
		a.Stats.NoResponse++
	case StatusOrphan:
		a.Stats.Orphans++
	case StatusBadCRC:
		a.Stats.CRCErrors++
	case StatusBadFrame:
		a.Stats.BadFrames++
	}
}
