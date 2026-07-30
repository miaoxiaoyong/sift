package controlplane

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRetryPolicyBackoffGoldenVector(t *testing.T) {
	// report.md §4 boundary vector: initial=100, multiplier_micros=2000000
	// (×2), max=1000, total=10000 -> 100,200,400,800, then 1000×8 (cumulative
	// 9500ms); the next 1000ms would exceed total_timeout and is rejected.
	policy := RetryPolicy{InitialDelayMS: 100, MultiplierMicros: 2000000, MaxDelayMS: 1000, TotalTimeoutMS: 10000}
	delays, err := policy.BackoffDelays()
	if err != nil {
		t.Fatal(err)
	}
	want := []int{100, 200, 400, 800, 1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	var sum int
	for _, d := range delays {
		sum += d
	}
	if sum != 9500 {
		t.Fatalf("cumulative = %d, want 9500", sum)
	}
}

func TestRetryPolicyUnitMultiplierStaysFlat(t *testing.T) {
	// multiplier_micros=1000000 (×1) keeps every wait at initial.
	policy := RetryPolicy{InitialDelayMS: 100, MultiplierMicros: 1000000, MaxDelayMS: 1000, TotalTimeoutMS: 1000}
	delays, err := policy.BackoffDelays()
	if err != nil {
		t.Fatal(err)
	}
	if len(delays) != 10 {
		t.Fatalf("len(delays) = %d, want 10", len(delays))
	}
	for _, d := range delays {
		if d != 100 {
			t.Fatalf("unit-multiplier wait = %d, want 100", d)
		}
	}
}

func TestRetryPolicyValidationRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy RetryPolicy
	}{
		{"initial zero", RetryPolicy{InitialDelayMS: 0, MultiplierMicros: 1000000, MaxDelayMS: 10, TotalTimeoutMS: 10}},
		{"multiplier below range", RetryPolicy{InitialDelayMS: 10, MultiplierMicros: 999999, MaxDelayMS: 10, TotalTimeoutMS: 10}},
		{"multiplier above range", RetryPolicy{InitialDelayMS: 10, MultiplierMicros: 10000001, MaxDelayMS: 10, TotalTimeoutMS: 10}},
		{"max below initial", RetryPolicy{InitialDelayMS: 1001, MultiplierMicros: 1000000, MaxDelayMS: 1000, TotalTimeoutMS: 2000}},
		{"total below max", RetryPolicy{InitialDelayMS: 10, MultiplierMicros: 1000000, MaxDelayMS: 1000, TotalTimeoutMS: 999}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.policy.Validate(); err == nil {
				t.Fatalf("Validate returned nil for %v", tc.policy)
			}
		})
	}
}

func TestReadControlFileRejectsUnsafeInputs(t *testing.T) {
	dir := t.TempDir()
	token := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeControl := func(extra string, mode os.FileMode) {
		t.Helper()
		content := `{"schema_version":1,"run_id":"run","attempt_no":1,"generation":1,"wrapper_instance_id":"w","run_token":"` + token + `","updated_at_ms":0` + extra + `}`
		path := filepath.Join(dir, "control.json")
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(path, mode)
	}
	// Missing directory.
	if _, err := ReadControlFile(""); err == nil {
		t.Fatal("empty SIFT_RUN_DIR accepted")
	}
	// Missing file.
	if _, err := ReadControlFile(t.TempDir()); err == nil {
		t.Fatal("missing control.json accepted")
	}
	// Unsafe mode (group-readable).
	writeControl("", 0o640)
	if _, err := ReadControlFile(dir); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	_ = os.Remove(filepath.Join(dir, "control.json"))
	// Valid owner-only file succeeds.
	writeControl("", 0o600)
	c, err := ReadControlFile(dir)
	if err != nil {
		t.Fatalf("valid control.json: %v", err)
	}
	if c.RunID != "run" || c.AttemptNo != 1 || c.Generation != 1 || c.RunToken != token {
		t.Fatalf("control = %#v", c)
	}
	_ = os.Remove(filepath.Join(dir, "control.json"))
	// Invalid token rejected.
	path := filepath.Join(dir, "control.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"run_id":"run","attempt_no":1,"generation":1,"run_token":"short"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadControlFile(dir); err == nil {
		t.Fatal("invalid token accepted")
	}
	_ = os.Remove(path)
	// Symlink rejected.
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "control.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadControlFile(dir); err == nil {
		t.Fatal("symlink accepted")
	}
}
