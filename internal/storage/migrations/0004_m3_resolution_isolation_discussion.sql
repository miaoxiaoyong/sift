-- Migration 0004: M3 resolution, isolation and frozen discussion targets.
--
-- These columns are projections, not a substitute for the M3 runtime or
-- Interrupt ports. The triggers below make the storage invariants hold even
-- when a future write port is bypassed.

ALTER TABLE daemon_boots ADD COLUMN recovery_completed_at_ms INTEGER;

ALTER TABLE runs ADD COLUMN discussion_target_kind TEXT
    CHECK (discussion_target_kind IN ('issue', 'change'));
ALTER TABLE runs ADD COLUMN discussion_target_id TEXT;
ALTER TABLE runs ADD COLUMN discussion_target_url TEXT;

ALTER TABLE attempts ADD COLUMN isolation_release_event_id TEXT
    REFERENCES events (id);

-- The M1 daemon trigger predates the recovery barrier column. Extend its
-- append-only completion rule without weakening the existing identity checks.
DROP TRIGGER daemon_boots_stop_completion_only;
CREATE TRIGGER daemon_boots_stop_completion_only
BEFORE UPDATE ON daemon_boots FOR EACH ROW
WHEN NOT (
    NEW.id IS OLD.id
    AND NEW.config_snapshot_id IS OLD.config_snapshot_id
    AND NEW.pid IS OLD.pid
    AND NEW.binary_version IS OLD.binary_version
    AND NEW.protocol_major IS OLD.protocol_major
    AND NEW.started_at_ms IS OLD.started_at_ms
    AND (OLD.recovery_completed_at_ms IS NULL
         AND NEW.recovery_completed_at_ms IS NOT NULL
         OR NEW.recovery_completed_at_ms IS OLD.recovery_completed_at_ms)
    AND ((OLD.stopped_at_ms IS NULL
          AND NEW.stopped_at_ms IS NOT NULL
          AND OLD.stop_reason IS NULL
          AND NEW.stop_reason IS NOT NULL)
         OR (NEW.stopped_at_ms IS OLD.stopped_at_ms
             AND NEW.stop_reason IS OLD.stop_reason))
    AND (NEW.recovery_completed_at_ms IS NOT OLD.recovery_completed_at_ms
         OR NEW.stopped_at_ms IS NOT OLD.stopped_at_ms)
)
BEGIN SELECT RAISE(ABORT, 'daemon_boots allows only one-time completion fields'); END;

-- A discussion target is either completely absent (forge source) or a
-- complete, frozen target (manual source). URL is the verified canonical URL,
-- so it is deliberately not accepted independently of kind and id.
CREATE TRIGGER runs_discussion_target_shape
BEFORE INSERT ON runs FOR EACH ROW
WHEN NOT (
    ((NEW.discussion_target_kind IS NULL) = (NEW.discussion_target_id IS NULL)
     AND (NEW.discussion_target_id IS NULL) = (NEW.discussion_target_url IS NULL))
    AND (
        (NEW.source_kind = 'forge'
         AND NEW.discussion_target_kind IS NULL)
        OR
        (NEW.source_kind = 'manual'
         AND NEW.discussion_target_kind IS NOT NULL)
    )
)
BEGIN SELECT RAISE(ABORT, 'invalid frozen discussion target'); END;

CREATE TRIGGER runs_discussion_target_shape_update
BEFORE UPDATE ON runs FOR EACH ROW
WHEN NOT (
    ((NEW.discussion_target_kind IS NULL) = (NEW.discussion_target_id IS NULL)
     AND (NEW.discussion_target_id IS NULL) = (NEW.discussion_target_url IS NULL))
    AND (
        (NEW.source_kind = 'forge'
         AND NEW.discussion_target_kind IS NULL)
        OR
        (NEW.source_kind = 'manual'
         AND NEW.discussion_target_kind IS NOT NULL)
    )
)
BEGIN SELECT RAISE(ABORT, 'invalid frozen discussion target'); END;

CREATE TRIGGER runs_discussion_target_immutable
BEFORE UPDATE ON runs FOR EACH ROW
WHEN NEW.discussion_target_kind IS NOT OLD.discussion_target_kind
  OR NEW.discussion_target_id IS NOT OLD.discussion_target_id
  OR NEW.discussion_target_url IS NOT OLD.discussion_target_url
BEGIN SELECT RAISE(ABORT, 'discussion target is immutable'); END;

-- Isolation has an independent lifecycle. A frozen attempt has no release
-- fields; a released projection retains its original reason and timestamp and
-- records both the evidence event and release time.
CREATE TRIGGER attempts_isolation_shape_insert
BEFORE INSERT ON attempts FOR EACH ROW
WHEN NOT (
    (NEW.isolation_state = 'frozen'
     AND NEW.isolation_reason IS NOT NULL
     AND NEW.isolated_at_ms IS NOT NULL
     AND NEW.isolation_released_at_ms IS NULL
     AND NEW.isolation_release_event_id IS NULL)
    OR
    (NEW.isolation_state = 'none'
     AND (
        (NEW.isolation_reason IS NULL AND NEW.isolated_at_ms IS NULL
         AND NEW.isolation_released_at_ms IS NULL
         AND NEW.isolation_release_event_id IS NULL)
        OR
        (NEW.isolation_reason IS NOT NULL AND NEW.isolated_at_ms IS NOT NULL
         AND NEW.isolation_released_at_ms IS NOT NULL
         AND NEW.isolation_release_event_id IS NOT NULL)
     ))
)
BEGIN SELECT RAISE(ABORT, 'invalid attempt isolation projection'); END;

CREATE TRIGGER attempts_isolation_shape_update
BEFORE UPDATE ON attempts FOR EACH ROW
WHEN NOT (
    (NEW.isolation_state = 'frozen'
     AND NEW.isolation_reason IS NOT NULL
     AND NEW.isolated_at_ms IS NOT NULL
     AND NEW.isolation_released_at_ms IS NULL
     AND NEW.isolation_release_event_id IS NULL)
    OR
    (NEW.isolation_state = 'none'
     AND (
        (NEW.isolation_reason IS NULL AND NEW.isolated_at_ms IS NULL
         AND NEW.isolation_released_at_ms IS NULL
         AND NEW.isolation_release_event_id IS NULL)
        OR
        (NEW.isolation_reason IS NOT NULL AND NEW.isolated_at_ms IS NOT NULL
         AND NEW.isolation_released_at_ms IS NOT NULL
         AND NEW.isolation_release_event_id IS NOT NULL)
     ))
)
BEGIN SELECT RAISE(ABORT, 'invalid attempt isolation projection'); END;

-- Closing an Interrupt is a one-time fact, just like its generation key.
CREATE TRIGGER interrupts_close_write_once
BEFORE UPDATE ON interrupts FOR EACH ROW
WHEN OLD.close_reason IS NOT NULL
 AND (NEW.close_reason IS NOT OLD.close_reason OR NEW.closed_at_ms IS NOT OLD.closed_at_ms)
BEGIN SELECT RAISE(ABORT, 'interrupt close reason is write-once'); END;
