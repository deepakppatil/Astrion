package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
)

// a is a typed nil used to reference activity methods by name.
var a *Activities

const maxAttempts = 25 // guard against unbounded resume loops

var (
	shortOpts = workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2,
			MaximumInterval: 30 * time.Second, MaximumAttempts: 10, NonRetryableErrorTypes: []string{"INVARIANT"}},
	}
	provisionOpts = workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 2,
			MaximumInterval: time.Minute, MaximumAttempts: 6, NonRetryableErrorTypes: []string{"INVARIANT"}},
	}
	cleanupOpts = workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 2,
			MaximumInterval: time.Minute, MaximumAttempts: 3, NonRetryableErrorTypes: []string{"INVARIANT"}},
	}
)

var cancelledFailure = &Failure{Class: domain.FailureCancelled, Message: "cancelled on request"}

// JobWorkflow orchestrates one job from submission to a terminal state.
// The business outcome is recorded in PostgreSQL; the workflow itself
// completes successfully whenever it managed to finalize the job.
func JobWorkflow(ctx workflow.Context, in JobInput) error {
	r := &run{
		ctx:      ctx,
		jobID:    in.JobID,
		nudge:    workflow.GetSignalChannel(ctx, SignalNudge),
		operator: workflow.GetSignalChannel(ctx, SignalOperator),
	}
	return r.execute()
}

type run struct {
	ctx      workflow.Context
	jobID    string
	v        ValidateResult
	nudge    workflow.ReceiveChannel
	operator workflow.ReceiveChannel
}

func (r *run) call(opts workflow.ActivityOptions, fn any, out any, args ...any) error {
	ctx := workflow.WithActivityOptions(r.ctx, opts)
	return workflow.ExecuteActivity(ctx, fn, args...).Get(ctx, out)
}

func (r *run) now() time.Time { return workflow.Now(r.ctx) }

func (r *run) execute() error {
	if err := r.call(shortOpts, a.Validate, &r.v, r.jobID); err != nil {
		return r.internal(err)
	}
	if r.v.Failure != nil {
		return r.finalize(domain.OutcomeFailed, r.v.Failure)
	}
	if obs, err := r.observe("", ""); err != nil {
		return r.internal(err)
	} else if obs.CancelRequested {
		return r.finalize(domain.OutcomeCancelled, cancelledFailure)
	}
	if r.v.NeedsApproval {
		if done, err := r.approvalGate(); done || err != nil {
			return err
		}
	}
	return r.attempts()
}

// internal finalizes the job as FAILED after an unexpected activity error.
func (r *run) internal(cause error) error {
	workflow.GetLogger(r.ctx).Error("job workflow internal error", "job", r.jobID, "error", cause)
	msg := cause.Error()
	var appErr *temporal.ApplicationError
	if errors.As(cause, &appErr) {
		msg = appErr.Message()
	}
	if err := r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureInternal, Message: msg}); err != nil {
		return fmt.Errorf("%v (finalize also failed: %w)", cause, err)
	}
	return nil
}

func (r *run) observe(execID, interactionID string) (*Observation, error) {
	var obs Observation
	err := r.call(shortOpts, a.Observe, &obs, ObserveInput{JobID: r.jobID, ExecutionID: execID, InteractionID: interactionID})
	return &obs, err
}

// wait blocks until a signal arrives or d elapses. It returns an operator
// signal if one was received and whether the wake-up was signal-driven.
func (r *run) wait(d time.Duration) (*OperatorSignal, bool) {
	if d < time.Second {
		d = time.Second
	}
	tctx, cancel := workflow.WithCancel(r.ctx)
	defer cancel()
	var op *OperatorSignal
	signalled := false
	sel := workflow.NewSelector(r.ctx)
	sel.AddFuture(workflow.NewTimer(tctx, d), func(workflow.Future) {})
	sel.AddReceive(r.nudge, func(c workflow.ReceiveChannel, _ bool) {
		var reason string
		c.Receive(r.ctx, &reason)
		signalled = true
	})
	sel.AddReceive(r.operator, func(c workflow.ReceiveChannel, _ bool) {
		var s OperatorSignal
		c.Receive(r.ctx, &s)
		op, signalled = &s, true
	})
	sel.Select(r.ctx)
	// Coalesce bursts of nudges.
	for {
		var reason string
		if !r.nudge.ReceiveAsync(&reason) {
			break
		}
	}
	return op, signalled
}

// ---------------------------------------------------------------------------
// Approval gate

func (r *run) approvalGate() (done bool, err error) {
	var expires *time.Time
	if r.v.HITLOnTimeout != string(registry.OnTimeoutPause) && r.v.HITLMaxWait > 0 {
		t := r.now().Add(time.Duration(r.v.HITLMaxWait) * time.Second)
		expires = &t
	}
	var iid string
	if err := r.call(shortOpts, a.RequestApproval, &iid, r.jobID, r.v.ApprovalPrompt, r.v.ApproverRole, expires); err != nil {
		return true, r.internal(err)
	}
	h, err := r.awaitHuman(iid, true, false)
	if err != nil {
		return true, r.internal(err)
	}
	switch h.kind {
	case humanResolved:
		if h.decision == domain.DecisionReject {
			msg := "execution approval rejected"
			if h.comment != "" {
				msg += ": " + h.comment
			}
			return true, r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureInteraction, Message: msg})
		}
		return false, nil
	case humanCancelled:
		return true, r.finalize(domain.OutcomeCancelled, cancelledFailure)
	default:
		return true, r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureHITLTimeout, Message: "approval was not given in time"})
	}
}

// ---------------------------------------------------------------------------
// Admission and attempts

type admitResult int

const (
	admitted admitResult = iota
	admitCancelled
	admitTimedOut
)

func (r *run) admit(enqueue bool) (admitResult, error) {
	deadline := r.now().Add(time.Duration(r.v.QueueTimeoutSecs) * time.Second)
	for {
		var c CapacityResult
		if err := r.call(shortOpts, a.AcquireCapacity, &c, r.jobID, enqueue); err != nil {
			return 0, err
		}
		enqueue = false
		if c.Acquired {
			return admitted, nil
		}
		obs, err := r.observe("", "")
		if err != nil {
			return 0, err
		}
		if obs.CancelRequested {
			return admitCancelled, nil
		}
		if r.now().After(deadline) {
			return admitTimedOut, nil
		}
		r.wait(10 * time.Second)
	}
}

func (r *run) attempts() error {
	enqueue := true
	var resumeInteraction, checkpoint string
	for attempt := 1; ; attempt++ {
		if attempt > maxAttempts {
			return r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureInternal, Message: "too many execution attempts"})
		}
		res, err := r.admit(enqueue)
		if err != nil {
			return r.internal(err)
		}
		enqueue = false
		switch res {
		case admitCancelled:
			return r.finalize(domain.OutcomeCancelled, cancelledFailure)
		case admitTimedOut:
			return r.finalize(domain.OutcomeTimedOut, &Failure{Class: domain.FailureQuota, Message: "timed out waiting for execution capacity"})
		}

		var p ProvisionResult
		if err := r.call(provisionOpts, a.Provision, &p, ProvisionInput{JobID: r.jobID, Attempt: attempt, Pool: r.v.Pool,
			Mode: r.v.Mode, TimeoutSeconds: r.v.TimeoutSeconds, ResumeInteractionID: resumeInteraction,
			CheckpointArtifact: checkpoint}); err != nil {
			var appErr *temporal.ApplicationError
			msg := err.Error()
			if errors.As(err, &appErr) {
				msg = appErr.Message()
			}
			return r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureProvisioning, Message: msg})
		}
		if p.Failure != nil {
			return r.finalize(domain.OutcomeFailed, p.Failure)
		}

		m, err := r.monitor(p.ExecutionID)
		if err != nil {
			return r.internal(err)
		}
		switch m.kind {
		case execDone:
			var f *Failure
			if err := r.call(shortOpts, a.VerifyOutputs, &f, r.jobID, p.ExecutionID); err != nil {
				return r.internal(err)
			}
			if f != nil {
				return r.finalize(domain.OutcomeFailed, f)
			}
			return r.finalize(domain.OutcomeSucceeded, nil)

		case execSuspended:
			// Checkpoint/resume: release compute while waiting for the human.
			if err := r.call(cleanupOpts, a.ReleaseEnvironment, nil, p.ExecutionID); err != nil {
				workflow.GetLogger(r.ctx).Warn("release suspended environment failed; reconciler will retry", "error", err)
			}
			if err := r.call(shortOpts, a.ReleaseCapacity, nil, r.jobID); err != nil {
				return r.internal(err)
			}
			h, err := r.awaitHuman(m.interactionID, m.decision, true)
			if err != nil {
				return r.internal(err)
			}
			switch h.kind {
			case humanResolved:
				resumeInteraction, checkpoint = m.interactionID, m.checkpoint
			case humanRetry:
				resumeInteraction, checkpoint = "", ""
			case humanCancelled:
				return r.finalize(domain.OutcomeCancelled, cancelledFailure)
			default:
				return r.finalize(domain.OutcomeFailed, &Failure{Class: domain.FailureHITLTimeout, Message: "human response not received in time"})
			}

		case execLost:
			if err := r.call(cleanupOpts, a.ReleaseEnvironment, nil, p.ExecutionID); err != nil {
				workflow.GetLogger(r.ctx).Warn("release lost environment failed; reconciler will retry", "error", err)
			}
			if err := r.call(shortOpts, a.ReleaseCapacity, nil, r.jobID); err != nil {
				return r.internal(err)
			}
			if err := r.call(shortOpts, a.SetState, nil, TransitionInput{JobID: r.jobID, To: domain.JobNeedsIntervention,
				Step: "needs intervention: execution environment lost (" + m.message + ")"}); err != nil {
				return r.internal(err)
			}
			action, err := r.awaitOperator(OperatorRetry)
			if err != nil {
				return r.internal(err)
			}
			if action == OperatorCancel {
				return r.finalize(domain.OutcomeCancelled, cancelledFailure)
			}
			// A fresh attempt: the lost process is never claimed restored.
			resumeInteraction, checkpoint = "", ""

		case execFailed:
			return r.finalize(domain.OutcomeFailed, m.failure)
		case execTimedOut:
			return r.finalize(domain.OutcomeTimedOut, &Failure{Class: domain.FailureTimeout, Message: "execution exceeded its time limit"})
		case execCancelled:
			return r.finalize(domain.OutcomeCancelled, cancelledFailure)
		}
	}
}

// ---------------------------------------------------------------------------
// Execution monitoring

type execKind int

const (
	execDone execKind = iota
	execSuspended
	execFailed
	execLost
	execTimedOut
	execCancelled
)

type monitorResult struct {
	kind          execKind
	failure       *Failure
	interactionID string
	decision      bool
	checkpoint    string
	message       string
}

func (r *run) setState(to domain.JobState, step string) error {
	return r.call(shortOpts, a.SetState, nil, TransitionInput{JobID: r.jobID, To: to, Step: step})
}

func (r *run) interactionExpiry() *time.Time {
	if r.v.HITLOnTimeout == string(registry.OnTimeoutPause) || r.v.HITLMaxWait <= 0 {
		if r.v.Mode != domain.ModeLiveSession {
			return nil
		}
	}
	wait := time.Duration(r.v.HITLMaxWait) * time.Second
	if r.v.Mode == domain.ModeLiveSession {
		// Live sessions hold compute while waiting, so they are bounded by
		// the execution timeout as well.
		if lim := time.Duration(r.v.TimeoutSeconds) * time.Second; wait <= 0 || wait > lim {
			wait = lim
		}
	}
	t := r.now().Add(wait)
	return &t
}

func (r *run) monitor(execID string) (*monitorResult, error) {
	deadline := r.now().Add(time.Duration(r.v.TimeoutSeconds) * time.Second)
	opened := map[string]bool{}
	decisions := map[string]bool{}
	var live string
	var liveExpiry *time.Time
	gone := 0
	interval := 2 * time.Second
	for {
		obs, err := r.observe(execID, live)
		if err != nil {
			return nil, err
		}
		if obs.CancelRequested {
			return &monitorResult{kind: execCancelled}, nil
		}
		state := obs.JobState
		ensureRunning := func() error {
			if state == domain.JobProvisioning || state == domain.JobPreparingWorkspace {
				if err := r.setState(domain.JobRunning, "running agent"); err != nil {
					return err
				}
				state = domain.JobRunning
			}
			return nil
		}
		switch obs.ExecStatus {
		case domain.ExecPreparing:
			if state == domain.JobProvisioning {
				if err := r.setState(domain.JobPreparingWorkspace, "preparing workspace"); err != nil {
					return nil, err
				}
				state = domain.JobPreparingWorkspace
			}
		case domain.ExecRunning, domain.ExecAwaitingInput:
			if err := ensureRunning(); err != nil {
				return nil, err
			}
		}

		// Open interactions recorded by the supervisor: question and waiting
		// state are persisted before anyone is notified.
		for _, rec := range obs.Recorded {
			if opened[rec.ID] {
				continue
			}
			if err := ensureRunning(); err != nil {
				return nil, err
			}
			exp := r.interactionExpiry()
			if err := r.call(shortOpts, a.OpenInteraction, nil, OpenInteractionInput{JobID: r.jobID, InteractionID: rec.ID,
				Decision: rec.Decision, ExpiresAt: exp}); err != nil {
				return nil, err
			}
			opened[rec.ID], decisions[rec.ID] = true, rec.Decision
			state = domain.JobWaitingForInput
			if r.v.Mode == domain.ModeLiveSession {
				live, liveExpiry = rec.ID, exp
			}
		}

		// Live session: the process is waiting on stdin; the supervisor
		// delivers the response. We only move the job back to RUNNING.
		if live != "" {
			switch {
			case obs.InteractionStatus == domain.InteractionResolved:
				if state.Waiting() {
					if err := r.setState(domain.JobRunning, "response delivered to live session"); err != nil {
						return nil, err
					}
				}
				live, liveExpiry = "", nil
			case liveExpiry != nil && r.now().After(*liveExpiry) && obs.InteractionStatus == domain.InteractionOpen:
				// The supervisor observes EXPIRED and aborts the agent.
				if err := r.call(shortOpts, a.ExpireInteraction, nil, live); err != nil {
					return nil, err
				}
			}
		}

		switch obs.ExecStatus {
		case domain.ExecCompleted:
			if err := ensureRunning(); err != nil {
				return nil, err
			}
			if state.Waiting() {
				if err := r.setState(domain.JobRunning, "finishing"); err != nil {
					return nil, err
				}
			}
			return &monitorResult{kind: execDone}, nil
		case domain.ExecSuspended:
			if obs.PendingInteract == "" || !opened[obs.PendingInteract] {
				return &monitorResult{kind: execFailed, failure: &Failure{Class: domain.FailureInternal,
					Message: "agent suspended without a recorded interaction"}}, nil
			}
			return &monitorResult{kind: execSuspended, interactionID: obs.PendingInteract,
				decision: decisions[obs.PendingInteract], checkpoint: obs.Checkpoint}, nil
		case domain.ExecFailed:
			class := obs.ExecFailure
			if class == "" {
				class = domain.FailureAgent
			}
			if class == domain.FailureCancelled {
				return &monitorResult{kind: execCancelled}, nil
			}
			msg := obs.ExecError
			if msg == "" {
				msg = "agent execution failed"
			}
			return &monitorResult{kind: execFailed, failure: &Failure{Class: class, Message: msg}}, nil
		case domain.ExecTerminated, domain.ExecLost:
			return &monitorResult{kind: execLost, message: string(obs.ExecStatus)}, nil
		}

		if obs.ProviderGone {
			gone++
			if gone >= 2 { // tolerate a completion report racing process exit
				return &monitorResult{kind: execLost, message: obs.ProviderMessage}, nil
			}
		} else {
			gone = 0
		}
		if r.now().After(deadline) {
			return &monitorResult{kind: execTimedOut}, nil
		}
		if _, signalled := r.wait(interval); signalled {
			interval = 2 * time.Second
		} else if interval < 30*time.Second {
			interval *= 2
		}
	}
}

// ---------------------------------------------------------------------------
// Human-in-the-loop waiting (checkpoint mode and approval gate)

type humanKind int

const (
	humanResolved humanKind = iota
	humanCancelled
	humanTimedOut
	humanRetry
)

type humanResult struct {
	kind     humanKind
	decision domain.Decision
	comment  string
}

func (r *run) awaitHuman(iid string, decision, allowRetry bool) (*humanResult, error) {
	policy := registry.HITLTimeoutAction(r.v.HITLOnTimeout)
	maxWait := time.Duration(r.v.HITLMaxWait) * time.Second
	var expiry *time.Time
	if policy != registry.OnTimeoutPause && maxWait > 0 {
		t := r.now().Add(maxWait)
		expiry = &t
	}
	for {
		obs, err := r.observe("", iid)
		if err != nil {
			return nil, err
		}
		if obs.CancelRequested {
			return &humanResult{kind: humanCancelled}, nil
		}
		expired := false
		switch obs.InteractionStatus {
		case domain.InteractionResolved:
			return &humanResult{kind: humanResolved, decision: obs.InteractionDecision, comment: obs.InteractionComment}, nil
		case domain.InteractionCancelled:
			return &humanResult{kind: humanCancelled}, nil
		case domain.InteractionExpired:
			expired = true
		}
		if !expired && expiry != nil && r.now().After(*expiry) {
			if err := r.call(shortOpts, a.ExpireInteraction, nil, iid); err != nil {
				return nil, err
			}
			expired = true
		}
		if expired {
			switch policy {
			case registry.OnTimeoutFail:
				return &humanResult{kind: humanTimedOut}, nil
			default: // NEEDS_INTERVENTION (default): preserve state, never auto-approve
				if err := r.setState(domain.JobNeedsIntervention, "needs intervention: human response timed out"); err != nil {
					return nil, err
				}
				allowed := []string{OperatorReopen}
				if allowRetry {
					allowed = append(allowed, OperatorRetry)
				}
				action, err := r.awaitOperator(allowed...)
				if err != nil {
					return nil, err
				}
				switch action {
				case OperatorCancel:
					return &humanResult{kind: humanCancelled}, nil
				case OperatorRetry:
					return &humanResult{kind: humanRetry}, nil
				}
				t := r.now().Add(maxWait)
				expiry = &t
				if err := r.call(shortOpts, a.OpenInteraction, nil, OpenInteractionInput{JobID: r.jobID, InteractionID: iid,
					Decision: decision, ExpiresAt: expiry}); err != nil {
					return nil, err
				}
				continue
			}
		}
		d := 30 * time.Second
		if expiry != nil {
			if rem := expiry.Sub(r.now()); rem < d {
				d = rem
			}
		}
		r.wait(d)
	}
}

// awaitOperator waits in NEEDS_INTERVENTION for an allowed operator action
// or a cancellation request.
func (r *run) awaitOperator(allowed ...string) (string, error) {
	for {
		obs, err := r.observe("", "")
		if err != nil {
			return "", err
		}
		if obs.CancelRequested {
			return OperatorCancel, nil
		}
		op, _ := r.wait(5 * time.Minute)
		if op == nil {
			continue
		}
		for _, x := range allowed {
			if op.Action == x {
				workflow.GetLogger(r.ctx).Info("operator action", "job", r.jobID, "action", op.Action, "actor", op.Actor)
				return op.Action, nil
			}
		}
		workflow.GetLogger(r.ctx).Warn("ignoring operator action not valid here", "action", op.Action)
	}
}

// ---------------------------------------------------------------------------
// Finalization

// finalize records the outcome, always attempts cleanup, and moves the job
// to its terminal state regardless of cleanup success (LIFE-001/003).
func (r *run) finalize(outcome domain.Outcome, f *Failure) error {
	in := FinalizeInput{JobID: r.jobID, Outcome: outcome, Failure: f}
	if err := r.call(shortOpts, a.BeginCleanup, nil, in); err != nil {
		return err
	}
	var cr CleanupResult
	if err := r.call(cleanupOpts, a.Cleanup, &cr, r.jobID); err != nil {
		workflow.GetLogger(r.ctx).Warn("cleanup activity failed; reconciler will retry", "job", r.jobID, "error", err)
	}
	return r.call(shortOpts, a.Complete, nil, in)
}
