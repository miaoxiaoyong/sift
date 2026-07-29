package gate

import (
	"errors"
	"sync"
)

// Cache is the Gate cache primitive. Persistence is deliberately outside this
// package; callers still record every evaluation when this returns a hit.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cachedVerdict
}
type cachedVerdict struct {
	verdict Verdict
	digest  string
}

// EvaluateCached uses exactly (gate_input_hash, gate_version) as its key.
// A disagreement for an existing key is a contract violation, never an
// overwrite.
func (c *Cache) EvaluateCached(in Input) (Verdict, bool, string, error) {
	_, inputHash, err := CanonicalInput(in)
	if err != nil {
		return Verdict{}, false, "", err
	}
	key := inputHash + "\x00" + Version
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries != nil {
		if entry, ok := c.entries[key]; ok {
			return entry.verdict, true, entry.digest, nil
		}
	}
	v, err := Evaluate(in)
	if err != nil {
		return Verdict{}, false, "", err
	}
	d, err := VerdictDigest(v)
	if err != nil {
		return Verdict{}, false, "", err
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedVerdict)
	}
	if old, ok := c.entries[key]; ok && old.digest != d {
		return Verdict{}, false, "", errors.New("gate: cache verdict disagreement")
	}
	c.entries[key] = cachedVerdict{v, d}
	return v, false, d, nil
}
