package models

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// dumpTables lists every table in an order that satisfies the foreign keys,
// so a restore can load them one after another without deferring anything.
// A table missing from this list is not backed up, so adding a migration
// means adding its tables here.
var dumpTables = []string{
	"schema_migrations",
	"settings",
	"users", "sessions",
	"projects", "experiments", "runs",
	"run_metrics", "run_logs_index", "run_artifacts", "run_events", "run_episodes",
	"nodes", "queues",
	"node_gpu_samples", "node_disk_samples",
	"datasets", "dataset_locations",
	"models", "model_versions",
	"sweeps",
	"api_tokens", "audit_log",
	"secrets",
}

const dumpHeader = "-- mldojo-dump 1"

// Dump writes a logical backup of every known table.
//
// It exists because the native deployment has no pg_dump: the embedded
// PostgreSQL build ships only initdb, pg_ctl and postgres, so a backup that
// shells out to pg_dump cannot run where MLDojo actually runs.
func (s *Store) Dump(ctx context.Context, w io.Writer) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	bw := bufio.NewWriterSize(w, 1<<20)
	if _, err := fmt.Fprintf(bw, "%s\n", dumpHeader); err != nil {
		return err
	}
	for _, t := range dumpTables {
		if _, err := fmt.Fprintf(bw, "-- table: %s\n", t); err != nil {
			return err
		}
		// COPY ... TO STDOUT is the same text format pg_restore reads back,
		// and streams without buffering the table in memory.
		if _, err := conn.Conn().PgConn().CopyTo(ctx, bw, `COPY "`+t+`" TO STDOUT`); err != nil {
			return fmt.Errorf("dump %s: %w", t, err)
		}
		if _, err := fmt.Fprint(bw, "\\.\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Restore loads a dump produced by Dump into an empty (migrated) database.
// Every listed table is truncated first, so restoring twice is idempotent.
func (s *Store) Restore(ctx context.Context, r io.Reader) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	br := bufio.NewReaderSize(r, 1<<20)
	line, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != dumpHeader {
		return fmt.Errorf("not an mldojo dump (expected %q)", dumpHeader)
	}

	// Truncating in reverse order keeps the foreign keys happy.
	for i := len(dumpTables) - 1; i >= 0; i-- {
		if _, err := conn.Exec(ctx, `TRUNCATE TABLE "`+dumpTables[i]+`" CASCADE`); err != nil {
			return fmt.Errorf("truncate %s: %w", dumpTables[i], err)
		}
	}

	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		table, ok := strings.CutPrefix(strings.TrimSpace(line), "-- table: ")
		if !ok {
			return fmt.Errorf("unexpected line in dump: %q", strings.TrimSpace(line))
		}
		if !known(table) {
			return fmt.Errorf("dump has unknown table %q", table)
		}
		// CopyFrom stops at the \. terminator, leaving the reader positioned
		// at the next "-- table:" line.
		if _, err := conn.Conn().PgConn().CopyFrom(ctx, &untilTerminator{r: br}, `COPY "`+table+`" FROM STDIN`); err != nil {
			return fmt.Errorf("restore %s: %w", table, err)
		}
	}
	return s.resetSequences(ctx)
}

func known(table string) bool {
	for _, t := range dumpTables {
		if t == table {
			return true
		}
	}
	return false
}

// untilTerminator yields lines up to the COPY terminator and then reports EOF,
// so one reader can feed several consecutive COPY statements.
type untilTerminator struct {
	r    *bufio.Reader
	done bool
	buf  []byte
}

func (u *untilTerminator) Read(p []byte) (int, error) {
	if len(u.buf) == 0 {
		if u.done {
			return 0, io.EOF
		}
		line, err := u.r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			u.done = true
			return 0, io.EOF
		}
		if string(line) == "\\.\n" || string(line) == "\\." {
			u.done = true
			return 0, io.EOF
		}
		u.buf = line
	}
	n := copy(p, u.buf)
	u.buf = u.buf[n:]
	return n, nil
}

// resetSequences puts every bigserial past the restored rows, otherwise the
// next insert collides with an id that came from the dump.
func (s *Store) resetSequences(ctx context.Context) error {
	// Restricted to public and quoted: pg_class also holds information_schema
	// tables, and pg_get_serial_sequence resolves its argument through
	// search_path, so an unqualified name there fails the whole query.
	rows, err := s.DB.Query(ctx, `SELECT c.relname, a.attname, pg_get_serial_sequence(quote_ident(c.relname), a.attname)
		FROM pg_class c
		JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE c.relkind = 'r'
		  AND c.relnamespace = 'public'::regnamespace
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_get_serial_sequence(quote_ident(c.relname), a.attname) IS NOT NULL`)
	if err != nil {
		return err
	}
	type seq struct{ table, col, name string }
	var all []seq
	for rows.Next() {
		var s seq
		if err := rows.Scan(&s.table, &s.col, &s.name); err != nil {
			rows.Close()
			return err
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, q := range all {
		if _, err := s.DB.Exec(ctx, fmt.Sprintf(
			`SELECT setval('%s', COALESCE((SELECT max(%q) FROM %q), 0) + 1, false)`, q.name, q.col, q.table)); err != nil {
			return fmt.Errorf("reset sequence %s: %w", q.name, err)
		}
	}
	return nil
}
