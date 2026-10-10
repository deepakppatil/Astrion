package workflow

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/deepakppatil/astrion/internal/domain"
)

// harness wires stateful fake activities into the Temporal test environment.
// The fakes model the database: tests script what Observe returns.
type harness struct {
	t   *testing.T
	env *testsuite.TestWorkflowEnvironment

	mu           sync.Mutex
	validate     ValidateResult
	observe      func(in ObserveInput, n int) Observation
	observeCalls int
	provisions   []ProvisionInput
	states       []domain.JobState
	opened       []OpenInteractionInput
	expired      []string
	released     []string
	verifyFail   *Failure
	finalized    *FinalizeInput
	cleaned      bool
	capacityFree int // number of AcquireCapacity calls that fail first
	capCalls     int
}

func newHarness(t *testing.T) *harness {
	var ts testsuite.WorkflowTestSuite
	h := &harness{t: t, env: ts.NewTestWorkflowEnvironment()}
	h.validate = ValidateResult{Pool: "local", Mode: domain.ModeCheckpointResume, TimeoutSeconds: 3600,
		HITLMaxWait: 3600, HITLOnTimeout: "NEEDS_INTERVENTION", QueueTimeoutSecs: 3600}
	env := h.env
	env.RegisterActivity(a)
	env.OnActivity(a.Validate, mock.Anything, mock.Anything).Return(func(context.Context, string) (*ValidateResult, error) {
		v := h.validate
		return &v, nil
	})
	env.OnActivity(a.Observe, mock.Anything, mock.Anything).Return(func(_ context.Context, in ObserveInput) (*Observation, error) {
		h.mu.Lock()
		h.observeCalls++
		n := h.observeCalls
		h.mu.Unlock()
		o := h.observe(in, n)
		return &o, nil
	})
	env.OnActivity(a.AcquireCapacity, mock.Anything, mock.Anything, mock.Anything).Return(func(context.Context, string, bool) (*CapacityResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.capCalls++
		if h.capCalls <= h.capacityFree {
			return &CapacityResult{Acquired: false, Reason: "busy"}, nil
		}
		return &CapacityResult{Acquired: true}, nil
	})
	env.OnActivity(a.ReleaseCapacity, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.RequestApproval, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("int_approval", nil)
	env.OnActivity(a.Provision, mock.Anything, mock.Anything).Return(func(_ context.Context, in ProvisionInput) (*ProvisionResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.provisions = append(h.provisions, in)
		return &ProvisionResult{ExecutionID: "exe_" + string(rune('0'+in.Attempt))}, nil
	})
	env.OnActivity(a.OpenInteraction, mock.Anything, mock.Anything).Return(func(_ context.Context, in OpenInteractionInput) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.opened = append(h.opened, in)
		return nil
	})
	env.OnActivity(a.ExpireInteraction, mock.Anything, mock.Anything).Return(func(_ context.Context, id string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.expired = append(h.expired, id)
		return nil
	})
	env.OnActivity(a.SetState, mock.Anything, mock.Anything).Return(func(_ context.Context, in TransitionInput) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.states = append(h.states, in.To)
		return nil
	})
	env.OnActivity(a.ReleaseEnvironment, mock.Anything, mock.Anything).Return(func(_ context.Context, id string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.released = append(h.released, id)
		return nil
	})
	env.OnActivity(a.VerifyOutputs, mock.Anything, mock.Anything, mock.Anything).Return(func(context.Context, string, string) (*Failure, error) {
		return h.verifyFail, nil
	})
	env.OnActivity(a.BeginCleanup, mock.Anything, mock.Anything).Return(func(_ context.Context, in FinalizeInput) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.finalized = &in
		return nil
	})
	env.OnActivity(a.Cleanup, mock.Anything, mock.Anything).Return(func(context.Context, string) (*CleanupResult, error) {
		h.cleaned = true
		return &CleanupResult{}, nil
	})
	env.OnActivity(a.Complete, mock.Anything, mock.Anything).Return(nil)
	return h
}

func (h *harness) run() {
	h.t.Helper()
	h.env.ExecuteWorkflow(JobWorkflow, JobInput{JobID: "job_1"})
	if !h.env.IsWorkflowCompleted() {
		h.t.Fatal("workflow did not complete")
	}
	if err := h.env.GetWorkflowError(); err != nil {
		h.t.Fatalf("workflow error: %v", err)
	}
	if h.finalized == nil || !h.cleaned {
		h.t.Fatal("job was not finalized with cleanup")
	}
}

func (h *harness) expectOutcome(o domain.Outcome, class domain.FailureClass) {
	h.t.Helper()
	if h.finalized.Outcome != o {
		h.t.Fatalf("outcome = %s, want %s (failure %+v)", h.finalized.Outcome, o, h.finalized.Failure)
	}
	got := domain.FailureClass("")
	if h.finalized.Failure != nil {
		got = h.finalized.Failure.Class
	}
	if got != class {
		h.t.Fatalf("failure class = %q, want %q (%+v)", got, class, h.finalized.Failure)
	}
}

func TestHappyPathNoHITL(t *testing.T) {
	h := newHarness(t)
	h.capacityFree = 2 // queued twice before admission
	h.observe = func(in ObserveInput, n int) Observation {
		if in.ExecutionID == "" {
			return Observation{JobState: domain.JobQueued}
		}
		if n < 5 {
			return Observation{JobState: domain.JobProvisioning, ExecStatus: domain.ExecRunning}
		}
		return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecCompleted}
	}
	h.run()
	h.expectOutcome(domain.OutcomeSucceeded, "")
	if len(h.provisions) != 1 || h.provisions[0].Attempt != 1 {
		t.Fatalf("provisions: %+v", h.provisions)
	}
	if h.states[0] != domain.JobRunning {
		t.Fatalf("expected transition to RUNNING first, got %v", h.states)
	}
}

func TestMissingArtifactsFailsJob(t *testing.T) {
	h := newHarness(t)
	h.verifyFail = &Failure{Class: domain.FailureOutputValidation, Message: "required artifact(s) missing: specification"}
	h.observe = func(in ObserveInput, n int) Observation {
		return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecCompleted}
	}
	h.run()
	h.expectOutcome(domain.OutcomeFailed, domain.FailureOutputValidation)
}

func TestCheckpointHITLResume(t *testing.T) {
	h := newHarness(t)
	h.observe = func(in ObserveInput, n int) Observation {
		switch {
		case in.InteractionID == "int_1":
			if n < 8 {
				return Observation{JobState: domain.JobWaitingForInput, InteractionStatus: domain.InteractionOpen}
			}
			return Observation{JobState: domain.JobWaitingForInput, InteractionStatus: domain.InteractionResolved, InteractionDecision: domain.DecisionAnswer}
		case in.ExecutionID == "exe_1":
			if n < 4 {
				return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecRunning,
					Recorded: []RecordedInteraction{{ID: "int_1", Kind: domain.InteractionMultipleChoice}}}
			}
			return Observation{JobState: domain.JobWaitingForInput, ExecStatus: domain.ExecSuspended, PendingInteract: "int_1", Checkpoint: "art_cp"}
		case in.ExecutionID == "exe_2":
			return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecCompleted}
		}
		return Observation{JobState: domain.JobQueued}
	}
	// Simulate the API nudging the workflow when the user responds.
	h.env.RegisterDelayedCallback(func() { h.env.SignalWorkflow(SignalNudge, "interaction.resolved") }, 10*time.Minute)
	h.run()
	h.expectOutcome(domain.OutcomeSucceeded, "")
	if len(h.provisions) != 2 {
		t.Fatalf("expected 2 attempts, got %+v", h.provisions)
	}
	p2 := h.provisions[1]
	if p2.Attempt != 2 || p2.ResumeInteractionID != "int_1" || p2.CheckpointArtifact != "art_cp" {
		t.Fatalf("second attempt must resume from checkpoint: %+v", p2)
	}
	if len(h.opened) != 1 || h.opened[0].InteractionID != "int_1" || h.opened[0].ExpiresAt == nil {
		t.Fatalf("interaction must be opened once with expiry: %+v", h.opened)
	}
	if len(h.released) == 0 || h.released[0] != "exe_1" {
		t.Fatalf("suspended environment must be released: %v", h.released)
	}
}

func TestCancelWhileWaitingForInput(t *testing.T) {
	h := newHarness(t)
	var cancelled bool
	h.env.RegisterDelayedCallback(func() { cancelled = true; h.env.SignalWorkflow(SignalNudge, "cancel") }, time.Hour)
	h.observe = func(in ObserveInput, n int) Observation {
		o := Observation{JobState: domain.JobWaitingForInput, CancelRequested: cancelled}
		if in.ExecutionID != "" {
			o.ExecStatus, o.PendingInteract = domain.ExecSuspended, "int_1"
			o.Recorded = []RecordedInteraction{{ID: "int_1", Kind: domain.InteractionClarification}}
		}
		if in.InteractionID != "" {
			o.InteractionStatus = domain.InteractionOpen
		}
		return o
	}
	h.run()
	h.expectOutcome(domain.OutcomeCancelled, domain.FailureCancelled)
}

func TestApprovalGateRejected(t *testing.T) {
	h := newHarness(t)
	h.validate.NeedsApproval = true
	h.validate.ApprovalPrompt = "Approve /Create-Jira?"
	h.observe = func(in ObserveInput, n int) Observation {
		if in.InteractionID == "int_approval" {
			return Observation{JobState: domain.JobWaitingForApproval, InteractionStatus: domain.InteractionResolved,
				InteractionDecision: domain.DecisionReject, InteractionComment: "not this sprint"}
		}
		return Observation{JobState: domain.JobValidating}
	}
	h.run()
	h.expectOutcome(domain.OutcomeFailed, domain.FailureInteraction)
	if len(h.provisions) != 0 {
		t.Fatal("no compute may be provisioned before approval")
	}
}

func TestHITLTimeoutNeedsInterventionThenReopen(t *testing.T) {
	h := newHarness(t)
	h.validate.HITLMaxWait = 600
	reopened := false
	h.env.RegisterDelayedCallback(func() {
		reopened = true
		h.env.SignalWorkflow(SignalOperator, OperatorSignal{Action: OperatorReopen, Actor: "admin"})
	}, 2*time.Hour)
	h.observe = func(in ObserveInput, n int) Observation {
		switch {
		case in.InteractionID == "int_1":
			if reopened {
				return Observation{JobState: domain.JobWaitingForInput, InteractionStatus: domain.InteractionResolved, InteractionDecision: domain.DecisionAnswer}
			}
			return Observation{JobState: domain.JobWaitingForInput, InteractionStatus: domain.InteractionOpen}
		case in.ExecutionID == "exe_1":
			return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecSuspended, PendingInteract: "int_1",
				Recorded: []RecordedInteraction{{ID: "int_1", Kind: domain.InteractionClarification}}}
		case in.ExecutionID == "exe_2":
			return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecCompleted}
		}
		return Observation{JobState: domain.JobNeedsIntervention}
	}
	h.run()
	h.expectOutcome(domain.OutcomeSucceeded, "")
	if len(h.expired) != 1 {
		t.Fatalf("interaction should have expired once: %v", h.expired)
	}
	foundNI := false
	for _, s := range h.states {
		if s == domain.JobNeedsIntervention {
			foundNI = true
		}
	}
	if !foundNI {
		t.Fatalf("expected NEEDS_INTERVENTION, states=%v", h.states)
	}
	if len(h.opened) != 2 {
		t.Fatalf("expected original open + reopen, got %+v", h.opened)
	}
}

func TestLostEnvironmentRetry(t *testing.T) {
	h := newHarness(t)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(SignalOperator, OperatorSignal{Action: OperatorRetry, Actor: "admin"})
	}, time.Hour)
	h.observe = func(in ObserveInput, n int) Observation {
		switch in.ExecutionID {
		case "exe_1":
			return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecRunning, ProviderGone: true, ProviderMessage: "pod evicted"}
		case "exe_2":
			return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecCompleted}
		}
		return Observation{JobState: domain.JobNeedsIntervention}
	}
	h.run()
	h.expectOutcome(domain.OutcomeSucceeded, "")
	if len(h.provisions) != 2 || h.provisions[1].ResumeInteractionID != "" {
		t.Fatalf("retry must be a fresh attempt: %+v", h.provisions)
	}
}

func TestExecutionTimeout(t *testing.T) {
	h := newHarness(t)
	h.validate.TimeoutSeconds = 120
	h.observe = func(in ObserveInput, n int) Observation {
		return Observation{JobState: domain.JobRunning, ExecStatus: domain.ExecRunning}
	}
	h.run()
	h.expectOutcome(domain.OutcomeTimedOut, domain.FailureTimeout)
}

func TestValidationFailure(t *testing.T) {
	h := newHarness(t)
	h.validate = ValidateResult{Failure: &Failure{Class: domain.FailureAuthorization, Message: "missing permission"}}
	h.observe = func(ObserveInput, int) Observation { return Observation{} }
	h.run()
	h.expectOutcome(domain.OutcomeFailed, domain.FailureAuthorization)
}
