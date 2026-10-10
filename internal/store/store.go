// Package store persists the platform's application state in PostgreSQL.
// Every tenant-facing query is scoped by tenant_id. The workflow engine owns
// workflow history; this package owns the business representation.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepakppatil/astrion/internal/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// NotifyChannel is the Postgres channel used to wake event subscribers.
const NotifyChannel = "astrion_job_events"

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxConns < 10 {
		cfg.MaxConns = 10
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies embedded migrations in order under an advisory lock so that
// concurrent replicas do not race.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7246001)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7246001)`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %s", e.Name())
		}
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, v).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version, name) VALUES ($1,$2)`, v, e.Name()); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// InTx runs fn in a transaction.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AppendEvent writes an ordered job event and schedules a notification that
// is delivered only if the transaction commits (transactional outbox).
func AppendEvent(ctx context.Context, q querier, tenantID, jobID, typ string, payload any) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := q.QueryRow(ctx,
		`INSERT INTO job_events (tenant_id, job_id, type, payload) VALUES ($1,$2,$3,$4) RETURNING id`,
		tenantID, jobID, typ, b).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := q.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, jobID); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) AppendEvent(ctx context.Context, tenantID, jobID, typ string, payload any) (int64, error) {
	return AppendEvent(ctx, s.Pool, tenantID, jobID, typ, payload)
}

// ListEvents returns events after afterID (exclusive) for a tenant's job.
func (s *Store) ListEvents(ctx context.Context, tenantID, jobID string, afterID int64, types []string, limit int) ([]domain.JobEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	q := `SELECT id, tenant_id, job_id, type, payload, created_at FROM job_events
	      WHERE tenant_id=$1 AND job_id=$2 AND id > $3`
	args := []any{tenantID, jobID, afterID}
	if len(types) > 0 {
		q += ` AND type = ANY($4)`
		args = append(args, types)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT %d`, limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.JobEvent
	for rows.Next() {
		var e domain.JobEvent
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.JobID, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = payload
		out = append(out, e)
	}
	return out, rows.Err()
}

// Audit records an audit event. Never include secret values in details.
func Audit(ctx context.Context, q querier, a domain.AuditEvent) error {
	details := a.Details
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_events (tenant_id, actor, actor_type, action, target_type, target_id, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, a.TenantID, a.Actor, a.ActorType, a.Action, a.TargetType, a.TargetID, []byte(details))
	return err
}

func (s *Store) Audit(ctx context.Context, a domain.AuditEvent) error { return Audit(ctx, s.Pool, a) }

func (s *Store) ListAudit(ctx context.Context, tenantID string, beforeID int64, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, tenant_id, actor, actor_type, action, target_type, target_id, details, created_at
		FROM audit_events WHERE tenant_id=$1 AND id < $2 ORDER BY id DESC LIMIT $3`, tenantID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var a domain.AuditEvent
		var d []byte
		if err := rows.Scan(&a.ID, &a.TenantID, &a.Actor, &a.ActorType, &a.Action, &a.TargetType, &a.TargetID, &d, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Details = d
		out = append(out, a)
	}
	return out, rows.Err()
}

func AuditDetails(kv ...any) json.RawMessage {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Errorf(domain.ErrNotFound, "%s", what)
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func jsonb(v any) []byte {
	if v == nil {
		return []byte(`{}`)
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return []byte(`{}`)
		}
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
