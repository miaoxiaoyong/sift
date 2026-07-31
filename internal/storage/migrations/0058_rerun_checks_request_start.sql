-- 0058_rerun_checks_request_start.sql
-- Durable at-most-once boundary for rerun_checks (storage.md §8.5).

CREATE TABLE outbox_attempt_request_starts (
    attempt_id    TEXT NOT NULL PRIMARY KEY REFERENCES outbox_attempts (id),
    started_at_ms INTEGER NOT NULL CHECK (started_at_ms > 0)
);

CREATE TRIGGER outbox_attempt_request_starts_kind
BEFORE INSERT ON outbox_attempt_request_starts
WHEN NOT EXISTS (
    SELECT 1
    FROM outbox_attempts a
    JOIN outbox_operations o ON o.id = a.operation_id
    WHERE a.id = NEW.attempt_id AND o.kind = 'rerun_checks'
)
BEGIN SELECT RAISE(ABORT, 'request start only allowed for rerun_checks'); END;

CREATE TRIGGER outbox_attempt_request_starts_no_update
BEFORE UPDATE ON outbox_attempt_request_starts
BEGIN SELECT RAISE(ABORT, 'request start is immutable'); END;

CREATE TRIGGER outbox_attempt_request_starts_no_delete
BEFORE DELETE ON outbox_attempt_request_starts
BEGIN SELECT RAISE(ABORT, 'request start is immutable'); END;
