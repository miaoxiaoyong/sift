package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xsift/sift/internal/schema"
)

// SourceInfo records the on-disk facts about config.yaml at load time. The
// drift checker compares these first (existence, mtime, size) before the more
// expensive hash recompute (config.md §4).
type SourceInfo struct {
	Path    string
	Present bool
	MTime   time.Time // zero when absent
	Size    int64
}

// Snapshot is the immutable startup product: the effective Config, its
// canonical-JSON fingerprint, and the source facts needed for drift detection.
// The runtime uses only Config + Hash; CanonicalJSON is what storage persists
// in config_snapshots.canonical_json (config.md §4 step 8).
type Snapshot struct {
	Config        *Config
	Hash          string
	CanonicalJSON []byte
	Source        SourceInfo
}

// ConfigPath returns the config.yaml path under a resolved home.
func ConfigPath(home Home) string {
	return filepath.Join(home.Path, "config.yaml")
}

// ParseYAML validates and normalizes configuration bytes without writing them.
// Local editors use it before rename so an invalid edit cannot replace the
// daemon's last known-good file.
func ParseYAML(data []byte) (*Snapshot, error) {
	jsonBytes, err := YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	raw := &RawConfig{}
	if err := schema.Decode(jsonBytes, raw, schema.Closed); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	return finalize(raw, SourceInfo{Present: true})
}

// Load is the single entry point by which global config enters the system
// (config.md §1.2, §4). It reads config.yaml once, converts YAML to JSON under
// the strict bridge, decodes via [schema.Closed], normalizes to the effective
// snapshot and computes the fingerprint. An absent file yields the full
// zero-config default (config.md §6 scenario 1). now is accepted for symmetry
// with the storage layer's injected clock; the fingerprint itself is
// time-independent.
func Load(home Home, _ time.Time) (*Snapshot, error) {
	path := ConfigPath(home)
	src := SourceInfo{Path: path}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		src.Present = true
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("config: stat %s: %w", path, statErr)
		}
		src.MTime = info.ModTime()
		src.Size = info.Size()

		jsonBytes, err := YAMLToJSON(data)
		if err != nil {
			return nil, err
		}
		raw := &RawConfig{}
		if err := schema.Decode(jsonBytes, raw, schema.Closed); err != nil {
			return nil, fmt.Errorf("config: decode %s: %w", path, err)
		}
		return finalize(raw, src)

	case errors.Is(err, fs.ErrNotExist):
		// Absent file ⇒ full defaults. No decode path: there is nothing to
		// close-validate, and version is implicit (config.md §6 scenario 1).
		return finalize(&RawConfig{}, src)

	default:
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
}

// finalize runs normalization + fingerprinting shared by the present- and
// absent-file paths.
func finalize(raw *RawConfig, src SourceInfo) (*Snapshot, error) {
	cfg, err := Normalize(raw)
	if err != nil {
		return nil, err
	}
	hash, canonical, err := Fingerprint(cfg)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Config: cfg, Hash: hash, CanonicalJSON: canonical, Source: src}, nil
}

// This file is the sole YAML entry point (config.md §1.2, §4 step 2). The
// decode gateway speaks JSON only; on-disk config.yaml is converted to JSON
// here under the four strict rules, then handed to [schema.Closed]:
//
//   - exactly one document (multi-document input is rejected),
//   - no duplicate mapping keys,
//   - no non-string mapping keys,
//   - no alias cycles.
//
// It depends on gopkg.in/yaml.v3, a pure-Go parser, so CGO_ENABLED=0 holds.

// Sentinels surfaced by YAMLToJSON so the loader can distinguish an empty
// present file (which the spec treats as distinct from an absent file).
var (
	ErrEmptyConfigFile = errors.New("config: file exists but is empty; delete it to use zero-config defaults")
	ErrMultiDocument   = errors.New("config: YAML multi-document input is not allowed")
)

// YAMLToJSON converts a single strict YAML document to compact JSON suitable
// for [schema.Closed]. See the file comment for the enforced rules.
func YAMLToJSON(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ErrEmptyConfigFile
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))

	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("config: parse YAML: %w", err)
	}
	// A second decodable value means the input carried more than one document.
	var probe yaml.Node
	if err := dec.Decode(&probe); err == nil || !errors.Is(err, io.EOF) {
		return nil, ErrMultiDocument
	}

	val, err := nodeToValue(&doc, map[*yaml.Node]bool{})
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("config: encode YAML as JSON: %w", err)
	}
	return out, nil
}

// nodeToValue walks a yaml.Node tree into a JSON-compatible Go value while
// enforcing duplicate-key, string-key and alias-cycle rules. path tracks the
// container nodes currently on the recursion stack so an alias pointing back
// into them is detected as a cycle.
type yamlNodeHandler func(*yaml.Node, map[*yaml.Node]bool) (any, error)

var yamlNodeHandlers map[yaml.Kind]yamlNodeHandler

func init() {
	yamlNodeHandlers = map[yaml.Kind]yamlNodeHandler{
		yaml.DocumentNode: yamlDocumentValue,
		yaml.ScalarNode:   yamlScalarValue,
		yaml.SequenceNode: yamlSequenceValue,
		yaml.MappingNode:  yamlMappingValue,
	}
}

func nodeToValue(n *yaml.Node, path map[*yaml.Node]bool) (any, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind == yaml.AliasNode {
		t := n.Alias
		if t == nil {
			return nil, errors.New("config: unresolvable YAML alias")
		}
		if path[t] {
			return nil, errors.New("config: YAML alias cycle detected")
		}
		return nodeToValue(t, path)
	}

	handler, ok := yamlNodeHandlers[n.Kind]
	if !ok {
		return nil, fmt.Errorf("config: unsupported YAML node kind %d", n.Kind)
	}
	return handler(n, path)
}

func yamlDocumentValue(n *yaml.Node, path map[*yaml.Node]bool) (any, error) {
	if len(n.Content) == 0 {
		return map[string]any{}, nil
	}
	return nodeToValue(n.Content[0], path)
}

func yamlScalarValue(n *yaml.Node, _ map[*yaml.Node]bool) (any, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, fmt.Errorf("config: decode YAML scalar: %w", err)
	}
	return v, nil
}

func yamlSequenceValue(n *yaml.Node, path map[*yaml.Node]bool) (any, error) {
	path[n] = true
	defer delete(path, n)
	out := make([]any, 0, len(n.Content))
	for _, c := range n.Content {
		cv, err := nodeToValue(c, path)
		if err != nil {
			return nil, err
		}
		out = append(out, cv)
	}
	return out, nil
}

func yamlMappingValue(n *yaml.Node, path map[*yaml.Node]bool) (any, error) {
	path[n] = true
	defer delete(path, n)
	out := make(map[string]any, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, err := mapKey(n.Content[i], path)
		if err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("config: duplicate YAML map key %q", key)
		}
		vv, err := nodeToValue(n.Content[i+1], path)
		if err != nil {
			return nil, err
		}
		out[key] = vv
	}
	return out, nil
}

// mapKey resolves a mapping key node to a string, rejecting aliases, non-string
// tags and the merge key "<<". YAML map keys must be strings (config.md §4.2).
func mapKey(kn *yaml.Node, path map[*yaml.Node]bool) (string, error) {
	n := kn
	if n.Kind == yaml.AliasNode {
		t := n.Alias
		if t == nil {
			return "", errors.New("config: unresolvable YAML alias in map key")
		}
		if path[t] {
			return "", errors.New("config: YAML alias cycle detected")
		}
		n = t
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", fmt.Errorf("config: non-string YAML map key (resolved tag %q)", n.Tag)
	}
	if n.Value == "<<" {
		return "", errors.New("config: YAML merge key << is not supported")
	}
	return n.Value, nil
}

// CanonicalJSON serializes the effective config into the canonical form
// mandated by config.md §4 step 6: UTF-8, object keys in dictionary order, no
// extraneous whitespace, no NaN/Infinity. Durations serialize via
// [time.Duration.String], which is deterministic for a given duration, so
// "30s" and "0.5m" normalize to the same bytes (and the same fingerprint).
//
// Implementation: the effective Config marshals with struct-field order, so the
// result is re-decoded into a generic map and re-encoded; encoding/json emits
// map keys in sorted order recursively. The first marshal returns an error on
// NaN/Infinity floats, which is exactly the §4 rejection.
func CanonicalJSON(cfg *Config) ([]byte, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: marshal effective config: %w", err)
	}
	var tree map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("config: re-decode for canonical ordering: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("config: encode canonical JSON: %w", err)
	}
	// json.Encoder appends a trailing newline; §4 wants no extraneous
	// whitespace, so trim it.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Fingerprint returns the SHA-256 lowercase-hex config_hash of the effective
// config plus its canonical JSON (config.md §4 step 7). Both are needed by the
// storage layer's config_snapshots row (config_hash UNIQUE, canonical_json
// TEXT); the runtime keeps only the in-memory snapshot.
func Fingerprint(cfg *Config) (hash string, canonical []byte, err error) {
	canonical, err = CanonicalJSON(cfg)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}

// CertificationRulesVersion identifies the frozen certification algorithm and
// its normalized global thresholds. It deliberately excludes evidence, which
// belongs to the task-kind-specific certification version.
func CertificationRulesVersion(certification Certification) (string, error) {
	canonical, err := canonicalValue(map[string]any{
		"algorithm_version": 1,
		"certification":     certification,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalValue(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("config: marshal canonical value: %w", err)
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("config: re-decode canonical value: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("config: encode canonical value: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// isAbsPath reports whether p is an absolute path. It is a thin wrapper kept
// in-package so config tests can stub the filesystem-independent rule without
// importing filepath at call sites.
func isAbsPath(p string) bool {
	return filepath.IsAbs(p)
}

// cleanPath canonicalizes p with filepath.Clean (config.md §2.1.3).
func cleanPath(p string) string {
	return filepath.Clean(p)
}

// File-mode contract (config.md §2.1). The home dir and config.yaml are
// owner-only; anything wider refuses startup and is never auto-corrected.
const (
	HomeDirMode    os.FileMode = 0o700
	ConfigFileMode os.FileMode = 0o600
)

// Home is the resolved SIFT_HOME. Path is the cleaned, stable external path
// used for every file under it; Resolved is the symlink-resolved real path
// recorded for diagnostics when the directory already exists (config.md §2.1.3).
type Home struct {
	Path     string
	Resolved string
}

// ResolveHome resolves SIFT_HOME from the process environment: a non-empty
// SIFT_HOME (which must be absolute) wins, otherwise $HOME/.sift. The result is
// filepath.Clean-ed. A user home that cannot be determined refuses startup
// (config.md §2.1.4).
func ResolveHome() (Home, error) {
	return ResolveHomeWith(os.UserHomeDir)
}

// ResolveHomeWith is ResolveHome with an injectable user-home resolver, for
// tests that must not touch the real $HOME.
func ResolveHomeWith(userHome func() (string, error)) (Home, error) {
	if env := os.Getenv("SIFT_HOME"); env != "" {
		if !filepath.IsAbs(env) {
			return Home{}, fmt.Errorf("SIFT_HOME must be an absolute path, got %q", env)
		}
		return makeHome(env), nil
	}
	hd, err := userHome()
	if err != nil {
		return Home{}, fmt.Errorf("resolve user home directory: %w", err)
	}
	return makeHome(filepath.Join(hd, ".sift")), nil
}

func makeHome(p string) Home {
	clean := filepath.Clean(p)
	h := Home{Path: clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			h.Resolved = filepath.Clean(resolved)
		}
	}
	return h
}

// EnsureHomeLayout verifies (or initially creates) the SIFT_HOME directory and,
// when present, config.yaml, against the §2.1 permission and ownership
// schema. It refuses startup on:
//   - a home path that exists but is not a directory,
//   - a home directory or config.yaml whose mode grants group/other access,
//   - a home directory not owned by the current user.
//
// It never relaxes permissions: an existing too-wide mode is an error, not a
// chmod target (config.md §2.1.4).
func EnsureHomeLayout(home Home) error {
	info, err := os.Stat(home.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(home.Path, HomeDirMode); err != nil {
			return fmt.Errorf("create SIFT_HOME %s: %w", home.Path, err)
		}
		// Re-verify the actual on-disk mode; never trust umask alone
		// (config.md §2.1 final paragraph).
		if err := os.Chmod(home.Path, HomeDirMode); err != nil {
			return fmt.Errorf("enforce SIFT_HOME mode: %w", err)
		}
		info, err = os.Stat(home.Path)
		if err != nil {
			return fmt.Errorf("stat SIFT_HOME after create: %w", err)
		}
	case err != nil:
		return fmt.Errorf("stat SIFT_HOME %s: %w", home.Path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("SIFT_HOME %s is not a directory", home.Path)
	}
	if err := checkOwnerExclusive(info, home.Path); err != nil {
		return err
	}
	if err := checkOwnedByCurrentUser(info, home.Path); err != nil {
		return err
	}

	cfgPath := filepath.Join(home.Path, "config.yaml")
	if cinfo, err := os.Stat(cfgPath); err == nil {
		if cinfo.IsDir() {
			return fmt.Errorf("config.yaml (%s) is a directory", cfgPath)
		}
		if err := checkOwnerExclusive(cinfo, cfgPath); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat config.yaml: %w", err)
	}
	return nil
}

// checkOwnerExclusive rejects any mode bit granting access beyond the owner
// (config.md §2.1.4: "权限宽于属主访问时，daemon 拒绝启动").
func checkOwnerExclusive(info fs.FileInfo, what string) error {
	if info.Mode()&0o077 != 0 {
		return fmt.Errorf("%s: permissions too open (%s); must be owner-only (no group/other access)", what, info.Mode())
	}
	return nil
}
