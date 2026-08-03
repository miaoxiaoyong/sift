//go:build !linux && !darwin

package runtime

import "errors"

// PTY is unavailable on platforms without the M6 PTY implementation. The
// process backend remains buildable, but it must not silently fall back to a
// non-terminal Agent launch.
type PTY struct{}

func NewPTY() (*PTY, error) {
	return nil, errors.New("runtime: wrapper-owned PTY is unsupported on this platform")
}
func (*PTY) CloseSlave() error  { return nil }
func (*PTY) CloseMaster() error { return nil }
func (*PTY) Close() error       { return nil }
