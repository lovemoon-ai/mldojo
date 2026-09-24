-- Model registry and lineage.
--
-- Artifacts were only ever "files a run produced": no name, no version, no
-- stage, and no record of which run consumed which artifact. Promoting a
-- checkpoint meant remembering a path on a node.

CREATE TABLE models (
  id          uuid PRIMARY KEY,
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name        text NOT NULL,
  description text NOT NULL DEFAULT '',
  owner       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);

-- A version points at the artifact a run produced. run_id is the lineage:
-- which run made this, with its code, params and environment already
-- recorded against it.
CREATE TABLE model_versions (
  id         uuid PRIMARY KEY,
  model_id   uuid NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  version    int NOT NULL,
  run_id     uuid REFERENCES runs(id) ON DELETE SET NULL,
  uri        text NOT NULL,
  sha256     text NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  -- none | staging | production | archived
  stage      text NOT NULL DEFAULT 'none',
  notes      text NOT NULL DEFAULT '',
  metrics    jsonb NOT NULL DEFAULT '{}',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (model_id, version)
);
CREATE INDEX model_versions_stage_idx ON model_versions (model_id, stage);
CREATE INDEX model_versions_run_idx ON model_versions (run_id);
