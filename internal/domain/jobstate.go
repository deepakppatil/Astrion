package domain

import "fmt"

// JobState is the platform's business representation of a job's lifecycle.
// The authoritative durable execution history lives in the workflow engine;
// this state is what users, APIs, and policies observe. See
// docs/architecture/state-machine.md for the full specification.
type JobState string

const (
	JobSubmitted          JobState = "SUBMITTED"
	JobValidating         JobState = "VALIDATING"
	JobQueued             JobState = "QUEUED"
	JobProvisioning       JobState = "PROVISIONING"
	JobPreparingWorkspace JobState = "PREPARING_WORKSPACE"
	JobRunning            JobState = "RUNNING"
	JobWaitingForInput    JobState = "WAITING_FOR_INPUT"
	JobWaitingForApproval JobState = "WAITING_FOR_APPROVAL"
	JobCollectingArtifact JobState = "COLLECTING_ARTIFACTS"
	JobCleaningUp         JobState = "CLEANING_UP"
	JobSucceeded          JobState = "SUCCEEDED"
	JobFailed             JobState = "FAILED"
	JobCancelled          JobState = "CANCELLED"
	JobTimedOut           JobState = "TIMED_OUT"
	JobNeedsIntervention  JobState = "NEEDS_INTERVENTION"
)

// AllJobStates lists every state in lifecycle order.
var AllJobStates = []JobState{
	JobSubmitted, JobValidating, JobQueued, JobProvisioning, JobPreparingWorkspace,
	JobRunning, JobWaitingForInput, JobWaitingForApproval, JobCollectingArtifact,
	JobCleaningUp, JobSucceeded, JobFailed, JobCancelled, JobTimedOut, JobNeedsIntervention,
}

// transitions is the single source of truth for legal state changes.
//
// Design notes:
//   - Every non-terminal path to a terminal state goes through CLEANING_UP so that
//     cleanup is always attempted (LIFE-001), even when nothing was provisioned.
//   - WAITING_* -> PROVISIONING models checkpoint/resume: compute is released while
//     waiting and a new execution attempt is provisioned after the user responds.
//   - WAITING_* -> RUNNING models live-session mode.
//   - VALIDATING -> WAITING_FOR_APPROVAL -> QUEUED models a platform approval
//     gate required by command metadata (e.g. /Create-Jira) before any compute
//     is provisioned or external side effect is attempted.
//   - NEEDS_INTERVENTION is not terminal: an operator may retry, reopen an expired
//     interaction, or cancel. Workflow state is preserved while it waits.
var transitions = map[JobState][]JobState{
	JobSubmitted:          {JobValidating, JobCleaningUp},
	JobValidating:         {JobQueued, JobWaitingForApproval, JobCleaningUp},
	JobQueued:             {JobProvisioning, JobCleaningUp},
	JobProvisioning:       {JobPreparingWorkspace, JobRunning, JobCleaningUp, JobNeedsIntervention},
	JobPreparingWorkspace: {JobRunning, JobCleaningUp, JobNeedsIntervention},
	JobRunning:            {JobWaitingForInput, JobWaitingForApproval, JobCollectingArtifact, JobCleaningUp, JobNeedsIntervention},
	JobWaitingForInput:    {JobRunning, JobProvisioning, JobCleaningUp, JobNeedsIntervention},
	JobWaitingForApproval: {JobQueued, JobRunning, JobProvisioning, JobCleaningUp, JobNeedsIntervention},
	JobCollectingArtifact: {JobCleaningUp},
	JobCleaningUp:         {JobSucceeded, JobFailed, JobCancelled, JobTimedOut},
	JobNeedsIntervention:  {JobProvisioning, JobWaitingForInput, JobWaitingForApproval, JobCleaningUp},
	JobSucceeded:          {},
	JobFailed:             {},
	JobCancelled:          {},
	JobTimedOut:           {},
}

// Valid reports whether s is a known state.
func (s JobState) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether no further transitions are possible.
func (s JobState) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobTimedOut:
		return true
	}
	return false
}

// Active reports whether the job may currently hold execution capacity.
func (s JobState) Active() bool {
	switch s {
	case JobProvisioning, JobPreparingWorkspace, JobRunning, JobWaitingForInput,
		JobWaitingForApproval, JobCollectingArtifact:
		return true
	}
	return false
}

// Waiting reports whether the job is blocked on a human.
func (s JobState) Waiting() bool {
	return s == JobWaitingForInput || s == JobWaitingForApproval
}

// CanTransitionTo reports whether from -> to is a legal transition.
func (s JobState) CanTransitionTo(to JobState) bool {
	for _, t := range transitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

// NextStates returns the legal successor states (copy).
func (s JobState) NextStates() []JobState {
	return append([]JobState(nil), transitions[s]...)
}

// ValidateTransition returns an *InvalidTransitionError if from -> to is illegal.
func ValidateTransition(from, to JobState) error {
	if !from.Valid() || !to.Valid() || !from.CanTransitionTo(to) {
		return &InvalidTransitionError{From: from, To: to}
	}
	return nil
}

// InvalidTransitionError is returned when a state change violates the state machine.
type InvalidTransitionError struct {
	From, To JobState
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid job state transition %s -> %s", e.From, e.To)
}

// Outcome is the terminal result decided before cleanup begins. The job sits in
// CLEANING_UP with a pending outcome, then moves to the matching terminal state
// regardless of whether cleanup itself succeeded (LIFE-003).
type Outcome string

const (
	OutcomeSucceeded Outcome = "SUCCEEDED"
	OutcomeFailed    Outcome = "FAILED"
	OutcomeCancelled Outcome = "CANCELLED"
	OutcomeTimedOut  Outcome = "TIMED_OUT"
)

// TerminalState maps an outcome to its terminal job state.
func (o Outcome) TerminalState() JobState {
	switch o {
	case OutcomeSucceeded:
		return JobSucceeded
	case OutcomeCancelled:
		return JobCancelled
	case OutcomeTimedOut:
		return JobTimedOut
	default:
		return JobFailed
	}
}

// FailureClass categorizes why a job did not succeed. It drives retry policy,
// alerting, and the diagnostic context shown to users.
type FailureClass string

const (
	FailureNone             FailureClass = ""
	FailureValidation       FailureClass = "VALIDATION"
	FailureAuthorization    FailureClass = "AUTHORIZATION"
	FailureQuota            FailureClass = "QUOTA"
	FailurePolicy           FailureClass = "POLICY"
	FailureProvisioning     FailureClass = "PROVISIONING"
	FailureWorkspace        FailureClass = "WORKSPACE_PREPARATION"
	FailureAgent            FailureClass = "AGENT_ERROR"
	FailureOutputValidation FailureClass = "OUTPUT_VALIDATION"
	FailureTimeout          FailureClass = "TIMEOUT"
	FailureEnvironmentLost  FailureClass = "ENVIRONMENT_LOST"
	FailureHITLTimeout      FailureClass = "HITL_TIMEOUT"
	FailureInteraction      FailureClass = "INTERACTION_REJECTED"
	FailureCancelled        FailureClass = "CANCELLED"
	FailureInternal         FailureClass = "INTERNAL"
)
