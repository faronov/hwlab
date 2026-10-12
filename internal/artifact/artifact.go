// Package artifact stores files produced by a session (native captures,
// pcaps, exports, plots) with an index so they can be listed and shared.
package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Artifact describes one stored file. Path is relative to the session dir.
type Artifact struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	MimeType  string            `json:"mime_type,omitempty"`
	Path      string            `json:"path"`
	Size      int64             `json:"size"`
	SourceID  string            `json:"source,omitempty"`
	CaptureID string            `json:"capture,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Store is the artifact storage abstraction.
type Store interface {
	// Put copies r into the store under dir/name and registers it.
	Put(a Artifact, r io.Reader) (Artifact, error)
	// Register indexes a file that is already inside the store root.
	Register(a Artifact) (Artifact, error)
	List() []Artifact
	Get(id string) (Artifact, bool)
	// Abs returns the absolute path of an artifact.
	Abs(a Artifact) string
}

// DirStore keeps artifacts below Root/artifacts and an index.json next to them.
type DirStore struct {
	Root string // session directory

	mu    sync.Mutex
	items []Artifact
}

// NewDirStore loads (or initialises) the store of a session directory.
func NewDirStore(root string) (*DirStore, error) {
	s := &DirStore{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.indexPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.items); err != nil {
			return nil, fmt.Errorf("parse artifact index: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return s, nil
}

func (s *DirStore) indexPath() string { return filepath.Join(s.Root, "artifacts", "index.json") }

func (s *DirStore) Abs(a Artifact) string { return filepath.Join(s.Root, filepath.FromSlash(a.Path)) }

func (s *DirStore) Put(a Artifact, r io.Reader) (Artifact, error) {
	if a.Path == "" {
		a.Path = "artifacts/" + a.Name
	}
	abs := s.Abs(a)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return a, err
	}
	f, err := os.Create(abs)
	if err != nil {
		return a, err
	}
	if _, err := io.Copy(f, r); err != nil {
		return a, errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return a, err
	}
	return s.Register(a)
}

func (s *DirStore) Register(a Artifact) (Artifact, error) {
	rel, err := filepath.Rel(s.Root, s.Abs(a))
	if err != nil || len(rel) >= 2 && rel[:2] == ".." {
		return a, fmt.Errorf("artifact %q is outside the session directory", a.Path)
	}
	st, err := os.Stat(s.Abs(a))
	if err != nil {
		return a, fmt.Errorf("stat artifact: %w", err)
	}
	a.Size = st.Size()
	a.Path = filepath.ToSlash(rel)
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == "" {
		a.ID = fmt.Sprintf("a%03d", len(s.items)+1)
	}
	s.items = append(s.items, a)
	return a, s.saveLocked()
}

func (s *DirStore) List() []Artifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Artifact(nil), s.items...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *DirStore) Get(id string) (Artifact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.items {
		if a.ID == id || a.Name == id {
			return a, true
		}
	}
	return Artifact{}, false
}

func (s *DirStore) saveLocked() error {
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}
