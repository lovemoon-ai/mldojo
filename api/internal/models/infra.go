package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Nodes -----------------------------------------------------------------

const nodeCols = `n.id, n.display_name, n.labels, n.connection, n.proxy, n.capacity, n.agent_status, n.agent_version,
	n.last_heartbeat, n.workdir_root, n.datasets_cache_root, n.created_at, n.updated_at,
	(SELECT count(*) FROM runs r WHERE r.backend_kind='node' AND r.backend_id=n.id AND r.status IN ('queued','starting','running')),
	n.max_runs`

// NodeBusyRuns counts the runs actually occupying a node. Queued runs are
// excluded on purpose: they hold nothing yet, so counting them towards a
// limit makes that limit self-blocking.
func (s *Store) NodeBusyRuns(ctx context.Context, nodeID string) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM runs
		WHERE backend_kind='node' AND backend_id=$1 AND status IN ('starting','running')`, nodeID).Scan(&n)
	return n, err
}

// NodeStartingGPUs counts the GPUs promised to runs on a node that have been
// admitted but whose process has not started yet. Telemetry cannot see them:
// the card still reads as free, so two runs dispatched seconds apart both
// believe they fit and the second one OOMs.
func (s *Store) NodeStartingGPUs(ctx context.Context, nodeID string) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(COALESCE((resources->>'gpus')::int, 0)), 0) FROM runs
		WHERE backend_kind='node' AND backend_id=$1 AND status='starting'`, nodeID).Scan(&n)
	return n, err
}

func scanNode(row pgx.Row) (*v1.Node, error) {
	var n v1.Node
	var conn, proxy, capa []byte
	err := row.Scan(&n.ID, &n.DisplayName, &n.Labels, &conn, &proxy, &capa, &n.AgentStatus, &n.AgentVersion,
		&n.LastHeartbeat, &n.WorkdirRoot, &n.DatasetsCacheRoot, &n.CreatedAt, &n.UpdatedAt, &n.ActiveRuns, &n.MaxRuns)
	if err != nil {
		return nil, err
	}
	n.Labels = nonNil(n.Labels)
	// Malformed JSON here means the node row is unusable; returning a
	// half-built node would surface as a confusing connection failure later.
	if err := json.Unmarshal(conn, &n.Connection); err != nil {
		return nil, fmt.Errorf("node %s: bad connection json: %w", n.ID, err)
	}
	if len(proxy) > 0 && string(proxy) != "null" {
		n.Proxy = &v1.Proxy{}
		if err := json.Unmarshal(proxy, n.Proxy); err != nil {
			return nil, fmt.Errorf("node %s: bad proxy json: %w", n.ID, err)
		}
	}
	if len(capa) > 0 && string(capa) != "null" {
		n.Capacity = &v1.Capacity{}
		if err := json.Unmarshal(capa, n.Capacity); err != nil {
			return nil, fmt.Errorf("node %s: bad capacity json: %w", n.ID, err)
		}
	}
	return &n, nil
}

func nullableJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return nil
	}
	return b
}

func (s *Store) InsertNode(ctx context.Context, n *v1.Node, tokenHash string) error {
	var proxy, capa []byte
	if n.Proxy != nil {
		proxy = nullableJSON(n.Proxy)
	}
	if n.Capacity != nil {
		capa = nullableJSON(n.Capacity)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO nodes(id, display_name, labels, connection, proxy, capacity, agent_status, agent_version,
		agent_token_hash, last_heartbeat, workdir_root, datasets_cache_root) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		n.ID, n.DisplayName, nonNil(n.Labels), mustJSON(n.Connection), proxy, capa, n.AgentStatus, n.AgentVersion,
		tokenHash, n.LastHeartbeat, n.WorkdirRoot, n.DatasetsCacheRoot)
	if isUnique(err) {
		return Conflict("node %q already exists", n.ID)
	}
	return err
}

func (s *Store) GetNode(ctx context.Context, id string) (*v1.Node, error) {
	n, err := scanNode(s.DB.QueryRow(ctx, `SELECT `+nodeCols+` FROM nodes n WHERE n.id=$1`, id))
	if err != nil {
		return nil, notFoundOr(err, "node %q", id)
	}
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]v1.Node, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+nodeCols+` FROM nodes n ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return NotFound("node %q", id)
	}
	return err
}

func (s *Store) NodeTokenHash(ctx context.Context, id string) (string, error) {
	var h string
	err := s.DB.QueryRow(ctx, `SELECT agent_token_hash FROM nodes WHERE id=$1`, id).Scan(&h)
	if err != nil {
		return "", notFoundOr(err, "node %q", id)
	}
	return h, nil
}

func (s *Store) SetNodeTokenHash(ctx context.Context, id, hash string) error {
	_, err := s.DB.Exec(ctx, `UPDATE nodes SET agent_token_hash=$2, updated_at=now() WHERE id=$1`, id, hash)
	return err
}

// UpdateNodeAgent records agent status/version/capacity. Nil fields are kept.
func (s *Store) UpdateNodeAgent(ctx context.Context, id, status, version string, capa *v1.Capacity, hb *time.Time) error {
	var capaJSON []byte
	if capa != nil {
		capaJSON = nullableJSON(capa)
	}
	_, err := s.DB.Exec(ctx, `UPDATE nodes SET
		agent_status = CASE WHEN $2 <> '' THEN $2 ELSE agent_status END,
		agent_version = CASE WHEN $3 <> '' THEN $3 ELSE agent_version END,
		capacity = COALESCE($4::jsonb, capacity),
		last_heartbeat = COALESCE($5, last_heartbeat),
		updated_at = now()
		WHERE id=$1`, id, status, version, capaJSON, hb)
	return err
}

// SetNodeConnection records how to reach a node (node upgrade --ssh/--port).
func (s *Store) SetNodeConnection(ctx context.Context, id string, c v1.NodeConnection) error {
	_, err := s.DB.Exec(ctx, `UPDATE nodes SET connection=$2, updated_at=now() WHERE id=$1`, id, nullableJSON(c))
	return err
}

func (s *Store) UpdateNodeRoots(ctx context.Context, id, workdir, datasets string) error {
	_, err := s.DB.Exec(ctx, `UPDATE nodes SET
		workdir_root = CASE WHEN $2 <> '' THEN $2 ELSE workdir_root END,
		datasets_cache_root = CASE WHEN $3 <> '' THEN $3 ELSE datasets_cache_root END WHERE id=$1`, id, workdir, datasets)
	return err
}

// Queues ----------------------------------------------------------------

const queueCols = `q.id, q.backend, q.display_name, q.labels, q.client, q.defaults, q.capacity_hint, q.proxy, q.created_at, q.updated_at,
	(SELECT count(*) FROM runs r WHERE r.backend_kind='queue' AND r.backend_id=q.id AND r.status IN ('queued','starting','running'))`

func scanQueue(row pgx.Row) (*v1.Queue, error) {
	var q v1.Queue
	var client, defaults, hint, proxy []byte
	err := row.Scan(&q.ID, &q.Backend, &q.DisplayName, &q.Labels, &client, &defaults, &hint, &proxy, &q.CreatedAt, &q.UpdatedAt, &q.ActiveRuns)
	if err != nil {
		return nil, err
	}
	q.Labels = nonNil(q.Labels)
	json.Unmarshal(client, &q.Client)
	json.Unmarshal(defaults, &q.Defaults)
	json.Unmarshal(hint, &q.CapacityHint)
	if len(proxy) > 0 && string(proxy) != "null" {
		q.Proxy = &v1.Proxy{}
		json.Unmarshal(proxy, q.Proxy)
	}
	if q.Defaults == nil {
		q.Defaults = map[string]any{}
	}
	if q.CapacityHint == nil {
		q.CapacityHint = map[string]any{}
	}
	return &q, nil
}

func (s *Store) InsertQueue(ctx context.Context, q *v1.Queue) error {
	var proxy []byte
	if q.Proxy != nil {
		proxy = nullableJSON(q.Proxy)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO queues(id, backend, display_name, labels, client, defaults, capacity_hint, proxy)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, q.ID, q.Backend, q.DisplayName, nonNil(q.Labels), mustJSON(q.Client),
		mustJSON(q.Defaults), mustJSON(q.CapacityHint), proxy)
	if isUnique(err) {
		return Conflict("queue %q already exists", q.ID)
	}
	return err
}

func (s *Store) GetQueue(ctx context.Context, id string) (*v1.Queue, error) {
	q, err := scanQueue(s.DB.QueryRow(ctx, `SELECT `+queueCols+` FROM queues q WHERE q.id=$1`, id))
	if err != nil {
		return nil, notFoundOr(err, "queue %q", id)
	}
	return q, nil
}

func (s *Store) ListQueues(ctx context.Context) ([]v1.Queue, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+queueCols+` FROM queues q ORDER BY q.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.Queue{}
	for rows.Next() {
		q, err := scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

func (s *Store) DeleteQueue(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM queues WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return NotFound("queue %q", id)
	}
	return err
}

// Datasets --------------------------------------------------------------

func (s *Store) CreateDataset(ctx context.Context, d *v1.Dataset) (*v1.Dataset, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	id := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO datasets(id, name, version, mount) VALUES ($1,$2,$3,$4)`, id, d.Name, d.Version, d.Mount)
	if isUnique(err) {
		return nil, Conflict("dataset %s@%s already exists", d.Name, d.Version)
	}
	if err != nil {
		return nil, err
	}
	for _, l := range d.Locations {
		spec := map[string]any{}
		for k, v := range map[string]string{"node": l.Node, "path": l.Path, "provider": l.Provider, "bucket": l.Bucket, "credentials": l.Credentials} {
			if v != "" {
				spec[k] = v
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dataset_locations(id, dataset_id, kind, spec, authoritative) VALUES ($1,$2,$3,$4,$5)`,
			uuid.New(), id, l.Kind, mustJSON(spec), l.Authoritative); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetDataset(ctx, d.Name, d.Version)
}

// GetDataset returns name@version; an empty version selects the newest.
func (s *Store) GetDataset(ctx context.Context, name, version string) (*v1.Dataset, error) {
	var d v1.Dataset
	var id uuid.UUID
	var err error
	if version == "" {
		err = s.DB.QueryRow(ctx, `SELECT id, name, version, mount, created_at FROM datasets WHERE name=$1 ORDER BY created_at DESC LIMIT 1`, name).
			Scan(&id, &d.Name, &d.Version, &d.Mount, &d.CreatedAt)
	} else {
		err = s.DB.QueryRow(ctx, `SELECT id, name, version, mount, created_at FROM datasets WHERE name=$1 AND version=$2`, name, version).
			Scan(&id, &d.Name, &d.Version, &d.Mount, &d.CreatedAt)
	}
	if err != nil {
		ref := name
		if version != "" {
			ref += "@" + version
		}
		return nil, notFoundOr(err, "dataset %s", ref)
	}
	d.ID = id.String()
	locs, err := s.datasetLocations(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	d.Locations = locs
	return &d, nil
}

func (s *Store) datasetLocations(ctx context.Context, datasetID string) ([]v1.DatasetLocation, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, kind, spec, authoritative FROM dataset_locations WHERE dataset_id=$1 ORDER BY authoritative DESC, id`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.DatasetLocation{}
	for rows.Next() {
		var l v1.DatasetLocation
		var id uuid.UUID
		var spec []byte
		if err := rows.Scan(&id, &l.Kind, &spec, &l.Authoritative); err != nil {
			return nil, err
		}
		json.Unmarshal(spec, &l)
		l.ID = id.String()
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ListDatasets(ctx context.Context) ([]v1.Dataset, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, name, version, mount, created_at FROM datasets ORDER BY name, created_at DESC`)
	if err != nil {
		return nil, err
	}
	var out []v1.Dataset
	for rows.Next() {
		var d v1.Dataset
		var id uuid.UUID
		if err := rows.Scan(&id, &d.Name, &d.Version, &d.Mount, &d.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.ID = id.String()
		out = append(out, d)
	}
	rows.Close()
	for i := range out {
		locs, err := s.datasetLocations(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Locations = locs
	}
	if out == nil {
		out = []v1.Dataset{}
	}
	return out, nil
}

func (s *Store) DeleteDataset(ctx context.Context, name, version string) error {
	d, err := s.GetDataset(ctx, name, version)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM datasets WHERE id=$1`, d.ID)
	return err
}

// AddDatasetLocation records a new node_path (e.g. after a cache fill).
func (s *Store) AddDatasetLocation(ctx context.Context, datasetID string, l v1.DatasetLocation) error {
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dataset_locations WHERE dataset_id=$1 AND kind=$2 AND spec->>'node'=$3 AND spec->>'path'=$4)`,
		datasetID, l.Kind, l.Node, l.Path).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO dataset_locations(id, dataset_id, kind, spec, authoritative) VALUES ($1,$2,$3,$4,false)`,
		uuid.New(), datasetID, l.Kind, mustJSON(map[string]string{"node": l.Node, "path": l.Path}))
	return err
}

// Secrets ---------------------------------------------------------------

func (s *Store) PutSecret(ctx context.Context, ns, name string, blob []byte, meta map[string]any) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO secrets(namespace, name, cipher_blob, metadata) VALUES ($1,$2,$3,$4)
		ON CONFLICT (namespace, name) DO UPDATE SET cipher_blob=excluded.cipher_blob, metadata=excluded.metadata, updated_at=now()`,
		ns, name, blob, mustJSON(meta))
	return err
}

func (s *Store) GetSecretBlob(ctx context.Context, ns, name string) ([]byte, error) {
	var b []byte
	err := s.DB.QueryRow(ctx, `SELECT cipher_blob FROM secrets WHERE namespace=$1 AND name=$2`, ns, name).Scan(&b)
	if err != nil {
		return nil, notFoundOr(err, "secret://%s/%s", ns, name)
	}
	return b, nil
}

func (s *Store) ListSecrets(ctx context.Context) ([]v1.SecretMeta, error) {
	rows, err := s.DB.Query(ctx, `SELECT namespace, name, metadata, created_at, updated_at FROM secrets ORDER BY namespace, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []v1.SecretMeta{}
	for rows.Next() {
		var m v1.SecretMeta
		var meta []byte
		if err := rows.Scan(&m.Namespace, &m.Name, &meta, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		var md struct {
			Description string `json:"description"`
			Size        int    `json:"size"`
		}
		json.Unmarshal(meta, &md)
		m.Description, m.Size = md.Description, md.Size
		m.Ref = "secret://" + m.Namespace + "/" + m.Name
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CountSecrets(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM secrets`).Scan(&n)
	return n, err
}

func (s *Store) DeleteSecret(ctx context.Context, ns, name string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM secrets WHERE namespace=$1 AND name=$2`, ns, name)
	if err == nil && tag.RowsAffected() == 0 {
		return NotFound("secret://%s/%s", ns, name)
	}
	return err
}

// IsNotFound / IsConflict helpers for callers.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// SetNodeMaxRuns caps how many runs a node takes at once. 0 = unlimited.
func (s *Store) SetNodeMaxRuns(ctx context.Context, id string, max int) (*v1.Node, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE nodes SET max_runs=$2, updated_at=now() WHERE id=$1`, id, max)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, NotFound("node %q", id)
	}
	return s.GetNode(ctx, id)
}
