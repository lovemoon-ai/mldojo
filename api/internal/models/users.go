package models

import (
	"context"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// UpsertUser records an identity coming back from the SSO provider.
func (s *Store) UpsertUser(ctx context.Context, u v1.User) (*v1.User, error) {
	_, err := s.DB.Exec(ctx, `INSERT INTO users(id, provider, email, phone, name, last_login)
		VALUES ($1,$2,$3,$4,$5, now())
		ON CONFLICT (id) DO UPDATE SET
			email = CASE WHEN excluded.email <> '' THEN excluded.email ELSE users.email END,
			phone = CASE WHEN excluded.phone <> '' THEN excluded.phone ELSE users.phone END,
			name  = CASE WHEN excluded.name  <> '' THEN excluded.name  ELSE users.name  END,
			last_login = now()`, u.ID, orDefault(u.Provider, "conductor"), u.Email, u.Phone, u.Name)
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, u.ID)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (s *Store) GetUser(ctx context.Context, id string) (*v1.User, error) {
	var u v1.User
	err := s.DB.QueryRow(ctx, `SELECT id, provider, email, phone, name, role, created_at, last_login FROM users WHERE id=$1`, id).
		Scan(&u.ID, &u.Provider, &u.Email, &u.Phone, &u.Name, &u.Role, &u.CreatedAt, &u.LastLogin)
	if err != nil {
		return nil, notFoundOr(err, "user %q", id)
	}
	return &u, nil
}

// CreateSession stores a session keyed by the hash of the cookie value.
func (s *Store) CreateSession(ctx context.Context, idHash, userID, userAgent string, expires time.Time) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO sessions(id, user_id, expires_at, user_agent) VALUES ($1,$2,$3,$4)`,
		idHash, userID, expires, userAgent)
	return err
}

// Session returns the user of a live session and refreshes last_seen.
func (s *Store) Session(ctx context.Context, idHash string) (*v1.User, error) {
	var u v1.User
	err := s.DB.QueryRow(ctx, `UPDATE sessions SET last_seen=now() WHERE id=$1 AND expires_at > now()
		RETURNING (SELECT id FROM users WHERE users.id=sessions.user_id)`, idHash).Scan(&u.ID)
	if err != nil {
		return nil, notFoundOr(err, "session")
	}
	return s.GetUser(ctx, u.ID)
}

func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, idHash)
	return err
}

// PurgeSessions drops expired rows (called periodically).
func (s *Store) PurgeSessions(ctx context.Context) {
	s.DB.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
}

// SetUserRole promotes or demotes a user. Called at login for the configured
// admin list, so an operator only edits config, not the database.
func (s *Store) SetUserRole(ctx context.Context, id, role string) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET role=$2 WHERE id=$1 AND role <> $2`, id, role)
	return err
}

// ListUsers returns everyone who has signed in.
func (s *Store) ListUsers(ctx context.Context) ([]v1.User, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, provider, email, phone, name, role, created_at, last_login
		FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.User{}
	for rows.Next() {
		var u v1.User
		if err := rows.Scan(&u.ID, &u.Provider, &u.Email, &u.Phone, &u.Name, &u.Role, &u.CreatedAt, &u.LastLogin); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
