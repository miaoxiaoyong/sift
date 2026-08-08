package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
)

type BackoffPolicy struct {
	InitialDelayMS, MaxDelayMS int64
	Multiplier                 float64
}

func (p BackoffPolicy) DelayMS(attempt int) int64 {
	if p.InitialDelayMS <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	m := p.Multiplier
	if m < 1 {
		m = 1
	}
	v := int64(math.Ceil(float64(p.InitialDelayMS) * math.Pow(m, float64(attempt-1))))
	if p.MaxDelayMS > 0 && v > p.MaxDelayMS {
		return p.MaxDelayMS
	}
	return v
}
func digestJSON(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// CanonicalJSON returns the canonical (sorted-key, no HTML escape, no trailing
// newline) JSON encoding of v. It is the encoding used for every closed
// outbox/gate payload and digest, so workers build result bytes with it.
func CanonicalJSON(v any) ([]byte, error) { return canonicalJSON(v) }

// SHA256Hex returns the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string { return sha256Hex(b) }
func validOperationKind(k OperationKind) bool {
	switch k {
	case OperationForgeComment, OperationForgeLabels, OperationCreateChange, OperationMergeChange, OperationRerunChecks, OperationChannelPublish, OperationLaunchAgent, OperationCommandAck, OperationGateReEvaluation, OperationForgeAlert:
		return true
	}
	return false
}
func terminalOrRetry(s OperationState) bool {
	return s == OperationSucceeded || s == OperationRetryable || s == OperationFailed || s == OperationStale || s == OperationConflict
}
