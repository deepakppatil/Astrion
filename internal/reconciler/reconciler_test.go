package reconciler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/bootstrap"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/provider/local"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/testutil"
)

type noRelease struct{ called []string }

func (n *noRelease) ReleaseEnvironment(_ context.Context, id string) error {
	n.called = append(n.called, id)
	return nil
}

// TestReconcilerTerminatesOrphans covers LIFE-006/007: environments whose
// job is terminal, or that no execution record claims (after a grace
// period), are terminated; live and unknown-but-young ones are kept.
func TestReconcilerTerminatesOrphans(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	boot, err := bootstrap.Run(ctx, s, bootstrap.Options{TenantID: "ten_r", Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	user := &domain.User{TenantID: "ten_r", Subject: "dev", Roles: []domain.Role{domain.RoleDeveloper}}
	if err := s.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.FindCommand(ctx, boot.FrameworkID, "/build-context")
	if err != nil {
		t.Fatal(err)
	}
	newJob := func() *domain.Job {
		j, _, err := s.CreateJob(ctx, store.NewJob{Job: &domain.Job{TenantID: "ten_r", WorkspaceID: boot.WorkspaceID,
			InitiatedBy: user.ID, Phase: cmd.Phase, Command: cmd.Canonical, CommandID: cmd.ID, AgentProfileID: boot.AgentProfileID,
			AgentProfileVersion: 1, FrameworkID: boot.FrameworkID, RequestedProvider: "AUTO", Inputs: json.RawMessage(`{}`)},
			CommandVersion: cmd.Version})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	newExec := func(j *domain.Job) *domain.ExecutionEnvironment {
		e := &domain.ExecutionEnvironment{TenantID: j.TenantID, JobID: j.ID, Attempt: 1, Provider: "local",
			InteractionMode: domain.ModeCheckpointResume, ExpiresAt: time.Now().Add(time.Hour)}
		if _, err := s.CreateExecution(ctx, e, store.HashToken("t"), time.Hour); err != nil {
			t.Fatal(err)
		}
		return e
	}

	dir := t.TempDir()
	lp, err := local.New("local", dir)
	if err != nil {
		t.Fatal(err)
	}
	pools := provider.NewRegistry()
	pools.Add(&provider.Pool{Name: "local", Type: "local", Network: "PRIVATE", MaxClassification: domain.ClassRestricted, Enabled: true, Provider: lp})
	alloc := func(owner provider.Ownership) {
		if _, err := lp.Allocate(ctx, provider.Spec{Owner: owner, Command: []string{"sleep", "60"}}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Terminal job with a leftover environment.
	done := newJob()
	doneExec := newExec(done)
	for _, st := range []domain.JobState{domain.JobValidating, domain.JobCleaningUp, domain.JobFailed} {
		if _, err := s.TransitionJob(ctx, store.Transition{JobID: done.ID, To: st, FailureClass: domain.FailureAgent}); err != nil {
			t.Fatal(err)
		}
	}
	alloc(provider.Ownership{TenantID: "ten_r", JobID: done.ID, ExecutionID: doneExec.ID, Attempt: 1, ExpiresAt: time.Now().Add(time.Hour)})
	// 2. Live job: must be kept.
	live := newJob()
	liveExec := newExec(live)
	alloc(provider.Ownership{TenantID: "ten_r", JobID: live.ID, ExecutionID: liveExec.ID, Attempt: 1, ExpiresAt: time.Now().Add(time.Hour)})
	// 3. Environment with no execution record (e.g. crash between allocate
	// and commit): kept during the grace period, terminated after it.
	alloc(provider.Ownership{TenantID: "ten_r", JobID: "job_ghost", ExecutionID: "exe_ghost", Attempt: 1, ExpiresAt: time.Now().Add(time.Hour)})

	rel := &noRelease{}
	r := &Reconciler{Store: s, Pools: pools, Releaser: rel, UnknownGrace: time.Hour}
	rep := r.RunOnce(ctx)
	if rep.TerminatedOrphans != 1 || len(rep.Errors) != 0 {
		t.Fatalf("first pass: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, doneExec.ID)); !os.IsNotExist(err) {
		t.Fatal("terminal job environment not removed")
	}
	for _, id := range []string{liveExec.ID, "exe_ghost"} {
		if _, err := os.Stat(filepath.Join(dir, id)); err != nil {
			t.Fatalf("%s should be kept: %v", id, err)
		}
	}

	r.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past grace and expiry
	rep = r.RunOnce(ctx)
	if rep.TerminatedOrphans != 2 {
		t.Fatalf("second pass: %+v", rep)
	}
	audit, err := s.ListAudit(ctx, "ten_r", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range audit {
		if a.Action == "reconciler.terminate_orphan" && a.Actor == "system:reconciler" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("expected 3 audited terminations, got %d", n)
	}
}
