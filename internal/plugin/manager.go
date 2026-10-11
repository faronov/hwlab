package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/faronov/hwlab/internal/apperr"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// ExePrefix is the file name prefix of plugin executables.
const ExePrefix = "hwlab-plugin-"

// Options configures a Manager.
type Options struct {
	// Paths are extra plugin directories searched first.
	Paths       []string
	CoreVersion string
	Logger      *slog.Logger
}

// Entry describes a discovered plugin.
type Entry struct {
	ID      string // identifier used to select the plugin (file suffix or builtin id)
	Path    string // executable path; empty for builtins
	Builtin bool
	Info    *pluginv1.PluginInfo
	Err     error
}

type slot struct {
	id      string
	path    string
	factory func() (Backend, error)
	mu      sync.Mutex
	b       Backend
}

// Manager finds plugin executables, starts them on demand and restarts
// plugins that crashed.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	slots map[string]*slot
}

// NewManager creates a manager and performs discovery.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	m := &Manager{opts: opts, log: opts.Logger, slots: map[string]*slot{}}
	m.Discover()
	return m
}

// SearchPaths returns the directories searched, in priority order.
func (m *Manager) SearchPaths() []string {
	var paths []string
	paths = append(paths, m.opts.Paths...)
	if env := os.Getenv("HWLAB_PLUGIN_PATH"); env != "" {
		paths = append(paths, filepath.SplitList(env)...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".hwlab", "plugins"))
	}
	paths = append(paths, "/usr/local/lib/hwlab/plugins")
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		paths = append(paths, filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "plugins"))
	}
	return paths
}

// Discover rescans the search paths. Earlier paths win on duplicate ids.
func (m *Manager) Discover() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, dir := range m.SearchPaths() {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, ExePrefix) {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, ExePrefix), ".exe")
			full := filepath.Join(dir, name)
			if st, err := os.Stat(full); err != nil || st.Mode()&0o111 == 0 && runtime.GOOS != "windows" {
				continue
			}
			if _, dup := m.slots[id]; dup {
				continue
			}
			m.slots[id] = &slot{id: id, path: full}
		}
	}
}

// RegisterBuiltin adds an in-process backend (e.g. replay).
func (m *Manager) RegisterBuiltin(id string, factory func() (Backend, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slots[id] = &slot{id: id, factory: factory}
}

func (m *Manager) slot(id string) (*slot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.slots[id]
	return s, ok
}

// IDs returns the selectable plugin ids.
func (m *Manager) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.slots))
	for id := range m.slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Get returns a running backend, launching or relaunching it if needed.
func (m *Manager) Get(ctx context.Context, id string) (Backend, error) {
	s, ok := m.slot(id)
	if !ok {
		return nil, apperr.Newf(apperr.NotFound, "plugin %q not found (searched: %s)", id, strings.Join(m.SearchPaths(), ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b != nil {
		select {
		case <-s.b.Done():
			m.log.Warn("plugin died, restarting", "plugin", id)
			_ = s.b.Close(ctx)
			s.b = nil
		default:
			return s.b, nil
		}
	}
	var (
		b   Backend
		err error
	)
	if s.factory != nil {
		b, err = s.factory()
	} else {
		b, err = launch(ctx, s.path, m.opts.CoreVersion, m.log)
	}
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", id, err)
	}
	if got := b.Info().GetId(); got != id && s.factory == nil {
		m.log.Warn("plugin id differs from executable name", "exe_id", id, "plugin_id", got)
	}
	s.b = b
	return b, nil
}

// List starts every discovered plugin (in parallel) and reports its metadata.
func (m *Manager) List(ctx context.Context) []Entry {
	ids := m.IDs()
	out := make([]Entry, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := m.slot(id)
			e := Entry{ID: id, Path: s.path, Builtin: s.factory != nil}
			b, err := m.Get(ctx, id)
			if err != nil {
				e.Err = err
			} else {
				e.Info = b.Info()
			}
			out[i] = e
		}()
	}
	wg.Wait()
	return out
}

// Close shuts down all running plugins.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	slots := make([]*slot, 0, len(m.slots))
	for _, s := range m.slots {
		slots = append(slots, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.b != nil {
				_ = s.b.Close(ctx)
				s.b = nil
			}
		}()
	}
	wg.Wait()
}
