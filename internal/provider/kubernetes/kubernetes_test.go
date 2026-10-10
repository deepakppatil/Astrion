package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/provider"
)

// fakeAPI is a minimal in-memory Kubernetes API for batch/v1 Jobs and v1 Secrets.
type fakeAPI struct {
	mu      sync.Mutex
	jobs    map[string]map[string]any
	secrets map[string]map[string]any
	calls   []string
	quota   map[string]any
}

func newFake() *fakeAPI {
	return &fakeAPI{jobs: map[string]map[string]any{}, secrets: map[string]map[string]any{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	notFound := func() { w.WriteHeader(404); w.Write([]byte(`{"message":"not found"}`)) }
	switch {
	case strings.HasPrefix(r.URL.Path, "/apis/batch/v1/namespaces/exec/jobs"):
		name := ""
		if len(parts) == 7 {
			name = parts[6]
		}
		switch r.Method {
		case http.MethodGet:
			if name == "" {
				items := []any{}
				sel := r.URL.Query().Get("labelSelector")
				for _, j := range f.jobs {
					if matches(j, sel) {
						items = append(items, j)
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items})
				return
			}
			j, ok := f.jobs[name]
			if !ok {
				notFound()
				return
			}
			json.NewEncoder(w).Encode(j)
		case http.MethodPost:
			var j map[string]any
			json.Unmarshal(body, &j)
			md := j["metadata"].(map[string]any)
			n := md["name"].(string)
			if _, ok := f.jobs[n]; ok {
				w.WriteHeader(409)
				return
			}
			md["uid"] = "uid-" + n
			md["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			f.jobs[n] = j
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(j)
		case http.MethodPatch:
			j, ok := f.jobs[name]
			if !ok {
				notFound()
				return
			}
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				w.WriteHeader(415)
				return
			}
			j["spec"].(map[string]any)["suspend"] = false
			j["status"] = map[string]any{"active": 1, "ready": 1}
			json.NewEncoder(w).Encode(j)
		case http.MethodDelete:
			if _, ok := f.jobs[name]; !ok {
				notFound()
				return
			}
			delete(f.jobs, name)
			w.Write([]byte(`{}`))
		}
	case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/exec/secrets"):
		name := ""
		if len(parts) == 6 {
			name = parts[5]
		}
		switch r.Method {
		case http.MethodPost:
			var s map[string]any
			json.Unmarshal(body, &s)
			n := s["metadata"].(map[string]any)["name"].(string)
			if _, ok := f.secrets[n]; ok {
				w.WriteHeader(409)
				return
			}
			f.secrets[n] = s
			w.WriteHeader(201)
		case http.MethodDelete:
			if _, ok := f.secrets[name]; !ok {
				notFound()
				return
			}
			delete(f.secrets, name)
		}
	case r.URL.Path == "/api/v1/namespaces/exec/resourcequotas":
		items := []any{}
		if f.quota != nil {
			items = append(items, f.quota)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	default:
		notFound()
	}
}

func matches(j map[string]any, sel string) bool {
	labels, _ := j["metadata"].(map[string]any)["labels"].(map[string]any)
	for _, kv := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func setup(t *testing.T) (*Provider, *fakeAPI) {
	t.Helper()
	fake := newFake()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p, err := New(Config{Name: "test-pool", Server: srv.URL, Namespace: "exec", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	return p, fake
}

func spec() provider.Spec {
	return provider.Spec{
		Owner: provider.Ownership{TenantID: "ten_a", JobID: "job_1", WorkflowID: "job-job_1", ExecutionID: "exe_ABC_1", Attempt: 1,
			ExpiresAt: time.Now().Add(time.Hour)},
		Image:          "registry.local/astrion-runtime:1.0.0",
		Command:        []string{"astrion-supervisor"},
		Env:            map[string]string{"ASTRION_API_URL": "http://api"},
		Secrets:        map[string][]byte{"execution-token": []byte("s3cr3t")},
		ActiveDeadline: 30 * time.Minute,
		NetworkProfile: "restricted-egress",
	}
}

func TestAllocateStatusTerminateLifecycle(t *testing.T) {
	p, fake := setup(t)
	ctx := context.Background()
	ref, err := p.Allocate(ctx, spec())
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "exec/astrion-exe-abc-1" {
		t.Fatalf("ref = %s", ref.ID)
	}
	// Idempotent: second allocate reuses the job and tolerates the existing secret.
	if _, err := p.Allocate(ctx, spec()); err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if len(fake.jobs) != 1 || len(fake.secrets) != 1 {
		t.Fatalf("jobs=%d secrets=%d", len(fake.jobs), len(fake.secrets))
	}
	sec := fake.secrets["astrion-exe-abc-1"]
	owners := sec["metadata"].(map[string]any)["ownerReferences"].([]any)
	if owners[0].(map[string]any)["uid"] != "uid-astrion-exe-abc-1" {
		t.Fatalf("secret must be owned by job: %v", owners)
	}
	st, err := p.Status(ctx, ref)
	if err != nil || st.Phase != provider.PhaseRunning {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if st.Owner.TenantID != "ten_a" || st.Owner.ExecutionID != "exe_ABC_1" {
		t.Fatalf("ownership not round-tripped: %+v", st.Owner)
	}
	owned, err := p.ListOwned(ctx, "ten_a")
	if err != nil || len(owned) != 1 {
		t.Fatalf("listOwned = %v, %v", owned, err)
	}
	if other, _ := p.ListOwned(ctx, "ten_b"); len(other) != 0 {
		t.Fatal("tenant-scoped listing leaked another tenant's job")
	}
	if err := p.Terminate(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(ctx, ref); err != nil {
		t.Fatalf("terminate must be idempotent: %v", err)
	}
	if len(fake.jobs) != 0 || len(fake.secrets) != 0 {
		t.Fatal("resources not deleted")
	}
	st, _ = p.Status(ctx, ref)
	if st.Phase != provider.PhaseNotFound {
		t.Fatalf("phase after delete = %s", st.Phase)
	}
}

func TestJobManifestIsHardened(t *testing.T) {
	p, _ := setup(t)
	job := p.BuildJob(spec())
	b, _ := json.Marshal(job)
	var m map[string]any
	json.Unmarshal(b, &m)
	js := m["spec"].(map[string]any)
	if js["suspend"] != true || js["backoffLimit"].(float64) != 0 {
		t.Fatalf("job must start suspended with backoffLimit 0: %v", js)
	}
	pod := js["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false || pod["hostNetwork"] != false {
		t.Fatal("pod must not mount SA token or use host network")
	}
	psc := pod["securityContext"].(map[string]any)
	if psc["runAsNonRoot"] != true || psc["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
		t.Fatalf("pod security context: %v", psc)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	csc := c["securityContext"].(map[string]any)
	if csc["readOnlyRootFilesystem"] != true || csc["allowPrivilegeEscalation"] != false || csc["privileged"] != false {
		t.Fatalf("container security context: %v", csc)
	}
	if drop := csc["capabilities"].(map[string]any)["drop"].([]any); drop[0] != "ALL" {
		t.Fatal("capabilities must drop ALL")
	}
	for _, e := range c["env"].([]any) {
		if strings.Contains(e.(map[string]any)["value"].(string), "s3cr3t") {
			t.Fatal("secret leaked into env")
		}
	}
	if !strings.Contains(string(b), "restricted-egress") {
		t.Fatal("network profile label missing")
	}
}

func TestCheckCapacityHonorsQuota(t *testing.T) {
	p, fake := setup(t)
	fake.quota = map[string]any{"metadata": map[string]any{"name": "exec-quota"}, "status": map[string]any{
		"hard": map[string]any{"count/jobs.batch": "2"}, "used": map[string]any{"count/jobs.batch": "2"}}}
	c, err := p.CheckCapacity(context.Background(), spec())
	if err != nil || c.Available {
		t.Fatalf("expected unavailable, got %+v %v", c, err)
	}
	fake.quota = nil
	c, _ = p.CheckCapacity(context.Background(), spec())
	if !c.Available {
		t.Fatal("expected available")
	}
}

func TestRefFromAnotherPoolRejected(t *testing.T) {
	p, _ := setup(t)
	if err := p.Terminate(context.Background(), provider.Ref{ID: "kube-system/coredns"}); err == nil {
		t.Fatal("terminate must refuse refs outside the pool namespace")
	}
}

func TestResourceName(t *testing.T) {
	if n := ResourceName("exe_01HXYZ"); n != "astrion-exe-01hxyz" {
		t.Fatal(n)
	}
	long := ResourceName(strings.Repeat("a", 100))
	if len(long) > 63 {
		t.Fatal("name too long")
	}
}
