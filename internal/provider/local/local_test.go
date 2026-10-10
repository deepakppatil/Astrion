package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/provider"
)

func TestLocalLifecycle(t *testing.T) {
	p, err := New("local", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := provider.Spec{
		Owner:   provider.Ownership{TenantID: "t1", JobID: "j1", ExecutionID: "exe_1", Attempt: 1},
		Command: []string{"sh", "-c", "test -s \"$ASTRION_SECRETS_DIR/token\" && sleep 5"},
		Secrets: map[string][]byte{"token": []byte("abc")},
	}
	ref, err := p.Allocate(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Allocate(ctx, spec); err != nil {
		t.Fatalf("allocate should be idempotent: %v", err)
	}
	info, err := os.Stat(filepath.Join(p.dir("exe_1"), "secrets", "token"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", info, err)
	}
	st, _ := p.Status(ctx, ref)
	if st.Phase != provider.PhaseRunning {
		t.Fatalf("phase = %s (%s)", st.Phase, st.Message)
	}
	owned, _ := p.ListOwned(ctx, "t1")
	if len(owned) != 1 {
		t.Fatalf("owned = %d", len(owned))
	}
	if other, _ := p.ListOwned(ctx, "t2"); len(other) != 0 {
		t.Fatal("tenant filter broken")
	}
	if err := p.Terminate(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(ctx, ref); err != nil {
		t.Fatalf("terminate idempotent: %v", err)
	}
	st, _ = p.Status(ctx, ref)
	if st.Phase != provider.PhaseNotFound {
		t.Fatalf("after terminate phase = %s", st.Phase)
	}
}

func TestLocalExitCodes(t *testing.T) {
	p, _ := New("local", t.TempDir())
	ctx := context.Background()
	ok, _ := p.Allocate(ctx, provider.Spec{Owner: provider.Ownership{ExecutionID: "ok"}, Command: []string{"true"}})
	bad, _ := p.Allocate(ctx, provider.Spec{Owner: provider.Ownership{ExecutionID: "bad"}, Command: []string{"false"}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := p.Status(ctx, ok)
		b, _ := p.Status(ctx, bad)
		if a.Phase == provider.PhaseSucceeded && b.Phase == provider.PhaseFailed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("exit codes not observed")
}
