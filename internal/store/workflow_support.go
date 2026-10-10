package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

// RecordedInteractions returns interactions persisted by a supervisor that
// the workflow has not yet opened (status RECORDED), oldest first.
func (s *Store) RecordedInteractions(ctx context.Context, jobID string) ([]domain.Interaction, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+interactionCols+` FROM interactions WHERE job_id=$1 AND status='RECORDED' ORDER BY created_at, id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Interaction
	for rows.Next() {
		it, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// ArtifactTypes returns the distinct USER artifact types recorded for a job.
func (s *Store) ArtifactTypes(ctx context.Context, jobID string) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT type, count(*) FROM artifacts WHERE job_id=$1 AND classification='USER' GROUP BY type`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// JobExecutions lists every execution attempt of a job (internal use).
func (s *Store) JobExecutions(ctx context.Context, jobID string) ([]domain.ExecutionEnvironment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+execCols+` FROM execution_environments WHERE job_id=$1 ORDER BY attempt`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExecutionEnvironment
	for rows.Next() {
		e, err := scanExec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// JobCancelRequested reports whether cancellation was requested.
func (s *Store) JobCancelRequested(ctx context.Context, jobID string) (bool, error) {
	var req bool
	err := s.Pool.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM jobs WHERE id=$1`, jobID).Scan(&req)
	return req, notFound(err, "job")
}

// ResolveJobCleanup marks terminal jobs' cleanup COMPLETED once every
// execution of the job has been released (used by the reconciler).
func (s *Store) ResolveJobCleanup(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE jobs j SET cleanup_status='COMPLETED', updated_at=now()
		WHERE j.cleanup_status IN ('FAILED','PENDING') AND j.state IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')
		AND NOT EXISTS (SELECT 1 FROM execution_environments e WHERE e.job_id=j.id AND e.cleanup_status <> 'COMPLETED' AND e.provider_ref <> '')`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AppendLogBatch stores a supervisor log batch as a job event. Batches are
// deduplicated per (execution, seq) so supervisor retries are harmless.
func (s *Store) AppendLogBatch(ctx context.Context, tenantID, jobID, execID string, seq int64, payload any) (bool, error) {
	appended := false
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		// Serialize per execution so the existence check is race-free.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "logs:"+execID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND type=$2
			AND payload->>'executionId'=$3 AND (payload->>'seq')::bigint=$4)`, jobID, domain.EventLogBatch, execID, seq).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		appended = true
		_, err := AppendEvent(ctx, tx, tenantID, jobID, domain.EventLogBatch, payload)
		return err
	})
	return appended, err
}
