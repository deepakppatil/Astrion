package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/testutil"
)

type fixture struct {
	s      *store.Store
	tenant *domain.Tenant
	user   *domain.User
	ws     *domain.Workspace
	agent  *domain.AgentProfile
	cmd    *registry.Command
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate must be idempotent: %v", err)
	}
	f := &fixture{s: s}
	f.tenant = &domain.Tenant{Name: "acme", Quotas: domain.TenantQuotas{MaxConcurrentJobs: 2}}
	must(t, s.CreateTenant(ctx, f.tenant))
	f.user = &domain.User{TenantID: f.tenant.ID, Subject: "alice", Roles: []domain.Role{domain.RoleDeveloper}}
	must(t, s.UpsertUser(ctx, f.user))
	bundles, err := registry.DefaultBundles()
	must(t, err)
	must(t, s.UpsertBundle(ctx, "test", nil, bundles[0]))
	f.cmd, err = s.FindCommand(ctx, bundles[0].Framework.ID, "/implement-task")
	must(t, err)
	f.agent = &domain.AgentProfile{TenantID: f.tenant.ID, Name: "mock", Provider: "mock", Enabled: true}
	must(t, s.SaveAgentProfile(ctx, f.user.ID, f.agent))
	f.ws = &domain.Workspace{TenantID: f.tenant.ID, Name: "ws", Type: domain.WorkspaceMultiRepo,
		LifecycleMode: domain.LifecycleEphemeralCheckout, DataClassification: domain.ClassInternal, CreatedBy: f.user.ID}
	must(t, s.CreateWorkspace(ctx, f.ws))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) newJob(t *testing.T, key, hash string) (*domain.Job, bool) {
	t.Helper()
	j, created, err := f.s.CreateJob(context.Background(), store.NewJob{
		Job: &domain.Job{TenantID: f.tenant.ID, WorkspaceID: f.ws.ID, InitiatedBy: f.user.ID, Phase: domain.PhaseImplement,
			Command: f.cmd.Canonical, CommandID: f.cmd.ID, AgentProfileID: f.agent.ID, AgentProfileVersion: 1,
			FrameworkID: f.cmd.FrameworkID, FrameworkRevision: "v1", RequestedProvider: "AUTO", Inputs: json.RawMessage(`{"taskId":"T-1"}`),
			Repositories: []domain.JobRepository{{RepositoryID: "repo-a", URL: "https://example.com/a.git", RequestedRevision: "main"}}},
		CommandVersion: f.cmd.Version, IdempotencyKey: key, RequestHash: hash,
	})
	must(t, err)
	return j, created
}

func TestJobIdempotency(t *testing.T) {
	f := setup(t)
	a, created := f.newJob(t, "k1", "h1")
	if !created {
		t.Fatal("first submission must create")
	}
	b, created := f.newJob(t, "k1", "h1")
	if created || b.ID != a.ID {
		t.Fatal("replay must return the original job")
	}
	_, _, err := f.s.CreateJob(context.Background(), store.NewJob{Job: &domain.Job{TenantID: f.tenant.ID, WorkspaceID: f.ws.ID,
		InitiatedBy: f.user.ID, Phase: domain.PhasePlan, Command: "/x", CommandID: "x", AgentProfileID: f.agent.ID, FrameworkID: "f", FrameworkRevision: "r"},
		IdempotencyKey: "k1", RequestHash: "different"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("key reuse with different body must conflict, got %v", err)
	}
	if len(a.Repositories) != 1 {
		t.Fatal("repositories not persisted")
	}
}

func TestConcurrentIdempotentSubmissions(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, _ := f.newJob(t, "same-key", "same-hash")
			ids <- j.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("concurrent identical submissions created multiple jobs")
		}
	}
}

func TestTransitions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j, _ := f.newJob(t, "", "")
	_, err := f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobRunning})
	var ite *domain.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("SUBMITTED -> RUNNING must be rejected, got %v", err)
	}
	for _, s := range []domain.JobState{domain.JobValidating, domain.JobQueued, domain.JobProvisioning} {
		if _, err := f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: s, Step: string(s)}); err != nil {
			t.Fatal(err)
		}
	}
	// Idempotent re-application (activity retry) must not duplicate the status event.
	before, _ := f.s.ListEvents(ctx, f.tenant.ID, j.ID, 0, []string{domain.EventJobStatusChanged}, 100)
	if _, err := f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobProvisioning}); err != nil {
		t.Fatal(err)
	}
	after, _ := f.s.ListEvents(ctx, f.tenant.ID, j.ID, 0, []string{domain.EventJobStatusChanged}, 100)
	if len(after) != len(before) {
		t.Fatal("idempotent transition emitted duplicate status event")
	}
	got, _ := f.s.GetJob(ctx, f.tenant.ID, j.ID)
	if got.StartedAt == nil {
		t.Fatal("started_at not set on provisioning")
	}
	// First failure wins.
	_, err = f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobCleaningUp, PendingOutcome: domain.OutcomeFailed,
		FailureClass: domain.FailureAgent, FailureMessage: "agent crashed"})
	must(t, err)
	_, err = f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobFailed, FailureClass: domain.FailureInternal, FailureMessage: "cleanup"})
	must(t, err)
	got, _ = f.s.GetJob(ctx, f.tenant.ID, j.ID)
	if got.FailureClass != domain.FailureAgent || got.FinishedAt == nil {
		t.Fatalf("got %+v", got)
	}
	// Cross-tenant reads look like not found.
	if _, err := f.s.GetJob(ctx, "ten_other", j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	evs, _ := f.s.ListEvents(ctx, "ten_other", j.ID, 0, nil, 10)
	if len(evs) != 0 {
		t.Fatal("events leaked across tenants")
	}
}

func TestCapacity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var jobs []*domain.Job
	for i := 0; i < 3; i++ {
		j, _ := f.newJob(t, "", "")
		jobs = append(jobs, j)
	}
	for i, j := range jobs {
		ok, reason, err := f.s.AcquireCapacity(ctx, j.ID)
		must(t, err)
		if (i < 2) != ok {
			t.Fatalf("job %d acquire=%v reason=%q (quota 2)", i, ok, reason)
		}
	}
	ok, _, _ := f.s.AcquireCapacity(ctx, jobs[0].ID)
	if !ok {
		t.Fatal("re-acquire by holder must be idempotent")
	}
	// Terminal transition releases capacity.
	for _, s := range []domain.JobState{domain.JobValidating, domain.JobCleaningUp, domain.JobCancelled} {
		_, err := f.s.TransitionJob(ctx, store.Transition{JobID: jobs[0].ID, To: s})
		must(t, err)
	}
	ok, _, _ = f.s.AcquireCapacity(ctx, jobs[2].ID)
	if !ok {
		t.Fatal("capacity should be released by terminal state")
	}
}

func TestInteractionLifecycle(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j, _ := f.newJob(t, "", "")
	for _, s := range []domain.JobState{domain.JobValidating, domain.JobQueued, domain.JobProvisioning, domain.JobRunning} {
		_, err := f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: s})
		must(t, err)
	}
	it := &domain.Interaction{TenantID: f.tenant.ID, JobID: j.ID, ExecutionID: "exe_1", AgentRequestID: "q-1",
		Kind: domain.InteractionMultipleChoice, Prompt: "Pick", Options: []domain.InteractionOption{{Value: "a", Label: "A"}}}
	rec, err := f.s.RecordInteraction(ctx, it)
	must(t, err)
	dup, err := f.s.RecordInteraction(ctx, &domain.Interaction{TenantID: f.tenant.ID, JobID: j.ID, ExecutionID: "exe_1", AgentRequestID: "q-1",
		Kind: domain.InteractionMultipleChoice, Prompt: "Pick"})
	must(t, err)
	if dup.ID != rec.ID {
		t.Fatal("recording must be idempotent per agent request")
	}
	// Responses are refused until the workflow opens the interaction.
	_, _, err = f.s.ResolveInteraction(ctx, f.tenant.ID, rec.ID, &domain.InteractionResponse{Decision: domain.DecisionAnswer, Values: []string{"a"}, RespondedBy: f.user.ID})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("response to RECORDED interaction: %v", err)
	}
	exp := time.Now().Add(time.Hour)
	_, err = f.s.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobWaitingForInput, Hook: store.OpenInteractionHook(rec.ID, &exp)})
	must(t, err)
	evs, _ := f.s.ListEvents(ctx, f.tenant.ID, j.ID, 0, []string{domain.EventInputRequired}, 10)
	if len(evs) != 1 {
		t.Fatal("input_required event must be emitted with the waiting transition")
	}
	resp := &domain.InteractionResponse{Decision: domain.DecisionAnswer, Values: []string{"a"}, RespondedBy: f.user.ID, IdempotencyKey: "r1"}
	got, replayed, err := f.s.ResolveInteraction(ctx, f.tenant.ID, rec.ID, resp)
	must(t, err)
	if replayed || got.Status != domain.InteractionResolved || got.Response == nil {
		t.Fatalf("resolve: %+v replayed=%v", got, replayed)
	}
	_, replayed, err = f.s.ResolveInteraction(ctx, f.tenant.ID, rec.ID, &domain.InteractionResponse{Decision: domain.DecisionAnswer, Values: []string{"a"}, RespondedBy: f.user.ID, IdempotencyKey: "r1"})
	if err != nil || !replayed {
		t.Fatalf("idempotent replay: replayed=%v err=%v", replayed, err)
	}
	_, _, err = f.s.ResolveInteraction(ctx, f.tenant.ID, rec.ID, &domain.InteractionResponse{Decision: domain.DecisionAnswer, Values: []string{"b"}, RespondedBy: f.user.ID, IdempotencyKey: "r2"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting submission: %v", err)
	}
	if _, _, err := f.s.ResolveInteraction(ctx, "ten_other", rec.ID, resp); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant response: %v", err)
	}
}

func TestExecutionTokenAuth(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j, _ := f.newJob(t, "", "")
	e := &domain.ExecutionEnvironment{TenantID: f.tenant.ID, JobID: j.ID, Attempt: 1, Provider: "local",
		InteractionMode: domain.ModeCheckpointResume, ExpiresAt: time.Now().Add(time.Hour)}
	created, err := f.s.CreateExecution(ctx, e, store.HashToken("tok"), time.Hour)
	must(t, err)
	if !created {
		t.Fatal("expected creation")
	}
	again := &domain.ExecutionEnvironment{TenantID: f.tenant.ID, JobID: j.ID, Attempt: 1, Provider: "local", ExpiresAt: time.Now().Add(time.Hour)}
	created, err = f.s.CreateExecution(ctx, again, store.HashToken("other"), time.Hour)
	must(t, err)
	if created || again.ID != e.ID {
		t.Fatal("create execution must be idempotent per attempt")
	}
	if _, err := f.s.AuthenticateExecution(ctx, e.ID, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AuthenticateExecution(ctx, e.ID, "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("wrong token accepted")
	}
	must(t, f.s.RevokeExecutionToken(ctx, e.ID))
	if _, err := f.s.AuthenticateExecution(ctx, e.ID, "tok"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("revoked token accepted")
	}
	// Finished executions never regress.
	_, err = f.s.UpdateExecution(ctx, e.ID, store.ExecutionUpdate{Status: domain.ExecCompleted})
	must(t, err)
	got, err := f.s.UpdateExecution(ctx, e.ID, store.ExecutionUpdate{Status: domain.ExecRunning})
	must(t, err)
	if got.Status != domain.ExecCompleted {
		t.Fatal("finished execution regressed to running")
	}
}

func TestCommandVersioning(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	bundles, _ := registry.DefaultBundles()
	must(t, f.s.UpsertBundle(ctx, "test", nil, bundles[0]))
	c, _ := f.s.FindCommand(ctx, bundles[0].Framework.ID, "/implement-task")
	if c.Version != 1 {
		t.Fatalf("unchanged re-registration bumped version to %d", c.Version)
	}
	changed := *c
	changed.Description = "updated"
	must(t, f.s.SaveCommand(ctx, "admin", changed))
	c2, _ := f.s.FindCommand(ctx, bundles[0].Framework.ID, "/implement-task")
	if c2.Version != 2 || c2.Description != "updated" {
		t.Fatalf("got v%d %q", c2.Version, c2.Description)
	}
	old, _ := f.s.GetCommand(ctx, c.ID, 1)
	if old.Description == "updated" {
		t.Fatal("old version mutated")
	}
}
