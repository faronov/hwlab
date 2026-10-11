package usbcapplugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/decode"
	"github.com/faronov/hwlab/internal/pcap"
	"github.com/faronov/hwlab/internal/usbcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

func synthPlugin() *Plugin {
	return NewWith(func() []Source { return []Source{{ID: SynthID, Backend: "synthetic", Synthetic: true, Reports: 20}} })
}

func runCapture(t *testing.T, p *Plugin, id string, cfg map[string]any, dir string, stop func(cancel context.CancelFunc)) ([]*pluginv1.Event, *pluginv1.StopCaptureResponse) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := pluginsdk.NewTestSink(256)
	c, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, CaptureID: "c1", Config: cfg, ArtifactDir: dir, Clock: pluginsdk.NewClock(5_000)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].StreamId != StreamPackets || start.Streams[0].Kind != "packet" {
		t.Fatalf("streams %+v", start.Streams)
	}
	if stop != nil {
		stop(cancel)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	return sink.Drain(), c.Result()
}

func TestDevicesAndInfo(t *testing.T) {
	p := synthPlugin()
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Id != SynthID || devs[0].Properties["synthetic"] != "true" || devs[0].Properties["live"] != "false" {
		t.Fatalf("%+v %v", devs, err)
	}
	caps := map[string]bool{}
	for _, c := range devs[0].Capabilities {
		caps[c.Id] = true
	}
	if !caps[pluginv1.CapUSBPacketCapture] || !caps[pluginv1.CapTimedCapture] || !caps[pluginv1.CapArtifactExport] {
		t.Errorf("caps %v", caps)
	}
	res := invoke(t, p, SynthID, "info", nil)
	if res["backend"] != "synthetic" || res["synthetic"] != true || res["capturing"] != false || res["snaplen"] != float64(defaultSnaplen) {
		t.Errorf("%v", res)
	}
	if _, err := p.find("nope"); status.Code(err) != codes.NotFound {
		t.Errorf("%v", err)
	}
	if err := p.OpenDevice(context.Background(), "nope", nil); status.Code(err) != codes.NotFound {
		t.Errorf("%v", err)
	}
	for _, bad := range []map[string]any{{"speed": 0.0}, {"speed": "fast"}, {"snaplen": -1.0}, {"snaplen": 1.5}, {"snaplen": 70000.0}, {"bogus": 1.0}} {
		if err := p.OpenDevice(context.Background(), SynthID, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v accepted: %v", bad, err)
		}
	}
}

func invoke(t *testing.T, p *Plugin, id, method string, args map[string]any) map[string]any {
	t.Helper()
	res, err := tryInvoke(p, id, method, args)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func tryInvoke(p *Plugin, id, method string, args map[string]any) (map[string]any, error) {
	s, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: pluginv1.CapUSBPacketCapture, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func TestSyntheticCaptureEventsAndPcapng(t *testing.T) {
	p := synthPlugin()
	dir := t.TempDir()
	want := usbcap.Synthetic(20)
	evs, res := runCapture(t, p, SynthID, map[string]any{"speed": 500.0}, dir, nil)
	if len(evs) != len(want) {
		t.Fatalf("%d events, want %d", len(evs), len(want))
	}
	var pkts []usbcap.Packet
	var prev int64
	for i, e := range evs {
		pk := e.GetPacket()
		if pk.Protocol != usbcap.Protocol || pk.LinkType != usbcap.LinkType || pk.Summary == "" || pk.Attributes["device"] == "" {
			t.Fatalf("event %d: %+v", i, pk)
		}
		if e.TimestampNs < prev || e.TimestampNs < 5_000 {
			t.Fatalf("event %d timestamp %d (prev %d)", i, e.TimestampNs, prev)
		}
		prev = e.TimestampNs
		d, err := usbcap.Decode(pk.Data)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, d)
	}
	a := usbcap.Analyze(pkts)
	if a.Stats.Transactions != len(want)/2 || a.Stats.Errors != 2 || len(a.Devices) != 3 {
		t.Errorf("analysis of the events %+v", a.Stats)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Kind != "pcap" || res.Artifacts[0].Metadata["synthetic"] != "true" ||
		res.Artifacts[0].Metadata["packets"] != fmt.Sprint(len(want)) {
		t.Fatalf("artifacts %+v", res.Artifacts)
	}
	if res.Stats.Events != uint64(len(want)) || res.Stats.DroppedBatches != 0 {
		t.Errorf("stats %+v", res.Stats)
	}
	if res.Clock == nil || !strings.Contains(res.Clock.Note, "synthetic") {
		t.Errorf("clock %+v", res.Clock)
	}
	f, err := os.Open(res.Artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ng, err := pcap.ReadNG(f)
	if err != nil {
		t.Fatal(err)
	}
	if ng.LinkType != usbcap.LinkType || ng.Received != uint64(len(want)) {
		t.Errorf("pcapng link type %d, received %d", ng.LinkType, ng.Received)
	}
	for _, pk := range ng.Packets {
		if _, err := usbcap.Decode(pk.Data); err != nil {
			t.Fatal(err)
		}
	}
	if len(ng.Packets) != len(want) {
		t.Errorf("pcapng holds %d packets, want %d", len(ng.Packets), len(want))
	}
}

func TestSnaplenKeepsHeadersOnly(t *testing.T) {
	p := synthPlugin()
	evs, res := runCapture(t, p, SynthID, map[string]any{"speed": 500.0, "snaplen": 0.0}, t.TempDir(), nil)
	for _, e := range evs {
		d, err := usbcap.Decode(e.GetPacket().Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Data) != 0 {
			t.Fatalf("payload kept with snaplen 0: %+v", d)
		}
	}
	if res.Artifacts[0].Metadata["truncated_by_snaplen"] == "0" {
		t.Errorf("%v", res.Artifacts[0].Metadata)
	}
	// The URB length survives truncation.
	var maxLen uint32
	for _, e := range evs {
		d, _ := usbcap.Decode(e.GetPacket().Data)
		if d.Length > maxLen {
			maxLen = d.Length
		}
	}
	if maxLen != 512 {
		t.Errorf("URB length lost: %d", maxLen)
	}
}

func writeText(t *testing.T, path string, pk []usbcap.Packet, extra ...string) {
	t.Helper()
	var sb strings.Builder
	for _, x := range pk {
		sb.WriteString(usbcap.FormatText(x) + "\n")
	}
	for _, x := range extra {
		sb.WriteString(x + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTextFileSourceEndsAtEOFAndCountsBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mon.txt")
	writeText(t, path, usbcap.Synthetic(4), "garbage line", "")
	p := NewWith(func() []Source { return []Source{{ID: "rec", Backend: "file", Path: path}} })
	done := make(chan struct{})
	var evs []*pluginv1.Event
	var res *pluginv1.StopCaptureResponse
	go func() {
		defer close(done)
		evs, res = runCapture(t, p, "rec", map[string]any{"speed": 500.0}, t.TempDir(), nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a recorded file did not end the capture at EOF")
	}
	if len(evs) != len(usbcap.Synthetic(4)) {
		t.Errorf("%d events", len(evs))
	}
	m := res.Artifacts[0].Metadata
	if m["skipped_lines"] != "1" || m["reached_end_of_source"] != "true" || m["backend"] != "file" || m["synthetic"] != "" {
		t.Errorf("%v", m)
	}
	if !strings.Contains(res.Clock.Note, "offline source") {
		t.Errorf("clock %+v", res.Clock)
	}
	// The 32-byte text limit cuts the long configuration descriptors; the analysis says so.
	var pk []usbcap.Packet
	for _, e := range evs {
		d, _ := usbcap.Decode(e.GetPacket().Data)
		pk = append(pk, d)
	}
	a := usbcap.Analyze(pk)
	if a.Stats.Errors != 1 { // the stalled GET_PROTOCOL; reports/2 error needs >4 reports
		t.Errorf("%+v", a.Stats)
	}
}

func TestPeek(t *testing.T) {
	p := synthPlugin()
	res := invoke(t, p, SynthID, "peek", map[string]any{"max_packets": 5.0})
	list, _ := res["packets"].([]any)
	if len(list) != 5 || res["count"] != float64(5) {
		t.Fatalf("%v", res)
	}
	if s := list[0].(map[string]any)["summary"].(string); !strings.Contains(s, "GET_DESCRIPTOR(DEVICE)") {
		t.Errorf("%q", s)
	}
	for _, bad := range []map[string]any{{"max_packets": 0.0}, {"max_packets": 1000.0}, {"timeout_ms": 100000.0}, {"bogus": 1.0}} {
		if _, err := tryInvoke(p, SynthID, "peek", bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := tryInvoke(p, SynthID, "nope", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("%v", err)
	}
}

func TestSecondCaptureAndPeekDuringCapture(t *testing.T) {
	p := synthPlugin()
	if err := p.OpenDevice(context.Background(), SynthID, map[string]any{"speed": 0.5}); err != nil { // slow: stays running
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: SynthID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: SynthID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	if _, err := tryInvoke(p, SynthID, "peek", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("peek during capture: %v", err)
	}
	if r := invoke(t, p, SynthID, "info", nil); r["capturing"] != true {
		t.Errorf("%v", r)
	}
	start := time.Now()
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
	if err := p.CloseDevice(context.Background(), SynthID); err != nil {
		t.Fatal(err)
	}
}

func TestFifoSourceCancelsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mon.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	// Keep a writer open so the reader blocks instead of seeing EOF.
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	pk := usbcap.Synthetic(2)[:4]
	p := NewWith(func() []Source { return []Source{{ID: "live", Backend: "usbmon", Path: path, Live: true}} })
	var evs []*pluginv1.Event
	var res *pluginv1.StopCaptureResponse
	start := time.Now()
	evs, res = runCapture(t, p, "live", nil, t.TempDir(), func(cancel context.CancelFunc) {
		for _, x := range pk {
			if _, err := w.WriteString(usbcap.FormatText(x) + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	})
	if time.Since(start) > 3*time.Second {
		t.Errorf("a blocked read delayed cancellation: %v", time.Since(start))
	}
	if len(evs) != 4 || res.Artifacts[0].Metadata["reached_end_of_source"] != "false" {
		t.Errorf("%d events, %v", len(evs), res.Artifacts[0].Metadata)
	}
	if !strings.Contains(res.Clock.Source, "usbmon kernel timestamps") {
		t.Errorf("%+v", res.Clock)
	}
}

func TestMissingSourceFileIsUnavailable(t *testing.T) {
	p := NewWith(func() []Source { return []Source{{ID: "gone", Backend: "file", Path: "/nonexistent/usbmon"}} })
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "gone", Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("%v", err)
	}
}

func TestHostSources(t *testing.T) {
	got := hostSources(true, "a=/tmp/x, b = /tmp/y ,junk,=,c=")
	ids := map[string]Source{}
	for _, s := range got {
		ids[s.ID] = s
	}
	if !ids[SynthID].Synthetic || ids["a"].Path != "/tmp/x" || ids["a"].Live || ids["b"].Path != "/tmp/y" {
		t.Errorf("%+v", got)
	}
	if _, ok := ids["junk"]; ok || len(ids["c"].ID) != 0 {
		t.Errorf("malformed entries accepted: %+v", got)
	}
	if len(hostSources(false, "")) > 0 {
		for _, s := range hostSources(false, "") {
			if s.Backend != "usbmon" || !s.Live {
				t.Errorf("%+v", s)
			}
		}
	}
}

// Wireshark must decode the artifact natively; skipped without tshark.
func TestPcapngDecodesInTshark(t *testing.T) {
	ts := &decode.Tshark{}
	if _, err := ts.Available(); err != nil {
		t.Skip("tshark not installed")
	}
	_, res := runCapture(t, synthPlugin(), SynthID, map[string]any{"speed": 500.0}, t.TempDir(), nil)
	sums, err := ts.Summaries(context.Background(), res.Artifacts[0].Path)
	if err != nil || len(sums) != len(usbcap.Synthetic(20)) {
		t.Fatalf("tshark: %d packets, %v", len(sums), err)
	}
	var enumeration, hid bool
	for i, s := range sums {
		if !strings.HasPrefix(s.Protocols, "usb") {
			t.Errorf("packet %d dissected as %q", i+1, s.Protocols)
		}
		enumeration = enumeration || strings.Contains(s.Info, "GET DESCRIPTOR")
		hid = hid || strings.Contains(s.Protocols, "usbhid")
	}
	if !enumeration {
		t.Errorf("no GET DESCRIPTOR in tshark's view: %+v", sums[:3])
	}
	_ = hid
}
