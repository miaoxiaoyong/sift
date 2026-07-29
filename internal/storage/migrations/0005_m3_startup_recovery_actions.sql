ALTER TABLE outbox_operations ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1);

CREATE TABLE startup_recovery_actions (
    boot_id            TEXT NOT NULL REFERENCES daemon_boots (id) ON DELETE RESTRICT,
    candidate_key      TEXT NOT NULL,
    observation_digest TEXT NOT NULL,
    action             TEXT NOT NULL,
    applied_at_ms      INTEGER NOT NULL,
    PRIMARY KEY (boot_id, candidate_key)
);
