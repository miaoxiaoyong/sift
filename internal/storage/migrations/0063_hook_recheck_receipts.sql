CREATE TABLE hook_recheck_receipts (
    run_id      TEXT NOT NULL,
    attempt_no  INTEGER NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects (id),
    state       TEXT NOT NULL CHECK (state IN ('pending','completed')),
    created_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER,
    PRIMARY KEY (run_id, attempt_no),
    FOREIGN KEY (run_id, attempt_no) REFERENCES attempts (run_id, attempt_no)
);

CREATE INDEX hook_recheck_receipts_pending ON hook_recheck_receipts (state, created_at_ms);
