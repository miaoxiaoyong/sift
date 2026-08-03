-- M6 #847: exact Agent/version topology qualification records.
ALTER TABLE attempts ADD COLUMN topology_qualification_key TEXT;

CREATE TABLE agent_topology_qualifications (
  id TEXT PRIMARY KEY,
  qualification_key TEXT NOT NULL CHECK (length(qualification_key)=64),
  method_version TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  agent_definition_hash TEXT NOT NULL CHECK (length(agent_definition_hash)=64),
  executable_path TEXT NOT NULL,
  executable_sha256 TEXT NOT NULL CHECK (length(executable_sha256)=64),
  version_output_digest TEXT NOT NULL CHECK (length(version_output_digest)=64),
  goos TEXT NOT NULL CHECK (goos IN ('linux','darwin')),
  goarch TEXT NOT NULL CHECK (goarch IN ('amd64','arm64')),
  status TEXT NOT NULL CHECK (status IN ('process-group-verified','process-group-unverified')),
  reason TEXT NOT NULL CHECK (reason IN ('qualified','detached_descendant','identity_incomplete','group_not_empty','unsupported')),
  evidence_json TEXT NOT NULL,
  evidence_digest TEXT NOT NULL CHECK (length(evidence_digest)=64),
  recorded_at_ms INTEGER NOT NULL,
  CHECK ((status='process-group-verified' AND reason='qualified') OR (status='process-group-unverified' AND reason<>'qualified')),
  UNIQUE (qualification_key, evidence_digest)
);
CREATE INDEX agent_topology_qualifications_key ON agent_topology_qualifications(qualification_key);
CREATE TRIGGER agent_topology_qualifications_no_update BEFORE UPDATE ON agent_topology_qualifications BEGIN SELECT RAISE(ABORT, 'agent topology qualifications are immutable'); END;
CREATE TRIGGER agent_topology_qualifications_no_delete BEFORE DELETE ON agent_topology_qualifications BEGIN SELECT RAISE(ABORT, 'agent topology qualifications are immutable'); END;
