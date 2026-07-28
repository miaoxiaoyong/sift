CREATE TABLE forge_reply_state (
    project_id       TEXT NOT NULL REFERENCES projects (id),
    issue_id         TEXT NOT NULL,
    cursor           TEXT,
    generation       INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    marker_at_ms     INTEGER NOT NULL DEFAULT 0 CHECK (marker_at_ms >= 0),
    updated_at_ms    INTEGER NOT NULL,
    PRIMARY KEY (project_id, issue_id)
);

CREATE INDEX forge_reply_state_updated ON forge_reply_state (updated_at_ms);
