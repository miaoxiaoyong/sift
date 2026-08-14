package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/xsift/sift/internal/controlplane"
)

// TestRmResolvesVersionAndArchives: `sift rm <id>` resolves the version via
// ops.ps (like kill) and sends ops.rm with force=false.
func TestRmResolvesVersionAndArchives(t *testing.T) {
	home := freshHome(t)
	var rmParams map[string]any
	serveFakeOperatorMulti(t, home, func(req controlplane.Request) map[string]any {
		if req.Method == "ops.ps" {
			return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": []any{map[string]any{"run_id": "run-1", "version": float64(5)}}})
		}
		if req.Method == "ops.rm" {
			rmParams = req.Params
			return fakeDoctorSuccess(req.RequestID, map[string]any{"removed": true, "run_id": "run-1", "archived": true})
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "rm", "run-1"}, &out, io.Discard); code != 0 {
		t.Fatalf("rm exit = %d; output=%q", code, out.String())
	}
	if got := rmParams["force"]; got != false {
		t.Fatalf("rm force = %#v, want false", got)
	}
	if got := rmParams["expected_version"]; got != float64(5) {
		t.Fatalf("rm expected_version = %#v, want 5 (resolved from ops.ps)", got)
	}
	if !strings.Contains(out.String(), "已移除运行 run-1") || !strings.Contains(out.String(), "归档") {
		t.Fatalf("rm output lacks archive confirmation: %q", out.String())
	}
}

// TestRmForceFlag: -f and --force send force=true.
func TestRmForceFlag(t *testing.T) {
	for _, args := range [][]string{{"sift", "rm", "-f", "run-1"}, {"sift", "rm", "--force", "run-1"}, {"sift", "rm", "run-1", "-f"}} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			home := freshHome(t)
			var force any
			serveFakeOperatorMulti(t, home, func(req controlplane.Request) map[string]any {
				if req.Method == "ops.ps" {
					return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": []any{map[string]any{"run_id": "run-1", "version": float64(1)}}})
				}
				force = req.Params["force"]
				return fakeDoctorSuccess(req.RequestID, map[string]any{"removed": true, "run_id": "run-1", "archived": true})
			})
			var out bytes.Buffer
			if code := run(args, &out, io.Discard); code != 0 {
				t.Fatalf("exit = %d; output=%q", code, out.String())
			}
			if force != true {
				t.Fatalf("force = %#v, want true", force)
			}
		})
	}
}

// TestRmConflictIsHumanized: an active-without-force conflict renders the
// actionable message, not the raw envelope.
func TestRmConflictIsHumanized(t *testing.T) {
	home := freshHome(t)
	serveFakeOperatorMulti(t, home, func(req controlplane.Request) map[string]any {
		if req.Method == "ops.ps" {
			return fakeDoctorSuccess(req.RequestID, map[string]any{"runs": []any{map[string]any{"run_id": "run-1", "version": float64(1)}}})
		}
		return fakeDoctorError(req.RequestID, controlplane.ProtocolMajor, controlplane.ProtocolMinor, controlplane.Version, "conflict", "运行仍在进行中；加 --force 先终止再移除")
	})
	var out bytes.Buffer
	if code := run([]string{"sift", "rm", "run-1"}, &out, io.Discard); code != 1 {
		t.Fatalf("rm conflict exit = %d, want 1; output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "--force") {
		t.Fatalf("rm conflict output lacks --force hint: %q", out.String())
	}
}
