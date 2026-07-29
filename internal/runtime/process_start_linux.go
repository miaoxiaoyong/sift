//go:build linux

package runtime

import (
	"os"
	"strconv"
	"time"
)

// ProcessStartedAtMS returns the procfs-derived start time used by
// PlatformProcessInspector, so persisted wrapper identity can match Observe.
func ProcessStartedAtMS(pid int) int64 {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Now().UnixMilli()
	}
	started, _, err := linuxProcessTimes(stat)
	if err != nil {
		return time.Now().UnixMilli()
	}
	boot, err := linuxBootTimeMS()
	if err != nil {
		return time.Now().UnixMilli()
	}
	return boot + started*10
}
