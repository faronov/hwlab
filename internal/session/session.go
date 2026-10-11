// Package session implements the directory-based session format:
//
//	<id>/
//	  session.json          metadata (devices, captures, streams, experiment)
//	  timeline.jsonl        low-rate events, one JSON object per line
//	  captures/<cap>/*.hwcap  binary sample chunks (power/analog/digital)
//	  artifacts/            plugin and generated files + index.json
//	  plots/                rendered plots
//	  devices/              reserved for per-device config snapshots
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/artifact"
	"github.com/faronov/hwlab/internal/timeline"
)

// FormatVersion is the on-disk session format version.
const FormatVersion = 1

// Session statuses.
const (
	StatusRunning  = "running"
	StatusComplete = "complete"
	StatusFailed   = "failed"
)

// Meta is the content of session.json.
type Meta struct {
	FormatVersion int       `json:"format_version"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	// WallEpochUnixNs is wall-clock metadata for timeline t=0. Never used for
	// ordering; the monotonic timeline is authoritative.
	WallEpochUnixNs int64         `json:"wall_epoch_unix_ns"`
	DurationNs      int64         `json:"duration_ns"`
	HwlabVersion    string        `json:"hwlab_version,omitempty"`
	Experiment      string        `json:"experiment,omitempty"`
	Devices         []DeviceMeta  `json:"devices"`
	Captures        []CaptureMeta `json:"captures"`
	// ClockCalibrations are clock mappings MEASURED from edges that two devices
	// saw simultaneously. They are evidence stored next to the data; the
	// timeline itself keeps the timestamps as recorded.
	ClockCalibrations []ClockCalibration `json:"clock_calibrations,omitempty"`
	Error             string             `json:"error,omitempty"`
}

// ClockCalibration maps the session timestamps of one device onto those of a
// reference device: reference = RefSessionNs + Rate*(t - RefDeviceNs).
type ClockCalibration struct {
	Alias     string `json:"alias"`
	Reference string `json:"reference"`
	Method    string `json:"method"`
	Signal    string `json:"signal,omitempty"`
	Pairs     int    `json:"pairs"`
	Rejected  int    `json:"rejected,omitempty"`
	SpanNs    int64  `json:"span_ns"`

	RefDeviceNs  int64   `json:"ref_device_ns"`
	RefSessionNs int64   `json:"ref_session_ns"`
	Rate         float64 `json:"rate"`

	InitialOffsetNs  int64   `json:"initial_offset_ns"`
	DriftPPM         float64 `json:"drift_ppm"`
	DriftStdErrPPM   float64 `json:"drift_stderr_ppm"`
	ResidualMedianNs int64   `json:"residual_median_ns"`
	ResidualP95Ns    int64   `json:"residual_p95_ns"`
	ResidualMaxNs    int64   `json:"residual_max_ns"`
	// UncertaintyNs is the claimed +/- bound of a mapped timestamp: the worst
	// residual plus half of each clock's timestamp resolution. Delays common to
	// both devices (and the skew between the two physical probe points) cannot
	// be seen by this method and are not included.
	UncertaintyNs int64 `json:"uncertainty_ns"`
	// OffsetOnlyP95Ns is the p95 residual a fixed offset would leave over the
	// same data (what the rate term corrects).
	OffsetOnlyP95Ns int64  `json:"offset_only_p95_ns"`
	Note            string `json:"note,omitempty"`
}

// DeviceMeta describes a device that took part in the session.
type DeviceMeta struct {
	Alias        string         `json:"alias"`
	DeviceID     string         `json:"device_id"`
	PluginID     string         `json:"plugin_id"`
	Name         string         `json:"name,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Config       map[string]any `json:"config,omitempty"`
	// ClockOffsetNs is applied to plugin timestamps on ingest (0 by default).
	ClockOffsetNs int64 `json:"clock_offset_ns"`
}

// CaptureMeta describes one capture of one device.
type CaptureMeta struct {
	ID        string `json:"id"`
	Alias     string `json:"alias"`
	StartedNs int64  `json:"started_ns"`
	StoppedNs int64  `json:"stopped_ns"`
	// RequestNs is the session time at which the start request was sent;
	// StartedNs is the anchor the plugin reported when it returned.
	RequestNs int64        `json:"request_ns,omitempty"`
	Clock     *ClockMeta   `json:"clock,omitempty"`
	Streams   []StreamMeta `json:"streams,omitempty"`
	Events    uint64       `json:"events"`
	Samples   uint64       `json:"samples"`
	Dropped   uint64       `json:"dropped_batches"`
}

// ClockMeta is the timing quality of one capture's timestamps, as reported by
// its plugin. stamped_time - true_time lies in [ErrorMinNs, ErrorMaxNs]; a side
// that is not *Bounded has no known limit (the value is only a floor). The
// bounds are effective: they already include OffsetAppliedNs.
type ClockMeta struct {
	Source          string  `json:"source"`
	ResolutionNs    int64   `json:"resolution_ns"`
	ErrorMinNs      int64   `json:"error_min_ns"`
	ErrorMinBounded bool    `json:"error_min_bounded"`
	ErrorMaxNs      int64   `json:"error_max_ns"`
	ErrorMaxBounded bool    `json:"error_max_bounded"`
	JitterNs        int64   `json:"jitter_ns,omitempty"`
	DriftPPM        float64 `json:"drift_ppm,omitempty"`
	Note            string  `json:"note,omitempty"`
	// OffsetAppliedNs is the manual/calibrated offset added on ingest.
	OffsetAppliedNs int64 `json:"offset_applied_ns,omitempty"`
}

// StreamMeta describes a stored sample stream.
type StreamMeta struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	File         string            `json:"file,omitempty"`
	Alias        string            `json:"alias"`
	CaptureID    string            `json:"capture"`
	DigitalNames []string          `json:"digital_names,omitempty"`
	Channels     []string          `json:"channels,omitempty"`
	Unit         string            `json:"unit,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// Session is an open session directory.
type Session struct {
	Dir string

	mu       sync.Mutex
	meta     Meta
	tl       *timeline.Timeline
	logf     *os.File
	logw     *bufio.Writer
	readonly bool

	Samples   SampleStore
	Artifacts artifact.Store
}

var nameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Create makes a new session directory below root.
func Create(root, name, hwlabVersion string) (*Session, error) {
	now := time.Now().UTC()
	slug := strings.Trim(nameRe.ReplaceAllString(name, "-"), "-")
	id := now.Format("2006-01-02T150405Z")
	if slug != "" {
		id += "-" + slug
	}
	dir := filepath.Join(root, id)
	for i := 2; ; i++ {
		err := os.MkdirAll(root, 0o755)
		if err == nil {
			err = os.Mkdir(dir, 0o755)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || i > 100 {
			return nil, fmt.Errorf("create session dir: %w", err)
		}
		id = fmt.Sprintf("%s-%d", now.Format("2006-01-02T150405Z"), i)
		if slug != "" {
			id += "-" + slug
		}
		dir = filepath.Join(root, id)
	}
	for _, d := range []string{"captures", "artifacts", "plots", "devices"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Session{Dir: dir, tl: timeline.New()}
	s.meta = Meta{
		FormatVersion: FormatVersion, ID: id, Name: name, Status: StatusRunning,
		CreatedAt: now, WallEpochUnixNs: now.UnixNano(), HwlabVersion: hwlabVersion,
		Devices: []DeviceMeta{}, Captures: []CaptureMeta{},
	}
	if err := s.initStores(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "timeline.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.logf, s.logw = f, bufio.NewWriterSize(f, 64<<10)
	return s, s.Save()
}

func (s *Session) initStores() error {
	s.Samples = DirSampleStore{Root: s.Dir}
	as, err := artifact.NewDirStore(s.Dir)
	if err != nil {
		return err
	}
	s.Artifacts = as
	return nil
}

// Open loads an existing session read-only (replay, analysis).
func Open(dir string) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, fmt.Errorf("open session %s: %w", dir, err)
	}
	s := &Session{Dir: dir, readonly: true}
	if err := json.Unmarshal(b, &s.meta); err != nil {
		return nil, fmt.Errorf("parse session.json: %w", err)
	}
	if s.meta.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("session format %d is newer than supported %d", s.meta.FormatVersion, FormatVersion)
	}
	evs, err := readEvents(filepath.Join(dir, "timeline.jsonl"))
	if err != nil {
		return nil, err
	}
	s.tl = timeline.FromEvents(evs)
	if err := s.initStores(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenWritable re-opens a session for appending (post-processing).
func OpenWritable(dir string) (*Session, error) {
	s, err := Open(dir)
	if err != nil {
		return nil, err
	}
	s.readonly = false
	f, err := os.OpenFile(filepath.Join(dir, "timeline.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.logf, s.logw = f, bufio.NewWriterSize(f, 64<<10)
	return s, nil
}

func readEvents(path string) ([]timeline.Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []timeline.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e timeline.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			break // truncated tail after a crash: keep what we have
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ID returns the session id.
func (s *Session) ID() string { return s.meta.ID }

// Timeline returns the in-memory timeline.
func (s *Session) Timeline() *timeline.Timeline { return s.tl }

// Meta returns a copy of the metadata.
func (s *Session) Meta() Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.meta)
	var m Meta
	_ = json.Unmarshal(b, &m)
	return m
}

// Update mutates metadata under the session lock.
func (s *Session) Update(fn func(*Meta)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.meta)
}

// AppendEvent adds e to the timeline and persists it.
func (s *Session) AppendEvent(e timeline.Event) (timeline.Event, error) {
	if s.readonly {
		return e, errors.New("session is read-only")
	}
	e = s.tl.Append(e)
	line, err := e.MarshalLine()
	if err != nil {
		return e, fmt.Errorf("encode event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.logw.Write(append(line, '\n')); err != nil {
		return e, err
	}
	return e, nil
}

// Flush writes buffered events to disk.
func (s *Session) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logw == nil {
		return nil
	}
	return s.logw.Flush()
}

// CreateStream opens a new sample stream for a capture.
func (s *Session) CreateStream(sm StreamMeta) (StreamWriter, StreamMeta, error) {
	sm.File = fmt.Sprintf("captures/%s/%s.hwcap", sm.CaptureID, sanitize(sm.ID))
	w, err := s.Samples.Create(sm.File)
	return w, sm, err
}

// OpenStream opens a stored sample stream.
func (s *Session) OpenStream(sm StreamMeta) (StreamReader, error) {
	return s.Samples.Open(sm.File)
}

// StreamsByKind lists stream metadata of a kind, optionally for one alias.
func (s *Session) StreamsByKind(kind, alias string) []StreamMeta {
	var out []StreamMeta
	for _, c := range s.Meta().Captures {
		for _, st := range c.Streams {
			if st.Kind == kind && (alias == "" || st.Alias == alias) {
				out = append(out, st)
			}
		}
	}
	return out
}

func sanitize(s string) string {
	s = nameRe.ReplaceAllString(s, "_")
	if s == "" {
		return "stream"
	}
	return s
}

// Save atomically writes session.json.
func (s *Session) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readonly {
		return nil
	}
	b, err := json.MarshalIndent(&s.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, "session.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, "session.json"))
}

// Close flushes events and saves metadata.
func (s *Session) Close() error {
	var errs []error
	if s.logw != nil {
		s.mu.Lock()
		errs = append(errs, s.logw.Flush(), s.logf.Close())
		s.logw, s.logf = nil, nil
		s.mu.Unlock()
	}
	errs = append(errs, s.Save())
	return errors.Join(errs...)
}

// List returns the sessions below root, newest first.
func List(root string) ([]Meta, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "session.json"))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Resolve turns a path, id, unique id prefix or "latest" into a directory.
func Resolve(root, ref string) (string, error) {
	if st, err := os.Stat(filepath.Join(ref, "session.json")); err == nil && !st.IsDir() {
		return ref, nil
	}
	metas, err := List(root)
	if err != nil {
		return "", err
	}
	if ref == "latest" || ref == "" {
		if len(metas) == 0 {
			return "", fmt.Errorf("no sessions in %s", root)
		}
		return filepath.Join(root, metas[0].ID), nil
	}
	var match []string
	for _, m := range metas {
		if m.ID == ref {
			return filepath.Join(root, m.ID), nil
		}
		if strings.HasPrefix(m.ID, ref) {
			match = append(match, m.ID)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("session %q not found in %s", ref, root)
	case 1:
		return filepath.Join(root, match[0]), nil
	}
	return "", fmt.Errorf("session reference %q is ambiguous (%d matches)", ref, len(match))
}
