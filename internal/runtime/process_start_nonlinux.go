//go:build !linux

package runtime

import "time"

// ProcessStartedAtMS falls back to wall clock outside Linux, where there is no
// native PlatformProcessInspector clock to align with.
func ProcessStartedAtMS(int) int64 {
	return time.Now().UnixMilli()
}
