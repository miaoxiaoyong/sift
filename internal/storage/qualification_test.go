package storage

import (
	"context"
	"strings"
	"testing"

	runtimepkg "github.com/miaoxiaoyong/sift/internal/runtime"
)

func topologyQualificationFixture(t *testing.T, status, reason string) TopologyQualification {
	t.Helper()
	q := runtimepkg.Qualification{
		MethodVersion: runtimepkg.TopologyMethodVersion, AgentID: "agent",
		AgentDefinitionHash: strings.Repeat("a", 64), ExecutablePath: "/resolved/agent",
		ExecutableSHA256: strings.Repeat("b", 64), VersionOutputDigest: strings.Repeat("c", 64),
		GOOS: "linux", GOARCH: "amd64",
	}
	key, err := runtimepkg.QualificationKey(q)
	if err != nil {
		t.Fatal(err)
	}
	out := TopologyQualification{ID: "qualification", QualificationKey: key, MethodVersion: q.MethodVersion, AgentID: q.AgentID, AgentDefinitionHash: q.AgentDefinitionHash, ExecutablePath: q.ExecutablePath, ExecutableSHA256: q.ExecutableSHA256, VersionOutputDigest: q.VersionOutputDigest, GOOS: q.GOOS, GOARCH: q.GOARCH, Status: status, Reason: reason, RecordedAtMS: testNow}
	out.EvidenceJSON, err = TopologyQualificationEvidenceJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordTopologyQualificationRejectsInconsistentProjection(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	for _, tc := range []struct {
		name  string
		forge func(*TopologyQualification)
	}{
		{"method", func(q *TopologyQualification) { q.MethodVersion = "forged" }},
		{"agent", func(q *TopologyQualification) { q.AgentID = "forged" }},
		{"definition_hash", func(q *TopologyQualification) { q.AgentDefinitionHash = strings.Repeat("d", 64) }},
		{"path", func(q *TopologyQualification) { q.ExecutablePath = "/forged" }},
		{"binary_hash", func(q *TopologyQualification) { q.ExecutableSHA256 = strings.Repeat("d", 64) }},
		{"version", func(q *TopologyQualification) { q.VersionOutputDigest = strings.Repeat("d", 64) }},
		{"os", func(q *TopologyQualification) { q.GOOS = "darwin" }},
		{"arch", func(q *TopologyQualification) { q.GOARCH = "arm64" }},
		{"status", func(q *TopologyQualification) { q.Status = "process-group-unverified" }},
		{"reason", func(q *TopologyQualification) { q.Reason = "detached_descendant" }},
		{"evidence", func(q *TopologyQualification) { q.EvidenceJSON = `{"forged":true}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := topologyQualificationFixture(t, "process-group-verified", "qualified")
			q.ID += tc.name
			tc.forge(&q)
			if err := db.RecordTopologyQualification(ctx, q); err == nil {
				t.Fatal("forged qualification was accepted")
			}
		})
	}
	q := topologyQualificationFixture(t, "process-group-verified", "qualified")
	if ok, err := db.ProcessGroupQualified(ctx, q.QualificationKey); err != nil || ok {
		t.Fatalf("forged verified projection qualified=%v err=%v, want false", ok, err)
	}
}
