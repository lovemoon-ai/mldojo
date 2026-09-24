package models

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

// AuditEntry records a state-changing request. run_events describes what
// happened to a run but carries no actor, so before this there was no way to
// answer "who deleted this project" or "who replaced that secret".
type AuditEntry struct {
	ID        int64          `json:"id"`
	TS        time.Time      `json:"ts"`
	Actor     string         `json:"actor"`
	ActorKind string         `json:"actor_kind"`
	Action    string         `json:"action"`
	Target    string         `json:"target,omitempty"`
	Status    int            `json:"status"`
	IP        string         `json:"ip,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// AddAudit appends an entry. Auditing must never fail a request, so problems
// are logged rather than returned.
func (s *Store) AddAudit(ctx context.Context, e AuditEntry) {
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO audit_log(actor, actor_kind, action, target, status, ip, request_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.Actor, e.ActorKind, e.Action, e.Target, e.Status, e.IP, e.RequestID, mustJSON(e.Payload))
	if err != nil {
		slog.Warn("audit write", "action", e.Action, "err", err)
	}
}

// AuditFilter narrows ListAudit.
type AuditFilter struct {
	Actor  string
	Action string
	Target string
	Limit  int
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	q := `SELECT id, ts, actor, actor_kind, action, target, status, ip, request_id, payload FROM audit_log WHERE 1=1`
	args := []any{}
	add := func(cond, v string) {
		if v != "" {
			args = append(args, v)
			q += cond + "$" + strconv.Itoa(len(args))
		}
	}
	add(" AND actor=", f.Actor)
	add(" AND action=", f.Action)
	add(" AND target=", f.Target)
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 200
	}
	q += " ORDER BY id DESC LIMIT " + strconv.Itoa(f.Limit)
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.ActorKind, &e.Action, &e.Target,
			&e.Status, &e.IP, &e.RequestID, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
