// Package models is the PostgreSQL data access layer (control plane).
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lovemoon-ai/mldojo/api/migrations"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// ErrUser means the request was malformed; it maps to 400.
var ErrUser = errors.New("user error")

// UserError wraps ErrUser with a message.
func UserError(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUser, fmt.Sprintf(format, a...))
}

func IsUserError(err error) bool { return errors.Is(err, ErrUser) }

// ErrForbidden means the caller is authenticated but not allowed.
var ErrForbidden = errors.New("forbidden")

// Forbidden wraps ErrForbidden with a message.
func Forbidden(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, a...))
}

func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }

// NotFound wraps ErrNotFound with a message.
func NotFound(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, a...))
}

// Conflict wraps ErrConflict with a message.
func Conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

type Store struct {
	DB *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Store{DB: pool}, nil
}

func (s *Store) Close() { s.DB.Close() }

// migrationLock is an arbitrary constant: two processes starting at once must
// pick the same number to serialise on.
const migrationLock = 0x6d6c646a // "mldj"

// Migrate applies embedded migrations in lexical order, once each.
//
// An advisory lock serialises concurrent starts. Without it the "has this
// version run?" check and the INSERT that records it straddle a gap, and two
// instances coming up together both decide a migration is pending -- which
// surfaces as a confusing duplicate-object error from the second one.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock); err != nil {
			slog.Warn("release migration lock", "err", err)
		}
	}()

	if _, err := s.DB.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		var exists bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, f).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, f); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func notFoundOr(err error, format string, a ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return NotFound(format, a...)
	}
	return err
}

func mustJSON(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Settings --------------------------------------------------------------

func (s *Store) GetSetting(ctx context.Context, key string, out any) (bool, error) {
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, out)
}

func (s *Store) PutSetting(ctx context.Context, key string, v any) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO settings(key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=now()`, key, mustJSON(v))
	return err
}

// likePrefix escapes a user-provided id prefix for LIKE.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}
