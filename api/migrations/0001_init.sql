-- MLDojo v1 control-plane schema.

CREATE TABLE projects (
  id          uuid PRIMARY KEY,
  name        text NOT NULL UNIQUE,
  description text NOT NULL DEFAULT '',
  owner       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE experiments (
  id          uuid PRIMARY KEY,
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name        text NOT NULL,
  description text NOT NULL DEFAULT '',
  recipe_yaml text NOT NULL DEFAULT '',
  tags        text[] NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);

CREATE TABLE runs (
  id             uuid PRIMARY KEY,
  experiment_id  uuid NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
  name           text NOT NULL DEFAULT '',
  target         text NOT NULL,
  backend_kind   text NOT NULL,
  backend_id     text NOT NULL,
  backend_handle jsonb NOT NULL DEFAULT '{}',
  resources      jsonb NOT NULL DEFAULT '{}',
  env            jsonb NOT NULL DEFAULT '{}',
  code_commit    text NOT NULL DEFAULT '',
  code_patch_uri text NOT NULL DEFAULT '',
  status         text NOT NULL,
  exit_code      int,
  started_at     timestamptz,
  finished_at    timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  metadata       jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX runs_experiment_idx ON runs (experiment_id, created_at DESC);
CREATE INDEX runs_status_idx ON runs (status);
CREATE INDEX runs_backend_idx ON runs (backend_kind, backend_id);

CREATE TABLE nodes (
  id                  text PRIMARY KEY,
  display_name        text NOT NULL DEFAULT '',
  labels              text[] NOT NULL DEFAULT '{}',
  connection          jsonb NOT NULL DEFAULT '{}',
  proxy               jsonb,
  capacity            jsonb,
  agent_status        text NOT NULL DEFAULT 'offline',
  agent_version       text NOT NULL DEFAULT '',
  agent_token_hash    text NOT NULL DEFAULT '',
  last_heartbeat      timestamptz,
  workdir_root        text NOT NULL DEFAULT '',
  datasets_cache_root text NOT NULL DEFAULT '',
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE queues (
  id            text PRIMARY KEY,
  backend       text NOT NULL,
  display_name  text NOT NULL DEFAULT '',
  labels        text[] NOT NULL DEFAULT '{}',
  client        jsonb NOT NULL DEFAULT '{}',
  defaults      jsonb NOT NULL DEFAULT '{}',
  capacity_hint jsonb NOT NULL DEFAULT '{}',
  proxy         jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE datasets (
  id         uuid PRIMARY KEY,
  name       text NOT NULL,
  version    text NOT NULL,
  mount      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (name, version)
);

CREATE TABLE dataset_locations (
  id            uuid PRIMARY KEY,
  dataset_id    uuid NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
  kind          text NOT NULL,
  spec          jsonb NOT NULL DEFAULT '{}',
  authoritative bool NOT NULL DEFAULT false
);

CREATE TABLE secrets (
  namespace   text NOT NULL,
  name        text NOT NULL,
  cipher_blob bytea NOT NULL,
  metadata    jsonb NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (namespace, name)
);

CREATE TABLE run_metrics (
  run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  step   bigint NOT NULL,
  key    text NOT NULL,
  value  double precision NOT NULL,
  ts     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, key, step)
);

CREATE TABLE run_logs_index (
  run_id       uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  stream       text NOT NULL,
  offset_bytes bigint NOT NULL,
  chunk_uri    text NOT NULL,
  ts           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_logs_index_idx ON run_logs_index (run_id, stream, offset_bytes);

CREATE TABLE run_artifacts (
  id         uuid PRIMARY KEY,
  run_id     uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  kind       text NOT NULL,
  uri        text NOT NULL,
  size_bytes bigint NOT NULL DEFAULT 0,
  sha256     text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, uri)
);

CREATE TABLE run_events (
  id      bigserial PRIMARY KEY,
  run_id  uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  ts      timestamptz NOT NULL DEFAULT now(),
  kind    text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX run_events_run_idx ON run_events (run_id, id);

-- Server-side key/value settings (tunnel state, secret recipient, ...).
CREATE TABLE settings (
  key        text PRIMARY KEY,
  value      jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
