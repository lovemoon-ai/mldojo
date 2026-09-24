-- Evaluation episodes.
--
-- MLDojo modelled training and assumed the interesting number was a loss
-- curve. For a policy it is not. On the task this table was built for,
-- vla_loss sat at 0.06-0.08 from start to finish and said nothing; every
-- decision came from "15/20 clean, 6/20 randomized" and from watching the
-- twenty rollout videos. Re-running the same checkpoint on the same seeds
-- scored 6/20 once and 5/20 the next time, and the only record of which
-- seeds flipped was a chat log.
--
-- So an evaluation is a run whose result is a rate over episodes, with one
-- row per episode: the seed that produced it, whether it succeeded, and the
-- video to watch when it did not.

CREATE TABLE run_episodes (
    run_id      uuid    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idx         int     NOT NULL,
    seed        bigint,
    success     boolean NOT NULL,
    steps       int,
    duration_ms bigint,
    -- node://<node>/<path> or bucket://..., same as artifacts.
    video_uri   text,
    -- Anything the harness reported that does not fit above: the task name,
    -- the instruction type, a failure reason.
    extra       jsonb   NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (run_id, idx)
);

-- "Which seeds flipped between these two runs" is the query that matters,
-- and it joins on seed.
CREATE INDEX run_episodes_seed_idx ON run_episodes (seed);
