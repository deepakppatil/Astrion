package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
)

// ExecConfig configures execution environments.
type ExecConfig struct {
	// APIURL is the control-plane URL reachable from execution environments.
	APIURL string
	// DefaultImage is used when the agent profile has no runtime image.
	DefaultImage string
	// SupervisorCommand launches the in-environment supervisor.
	SupervisorCommand []string
	TokenTTLSlack     time.Duration
	NetworkProfile    string
	ServiceAccount    string
	// Env is extra non-secret environment passed to every supervisor.
	Env map[string]string
}

// Activities holds dependencies for all job activities.
type Activities struct {
	Store  *store.Store
	Pools  *provider.Registry
	Agents *agent.Registry
	Exec   ExecConfig
	Log    *slog.Logger
}

func (a *Activities) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func fail(class domain.FailureClass, format string, args ...any) *Failure {
	return &Failure{Class: class, Message: fmt.Sprintf(format, args...)}
}

// nonRetryable wraps programming/invariant errors that retrying cannot fix.
func nonRetryable(err error) error {
	var inv *domain.InvalidTransitionError
	if errors.As(err, &inv) || errors.Is(err, domain.ErrNotFound) {
		return temporal.NewNonRetryableApplicationError(err.Error(), "INVARIANT", err)
	}
	return err
}

func (a *Activities) loadCommand(ctx context.Context, job *domain.Job) (*registry.Command, error) {
	ver, err := a.Store.JobCommandVersion(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	cmd, err := a.Store.GetCommand(ctx, job.CommandID, ver)
	if err != nil {
		return nil, err
	}
	c := cmd.WithDefaults()
	return &c, nil
}

// Validate re-checks everything at execution time (authorization may have
// changed since submission), resolves the interaction mode and selects a
// compliant execution pool.
func (a *Activities) Validate(ctx context.Context, jobID string) (*ValidateResult, error) {
	s := a.Store
	job, err := s.GetJobInternal(ctx, jobID)
	if err != nil {
		return nil, nonRetryable(err)
	}
	if job.State == domain.JobSubmitted {
		if job, err = s.TransitionJob(ctx, store.Transition{JobID: jobID, To: domain.JobValidating, Step: "validating"}); err != nil {
			return nil, nonRetryable(err)
		}
	}
	cmd, err := a.loadCommand(ctx, job)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "command definition unavailable: %v", err)}, nil
	}
	user, err := s.GetUser(ctx, job.TenantID, job.InitiatedBy)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureAuthorization, "initiating user no longer exists")}, nil
	}
	p := &authz.Principal{UserID: user.ID, TenantID: user.TenantID, Subject: user.Subject, Roles: user.Roles}
	if err := registry.Authorize(p, cmd); err != nil {
		return &ValidateResult{Failure: fail(domain.FailureAuthorization, "%v", err)}, nil
	}
	profile, err := s.GetAgentProfile(ctx, job.TenantID, job.AgentProfileID, job.AgentProfileVersion)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "agent profile unavailable: %v", err)}, nil
	}
	if !profile.Enabled {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "agent profile %s is disabled", profile.Name)}, nil
	}
	if !cmd.SupportsAgent(profile.Provider) {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "command %s is not compatible with agent %s", cmd.Canonical, profile.Provider)}, nil
	}
	adapter, err := a.Agents.Get(profile.Provider)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "%v", err)}, nil
	}
	if err := adapter.ValidateProfile(*profile); err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "agent profile invalid: %v", err)}, nil
	}
	mode, err := agent.ResolveMode(adapter, *profile)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "%v", err)}, nil
	}
	ws, err := s.GetWorkspace(ctx, job.TenantID, job.WorkspaceID)
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailureValidation, "workspace unavailable: %v", err)}, nil
	}
	tenant, err := s.GetTenant(ctx, job.TenantID)
	if err != nil {
		return nil, err
	}
	pool, err := provider.Select(a.Pools.All(), provider.SelectionRequest{
		Requested: job.RequestedProvider, DataClassification: ws.DataClassification,
		TenantPermitted: tenant.PermittedProviders, WorkspacePermitted: ws.PermittedProviders,
	})
	if err != nil {
		return &ValidateResult{Failure: fail(domain.FailurePolicy, "%v", err)}, nil
	}
	if _, err := s.TransitionJob(ctx, store.Transition{JobID: jobID, To: domain.JobValidating, SelectedProvider: pool.Name}); err != nil {
		return nil, nonRetryable(err)
	}
	res := &ValidateResult{Pool: pool.Name, Mode: mode, TimeoutSeconds: cmd.TimeoutSeconds,
		HITLMaxWait: cmd.HITL.MaxWaitSeconds, HITLOnTimeout: string(cmd.HITL.OnTimeout), QueueTimeoutSecs: 24 * 3600}
	if cmd.Approval.BeforeExecution || cmd.RequiresProductionApproval() {
		res.NeedsApproval = true
		res.ApproverRole = cmd.Approval.ApproverRole
		res.ApprovalPrompt = approvalPrompt(cmd, job, ws)
	}
	return res, nil
}

func approvalPrompt(cmd *registry.Command, job *domain.Job, ws *domain.Workspace) string {
	var effects []string
	for _, e := range cmd.SideEffects {
		if e != registry.EffectNone {
			effects = append(effects, string(e))
		}
	}
	sort.Strings(effects)
	msg := fmt.Sprintf("Approve execution of %s in workspace %q?", cmd.Canonical, ws.Name)
	if len(effects) > 0 {
		msg += " Declared side effects: " + strings.Join(effects, ", ") + "."
	}
	return msg
}

// RequestApproval records and opens a platform approval gate before any
// compute is provisioned.
func (a *Activities) RequestApproval(ctx context.Context, jobID, prompt string, role domain.Role, expiresAt *time.Time) (string, error) {
	job, err := a.Store.GetJobInternal(ctx, jobID)
	if err != nil {
		return "", nonRetryable(err)
	}
	it, err := a.Store.RecordInteraction(ctx, &domain.Interaction{TenantID: job.TenantID, JobID: jobID,
		AgentRequestID: "platform-approval", StepID: "approval-gate", Kind: domain.InteractionApproval,
		Prompt: prompt, RequestedRole: role})
	if err != nil {
		return "", err
	}
	if _, err := a.Store.TransitionJob(ctx, store.Transition{JobID: jobID, To: domain.JobWaitingForApproval,
		Step: "awaiting approval", Hook: store.OpenInteractionHook(it.ID, expiresAt)}); err != nil {
		return "", nonRetryable(err)
	}
	return it.ID, nil
}

// AcquireCapacity reserves a tenant concurrency slot. With enqueue=true the
// job is first moved to QUEUED.
func (a *Activities) AcquireCapacity(ctx context.Context, jobID string, enqueue bool) (*CapacityResult, error) {
	if enqueue {
		if _, err := a.Store.TransitionJob(ctx, store.Transition{JobID: jobID, To: domain.JobQueued, Step: "waiting for capacity"}); err != nil {
			return nil, nonRetryable(err)
		}
	}
	ok, reason, err := a.Store.AcquireCapacity(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &CapacityResult{Acquired: ok, Reason: reason}, nil
}

func (a *Activities) ReleaseCapacity(ctx context.Context, jobID string) error {
	return a.Store.ReleaseCapacity(ctx, jobID)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Provision creates the execution record and allocates the environment.
// Idempotent per (job, attempt).
func (a *Activities) Provision(ctx context.Context, in ProvisionInput) (*ProvisionResult, error) {
	s := a.Store
	job, err := s.TransitionJob(ctx, store.Transition{JobID: in.JobID, To: domain.JobProvisioning,
		Step: fmt.Sprintf("provisioning attempt %d", in.Attempt), Attempt: in.Attempt})
	if err != nil {
		return nil, nonRetryable(err)
	}
	pool, err := a.Pools.Get(in.Pool)
	if err != nil {
		return &ProvisionResult{Failure: fail(domain.FailureProvisioning, "%v", err)}, nil
	}
	profile, err := s.GetAgentProfile(ctx, job.TenantID, job.AgentProfileID, job.AgentProfileVersion)
	if err != nil {
		return nil, err
	}
	cmd, err := a.loadCommand(ctx, job)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	e := &domain.ExecutionEnvironment{TenantID: job.TenantID, JobID: job.ID, Attempt: in.Attempt, Provider: pool.Name,
		InteractionMode: in.Mode, ResumeInteractionID: in.ResumeInteractionID, CheckpointArtifactID: in.CheckpointArtifact,
		ExpiresAt: time.Now().Add(timeout + time.Hour)}
	ttl := timeout + a.Exec.TokenTTLSlack + 15*time.Minute
	created, err := s.CreateExecution(ctx, e, store.HashToken(token), ttl)
	if err != nil {
		return nil, err
	}
	owner := provider.Ownership{TenantID: job.TenantID, JobID: job.ID, WorkflowID: WorkflowID(job.ID),
		ExecutionID: e.ID, Attempt: in.Attempt, ExpiresAt: e.ExpiresAt}
	if !created {
		if e.ProviderRef != "" {
			return &ProvisionResult{ExecutionID: e.ID}, nil // already allocated
		}
		// A previous try may have partially allocated with an unknown token:
		// tear it down, then mint a new token.
		_ = pool.Provider.Terminate(ctx, provider.Ref{Provider: pool.Name, ID: e.ID})
		if err := s.RotateExecutionToken(ctx, e.ID, store.HashToken(token), ttl); err != nil {
			return nil, err
		}
	}
	capacity, err := pool.Provider.CheckCapacity(ctx, provider.Spec{Owner: owner})
	if err != nil {
		return nil, err
	}
	if !capacity.Available {
		// Retryable: Temporal backs off; the activity's retry budget bounds it.
		return nil, temporal.NewApplicationError("execution pool at capacity: "+capacity.Reason, "CAPACITY")
	}
	image := profile.RuntimeImage
	if image == "" {
		image = a.Exec.DefaultImage
	}
	res := profile.Resources
	if cmd.Resources != nil {
		res = *cmd.Resources
	}
	env := map[string]string{"ASTRION_API_URL": a.Exec.APIURL, "ASTRION_EXECUTION_ID": e.ID}
	for k, v := range a.Exec.Env {
		env[k] = v
	}
	spec := provider.Spec{Owner: owner, Image: image, Command: a.Exec.SupervisorCommand, Env: env,
		Secrets:   map[string][]byte{"execution-token": []byte(token)},
		Resources: res, ActiveDeadline: timeout + 5*time.Minute,
		ServiceAccount: a.Exec.ServiceAccount, NetworkProfile: a.Exec.NetworkProfile}
	ref, err := pool.Provider.Allocate(ctx, spec)
	if err != nil {
		if activity.GetInfo(ctx).Attempt >= 5 {
			return &ProvisionResult{ExecutionID: e.ID, Failure: fail(domain.FailureProvisioning, "allocate environment: %v", err)}, nil
		}
		return nil, err
	}
	if err := s.SetExecutionProviderRef(ctx, e.ID, ref.ID); err != nil {
		return nil, err
	}
	return &ProvisionResult{ExecutionID: e.ID}, nil
}

// Observe returns a snapshot of job, execution, provider, and interaction
// facts. It has no side effects.
func (a *Activities) Observe(ctx context.Context, in ObserveInput) (*Observation, error) {
	s := a.Store
	job, err := s.GetJobInternal(ctx, in.JobID)
	if err != nil {
		return nil, nonRetryable(err)
	}
	obs := &Observation{JobState: job.State, CancelRequested: job.CancelRequestedAt != nil}
	if in.ExecutionID != "" {
		e, err := s.GetExecution(ctx, in.ExecutionID)
		if err != nil {
			return nil, err
		}
		obs.ExecStatus, obs.ExecError, obs.ExecFailure = e.Status, e.ErrorMessage, e.FailureClass
		obs.PendingInteract, obs.Checkpoint = e.PendingInteractionID, e.CheckpointArtifactID
		if !e.Status.Finished() && e.ProviderRef != "" {
			if pool, err := a.Pools.Get(e.Provider); err == nil {
				st, err := pool.Provider.Status(ctx, provider.Ref{Provider: e.Provider, ID: e.ProviderRef})
				if err == nil {
					switch st.Phase {
					case provider.PhaseSucceeded, provider.PhaseFailed, provider.PhaseNotFound:
						obs.ProviderGone, obs.ProviderMessage = true, fmt.Sprintf("environment %s: %s %s", e.ProviderRef, st.Phase, st.Message)
					}
				}
			}
		}
		recorded, err := s.RecordedInteractions(ctx, in.JobID)
		if err != nil {
			return nil, err
		}
		for _, it := range recorded {
			if it.ExecutionID == in.ExecutionID {
				obs.Recorded = append(obs.Recorded, RecordedInteraction{ID: it.ID, Kind: it.Kind, Decision: it.Kind.IsDecision()})
			}
		}
	}
	if in.InteractionID != "" {
		it, err := s.GetInteractionInternal(ctx, in.InteractionID)
		if err != nil {
			return nil, err
		}
		obs.InteractionStatus = it.Status
		if it.Response != nil {
			obs.InteractionDecision, obs.InteractionComment, obs.InteractionBy = it.Response.Decision, it.Response.Comment, it.Response.RespondedBy
		}
	}
	return obs, nil
}

// OpenInteraction atomically opens an interaction and moves the job into a
// waiting state; the user-facing notification is emitted in the same
// transaction.
func (a *Activities) OpenInteraction(ctx context.Context, in OpenInteractionInput) error {
	to := domain.JobWaitingForInput
	if in.Decision {
		to = domain.JobWaitingForApproval
	}
	_, err := a.Store.TransitionJob(ctx, store.Transition{JobID: in.JobID, To: to, Step: "awaiting human response",
		Hook: store.OpenInteractionHook(in.InteractionID, in.ExpiresAt)})
	return nonRetryable(err)
}

func (a *Activities) ExpireInteraction(ctx context.Context, interactionID string) error {
	return a.Store.ExpireInteraction(ctx, interactionID)
}

// SetState applies a plain transition.
func (a *Activities) SetState(ctx context.Context, in TransitionInput) error {
	t := store.Transition{JobID: in.JobID, To: in.To, Step: in.Step}
	if in.Failure != nil {
		t.FailureClass, t.FailureMessage = in.Failure.Class, in.Failure.Message
	}
	_, err := a.Store.TransitionJob(ctx, t)
	return nonRetryable(err)
}

// ReleaseEnvironment terminates an execution environment and revokes its
// token. Safe to call repeatedly (LIFE-002).
func (a *Activities) ReleaseEnvironment(ctx context.Context, executionID string) error {
	return a.releaseEnvironment(ctx, executionID)
}

func (a *Activities) releaseEnvironment(ctx context.Context, executionID string) error {
	s := a.Store
	e, err := s.GetExecution(ctx, executionID)
	if err != nil {
		return err
	}
	if err := s.RevokeExecutionToken(ctx, e.ID); err != nil {
		return err
	}
	if e.ProviderRef != "" {
		pool, err := a.Pools.Get(e.Provider)
		if err != nil {
			_ = s.SetExecutionCleanup(ctx, e.ID, domain.CleanupFailed, err.Error())
			return err
		}
		if err := pool.Provider.Terminate(ctx, provider.Ref{Provider: e.Provider, ID: e.ProviderRef}); err != nil {
			_ = s.SetExecutionCleanup(ctx, e.ID, domain.CleanupFailed, err.Error())
			return fmt.Errorf("terminate %s: %w", e.ProviderRef, err)
		}
	}
	if !e.Status.Finished() {
		if _, err := s.UpdateExecution(ctx, e.ID, store.ExecutionUpdate{Status: domain.ExecTerminated}); err != nil {
			return err
		}
	}
	if e.CleanupStatus != domain.CleanupCompleted {
		end := time.Now()
		if e.FinishedAt != nil {
			end = *e.FinishedAt
		}
		_ = s.RecordUsage(ctx, e, end.Sub(e.CreatedAt))
	}
	return s.SetExecutionCleanup(ctx, e.ID, domain.CleanupCompleted, "")
}

// VerifyOutputs checks completion criteria: a successful agent result and
// every required artifact (SRS 12.2: exit code alone is not success).
func (a *Activities) VerifyOutputs(ctx context.Context, jobID, executionID string) (*Failure, error) {
	s := a.Store
	job, err := s.TransitionJob(ctx, store.Transition{JobID: jobID, To: domain.JobCollectingArtifact, Step: "verifying outputs"})
	if err != nil {
		return nil, nonRetryable(err)
	}
	e, err := s.GetExecution(ctx, executionID)
	if err != nil {
		return nil, err
	}
	if e.ResultStatus != "success" {
		return fail(domain.FailureAgent, "agent did not report success (result=%q)", e.ResultStatus), nil
	}
	cmd, err := a.loadCommand(ctx, job)
	if err != nil {
		return nil, err
	}
	have, err := s.ArtifactTypes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, ex := range cmd.ExpectedArtifacts {
		if ex.Required && have[ex.Type] == 0 {
			missing = append(missing, ex.Type)
		}
	}
	if len(missing) > 0 {
		return fail(domain.FailureOutputValidation, "required artifact(s) missing: %s", strings.Join(missing, ", ")), nil
	}
	return nil, nil
}

// BeginCleanup records the decided outcome and enters CLEANING_UP.
func (a *Activities) BeginCleanup(ctx context.Context, in FinalizeInput) error {
	t := store.Transition{JobID: in.JobID, To: domain.JobCleaningUp, Step: "cleaning up",
		PendingOutcome: in.Outcome, CleanupStatus: domain.CleanupPending}
	if in.Failure != nil {
		t.FailureClass, t.FailureMessage = in.Failure.Class, in.Failure.Message
	}
	_, err := a.Store.TransitionJob(ctx, t)
	return nonRetryable(err)
}

// Cleanup releases every environment, token, interaction, and capacity slot
// of the job. Failures are recorded and handed to the reconciler; they never
// change the job outcome (LIFE-003).
func (a *Activities) Cleanup(ctx context.Context, jobID string) (*CleanupResult, error) {
	s := a.Store
	if err := s.CancelOpenInteractions(ctx, jobID); err != nil {
		return nil, err
	}
	execs, err := s.JobExecutions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	res := &CleanupResult{}
	for _, e := range execs {
		if e.CleanupStatus == domain.CleanupCompleted {
			continue
		}
		if err := a.releaseEnvironment(ctx, e.ID); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.ID, err))
		}
	}
	if err := s.ReleaseCapacity(ctx, jobID); err != nil {
		res.Errors = append(res.Errors, "release capacity: "+err.Error())
	}
	if len(res.Errors) > 0 && activity.GetInfo(ctx).Attempt < 3 {
		return nil, errors.New(strings.Join(res.Errors, "; "))
	}
	status := domain.CleanupCompleted
	if len(res.Errors) > 0 {
		status = domain.CleanupFailed
		job, _ := s.GetJobInternal(ctx, jobID)
		if job != nil {
			_, _ = s.AppendEvent(ctx, job.TenantID, jobID, domain.EventCleanupFailed, map[string]any{"errors": res.Errors})
		}
		a.log().Warn("cleanup incomplete; reconciler will retry", "job", jobID, "errors", res.Errors)
	}
	if err := s.SetJobCleanupStatus(ctx, jobID, status); err != nil {
		return nil, err
	}
	return res, nil
}

// Complete moves the job from CLEANING_UP to the terminal state matching
// its pending outcome.
func (a *Activities) Complete(ctx context.Context, in FinalizeInput) error {
	to := map[domain.Outcome]domain.JobState{
		domain.OutcomeSucceeded: domain.JobSucceeded, domain.OutcomeFailed: domain.JobFailed,
		domain.OutcomeCancelled: domain.JobCancelled, domain.OutcomeTimedOut: domain.JobTimedOut,
	}[in.Outcome]
	if to == "" {
		to = domain.JobFailed
	}
	_, err := a.Store.TransitionJob(ctx, store.Transition{JobID: in.JobID, To: to, Step: "done"})
	return nonRetryable(err)
}
