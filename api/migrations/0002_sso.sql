-- Conductor SSO: identities and browser sessions. v1 records who did what
-- but does not enforce ACLs.

CREATE TABLE users (
  id         text PRIMARY KEY,                        -- provider user id
  provider   text NOT NULL DEFAULT 'conductor',
  email      text NOT NULL DEFAULT '',
  phone      text NOT NULL DEFAULT '',
  name       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_login timestamptz
);

-- id is the sha256 of the cookie value: a DB leak does not hand out sessions.
CREATE TABLE sessions (
  id         text PRIMARY KEY,
  user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  last_seen  timestamptz NOT NULL DEFAULT now(),
  user_agent text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);
