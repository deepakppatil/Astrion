// Package workflow contains the durable job orchestration (SRS 5.4, 12).
//
// One Temporal workflow instance exists per job (workflow ID "job-<jobID>").
// The workflow owns all job state transitions; the API and supervisor only
// record facts (status reports, interactions, responses) and nudge the
// workflow with a signal. On every wake-up the workflow re-observes the
// database, so a lost or duplicated signal only affects latency, never
// correctness (ADR-0002).
//
// The workflow is deliberately composed of small phases (validate, approval
// gate, admission, attempt monitoring, HITL wait, finalize) rather than one
// monolithic function, and every side effect is an idempotent activity.
package workflow

import (
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
)

const (
	TaskQueue = "astrion-jobs"

	// SignalNudge asks the workflow to re-observe state. Payload: reason string.
	SignalNudge = "nudge"
	// SignalOperator carries an operator decision for NEEDS_INTERVENTION.
	SignalOperator = "operator-action"
)

// WorkflowID returns the deterministic workflow ID for a job. Starting a
// workflow with this ID is idempotent (duplicate starts are rejected).
func WorkflowID(jobID string) string { return "job-" + jobID }

type JobInput struct {
	JobID string `json:"jobId"`
}

// OperatorAction values.
const (
	OperatorRetry  = "retry"  // start a fresh attempt (no restoration claimed)
	OperatorReopen = "reopen" // reopen an expired interaction
	OperatorCancel = "cancel"
)

type OperatorSignal struct {
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Reason string `json:"reason,omitempty"`
}

// Failure is a business failure returned by activities (not an error: it
// must not be retried).
type Failure struct {
	Class   domain.FailureClass `json:"class"`
	Message string              `json:"message"`
}

type ValidateResult struct {
	Failure          *Failure               `json:"failure,omitempty"`
	Pool             string                 `json:"pool"`
	Mode             domain.InteractionMode `json:"mode"`
	TimeoutSeconds   int                    `json:"timeoutSeconds"`
	HITLMaxWait      int                    `json:"hitlMaxWaitSeconds"`
	HITLOnTimeout    string                 `json:"hitlOnTimeout"`
	NeedsApproval    bool                   `json:"needsApproval"`
	ApprovalPrompt   string                 `json:"approvalPrompt,omitempty"`
	ApproverRole     domain.Role            `json:"approverRole,omitempty"`
	QueueTimeoutSecs int                    `json:"queueTimeoutSeconds"`
}

type CapacityResult struct {
	Acquired bool   `json:"acquired"`
	Reason   string `json:"reason,omitempty"`
}

type ProvisionInput struct {
	JobID               string                 `json:"jobId"`
	Attempt             int                    `json:"attempt"`
	Pool                string                 `json:"pool"`
	Mode                domain.InteractionMode `json:"mode"`
	TimeoutSeconds      int                    `json:"timeoutSeconds"`
	ResumeInteractionID string                 `json:"resumeInteractionId,omitempty"`
	CheckpointArtifact  string                 `json:"checkpointArtifactId,omitempty"`
}

type ProvisionResult struct {
	ExecutionID string   `json:"executionId"`
	Failure     *Failure `json:"failure,omitempty"`
}

type ObserveInput struct {
	JobID       string `json:"jobId"`
	ExecutionID string `json:"executionId,omitempty"`
	// InteractionID, if set, is reported in the observation.
	InteractionID string `json:"interactionId,omitempty"`
}

// Observation is a consistent snapshot of the facts the workflow acts on.
type Observation struct {
	JobState        domain.JobState        `json:"jobState"`
	CancelRequested bool                   `json:"cancelRequested"`
	ExecStatus      domain.ExecutionStatus `json:"execStatus,omitempty"`
	ExecError       string                 `json:"execError,omitempty"`
	ExecFailure     domain.FailureClass    `json:"execFailure,omitempty"`
	PendingInteract string                 `json:"pendingInteractionId,omitempty"`
	Checkpoint      string                 `json:"checkpointArtifactId,omitempty"`
	// ProviderGone is true when the provider reports the environment ended
	// or missing while the execution has not reported a final status.
	ProviderGone    bool   `json:"providerGone"`
	ProviderMessage string `json:"providerMessage,omitempty"`
	// Recorded lists interactions persisted but not yet opened.
	Recorded []RecordedInteraction `json:"recorded,omitempty"`
	// Interaction state for ObserveInput.InteractionID.
	InteractionStatus   domain.InteractionStatus `json:"interactionStatus,omitempty"`
	InteractionDecision domain.Decision          `json:"interactionDecision,omitempty"`
	InteractionComment  string                   `json:"interactionComment,omitempty"`
	InteractionBy       string                   `json:"interactionBy,omitempty"`
}

type RecordedInteraction struct {
	ID       string                 `json:"id"`
	Kind     domain.InteractionKind `json:"kind"`
	Decision bool                   `json:"decision"`
}

type OpenInteractionInput struct {
	JobID         string     `json:"jobId"`
	InteractionID string     `json:"interactionId"`
	Decision      bool       `json:"decision"` // approval-type → WAITING_FOR_APPROVAL
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
}

type TransitionInput struct {
	JobID   string          `json:"jobId"`
	To      domain.JobState `json:"to"`
	Step    string          `json:"step,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

type FinalizeInput struct {
	JobID   string         `json:"jobId"`
	Outcome domain.Outcome `json:"outcome"`
	Failure *Failure       `json:"failure,omitempty"`
}

type CleanupResult struct {
	Errors []string `json:"errors,omitempty"`
}
