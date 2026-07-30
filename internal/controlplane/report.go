package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
)

// ControlFile is the closed binding view of SIFT_RUN_DIR/control.json that
// `sift report` reads (control-plane.md §7.3). The run token is consumed
// in-place to authorize a single report.submit and is never logged.
type ControlFile struct {
	SchemaVersion     int    `json:"schema_version"`
	RunID             string `json:"run_id"`
	AttemptNo         int    `json:"attempt_no"`
	Generation        int    `json:"generation"`
	WrapperInstanceID string `json:"wrapper_instance_id"`
	RunToken          string `json:"run_token"`
}

// ReadControlFile reads and validates SIFT_RUN_DIR/control.json. It refuses a
// missing/unsafe directory, a symlink or non-regular file, an owner or mode
// wider than 0600, or an invalid run token; it never falls back to another
// credential, socket, SQLite, or offline writes (report.md §1, §2).
func ReadControlFile(dir string) (ControlFile, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return ControlFile{}, errors.New("SIFT_RUN_DIR is unset or not absolute")
	}
	path := filepath.Join(dir, "control.json")
	info, err := os.Lstat(path)
	if err != nil {
		return ControlFile{}, fmt.Errorf("control.json: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ControlFile{}, errors.New("control.json is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return ControlFile{}, errors.New("control.json permissions are not owner-only")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return ControlFile{}, errors.New("control.json is not owned by the current user")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ControlFile{}, fmt.Errorf("control.json: %w", err)
	}
	var c ControlFile
	if err := json.Unmarshal(data, &c); err != nil {
		return ControlFile{}, errors.New("control.json is not valid JSON")
	}
	if c.SchemaVersion != 1 || c.RunID == "" || c.AttemptNo < 1 || c.Generation < 1 || !validToken(c.RunToken) {
		return ControlFile{}, errors.New("control.json binding or token is invalid")
	}
	return c, nil
}

// RetryPolicy is the closed not_ready backoff the daemon derives from the Run's
// frozen config snapshot (report.md §4). The CLI consumes it verbatim and never
// reads config.yaml.
type RetryPolicy struct {
	InitialDelayMS   int `json:"initial_delay_ms"`
	MultiplierMicros int `json:"multiplier_micros"`
	MaxDelayMS       int `json:"max_delay_ms"`
	TotalTimeoutMS   int `json:"total_timeout_ms"`
}

// Validate fail-closes an out-of-contract policy. The CLI never guesses a
// default, rounds a value, or trusts a second response with a different schema
// (report.md §4, control-plane.md §8).
func (p RetryPolicy) Validate() error {
	if p.InitialDelayMS < 1 || p.MultiplierMicros < 1000000 || p.MultiplierMicros > 10000000 {
		return errors.New("retry_policy fields are out of range")
	}
	if p.InitialDelayMS > p.MaxDelayMS || p.MaxDelayMS > p.TotalTimeoutMS {
		return errors.New("retry_policy ordering is invalid")
	}
	return nil
}

// BackoffDelays computes the full delay sequence from a validated policy. The
// nth wait (from 0) is min(max_delay_ms, floor(initial_delay_ms *
// multiplier_micros^n / 1000000^n)); the cumulative sum must not exceed
// total_timeout_ms. Integer arithmetic uses big.Int so overflow is impossible
// rather than silently wrapped (report.md §4).
func (p RetryPolicy) BackoffDelays() ([]int, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	initial := big.NewInt(int64(p.InitialDelayMS))
	mult := big.NewInt(int64(p.MultiplierMicros))
	base := big.NewInt(1000000)
	maxDelay := big.NewInt(int64(p.MaxDelayMS))
	total := big.NewInt(int64(p.TotalTimeoutMS))
	num := new(big.Int).Set(initial)
	den := big.NewInt(1)
	cum := big.NewInt(0)
	var delays []int
	for len(delays) < 100000 {
		wait := new(big.Int).Quo(num, den)
		if wait.Cmp(maxDelay) > 0 {
			wait.Set(maxDelay)
		}
		next := new(big.Int).Add(cum, wait)
		if next.Cmp(total) > 0 {
			break
		}
		delays = append(delays, int(wait.Int64()))
		cum = next
		num.Mul(num, mult)
		den.Mul(den, base)
	}
	return delays, nil
}
