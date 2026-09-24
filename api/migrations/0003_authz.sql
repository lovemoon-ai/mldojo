-- Authorization, audit and multi-token auth.
--
-- v1 froze "no multi-user": every authenticated caller was a
-- full administrator, owner columns were written but never read, and nothing
-- recorded who deleted what. This is the step that makes the platform safe to
-- hand to more than one person.

-- 'admin' or 'member'. Members read everything (this is a team tool) but may
-- only delete what they own, and may not touch nodes, queues or secrets.
ALTER TABLE users ADD COLUMN role text NOT NULL DEFAULT 'member';

-- Named, revocable API tokens. The previous single token lived in plaintext
-- in settings.api.token; only its hash is kept here, like sessions.
CREATE TABLE api_tokens (
  id           uuid PRIMARY KEY,
  name         text NOT NULL,
  token_hash   text NOT NULL UNIQUE,
  role         text NOT NULL DEFAULT 'admin',
  created_by   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz,
  last_used_at timestamptz,
  revoked_at   timestamptz
);
CREATE INDEX api_tokens_active_idx ON api_tokens (revoked_at, expires_at);

-- Who did what. run_events describes a run's life and has no actor; this
-- answers "who deleted this project", "who changed that secret".
CREATE TABLE audit_log (
  id         bigserial PRIMARY KEY,
  ts         timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL DEFAULT '',
  actor_kind text NOT NULL DEFAULT '',  -- user | token
  action     text NOT NULL,             -- run.delete, secret.set, node.add ...
  target     text NOT NULL DEFAULT '',
  status     int  NOT NULL DEFAULT 0,   -- HTTP status of the request
  ip         text NOT NULL DEFAULT '',
  request_id text NOT NULL DEFAULT '',
  payload    jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_ts_idx ON audit_log (ts DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (actor, ts DESC);
CREATE INDEX audit_log_action_idx ON audit_log (action, ts DESC);

-- Ownership lookups: runs.metadata->>'submitter' is how a run is attributed.
CREATE INDEX runs_submitter_idx ON runs ((metadata->>'submitter'));
