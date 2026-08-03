package launchworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/controlplane"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

func TestQualificationProbeTimeoutPreventsSpawnAndBinding(t *testing.T) {
	root, db, boot, now := qualificationLaunchFixture(t)
	agentPath := filepath.Join(root, "agent")
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	host := &recordingBackend{}
	worker := &Worker{DB: db, BootID: boot, WorkerID: "timeout", Root: root, Lease: time.Minute, Now: func() time.Time { return now.Add(2 * time.Millisecond) }, Backends: BackendRouter{config.BackendProcess: host}, Agents: []config.Agent{{ID: "agent", Executable: agentPath, TaskTransport: config.TaskTransportStdin}}, QualificationProbeTimeout: 25 * time.Millisecond}
	if err := worker.RunOnce(context.Background()); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging qualification probe error = %v, want deadline exceeded", err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("backend spawned after failed qualification probe: %#v", host.calls)
	}
	var key string
	if err := db.QueryRowForTest(context.Background(), `SELECT COALESCE(topology_qualification_key,'') FROM attempts WHERE run_id='run-qualification'`).Scan(&key); err != nil || key != "" {
		t.Fatalf("failed qualification bound key=%q err=%v", key, err)
	}
}

func TestQualificationBinaryReplacementBetweenMeasurementAndAgentExecFailsClosed(t *testing.T) {
	root, db, boot, now := qualificationLaunchFixture(t)
	agentPath := filepath.Join(root, "agent")
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nprintf old\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	host := &recordingBackend{}
	worker := &Worker{DB: db, BootID: boot, WorkerID: "replacement", Root: root, Lease: time.Minute, Now: func() time.Time { return now.Add(2 * time.Millisecond) }, Backends: BackendRouter{config.BackendProcess: host}, Agents: []config.Agent{{ID: "agent", Executable: agentPath, TaskTransport: config.TaskTransportStdin}}}
	worker.hooks.beforeSpawn = func() error {
		return os.WriteFile(agentPath, []byte("#!/bin/sh\nprintf replacement\n"), 0o700)
	}
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("binary replacement between measurement and spawn was accepted")
	}
	if len(host.calls) != 0 {
		t.Fatalf("backend spawned replacement under old qualification: %#v", host.calls)
	}
	var key string
	if err := db.QueryRowForTest(context.Background(), `SELECT COALESCE(topology_qualification_key,'') FROM attempts WHERE run_id='run-qualification'`).Scan(&key); err != nil || key != "" {
		t.Fatalf("replacement retained old qualification authorization=%q err=%v", key, err)
	}
}

func qualificationLaunchFixture(t *testing.T) (string, *storage.DB, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	root := t.TempDir()
	db, err := storage.Open(ctx, storage.OpenConfig{Path: filepath.Join(root, "sift.db"), BinaryVersion: controlplane.Version, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SeedProjectForTest(ctx, "cfg", "project", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.SeedLaunchRunForTest(ctx, "run-qualification", "project", "cfg", now.UnixMilli(), root); err != nil {
		t.Fatal(err)
	}
	boot, err := db.StartDaemonBoot(ctx, "hash-cfg", controlplane.Version, controlplane.ProtocolMajor, os.Getpid(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	completeLaunchRecovery(t, db, boot, now.Add(time.Millisecond).UnixMilli(), "supervise")
	return root, db, boot, now
}
