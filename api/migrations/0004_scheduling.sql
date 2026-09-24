-- Queueing and limits.
--
-- Decision #1 froze "no scheduler of our own; resources are named explicitly
-- at submit time". That is still true -- MLDojo does not pack jobs or
-- preempt -- but "no scheduler" had been taken to mean no admission control
-- at all: a node accepted unlimited concurrent runs, a queued run was only
-- retried when its agent happened to reconnect, and there was no way to say
-- "any 5090 will do".

-- 0 means unlimited, which is the behaviour every existing node has.
ALTER TABLE nodes ADD COLUMN max_runs int NOT NULL DEFAULT 0;

-- Per-project concurrency, so one sweep cannot take the whole cluster.
ALTER TABLE projects ADD COLUMN max_concurrent_runs int NOT NULL DEFAULT 0;

-- Runs waiting for a node get dispatched by a periodic reconcile, which
-- needs to find them quickly.
CREATE INDEX runs_pending_idx ON runs (status, created_at) WHERE status = 'queued';
