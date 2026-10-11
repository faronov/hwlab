package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/faronov/hwlab/internal/apperr"
)

// MaxArtifactRead bounds one bounded read; artifacts are referenced, never
// inlined wholesale.
const MaxArtifactRead = 64 << 10

// ArtifactRef describes a stored file without carrying its bytes.
type ArtifactRef struct {
	// Ref is the stable handle: "session/<session id>/<artifact id>" for
	// files in a session, "file/<path below the artifacts directory>" for
	// host artifacts (flash backups, exports).
	Ref     string    `json:"ref"`
	Kind    string    `json:"kind,omitempty"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size_bytes"`
	SHA256  string    `json:"sha256,omitempty"`
	Time    time.Time `json:"modified,omitempty"`
	Session string    `json:"session,omitempty"`
	Mime    string    `json:"mime_type,omitempty"`
}

// ArtifactChunk is one bounded read.
type ArtifactChunk struct {
	Ref      string `json:"ref"`
	Offset   int64  `json:"offset"`
	Length   int    `json:"length"`
	Size     int64  `json:"size_bytes"`
	EOF      bool   `json:"eof"`
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
	SHA256   string `json:"chunk_sha256"`
}

// ListArtifacts lists the artifacts of one session, or (session == "") the
// host artifacts below ArtifactsRoot. Hashes are not computed here.
func (a *App) ListArtifacts(session string) ([]ArtifactRef, error) {
	if session != "" {
		s, err := a.OpenSession(session)
		if err != nil {
			return nil, err
		}
		var out []ArtifactRef
		for _, art := range s.Artifacts.List() {
			abs := s.Artifacts.Abs(art)
			out = append(out, ArtifactRef{Ref: "session/" + s.ID() + "/" + art.ID, Kind: art.Kind, Name: art.Name, Path: abs,
				Size: art.Size, Time: art.CreatedAt, Session: s.ID(), Mime: art.MimeType})
		}
		return out, nil
	}
	root := a.ArtifactsRoot()
	var out []ArtifactRef
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return fs.SkipAll
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, ArtifactRef{Ref: "file/" + filepath.ToSlash(rel), Kind: kindOf(rel), Name: d.Name(), Path: p, Size: fi.Size(), Time: fi.ModTime()})
		if len(out) >= 1000 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func kindOf(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".json"):
		return "manifest"
	case strings.HasSuffix(rel, ".bin"):
		return "binary"
	}
	return "file"
}

// resolveArtifact maps a ref to a regular file inside the allowed roots.
func (a *App) resolveArtifact(ref string) (ArtifactRef, error) {
	bad := func() (ArtifactRef, error) {
		return ArtifactRef{}, apperr.Newf(apperr.NotFound, "artifact %q not found (refs look like file/<path> or session/<id>/<artifact>)", ref)
	}
	switch {
	case strings.HasPrefix(ref, "session/"):
		parts := strings.SplitN(strings.TrimPrefix(ref, "session/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return bad()
		}
		s, err := a.OpenSession(parts[0])
		if err != nil {
			return ArtifactRef{}, err
		}
		art, ok := s.Artifacts.Get(parts[1])
		if !ok {
			return bad()
		}
		abs := s.Artifacts.Abs(art)
		if !within(s.Dir, abs) {
			return ArtifactRef{}, apperr.Newf(apperr.PathDenied, "artifact %q points outside its session", ref)
		}
		return ArtifactRef{Ref: ref, Kind: art.Kind, Name: art.Name, Path: abs, Size: art.Size, Time: art.CreatedAt, Session: s.ID(), Mime: art.MimeType}, nil
	case strings.HasPrefix(ref, "file/"):
		rel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(ref, "file/")))
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ArtifactRef{}, apperr.Newf(apperr.PathDenied, "artifact %q escapes the artifacts directory", ref)
		}
		root, err := filepath.EvalSymlinks(a.ArtifactsRoot())
		if err != nil {
			return bad()
		}
		real, err := filepath.EvalSymlinks(filepath.Join(root, rel))
		if err != nil {
			return bad()
		}
		if !within(root, real) {
			return ArtifactRef{}, apperr.Newf(apperr.PathDenied, "artifact %q resolves outside the artifacts directory", ref)
		}
		fi, err := os.Stat(real)
		if err != nil || !fi.Mode().IsRegular() {
			return bad()
		}
		return ArtifactRef{Ref: ref, Kind: kindOf(rel), Name: filepath.Base(rel), Path: real, Size: fi.Size(), Time: fi.ModTime()}, nil
	}
	return bad()
}

// ArtifactInfo describes one artifact including its SHA-256.
func (a *App) ArtifactInfo(ref string) (ArtifactRef, error) {
	r, err := a.resolveArtifact(ref)
	if err != nil {
		return r, err
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return r, apperr.Newf(apperr.NotFound, "artifact %q: %v", ref, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return r, err
	}
	r.Size, r.SHA256 = n, hex.EncodeToString(h.Sum(nil))
	return r, nil
}

// ReadArtifact returns at most MaxArtifactRead bytes starting at offset,
// encoded "hex" (default) or "text".
func (a *App) ReadArtifact(ref string, offset int64, length int, encoding string) (*ArtifactChunk, error) {
	if offset < 0 {
		return nil, apperr.Newf(apperr.InvalidArgument, "offset must be >= 0")
	}
	if length <= 0 {
		length = 4096
	}
	if length > MaxArtifactRead {
		return nil, apperr.Newf(apperr.InvalidArgument, "length %d exceeds the %d byte limit of one read; read in windows", length, MaxArtifactRead)
	}
	if encoding == "" {
		encoding = "hex"
	}
	if encoding != "hex" && encoding != "text" {
		return nil, apperr.Newf(apperr.InvalidArgument, "encoding %q: want hex or text", encoding)
	}
	r, err := a.resolveArtifact(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return nil, apperr.Newf(apperr.NotFound, "artifact %q: %v", ref, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if offset > fi.Size() {
		return nil, apperr.Newf(apperr.BadAddress, "offset %d is beyond the end of the artifact (%d bytes)", offset, fi.Size())
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]
	sum := sha256.Sum256(buf)
	c := &ArtifactChunk{Ref: ref, Offset: offset, Length: n, Size: fi.Size(), EOF: offset+int64(n) >= fi.Size(), Encoding: encoding, SHA256: hex.EncodeToString(sum[:])}
	if encoding == "hex" {
		c.Data = hex.EncodeToString(buf)
	} else {
		c.Data = strings.ToValidUTF8(string(buf), "\uFFFD")
	}
	return c, nil
}
