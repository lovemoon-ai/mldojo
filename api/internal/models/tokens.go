package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// APIToken is a named, revocable credential. The single token this replaces
// lived in plaintext in settings.api.token, never expired, could not be
// revoked, and rotating it logged out every CLI and script at once.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the token may still authenticate.
func (t APIToken) Active() bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(time.Now()))
}

func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// CreateAPIToken returns the plaintext token once; only its hash is stored,
// so it can never be read back out of the database.
func (s *Store) CreateAPIToken(ctx context.Context, name, role, createdBy string, ttl time.Duration) (string, *APIToken, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain := "mld_" + hex.EncodeToString(b)
	t := &APIToken{ID: uuid.NewString(), Name: name, Role: role, CreatedBy: createdBy, CreatedAt: time.Now()}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		t.ExpiresAt = &exp
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO api_tokens(id, name, token_hash, role, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, t.ID, t.Name, hashToken(plain), t.Role, t.CreatedBy, t.ExpiresAt)
	if err != nil {
		return "", nil, err
	}
	return plain, t, nil
}

// AuthAPIToken resolves a presented token, refreshing last_used_at. The
// lookup is by hash, so a database leak does not hand out working tokens.
func (s *Store) AuthAPIToken(ctx context.Context, plain string) (*APIToken, error) {
	var t APIToken
	err := s.DB.QueryRow(ctx, `UPDATE api_tokens SET last_used_at = now()
		WHERE token_hash=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		RETURNING id, name, role, created_by, created_at, expires_at, last_used_at, revoked_at`, hashToken(plain)).
		Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt)
	if err != nil {
		return nil, notFoundOr(err, "api token")
	}
	return &t, nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, name, role, created_by, created_at, expires_at, last_used_at, revoked_at
		FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt,
			&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken accepts an id or a unique id prefix, like run ids.
func (s *Store) RevokeAPIToken(ctx context.Context, id string) (string, error) {
	var full string
	err := s.DB.QueryRow(ctx, `UPDATE api_tokens SET revoked_at = now()
		WHERE id::text = $1 OR (length($1) >= 8 AND id::text LIKE $1 || '%')
		RETURNING id`, id).Scan(&full)
	if err != nil {
		return "", notFoundOr(err, "api token %q", id)
	}
	return full, nil
}
