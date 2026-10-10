// Package reconciler finds and removes abandoned execution resources
// (LIFE-004..008). It is the safety net behind workflow cleanup: anything a
// crashed worker, failed cleanup, or lost signal leaves behind is eventually
// terminated. It only ever touches resources carrying Astrion ownership
// labels, and resolves ownership against the database before acting.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/store"
)

// Releaser releases one execution environment (the workflow activity).
type Releaser interface {
	ReleaseEnvironment(ctx context.Context, executionID string) error
}

type Reconciler struct {
	Store    *store.Store
	Pools    *provider.Registry
	Releaser Releaser
	Log      *slog.Logger
	// UnknownGrace protects environments whose DB row may not be visible yet.
	UnknownGrace time.Duration
	now          func() time.Time
}

type Report struct {
	ReleasedExecutions int      `json:"releasedExecutions"`
	TerminatedOrphans  int      `json:"terminatedOrphans"`
	Errors             []string `json:"errors,omitempty"`
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// RunOnce performs one reconciliation pass.
func (r *Reconciler) RunOnce(ctx context.Context) Report {
	var rep Report
	// 1. Executions the database knows still need cleanup.
	execs, err := r.Store.ExecutionsNeedingCleanup(ctx, 200)
	if err != nil {
		rep.Errors = append(rep.Errors, "list executions: "+err.Error())
	}
	for _, e := range execs {
		if err := r.Releaser.ReleaseEnvironment(ctx, e.ID); err != nil {
			rep.Errors = append(rep.Errors, e.ID+": "+err.Error())
			continue
		}
		rep.ReleasedExecutions++
		r.audit(ctx, e.TenantID, e.ID, "reconciler.release", "job="+e.JobID)
	}

	// 2. Resources providers report that the database does not need.
	grace := r.UnknownGrace
	if grace == 0 {
		grace = 10 * time.Minute
	}
	for _, pool := range r.Pools.All() {
		owned, err := pool.Provider.ListOwned(ctx, "")
		if err != nil {
			rep.Errors = append(rep.Errors, pool.Name+": list owned: "+err.Error())
			continue
		}
		for _, st := range owned {
			reason := r.orphanReason(ctx, st, grace)
			if reason == "" {
				continue
			}
			if err := pool.Provider.Terminate(ctx, st.Ref); err != nil {
				rep.Errors = append(rep.Errors, st.Ref.ID+": terminate: "+err.Error())
				continue
			}
			rep.TerminatedOrphans++
			r.log().Info("terminated orphaned environment", "pool", pool.Name, "ref", st.Ref.ID, "reason", reason)
			r.audit(ctx, st.Owner.TenantID, st.Owner.ExecutionID, "reconciler.terminate_orphan", reason)
		}
	}
	if n, err := r.Store.ResolveJobCleanup(ctx); err != nil {
		rep.Errors = append(rep.Errors, "resolve job cleanup: "+err.Error())
	} else if n > 0 {
		r.log().Info("job cleanup completed by reconciler", "jobs", n)
	}
	return rep
}

func (r *Reconciler) orphanReason(ctx context.Context, st provider.Status, grace time.Duration) string {
	now := r.clock()
	if st.Owner.ExecutionID == "" {
		if now.Sub(st.Created) > grace {
			return "missing execution ownership label"
		}
		return ""
	}
	state, cleanup, err := r.Store.ExecutionOwnerState(ctx, st.Owner.ExecutionID)
	if errors.Is(err, domain.ErrNotFound) {
		if now.Sub(st.Created) > grace {
			return "no matching execution record"
		}
		return ""
	}
	if err != nil {
		return "" // unsure: never delete on a lookup error
	}
	switch {
	case state.Terminal():
		return "job is " + string(state)
	case cleanup == domain.CleanupCompleted:
		return "execution already released"
	case !st.Owner.ExpiresAt.IsZero() && now.After(st.Owner.ExpiresAt):
		return "environment expired at " + st.Owner.ExpiresAt.Format(time.RFC3339)
	}
	return ""
}

func (r *Reconciler) audit(ctx context.Context, tenantID, target, action, detail string) {
	if tenantID == "" {
		return
	}
	_ = r.Store.Audit(ctx, domain.AuditEvent{TenantID: tenantID, Actor: "system:reconciler", ActorType: "system",
		Action: action, TargetType: "execution", TargetID: target, Details: store.AuditDetails("detail", detail)})
}

// Run reconciles periodically until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rep := r.RunOnce(ctx)
		if len(rep.Errors) > 0 {
			r.log().Warn("reconciliation pass had errors", "errors", rep.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
