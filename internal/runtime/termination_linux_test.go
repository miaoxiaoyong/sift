//go:build linux

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformProcessInspectorRequiresMatchingControlNonce(t *testing.T) {
	inspector := PlatformProcessInspector{}
	id := ProcessIdentity{PID: os.Getpid(), ControlPath: filepath.Join(t.TempDir(), "control.json")}
	withoutControl, err := inspector.Observe(context.Background(), id)
	if err != nil || !withoutControl.Exists || withoutControl.ControlNonceHash != "" {
		t.Fatalf("without control = %#v, %v", withoutControl, err)
	}

	nonce := "a control nonce"
	if err := os.WriteFile(id.ControlPath, []byte(`{"control_nonce":"`+nonce+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := inspector.Observe(context.Background(), id)
	if err != nil || !got.Exists || got.PID != id.PID || got.PGID <= 0 || got.StartedAtMS <= 0 || got.Executable == "" {
		t.Fatalf("observation = %#v, %v", got, err)
	}
	digest := sha256.Sum256([]byte(nonce))
	if got.ControlNonceHash != hex.EncodeToString(digest[:]) {
		t.Fatalf("nonce hash = %q", got.ControlNonceHash)
	}

	id.PGID = got.PGID
	id.StartedAtMS = got.StartedAtMS
	id.Executable = got.Executable
	id.ControlNonceHash = got.ControlNonceHash
	if !sameIdentity(id, got.ProcessIdentity) {
		t.Fatal("complete platform observation did not match identity")
	}
	if sameIdentity(ProcessIdentity{PID: id.PID, PGID: got.PGID, StartedAtMS: got.StartedAtMS, Executable: got.Executable, ControlNonceHash: "wrong"}, got.ProcessIdentity) {
		t.Fatal("wrong nonce matched process identity")
	}
}

func TestPlatformProcessInspectorReportsAbsentPID(t *testing.T) {
	got, err := (PlatformProcessInspector{}).Observe(context.Background(), ProcessIdentity{PID: 1 << 30})
	if err != nil || got.Exists {
		t.Fatalf("observation = %#v, %v", got, err)
	}
}
