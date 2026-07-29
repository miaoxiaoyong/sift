//go:build !linux

package runtime

import "context"

// PlatformProcessInspector is deliberately fail-closed outside Linux. Darwin
// needs a native proc_pidinfo implementation before it can supply the complete
// PID/start/executable/control-nonce proof required for signalling.
type PlatformProcessInspector struct{ UnknownProcessInspector }

func (PlatformProcessInspector) Observe(ctx context.Context, want ProcessIdentity) (ProcessObservation, error) {
	return UnknownProcessInspector{}.Observe(ctx, want)
}
