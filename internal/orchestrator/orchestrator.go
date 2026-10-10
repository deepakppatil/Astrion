// Package orchestrator connects the API to the workflow engine. It starts
// job workflows idempotently, relays nudges and operator actions, and runs
// the dispatcher that recovers jobs whose workflow start was not confirmed
// (transactional-outbox pattern: the job row is the outbox record).
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/workflow"
)

// Engine is what the API needs from the workflow engine.
type Engine interface {
	StartJob(ctx context.Context, jobID string) error
	Nudge(ctx context.Context, jobID, reason string)
	Operator(ctx context.Context, jobID string, sig workflow.OperatorSignal) error
}

type Temporal struct {
	Client client.Client
	Store  *store.Store
	Log    *slog.Logger
}

func (t *Temporal) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

// StartJob starts the job workflow. Duplicate starts are treated as success,
// so the API and the dispatcher may both call it safely.
func (t *Temporal) StartJob(ctx context.Context, jobID string) error {
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    workflow.WorkflowID(jobID),
		TaskQueue:             workflow.TaskQueue,
		WorkflowIDReusePolicy: 3, // REJECT_DUPLICATE: a job ID maps to exactly one run
	}, workflow.JobWorkflow, workflow.JobInput{JobID: jobID})
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if err != nil && !errors.As(err, &already) {
		return err
	}
	return t.Store.MarkWorkflowStarted(ctx, jobID)
}

// Nudge asks the workflow to re-observe state. Failures are logged only:
// the workflow also polls, so a lost nudge costs latency, not correctness.
func (t *Temporal) Nudge(ctx context.Context, jobID, reason string) {
	err := t.Client.SignalWorkflow(ctx, workflow.WorkflowID(jobID), "", workflow.SignalNudge, reason)
	var nf *serviceerror.NotFound
	if err != nil && !errors.As(err, &nf) {
		t.log().Warn("nudge failed", "job", jobID, "reason", reason, "err", err)
	}
}

func (t *Temporal) Operator(ctx context.Context, jobID string, sig workflow.OperatorSignal) error {
	return t.Client.SignalWorkflow(ctx, workflow.WorkflowID(jobID), "", workflow.SignalOperator, sig)
}

// RunDispatcher periodically starts workflows for accepted jobs whose start
// was not confirmed (e.g. the API crashed after commit).
func RunDispatcher(ctx context.Context, s *store.Store, e Engine, log *slog.Logger, every time.Duration) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		jobs, err := s.UnstartedJobs(ctx, 3*time.Second, 100)
		if err != nil && ctx.Err() == nil {
			log.Warn("dispatcher query failed", "err", err)
		}
		for _, j := range jobs {
			if err := e.StartJob(ctx, j.ID); err != nil {
				log.Warn("dispatcher failed to start workflow", "job", j.ID, "err", err)
			} else {
				log.Info("dispatcher started workflow", "job", j.ID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
