package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

const execCols = `id, tenant_id, job_id, attempt, provider, provider_ref, status, interaction_mode, exit_code, result_status,
	summary, error_message, failure_class, pending_interaction_id, checkpoint_artifact_id, resume_interaction_id, cleanup_status,
	cleanup_error, expires_at, created_at, updated_at, finished_at`

func scanExec(row pgx.Row) (*domain.ExecutionEnvironment, error) {
	var e domain.ExecutionEnvironment
	var status, mode, failure string
	err := row.Scan(&e.ID, &e.TenantID, &e.JobID, &e.Attempt, &e.Provider, &e.ProviderRef, &status, &mode, &e.ExitCode,
		&e.ResultStatus, &e.Summary, &e.ErrorMessage, &failure, &e.PendingInteractionID, &e.CheckpointArtifactID, &e.ResumeInteractionID,
		&e.CleanupStatus, &e.CleanupError, &e.ExpiresAt, &e.CreatedAt, &e.UpdatedAt, &e.FinishedAt)
	e.Status, e.InteractionMode = domain.ExecutionStatus(status), domain.InteractionMode(mode)
	e.FailureClass = domain.FailureClass(failure)
	return &e, err
}

// HashToken returns the stored representation of an execution token. Tokens
// are random 256-bit values, so a fast hash is appropriate.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateExecution records a new attempt. Idempotent on (job_id, attempt): a
// retried activity gets the existing record back, and the caller must mint a
// fresh token via RotateExecutionToken in that case.
func (s *Store) CreateExecution(ctx context.Context, e *domain.ExecutionEnvironment, tokenHash string, tokenTTL time.Duration) (created bool, err error) {
	if e.ID == "" {
		e.ID = domain.NewID(domain.PrefixExecution)
	}
	e.Status = domain.ExecProvisioning
	e.CleanupStatus = domain.CleanupNotStarted
	tag, err := s.Pool.Exec(ctx, `INSERT INTO execution_environments (id, tenant_id, job_id, attempt, provider, status, interaction_mode,
		token_hash, token_expires_at, resume_interaction_id, checkpoint_artifact_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (job_id, attempt) DO NOTHING`,
		e.ID, e.TenantID, e.JobID, e.Attempt, e.Provider, string(e.Status), string(e.InteractionMode), tokenHash,
		time.Now().Add(tokenTTL), e.ResumeInteractionID, e.CheckpointArtifactID, e.ExpiresAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		existing, err := scanExec(s.Pool.QueryRow(ctx, `SELECT `+execCols+` FROM execution_environments WHERE job_id=$1 AND attempt=$2`, e.JobID, e.Attempt))
		if err != nil {
			return false, err
		}
		*e = *existing
		return false, nil
	}
	return true, nil
}

func (s *Store) RotateExecutionToken(ctx context.Context, execID, tokenHash string, ttl time.Duration) error {
	_, err := s.Pool.Exec(ctx, `UPDATE execution_environments SET token_hash=$2, token_expires_at=$3, token_revoked_at=NULL WHERE id=$1`,
		execID, tokenHash, time.Now().Add(ttl))
	return err
}

func (s *Store) SetExecutionProviderRef(ctx context.Context, execID, ref string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE execution_environments SET provider_ref=$2, updated_at=now() WHERE id=$1`, execID, ref)
	return err
}

func (s *Store) GetExecution(ctx context.Context, execID string) (*domain.ExecutionEnvironment, error) {
	e, err := scanExec(s.Pool.QueryRow(ctx, `SELECT `+execCols+` FROM execution_environments WHERE id=$1`, execID))
	return e, notFound(err, "execution")
}

func (s *Store) ListExecutions(ctx context.Context, tenantID, jobID string) ([]domain.ExecutionEnvironment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+execCols+` FROM execution_environments WHERE tenant_id=$1 AND job_id=$2 ORDER BY attempt`, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ExecutionEnvironment{}
	for rows.Next() {
		e, err := scanExec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// AuthenticateExecution validates an execution token: it must match, be
// unexpired, and not revoked. Tokens are revoked at cleanup (AUTH-006).
func (s *Store) AuthenticateExecution(ctx context.Context, execID, token string) (*domain.ExecutionEnvironment, error) {
	var hash string
	var expires time.Time
	var revoked *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT token_hash, token_expires_at, token_revoked_at FROM execution_environments WHERE id=$1`, execID).
		Scan(&hash, &expires, &revoked)
	if err != nil {
		return nil, domain.ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(token))) != 1 || revoked != nil || time.Now().After(expires) {
		return nil, domain.ErrUnauthorized
	}
	return s.GetExecution(ctx, execID)
}

func (s *Store) RevokeExecutionToken(ctx context.Context, execID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE execution_environments SET token_revoked_at=COALESCE(token_revoked_at, now()) WHERE id=$1`, execID)
	return err
}

// ExecutionUpdate is a status report from the supervisor or the workflow.
type ExecutionUpdate struct {
	Status               domain.ExecutionStatus
	ExitCode             *int
	ResultStatus         string
	Summary              string
	ErrorMessage         string
	FailureClass         domain.FailureClass
	PendingInteractionID *string
	CheckpointArtifactID string
}

// UpdateExecution applies a status report. Finished executions never move
// back to a running status (stale or replayed reports are ignored).
func (s *Store) UpdateExecution(ctx context.Context, execID string, u ExecutionUpdate) (*domain.ExecutionEnvironment, error) {
	var out *domain.ExecutionEnvironment
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		e, err := scanExec(tx.QueryRow(ctx, `SELECT `+execCols+` FROM execution_environments WHERE id=$1 FOR UPDATE`, execID))
		if err != nil {
			return notFound(err, "execution")
		}
		if e.Status.Finished() && u.Status != "" && !u.Status.Finished() {
			out = e
			return nil
		}
		sets := []string{"updated_at=now()"}
		args := []any{execID}
		add := func(col string, v any) {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s=$%d", col, len(args)))
		}
		if u.Status != "" {
			add("status", string(u.Status))
			if u.Status.Finished() && e.FinishedAt == nil {
				sets = append(sets, "finished_at=now()")
			}
		}
		if u.ExitCode != nil {
			add("exit_code", *u.ExitCode)
		}
		if u.ResultStatus != "" {
			add("result_status", u.ResultStatus)
		}
		if u.Summary != "" {
			add("summary", truncate(u.Summary, 4000))
		}
		if u.ErrorMessage != "" {
			add("error_message", truncate(u.ErrorMessage, 4000))
		}
		if u.FailureClass != "" {
			add("failure_class", string(u.FailureClass))
		}
		if u.PendingInteractionID != nil {
			add("pending_interaction_id", *u.PendingInteractionID)
		}
		if u.CheckpointArtifactID != "" {
			add("checkpoint_artifact_id", u.CheckpointArtifactID)
		}
		if _, err := tx.Exec(ctx, `UPDATE execution_environments SET `+strings.Join(sets, ", ")+` WHERE id=$1`, args...); err != nil {
			return err
		}
		out, err = scanExec(tx.QueryRow(ctx, `SELECT `+execCols+` FROM execution_environments WHERE id=$1`, execID))
		return err
	})
	return out, err
}

func (s *Store) SetExecutionCleanup(ctx context.Context, execID, status, cleanupErr string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE execution_environments SET cleanup_status=$2, cleanup_error=$3,
		cleanup_attempts=cleanup_attempts+1, updated_at=now() WHERE id=$1`, execID, status, truncate(cleanupErr, 2000))
	return err
}

// ExecutionsNeedingCleanup returns executions whose cleanup has not completed
// and which are no longer needed: their job is terminal, or they have expired.
func (s *Store) ExecutionsNeedingCleanup(ctx context.Context, limit int) ([]domain.ExecutionEnvironment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+prefixCols("e", execCols)+` FROM execution_environments e JOIN jobs j ON j.id=e.job_id
		WHERE e.cleanup_status <> 'COMPLETED' AND e.provider_ref <> ''
		AND (j.state IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT') OR e.expires_at < now())
		ORDER BY e.updated_at LIMIT $1`, limit)
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

// ExecutionOwnerState returns the job state for an execution ID (reconciler).
func (s *Store) ExecutionOwnerState(ctx context.Context, execID string) (domain.JobState, string, error) {
	var state, cleanup string
	err := s.Pool.QueryRow(ctx, `SELECT j.state, e.cleanup_status FROM execution_environments e JOIN jobs j ON j.id=e.job_id WHERE e.id=$1`, execID).Scan(&state, &cleanup)
	if err != nil {
		return "", "", notFound(err, "execution")
	}
	return domain.JobState(state), cleanup, nil
}

func (s *Store) RecordUsage(ctx context.Context, e *domain.ExecutionEnvironment, wall time.Duration) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO resource_usage (tenant_id, job_id, execution_id, provider, wall_seconds) VALUES ($1,$2,$3,$4,$5)`,
		e.TenantID, e.JobID, e.ID, e.Provider, wall.Seconds())
	return err
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
