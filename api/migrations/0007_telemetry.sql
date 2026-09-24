-- Node telemetry history.
--
-- Until now every resource number MLDojo had was "right now": the heartbeat
-- landed in a map on the API process and was deleted the moment the agent
-- disconnected. So "what was this node doing yesterday", "how many GPU-hours
-- did this run cost", "did anything hold eight cards at 0% for six hours"
-- and "which project uses the most compute" were all unanswerable -- not
-- hard, unanswerable, because nothing was ever written down.
--
-- One narrow table unblocks all four. Rows are small and written at the
-- heartbeat rate, so they are aggregated on read and deleted by a retention
-- sweep rather than kept forever.

CREATE TABLE node_gpu_samples (
    node_id    text        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    gpu_index  int         NOT NULL,
    ts         timestamptz NOT NULL,
    util       real        NOT NULL,
    mem_used_mb real       NOT NULL,
    mem_total_mb real      NOT NULL,
    temp       real        NOT NULL,
    -- The run holding the card, when it is ours. NULL covers both an idle
    -- card and one another tenant is using; foreign_mem_mb separates them.
    run_id     uuid        REFERENCES runs(id) ON DELETE SET NULL,
    foreign_mem_mb real    NOT NULL DEFAULT 0,
    foreign_procs  int     NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, gpu_index, ts)
);

-- The two reads that matter: a node's history over a window, and one run's.
CREATE INDEX node_gpu_samples_ts_idx ON node_gpu_samples (ts);
CREATE INDEX node_gpu_samples_run_idx ON node_gpu_samples (run_id, ts) WHERE run_id IS NOT NULL;

-- Filesystem readings, so "26 GiB free" stops meaning whichever disk the
-- agent happened to stat when it last reconnected.
CREATE TABLE node_disk_samples (
    node_id  text        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    path     text        NOT NULL,
    ts       timestamptz NOT NULL,
    total_gb real        NOT NULL,
    free_gb  real        NOT NULL,
    PRIMARY KEY (node_id, path, ts)
);

CREATE INDEX node_disk_samples_ts_idx ON node_disk_samples (ts);
