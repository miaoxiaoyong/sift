//go:build linux

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PlatformProcessInspector independently reads Linux procfs and the wrapper's
// owner-only control file. Missing or malformed evidence produces a live,
// incomplete observation, so Terminator fails closed without signalling.
type PlatformProcessInspector struct{}

func (PlatformProcessInspector) Observe(ctx context.Context, want ProcessIdentity) (ProcessObservation, error) {
	if err := ctx.Err(); err != nil {
		return ProcessObservation{}, err
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(want.PID), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return ProcessObservation{}, nil
	}
	if err != nil {
		return ProcessObservation{}, fmt.Errorf("read proc stat: %w", err)
	}
	started, pgid, err := linuxProcessTimes(stat)
	if err != nil {
		return ProcessObservation{Exists: true}, nil
	}
	boot, err := linuxBootTimeMS()
	if err != nil {
		return ProcessObservation{}, err
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(want.PID), "exe"))
	if err != nil {
		return ProcessObservation{Exists: true}, nil
	}
	observation := ProcessObservation{Exists: true, ProcessIdentity: ProcessIdentity{
		PID: want.PID, PGID: int(pgid), StartedAtMS: boot + started*10, Executable: executable,
	}}
	observation.ControlNonceHash = controlNonceHash(want.ControlPath)
	return observation, nil
}

// /proc/<pid>/stat starttime is documented in clock ticks. Linux exports it in
// USER_HZ, which is 100 for procfs ABI purposes; therefore one tick is 10 ms.
func linuxProcessTimes(stat []byte) (startedMS, pgid int64, err error) {
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return 0, 0, errors.New("malformed proc stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return 0, 0, errors.New("short proc stat")
	}
	pgid, err = strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	started, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return started, pgid, nil
}

func linuxBootTimeMS() (int64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("read proc boot time: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			seconds, err := strconv.ParseInt(fields[1], 10, 64)
			return seconds * 1000, err
		}
	}
	return 0, errors.New("proc boot time unavailable")
}

func controlNonceHash(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var control struct {
		ControlNonce string `json:"control_nonce"`
	}
	if json.Unmarshal(data, &control) != nil || control.ControlNonce == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(control.ControlNonce))
	return hex.EncodeToString(digest[:])
}
