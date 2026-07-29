package runtime

import "context"

// UnknownProcessInspector deliberately supplies no identity proof. It is the
// safe V0 production fallback until a platform inspector can independently
// validate the persisted start time, executable and control nonce. Terminator
// consequently records process_identity_unknown and never sends a signal.
type UnknownProcessInspector struct{}

func (UnknownProcessInspector) Observe(context.Context, int) (ProcessObservation, error) {
	return ProcessObservation{Exists: true}, nil
}
