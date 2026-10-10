package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

const jobCols = `id, tenant_id, workspace_id, initiated_by, phase, command, command_id, agent_profile_id, agent_profile_version,
	framework_id, framework_revision, requested_provider, selected_provider, inputs, workflow_id, state, current_step,
	pending_outcome, failure_class, failure_message, cleanup_status, attempt, retry_of, parent_job_id, links,
	cancel_requested_at, created_at, updated_at, started_at, finished_at`

func scanJob(row pgx.Row) (*domain.Job, error) {
	var j domain.Job
	var phase, state, outcome, fclass string
	var inputs, links []byte
	err := row.Scan(&j.ID, &j.TenantID, &j.WorkspaceID, &j.InitiatedBy, &phase, &j.Command, &j.CommandID, &j.AgentProfileID,
		&j.AgentProfileVersion, &j.FrameworkID, &j.FrameworkRevision, &j.RequestedProvider, &j.SelectedProvider, &inputs,
		&j.WorkflowID, &state, &j.CurrentStep, &outcome, &fclass, &j.FailureMessage, &j.CleanupStatus, &j.Attempt,
		&j.RetryOf, &j.ParentJobID, &links, &j.CancelRequestedAt, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return nil, err
	}
	j.Phase, j.State, j.PendingOutcome, j.FailureClass = domain.Phase(phase), domain.JobState(state), domain.Outcome(outcome), domain.FailureClass(fclass)
	j.Inputs = inputs
	_ = json.Unmarshal(links, &j.Links)
	return &j, nil
}

// NewJob carries creation parameters that are not part of domain.Job.
type NewJob struct {
	Job            *domain.Job
	CommandVersion int
	IdempotencyKey string
	RequestHash    string
}

// CreateJob durably accepts a job. With an idempotency key, a replay of the
// same request returns the original job (created=false); a different request
// with the same key is a conflict.
func (s *Store) CreateJob(ctx context.Context, n NewJob) (job *domain.Job, created bool, err error) {
	j := n.Job
	if n.IdempotencyKey != "" {
		existing, hash, err := s.jobByIdempotencyKey(ctx, j.TenantID, j.InitiatedBy, n.IdempotencyKey)
		if err == nil {
			if hash != n.RequestHash {
				return nil, false, domain.Errorf(domain.ErrConflict, "idempotency key reused with a different request")
			}
			return existing, false, nil
		}
	}
	if j.ID == "" {
		j.ID = domain.NewID(domain.PrefixJob)
	}
	j.WorkflowID = "job-" + j.ID
	j.State = domain.JobSubmitted
	j.CleanupStatus = domain.CleanupNotStarted
	var idem any
	if n.IdempotencyKey != "" {
		idem = n.IdempotencyKey
	}
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO jobs (id, tenant_id, workspace_id, initiated_by, phase, command, command_id, command_version,
			agent_profile_id, agent_profile_version, framework_id, framework_revision, requested_provider, inputs, workflow_id,
			state, cleanup_status, retry_of, parent_job_id, links, idempotency_key, request_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) RETURNING created_at, updated_at`,
			j.ID, j.TenantID, j.WorkspaceID, j.InitiatedBy, string(j.Phase), j.Command, j.CommandID, n.CommandVersion,
			j.AgentProfileID, j.AgentProfileVersion, j.FrameworkID, j.FrameworkRevision, j.RequestedProvider, jsonb(j.Inputs),
			j.WorkflowID, string(j.State), j.CleanupStatus, j.RetryOf, j.ParentJobID, jsonb(j.Links), idem, n.RequestHash,
		).Scan(&j.CreatedAt, &j.UpdatedAt); err != nil {
			return err
		}
		for i, r := range j.Repositories {
			if _, err := tx.Exec(ctx, `INSERT INTO job_repositories (job_id, repository_id, url, requested_revision, position) VALUES ($1,$2,$3,$4,$5)`,
				j.ID, r.RepositoryID, r.URL, r.RequestedRevision, i); err != nil {
				return err
			}
		}
		if _, err := AppendEvent(ctx, tx, j.TenantID, j.ID, domain.EventJobStatusChanged, map[string]any{
			"from": "", "to": j.State, "step": "submitted",
		}); err != nil {
			return err
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: j.TenantID, Actor: j.InitiatedBy, ActorType: "user", Action: "job.submit",
			TargetType: "job", TargetID: j.ID, Details: AuditDetails("command", j.Command, "phase", j.Phase, "workspaceId", j.WorkspaceID,
				"agentProfileId", j.AgentProfileID, "agentProfileVersion", j.AgentProfileVersion, "frameworkRevision", j.FrameworkRevision)})
	})
	if err != nil {
		if isUniqueViolation(err) && n.IdempotencyKey != "" {
			// Lost a race with a concurrent identical submission.
			existing, hash, gerr := s.jobByIdempotencyKey(ctx, j.TenantID, j.InitiatedBy, n.IdempotencyKey)
			if gerr == nil && hash == n.RequestHash {
				return existing, false, nil
			}
			return nil, false, domain.Errorf(domain.ErrConflict, "idempotency key reused with a different request")
		}
		return nil, false, err
	}
	return j, true, nil
}

func (s *Store) jobByIdempotencyKey(ctx context.Context, tenantID, userID, key string) (*domain.Job, string, error) {
	var id, hash string
	if err := s.Pool.QueryRow(ctx, `SELECT id, request_hash FROM jobs WHERE tenant_id=$1 AND initiated_by=$2 AND idempotency_key=$3`,
		tenantID, userID, key).Scan(&id, &hash); err != nil {
		return nil, "", err
	}
	j, err := s.GetJob(ctx, tenantID, id)
	return j, hash, err
}

// GetJob loads a job scoped to a tenant.
func (s *Store) GetJob(ctx context.Context, tenantID, id string) (*domain.Job, error) {
	j, err := scanJob(s.Pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if err != nil {
		return nil, notFound(err, "job")
	}
	j.Repositories, err = s.jobRepositories(ctx, j.ID)
	return j, err
}

// GetJobInternal loads a job without tenant scoping. Only for the workflow
// worker, which receives job IDs from durable workflow input.
func (s *Store) GetJobInternal(ctx context.Context, id string) (*domain.Job, error) {
	j, err := scanJob(s.Pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=$1`, id))
	if err != nil {
		return nil, notFound(err, "job")
	}
	j.Repositories, err = s.jobRepositories(ctx, j.ID)
	return j, err
}

func (s *Store) JobCommandVersion(ctx context.Context, jobID string) (int, error) {
	var v int
	err := s.Pool.QueryRow(ctx, `SELECT command_version FROM jobs WHERE id=$1`, jobID).Scan(&v)
	return v, notFound(err, "job")
}

func (s *Store) jobRepositories(ctx context.Context, jobID string) ([]domain.JobRepository, error) {
	rows, err := s.Pool.Query(ctx, `SELECT repository_id, url, requested_revision, resolved_commit, checkout_path, working_branch,
		final_commit, change_request_url FROM job_repositories WHERE job_id=$1 ORDER BY position`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.JobRepository{}
	for rows.Next() {
		var r domain.JobRepository
		if err := rows.Scan(&r.RepositoryID, &r.URL, &r.RequestedRevision, &r.ResolvedCommit, &r.CheckoutPath, &r.WorkingBranch,
			&r.FinalCommit, &r.ChangeRequestURL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateJobRepository records the exact revision used (IMPL-006, TEST-005).
func (s *Store) UpdateJobRepository(ctx context.Context, jobID string, r domain.JobRepository) error {
	_, err := s.Pool.Exec(ctx, `UPDATE job_repositories SET resolved_commit=COALESCE(NULLIF($3,''), resolved_commit),
		checkout_path=COALESCE(NULLIF($4,''), checkout_path), working_branch=COALESCE(NULLIF($5,''), working_branch),
		final_commit=COALESCE(NULLIF($6,''), final_commit), change_request_url=COALESCE(NULLIF($7,''), change_request_url)
		WHERE job_id=$1 AND repository_id=$2`, jobID, r.RepositoryID, r.ResolvedCommit, r.CheckoutPath, r.WorkingBranch, r.FinalCommit, r.ChangeRequestURL)
	return err
}

type JobFilter struct {
	TenantID    string
	WorkspaceID string
	States      []domain.JobState
	InitiatedBy string
	Limit       int
	Before      *time.Time
}

func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]domain.Job, error) {
	where := []string{"tenant_id=$1"}
	args := []any{f.TenantID}
	if f.WorkspaceID != "" {
		args = append(args, f.WorkspaceID)
		where = append(where, fmt.Sprintf("workspace_id=$%d", len(args)))
	}
	if len(f.States) > 0 {
		st := make([]string, len(f.States))
		for i, s := range f.States {
			st[i] = string(s)
		}
		args = append(args, st)
		where = append(where, fmt.Sprintf("state = ANY($%d)", len(args)))
	}
	if f.InitiatedBy != "" {
		args = append(args, f.InitiatedBy)
		where = append(where, fmt.Sprintf("initiated_by=$%d", len(args)))
	}
	if f.Before != nil {
		args = append(args, *f.Before)
		where = append(where, fmt.Sprintf("created_at < $%d", len(args)))
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Transition describes a job state change and its accompanying mutations.
// All of it — state, fields, events, and the optional hook — commits atomically.
type Transition struct {
	JobID            string
	To               domain.JobState
	Step             string
	PendingOutcome   domain.Outcome
	FailureClass     domain.FailureClass
	FailureMessage   string
	SelectedProvider string
	Attempt          int
	CleanupStatus    string
	Details          map[string]any
	// Hook runs inside the transaction after the state update.
	Hook func(ctx context.Context, tx pgx.Tx, job *domain.Job) error
}

// TransitionJob applies a legal state transition. It is idempotent: if the job
// is already in the target state, field updates are applied without emitting
// a duplicate status event, so activity retries are safe.
func (s *Store) TransitionJob(ctx context.Context, t Transition) (*domain.Job, error) {
	var out *domain.Job
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=$1 FOR UPDATE`, t.JobID))
		if err != nil {
			return notFound(err, "job")
		}
		from := j.State
		same := from == t.To
		if !same {
			if err := domain.ValidateTransition(from, t.To); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		sets := []string{"state=$2", "updated_at=$3"}
		args := []any{j.ID, string(t.To), now}
		add := func(col string, v any) {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s=$%d", col, len(args)))
		}
		if t.Step != "" {
			add("current_step", t.Step)
			j.CurrentStep = t.Step
		}
		if t.PendingOutcome != "" {
			add("pending_outcome", string(t.PendingOutcome))
			j.PendingOutcome = t.PendingOutcome
		}
		if t.FailureClass != "" && j.FailureClass == "" {
			// First failure wins; later cleanup problems must not overwrite the cause.
			add("failure_class", string(t.FailureClass))
			add("failure_message", truncate(t.FailureMessage, 4000))
			j.FailureClass, j.FailureMessage = t.FailureClass, t.FailureMessage
		}
		if t.SelectedProvider != "" {
			add("selected_provider", t.SelectedProvider)
			j.SelectedProvider = t.SelectedProvider
		}
		if t.Attempt > 0 {
			add("attempt", t.Attempt)
			j.Attempt = t.Attempt
		}
		if t.CleanupStatus != "" {
			add("cleanup_status", t.CleanupStatus)
			j.CleanupStatus = t.CleanupStatus
		}
		if t.To == domain.JobProvisioning && j.StartedAt == nil {
			add("started_at", now)
			j.StartedAt = &now
		}
		if t.To.Terminal() && !same {
			add("finished_at", now)
			sets = append(sets, "capacity_held=FALSE")
			j.FinishedAt = &now
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET `+strings.Join(sets, ", ")+` WHERE id=$1`, args...); err != nil {
			return err
		}
		j.State, j.UpdatedAt = t.To, now
		if !same {
			payload := map[string]any{"from": from, "to": t.To, "step": j.CurrentStep}
			if j.PendingOutcome != "" {
				payload["outcome"] = j.PendingOutcome
			}
			if t.FailureClass != "" {
				payload["failureClass"] = t.FailureClass
				payload["failureMessage"] = t.FailureMessage
			}
			for k, v := range t.Details {
				payload[k] = v
			}
			if _, err := AppendEvent(ctx, tx, j.TenantID, j.ID, domain.EventJobStatusChanged, payload); err != nil {
				return err
			}
			if t.To.Terminal() {
				typ := domain.EventJobCompleted
				if t.To != domain.JobSucceeded {
					typ = domain.EventJobFailed
				}
				if _, err := AppendEvent(ctx, tx, j.TenantID, j.ID, typ, map[string]any{
					"state": t.To, "failureClass": j.FailureClass, "failureMessage": j.FailureMessage, "cleanupStatus": j.CleanupStatus,
				}); err != nil {
					return err
				}
			}
		} else if t.Step != "" {
			if _, err := AppendEvent(ctx, tx, j.TenantID, j.ID, domain.EventStepChanged, map[string]any{"step": t.Step}); err != nil {
				return err
			}
		}
		if t.Hook != nil {
			if err := t.Hook(ctx, tx, j); err != nil {
				return err
			}
		}
		out = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Repositories, err = s.jobRepositories(ctx, out.ID)
	return out, err
}

// SetJobStep records progress without a state change.
func (s *Store) SetJobStep(ctx context.Context, jobID, step string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		var tenant, cur string
		if err := tx.QueryRow(ctx, `UPDATE jobs SET current_step=$2, updated_at=now() WHERE id=$1 RETURNING tenant_id, current_step`, jobID, step).Scan(&tenant, &cur); err != nil {
			return notFound(err, "job")
		}
		_, err := AppendEvent(ctx, tx, tenant, jobID, domain.EventStepChanged, map[string]any{"step": step})
		return err
	})
}

func (s *Store) SetJobCleanupStatus(ctx context.Context, jobID, status string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE jobs SET cleanup_status=$2, updated_at=now() WHERE id=$1`, jobID, status)
	return err
}

func (s *Store) MarkWorkflowStarted(ctx context.Context, jobID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE jobs SET workflow_started=TRUE WHERE id=$1`, jobID)
	return err
}

// UnstartedJobs returns jobs accepted but whose workflow start was not
// confirmed (e.g. API crashed between commit and start). A dispatcher
// re-issues the idempotent start for these.
func (s *Store) UnstartedJobs(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Job, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE workflow_started=FALSE AND state='SUBMITTED'
		AND created_at < $1 ORDER BY created_at LIMIT $2`, time.Now().Add(-olderThan), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// RequestCancel records a cancellation request (the workflow performs it).
func (s *Store) RequestCancel(ctx context.Context, tenantID, jobID, actor string) (*domain.Job, error) {
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE jobs SET cancel_requested_at=COALESCE(cancel_requested_at, now()), updated_at=now()
			WHERE tenant_id=$1 AND id=$2`, tenantID, jobID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.ErrNotFound, "job")
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: tenantID, Actor: actor, ActorType: "user", Action: "job.cancel",
			TargetType: "job", TargetID: jobID})
	})
	if err != nil {
		return nil, err
	}
	return s.GetJob(ctx, tenantID, jobID)
}

// --- Admission control and capacity ---

// CountRecentSubmissions counts a user's submissions within a window.
func (s *Store) CountRecentSubmissions(ctx context.Context, userID string, window time.Duration) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE initiated_by=$1 AND created_at > $2`, userID, time.Now().Add(-window)).Scan(&n)
	return n, err
}

// CountQueued counts a tenant's jobs that have not yet acquired capacity.
func (s *Store) CountQueued(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE tenant_id=$1 AND state IN ('SUBMITTED','VALIDATING','QUEUED')`, tenantID).Scan(&n)
	return n, err
}

// CountNonTerminal counts all in-flight jobs platform-wide (global admission).
func (s *Store) CountNonTerminal(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state NOT IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')`).Scan(&n)
	return n, err
}

// AcquireCapacity atomically reserves one of the tenant's concurrent job slots.
// Serialization is per tenant via a row lock on the tenant record.
func (s *Store) AcquireCapacity(ctx context.Context, jobID string) (bool, string, error) {
	acquired := false
	reason := ""
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var tenantID string
		var held bool
		if err := tx.QueryRow(ctx, `SELECT tenant_id, capacity_held FROM jobs WHERE id=$1`, jobID).Scan(&tenantID, &held); err != nil {
			return notFound(err, "job")
		}
		if held {
			acquired = true
			return nil
		}
		var quotas []byte
		if err := tx.QueryRow(ctx, `SELECT quotas FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&quotas); err != nil {
			return err
		}
		var q domain.TenantQuotas
		_ = json.Unmarshal(quotas, &q)
		max := q.MaxConcurrentJobs
		if max <= 0 {
			max = 10
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE tenant_id=$1 AND capacity_held`, tenantID).Scan(&active); err != nil {
			return err
		}
		if active >= max {
			reason = fmt.Sprintf("tenant concurrency limit reached (%d/%d)", active, max)
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET capacity_held=TRUE WHERE id=$1`, jobID); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return acquired, reason, err
}

func (s *Store) ReleaseCapacity(ctx context.Context, jobID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE jobs SET capacity_held=FALSE WHERE id=$1`, jobID)
	return err
}

// DashboardStats summarizes a tenant's jobs.
type DashboardStats struct {
	Active          int            `json:"active"`
	AwaitingInput   int            `json:"awaitingInput"`
	Failed24h       int            `json:"failed24h"`
	Succeeded24h    int            `json:"succeeded24h"`
	AvgDurationSec  float64        `json:"avgDurationSeconds"`
	ByState         map[string]int `json:"byState"`
	CleanupFailures int            `json:"cleanupFailures"`
}

func (s *Store) Dashboard(ctx context.Context, tenantID string) (*DashboardStats, error) {
	st := &DashboardStats{ByState: map[string]int{}}
	rows, err := s.Pool.Query(ctx, `SELECT state, count(*) FROM jobs WHERE tenant_id=$1 GROUP BY state`, tenantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		st.ByState[state] = n
		js := domain.JobState(state)
		if js.Waiting() || js == domain.JobNeedsIntervention {
			st.AwaitingInput += n
		}
		if !js.Terminal() {
			st.Active += n
		}
	}
	rows.Close()
	err = s.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state IN ('FAILED','TIMED_OUT')),
		count(*) FILTER (WHERE state='SUCCEEDED'),
		COALESCE(avg(EXTRACT(EPOCH FROM finished_at - created_at)) FILTER (WHERE finished_at IS NOT NULL), 0),
		count(*) FILTER (WHERE cleanup_status='FAILED')
		FROM jobs WHERE tenant_id=$1 AND created_at > now() - interval '24 hours'`, tenantID).
		Scan(&st.Failed24h, &st.Succeeded24h, &st.AvgDurationSec, &st.CleanupFailures)
	return st, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
