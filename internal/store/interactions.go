package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

const interactionCols = `id, tenant_id, job_id, execution_id, step_id, agent_request_id, kind, prompt, options, schema,
	requested_user, requested_role, status, created_at, opened_at, expires_at, resolved_at`

func scanInteraction(row pgx.Row) (*domain.Interaction, error) {
	var it domain.Interaction
	var kind, role, status string
	var opts, schema []byte
	err := row.Scan(&it.ID, &it.TenantID, &it.JobID, &it.ExecutionID, &it.StepID, &it.AgentRequestID, &kind, &it.Prompt,
		&opts, &schema, &it.RequestedUser, &role, &status, &it.CreatedAt, &it.OpenedAt, &it.ExpiresAt, &it.ResolvedAt)
	if err != nil {
		return nil, err
	}
	it.Kind, it.RequestedRole, it.Status = domain.InteractionKind(kind), domain.Role(role), domain.InteractionStatus(status)
	_ = json.Unmarshal(opts, &it.Options)
	_ = json.Unmarshal(schema, &it.Schema)
	return &it, nil
}

// RecordInteraction persists an interaction request in RECORDED status. It is
// idempotent per (job, execution, agent request id) so supervisor retries do
// not create duplicates. Users are not notified until the workflow opens it.
func (s *Store) RecordInteraction(ctx context.Context, it *domain.Interaction) (*domain.Interaction, error) {
	if it.ID == "" {
		it.ID = domain.NewID(domain.PrefixInteraction)
	}
	if !it.Kind.Valid() {
		return nil, domain.Invalid("kind", "unknown interaction kind %q", it.Kind)
	}
	opts, _ := json.Marshal(it.Options)
	if it.Options == nil {
		opts = []byte(`[]`)
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO interactions (id, tenant_id, job_id, execution_id, step_id, agent_request_id, kind, prompt,
		options, schema, requested_user, requested_role, status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'RECORDED')
		ON CONFLICT (job_id, execution_id, agent_request_id) DO NOTHING`,
		it.ID, it.TenantID, it.JobID, it.ExecutionID, it.StepID, it.AgentRequestID, string(it.Kind), truncate(it.Prompt, 20000),
		opts, jsonb(it.Schema), it.RequestedUser, string(it.RequestedRole))
	if err != nil {
		return nil, err
	}
	return scanInteraction(s.Pool.QueryRow(ctx, `SELECT `+interactionCols+` FROM interactions WHERE job_id=$1 AND execution_id=$2 AND agent_request_id=$3`,
		it.JobID, it.ExecutionID, it.AgentRequestID))
}

// OpenInteractionHook returns a Transition hook that opens the interaction in
// the same transaction that moves the job into a waiting state, then emits the
// user-facing notification event. This guarantees the question and waiting
// state are persisted before the frontend is told (SRS 28).
func OpenInteractionHook(interactionID string, expiresAt *time.Time) func(context.Context, pgx.Tx, *domain.Job) error {
	return func(ctx context.Context, tx pgx.Tx, job *domain.Job) error {
		it, err := scanInteraction(tx.QueryRow(ctx, `UPDATE interactions SET status='OPEN', opened_at=COALESCE(opened_at, now()), expires_at=$3
			WHERE id=$1 AND job_id=$2 AND status IN ('RECORDED','OPEN','EXPIRED') RETURNING `+interactionCols, interactionID, job.ID, expiresAt))
		if err != nil {
			return notFound(err, "open interaction")
		}
		typ := domain.EventInputRequired
		if it.Kind.IsDecision() {
			typ = domain.EventApprovalRequired
		}
		_, err = AppendEvent(ctx, tx, job.TenantID, job.ID, typ, it)
		return err
	}
}

func (s *Store) loadResponse(ctx context.Context, it *domain.Interaction) error {
	var r domain.InteractionResponse
	var decision string
	err := s.Pool.QueryRow(ctx, `SELECT id, interaction_id, decision, response_values, text, comment, responded_by, idempotency_key, created_at
		FROM interaction_responses WHERE interaction_id=$1`, it.ID).Scan(&r.ID, &r.InteractionID, &decision, &r.Values, &r.Text, &r.Comment,
		&r.RespondedBy, &r.IdempotencyKey, &r.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	r.Decision = domain.Decision(decision)
	it.Response = &r
	return nil
}

func (s *Store) GetInteraction(ctx context.Context, tenantID, id string) (*domain.Interaction, error) {
	it, err := scanInteraction(s.Pool.QueryRow(ctx, `SELECT `+interactionCols+` FROM interactions WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if err != nil {
		return nil, notFound(err, "interaction")
	}
	return it, s.loadResponse(ctx, it)
}

func (s *Store) GetInteractionInternal(ctx context.Context, id string) (*domain.Interaction, error) {
	it, err := scanInteraction(s.Pool.QueryRow(ctx, `SELECT `+interactionCols+` FROM interactions WHERE id=$1`, id))
	if err != nil {
		return nil, notFound(err, "interaction")
	}
	return it, s.loadResponse(ctx, it)
}

func (s *Store) ListInteractions(ctx context.Context, tenantID, jobID string) ([]domain.Interaction, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+interactionCols+` FROM interactions WHERE tenant_id=$1 AND job_id=$2 ORDER BY created_at`, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	var out []domain.Interaction
	for rows.Next() {
		it, err := scanInteraction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *it)
	}
	rows.Close()
	for i := range out {
		if err := s.loadResponse(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []domain.Interaction{}
	}
	return out, nil
}

// ListOpenInteractions returns a tenant's open interactions (dashboard inbox).
func (s *Store) ListOpenInteractions(ctx context.Context, tenantID string, limit int) ([]domain.Interaction, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+interactionCols+` FROM interactions WHERE tenant_id=$1 AND status='OPEN' ORDER BY opened_at LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Interaction{}
	for rows.Next() {
		it, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// ResolveInteraction records a response exactly once. Replays with the same
// idempotency key return the original (replayed=true); any other submission
// to a non-open interaction is a conflict, and late ones are expired.
func (s *Store) ResolveInteraction(ctx context.Context, tenantID, id string, resp *domain.InteractionResponse) (it *domain.Interaction, replayed bool, err error) {
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanInteraction(tx.QueryRow(ctx, `SELECT `+interactionCols+` FROM interactions WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, id))
		if err != nil {
			return notFound(err, "interaction")
		}
		switch cur.Status {
		case domain.InteractionResolved:
			var key string
			_ = tx.QueryRow(ctx, `SELECT idempotency_key FROM interaction_responses WHERE interaction_id=$1`, id).Scan(&key)
			if resp.IdempotencyKey != "" && key == resp.IdempotencyKey {
				replayed = true
				return nil
			}
			return domain.Errorf(domain.ErrConflict, "interaction already resolved")
		case domain.InteractionExpired:
			return domain.Errorf(domain.ErrExpired, "interaction expired")
		case domain.InteractionOpen:
		default:
			return domain.Errorf(domain.ErrConflict, "interaction is %s", cur.Status)
		}
		if cur.ExpiresAt != nil && time.Now().After(*cur.ExpiresAt) {
			return domain.Errorf(domain.ErrExpired, "interaction expired at %s", cur.ExpiresAt.Format(time.RFC3339))
		}
		if resp.ID == "" {
			resp.ID = domain.NewID(domain.PrefixResponse)
		}
		resp.InteractionID = id
		if err := tx.QueryRow(ctx, `INSERT INTO interaction_responses (id, tenant_id, interaction_id, decision, response_values, text, comment,
			responded_by, idempotency_key) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
			resp.ID, tenantID, id, string(resp.Decision), nz(resp.Values), truncate(resp.Text, 50000), truncate(resp.Comment, 5000),
			resp.RespondedBy, resp.IdempotencyKey).Scan(&resp.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE interactions SET status='RESOLVED', resolved_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err := AppendEvent(ctx, tx, tenantID, cur.JobID, domain.EventInteractionResolved, map[string]any{
			"interactionId": id, "decision": resp.Decision, "respondedBy": resp.RespondedBy,
		}); err != nil {
			return err
		}
		action := "interaction.respond"
		switch resp.Decision {
		case domain.DecisionApprove:
			action = "interaction.approve"
		case domain.DecisionReject:
			action = "interaction.reject"
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: tenantID, Actor: resp.RespondedBy, ActorType: "user",
			Action:     action,
			TargetType: "interaction", TargetID: id, Details: AuditDetails("jobId", cur.JobID, "kind", cur.Kind, "decision", resp.Decision)})
	})
	if err != nil {
		return nil, false, err
	}
	it, err = s.GetInteraction(ctx, tenantID, id)
	return it, replayed, err
}

// ExpireInteraction marks an open interaction expired (HITL timeout).
func (s *Store) ExpireInteraction(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		var tenant, job string
		err := tx.QueryRow(ctx, `UPDATE interactions SET status='EXPIRED' WHERE id=$1 AND status='OPEN' RETURNING tenant_id, job_id`, id).Scan(&tenant, &job)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = AppendEvent(ctx, tx, tenant, job, domain.EventInteractionResolved, map[string]any{"interactionId": id, "status": "EXPIRED"})
		return err
	})
}

// CancelOpenInteractions cancels pending interactions of a job.
func (s *Store) CancelOpenInteractions(ctx context.Context, jobID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE interactions SET status='CANCELLED' WHERE job_id=$1 AND status IN ('RECORDED','OPEN')`, jobID)
	return err
}
