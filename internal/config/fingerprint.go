package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

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
