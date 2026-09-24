-- Hyperparameter sweeps.
--
-- v1 could only expand a literal matrix: a full cartesian product submitted
-- in one go, with no budget, no concurrency limit and no notion of which
-- result was better. A sweep owns a search space and launches runs against a
-- budget, ranking them by a named metric.

CREATE TABLE sweeps (
  id          uuid PRIMARY KEY,
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name        text NOT NULL,
  spec        jsonb NOT NULL,          -- the parsed Sweep definition
  recipe_yaml text NOT NULL,
  target      text NOT NULL,
  status      text NOT NULL DEFAULT 'running', -- running | done | stopped
  -- Sampling is seeded so a sweep can be replayed, and so a restarted
  -- controller does not redraw the points it already launched.
  seed        bigint NOT NULL,
  launched    int NOT NULL DEFAULT 0,
  owner       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);
CREATE INDEX sweeps_status_idx ON sweeps (status);

-- Runs belonging to a sweep are tagged in metadata; this makes counting the
-- active ones cheap.
CREATE INDEX runs_sweep_idx ON runs ((metadata->>'sweep_id'));
