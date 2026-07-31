CREATE INDEX outbox_operations_run_fairness
ON outbox_operations (run_id, id);
