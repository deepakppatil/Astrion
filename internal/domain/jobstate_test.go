package domain

import (
	"errors"
	"testing"
)

func TestEveryStateHasTransitionEntry(t *testing.T) {
	for _, s := range AllJobStates {
		if !s.Valid() {
			t.Errorf("state %s missing from transition table", s)
		}
	}
	if len(transitions) != len(AllJobStates) {
		t.Fatalf("transition table has %d entries, AllJobStates has %d", len(transitions), len(AllJobStates))
	}
}

func TestTerminalStatesHaveNoSuccessors(t *testing.T) {
	for _, s := range AllJobStates {
		if s.Terminal() && len(s.NextStates()) != 0 {
			t.Errorf("terminal state %s has successors %v", s, s.NextStates())
		}
		if !s.Terminal() && len(s.NextStates()) == 0 {
			t.Errorf("non-terminal state %s has no successors", s)
		}
	}
}

// Every terminal state must only be reachable from CLEANING_UP so that cleanup
// is always attempted (LIFE-001).
func TestTerminalStatesOnlyReachableFromCleaningUp(t *testing.T) {
	for from, tos := range transitions {
		for _, to := range tos {
			if to.Terminal() && from != JobCleaningUp {
				t.Errorf("%s -> %s bypasses CLEANING_UP", from, to)
			}
		}
	}
}

// Every non-terminal state must be able to reach CLEANING_UP (cancellation and
// failure must always be possible) — either directly or, for COLLECTING_ARTIFACTS, by design.
func TestEveryNonTerminalStateCanReachCleanup(t *testing.T) {
	for _, s := range AllJobStates {
		if s.Terminal() || s == JobCleaningUp {
			continue
		}
		if !s.CanTransitionTo(JobCleaningUp) {
			t.Errorf("%s cannot transition to CLEANING_UP", s)
		}
	}
}

func TestEveryStateReachableFromSubmitted(t *testing.T) {
	seen := map[JobState]bool{JobSubmitted: true}
	queue := []JobState{JobSubmitted}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, n := range s.NextStates() {
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	for _, s := range AllJobStates {
		if !seen[s] {
			t.Errorf("state %s unreachable from SUBMITTED", s)
		}
	}
}

func TestValidateTransition(t *testing.T) {
	cases := []struct {
		from, to JobState
		ok       bool
	}{
		{JobSubmitted, JobValidating, true},
		{JobValidating, JobWaitingForApproval, true},
		{JobWaitingForApproval, JobQueued, true},
		{JobWaitingForInput, JobQueued, false},
		{JobRunning, JobWaitingForInput, true},
		{JobWaitingForInput, JobProvisioning, true}, // checkpoint/resume
		{JobWaitingForInput, JobRunning, true},      // live session
		{JobCollectingArtifact, JobCleaningUp, true},
		{JobCleaningUp, JobSucceeded, true},
		{JobRunning, JobSucceeded, false}, // must collect + clean first
		{JobSucceeded, JobRunning, false},
		{JobQueued, JobRunning, false},
		{JobState("BOGUS"), JobRunning, false},
		{JobRunning, JobState("BOGUS"), false},
	}
	for _, c := range cases {
		err := ValidateTransition(c.from, c.to)
		if (err == nil) != c.ok {
			t.Errorf("%s -> %s: got err=%v, want ok=%v", c.from, c.to, err, c.ok)
		}
		if err != nil {
			var ite *InvalidTransitionError
			if !errors.As(err, &ite) {
				t.Errorf("expected InvalidTransitionError, got %T", err)
			}
		}
	}
}

func TestOutcomeTerminalState(t *testing.T) {
	m := map[Outcome]JobState{
		OutcomeSucceeded: JobSucceeded,
		OutcomeFailed:    JobFailed,
		OutcomeCancelled: JobCancelled,
		OutcomeTimedOut:  JobTimedOut,
		Outcome(""):      JobFailed,
	}
	for o, want := range m {
		if got := o.TerminalState(); got != want {
			t.Errorf("%q.TerminalState() = %s, want %s", o, got, want)
		}
		if !JobCleaningUp.CanTransitionTo(o.TerminalState()) {
			t.Errorf("CLEANING_UP cannot reach %s", o.TerminalState())
		}
	}
}
