package app

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/faronov/hwlab/internal/apperr"
)

// maxExactInt is the largest integer a JSON number (float64) holds exactly.
const maxExactInt = 1 << 53

// maxStringArg bounds a single string argument; bulk data goes through a file
// in the artifacts directory instead.
const maxStringArg = 2 << 20

// addressKeys name arguments that carry target addresses or sizes. They may
// be given as a hex/decimal string (full 64-bit range) or as an integer below
// 2^53.
var addressKeys = map[string]bool{
	"address": true, "addr": true, "base": true, "start": true, "end": true,
	"offset": true, "length": true, "size": true, "count": true,
}

// fileKeys name arguments that are host file or directory paths.
var fileKeys = map[string]bool{"path": true, "backup": true, "backup_path": true, "dir": true, "file": true}

// ParseUint64 parses "0x.." hex or plain decimal; nothing else (no octal, no
// underscores, no sign) so an address means one thing everywhere.
func ParseUint64(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return strconv.ParseUint(s[2:], 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

// ValidateArgs rejects malformed numbers and addresses before anything is
// sent to a plugin: NaN/Inf, integers that a JSON number cannot hold exactly,
// negative or fractional addresses, unparsable address strings, address+length
// overflowing 64 bits and oversized strings.
func ValidateArgs(args map[string]any) error {
	if err := walkArgs("args", args); err != nil {
		return err
	}
	a, aok := addrValue(args, "address", "addr")
	l, lok := addrValue(args, "length", "size")
	if aok && lok && l > 0 && a+l-1 < a {
		return apperr.Newf(apperr.BadAddress, "args: address 0x%x + length 0x%x overflows the 64-bit address space", a, l)
	}
	return nil
}

func addrValue(args map[string]any, keys ...string) (uint64, bool) {
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			if n, err := ParseUint64(v); err == nil {
				return n, true
			}
		case float64:
			if v >= 0 && v == math.Trunc(v) && v < maxExactInt {
				return uint64(v), true
			}
		case int, int8, int16, int32, int64:
			if n := reflect.ValueOf(v).Int(); n >= 0 {
				return uint64(n), true
			}
		case uint, uint8, uint16, uint32, uint64:
			return reflect.ValueOf(v).Uint(), true
		}
	}
	return 0, false
}

func walkArgs(path string, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + k
			if addressKeys[k] {
				if err := checkAddress(p, x[k]); err != nil {
					return err
				}
				continue
			}
			if err := walkArgs(p, x[k]); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := walkArgs(fmt.Sprintf("%s[%d]", path, i), e); err != nil {
				return err
			}
		}
	case float64:
		return checkNumber(path, x)
	case string:
		if len(x) > maxStringArg {
			return apperr.Newf(apperr.InvalidArgument, "%s: string of %d bytes exceeds %d; pass bulk data as a file in the artifacts directory", path, len(x), maxStringArg)
		}
	}
	return nil
}

func checkNumber(path string, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return apperr.Newf(apperr.InvalidArgument, "%s: not a finite number", path)
	}
	if f == math.Trunc(f) && math.Abs(f) >= maxExactInt {
		return apperr.Newf(apperr.InvalidArgument, "%s: %v cannot be represented exactly in JSON; pass large integers as a hex string such as \"0x...\"", path, f)
	}
	return nil
}

func checkAddress(path string, v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if _, err := ParseUint64(x); err != nil {
			return apperr.Newf(apperr.InvalidArgument, "%s: %q is not a 0x-hex or decimal unsigned number below 2^64", path, x)
		}
	case float64:
		if err := checkNumber(path, x); err != nil {
			return err
		}
		if x < 0 || x != math.Trunc(x) {
			return apperr.Newf(apperr.InvalidArgument, "%s: %v is not a non-negative integer", path, x)
		}
	case int, int8, int16, int32, int64:
		if reflect.ValueOf(x).Int() < 0 {
			return apperr.Newf(apperr.InvalidArgument, "%s: %v is not a non-negative integer", path, x)
		}
	case uint, uint8, uint16, uint32, uint64:
		// Go-native integers (from YAML or in-process callers) are exact.
	default:
		return apperr.Newf(apperr.InvalidArgument, "%s: want a hex/decimal string or an integer", path)
	}
	return nil
}

// validateSchema checks args against the method's declared JSON schema. A
// schema that cannot be compiled is ignored: the plugin still validates.
func validateSchema(schema string, args map[string]any) error {
	if strings.TrimSpace(schema) == "" {
		return nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return nil
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil
	}
	in := args
	if in == nil {
		in = map[string]any{}
	}
	if err := rs.Validate(in); err != nil {
		return apperr.Newf(apperr.InvalidArgument, "args do not match the method schema: %s", firstLine(err.Error()))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ArtifactsRoot is the only host directory file arguments may point into
// when a request is file-confined (MCP).
func (a *App) ArtifactsRoot() string { return filepath.Join(a.Cfg.Home, "artifacts") }

// confineFiles returns a copy of args whose file/dir arguments are resolved
// inside ArtifactsRoot: relative paths land under artifacts/mcp, absolute
// ones must already be inside; ".." escapes and symlink escapes are refused.
func (a *App) confineFiles(args map[string]any) (map[string]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	root, err := filepath.Abs(a.ArtifactsRoot())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
		s, ok := v.(string)
		if !ok || !fileKeys[k] || s == "" {
			continue
		}
		p := s
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, "mcp", p)
		}
		p = filepath.Clean(p)
		if !within(root, p) && !within(realRoot, p) {
			return nil, apperr.Newf(apperr.PathDenied, "args.%s: %q is outside the hwlab artifacts directory %s", k, s, root)
		}
		// Resolve symlinks of the deepest existing ancestor.
		real, err := evalExisting(p)
		if err != nil || !within(realRoot, real) {
			return nil, apperr.Newf(apperr.PathDenied, "args.%s: %q resolves outside the hwlab artifacts directory", k, s)
		}
		if k != "dir" {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return nil, err
			}
		}
		out[k] = p
	}
	return out, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func evalExisting(p string) (string, error) {
	cur, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// prepareArgs confines file arguments (when asked), validates numbers and
// addresses and checks the method's declared schema.
func (a *App) prepareArgs(spec interface{ GetArgsSchemaJson() string }, args *map[string]any, confine bool) error {
	if confine {
		c, err := a.confineFiles(*args)
		if err != nil {
			return err
		}
		*args = c
	}
	if err := ValidateArgs(*args); err != nil {
		return err
	}
	return validateSchema(spec.GetArgsSchemaJson(), *args)
}

// CheckExperimentPath restricts experiment files read over MCP to YAML files
// below the hwlab home or the server's working directory.
func (a *App) CheckExperimentPath(p string) (string, error) {
	ext := strings.ToLower(filepath.Ext(p))
	if ext != ".yaml" && ext != ".yml" {
		return "", apperr.Newf(apperr.PathDenied, "experiment path %q: only .yaml/.yml files may be loaded", p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", apperr.Newf(apperr.NotFound, "experiment file %q not found", p)
	}
	roots := []string{a.Cfg.Home}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	for _, r := range roots {
		if rr, err := filepath.EvalSymlinks(r); err == nil && within(rr, real) {
			return real, nil
		}
	}
	return "", apperr.Newf(apperr.PathDenied, "experiment path %q is outside the hwlab home and the working directory", p)
}
