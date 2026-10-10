package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/agent/mock"
	"github.com/deepakppatil/astrion/internal/api"
	"github.com/deepakppatil/astrion/internal/auth"
	"github.com/deepakppatil/astrion/internal/bootstrap"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/objectstore"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/testutil"
	"github.com/deepakppatil/astrion/internal/workflow"
)

// fakeEngine records orchestration calls instead of talking to Temporal.
type fakeEngine struct {
	mu      sync.Mutex
	started []string
	nudges  []string
	ops     []workflow.OperatorSignal
}

func (f *fakeEngine) StartJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}
func (f *fakeEngine) Nudge(_ context.Context, id, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nudges = append(f.nudges, id+":"+reason)
}
func (f *fakeEngine) Operator(_ context.Context, id string, sig workflow.OperatorSignal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, sig)
	return nil
}

// multiTenantAuth maps bearer tokens to identities across tenants.
type multiTenantAuth map[string]auth.Identity

func (m multiTenantAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	tok, err := auth.BearerToken(r)
	if err != nil {
		return nil, err
	}
	id, ok := m[tok]
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &id, nil
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	store  *store.Store
	engine *fakeEngine
	boot   *bootstrap.Result
}

const (
	tokDev   = "dev-token-alice"
	tokBA    = "ba-token-bob"
	tokQA    = "qa-token-quinn"
	tokView  = "viewer-token-vic"
	tokAdmin = "admin-token-ada"
	tokOther = "other-tenant-token"
)

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	boot, err := bootstrap.Run(ctx, s, bootstrap.Options{TenantID: "ten_acme", Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Run(ctx, s, bootstrap.Options{TenantID: "ten_acme", Demo: true}); err != nil {
		t.Fatalf("bootstrap must be idempotent: %v", err)
	}
	if _, err := bootstrap.Run(ctx, s, bootstrap.Options{TenantID: "ten_other"}); err != nil {
		t.Fatal(err)
	}
	ident := func(tenant, sub string, roles ...domain.Role) auth.Identity {
		return auth.Identity{TenantID: tenant, Subject: sub, Name: sub, Roles: roles, RolesAuthoritative: true}
	}
	authn := multiTenantAuth{
		tokDev:   ident("ten_acme", "alice", domain.RoleDeveloper),
		tokBA:    ident("ten_acme", "bob", domain.RoleBusinessAnalyst),
		tokQA:    ident("ten_acme", "quinn", domain.RoleQA),
		tokView:  ident("ten_acme", "vic", domain.RoleViewer),
		tokAdmin: ident("ten_acme", "ada", domain.RolePlatformAdmin),
		tokOther: ident("ten_other", "mallory", domain.RoleDeveloper, domain.RolePlatformAdmin),
	}
	objs, err := objectstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pools := provider.NewRegistry()
	pools.Add(&provider.Pool{Name: "local", Type: "local", Network: "PRIVATE", MaxClassification: domain.ClassRestricted, Enabled: true})
	eng := &fakeEngine{}
	srv := &api.Server{Store: s, Engine: eng, Auth: authn, Objects: objs, Agents: agent.NewRegistry(mock.New()),
		Pools: pools, Config: api.Config{MaxArtifactBytes: 1 << 20, SSEPollInterval: 50 * time.Millisecond}}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	e := &env{t: t, srv: hs, store: s, engine: eng, boot: boot}
	// Add a repository to the demo workspace (implement commands need one).
	e.do(tokAdmin, "POST", "/api/v1/workspaces/"+boot.WorkspaceID+"/repositories",
		map[string]any{"url": "https://github.com/example/app.git"}, nil, 201)
	return e
}

func (e *env) req(token, method, path string, body any, hdr map[string]string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// do performs a request, asserts the status and decodes the JSON response.
func (e *env) do(token, method, path string, body any, out any, want int, hdr ...map[string]string) {
	e.t.Helper()
	var h map[string]string
	if len(hdr) > 0 {
		h = hdr[0]
	}
	resp := e.req(token, method, path, body, h)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("decode %s: %v (%s)", path, err, b)
		}
	}
}

func (e *env) submit(token string, body map[string]any, want int, hdr ...map[string]string) *domain.Job {
	e.t.Helper()
	var j domain.Job
	if want == 202 {
		e.do(token, "POST", "/api/v1/jobs", body, &j, want, hdr...)
		return &j
	}
	e.do(token, "POST", "/api/v1/jobs", body, nil, want, hdr...)
	return nil
}

func (e *env) implementTask(taskID string) map[string]any {
	return map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/implement-task", "inputs": map[string]any{"taskId": taskID}}
}

func TestAuthenticationRequired(t *testing.T) {
	e := setup(t)
	e.do("", "GET", "/api/v1/me", nil, nil, 401)
	e.do("bogus-token-123", "GET", "/api/v1/jobs", nil, nil, 401)
	var me struct {
		User struct {
			Roles []domain.Role `json:"roles"`
		} `json:"user"`
		Permissions []domain.Permission `json:"permissions"`
	}
	e.do(tokDev, "GET", "/api/v1/me", nil, &me, 200)
	if len(me.User.Roles) != 1 || me.User.Roles[0] != domain.RoleDeveloper {
		t.Fatalf("roles = %v", me.User.Roles)
	}
	e.do("", "GET", "/healthz", nil, nil, 200)
	e.do("", "GET", "/readyz", nil, nil, 200)
}

func TestSubmitJobIdempotentAndAuthorized(t *testing.T) {
	e := setup(t)
	hdr := map[string]string{"Idempotency-Key": "submit-0001"}
	j1 := e.submit(tokDev, e.implementTask("T-1"), 202, hdr)
	if j1.ID == "" || j1.State != domain.JobSubmitted || j1.Command != "/implement-task" {
		t.Fatalf("job = %+v", j1)
	}
	if len(j1.Repositories) != 1 {
		t.Fatalf("expected workspace repository to be attached by default, got %v", j1.Repositories)
	}
	// Same key, same body: same job, no new workflow.
	j2 := e.submit(tokDev, e.implementTask("T-1"), 202, hdr)
	if j2.ID != j1.ID {
		t.Fatalf("replay created a new job: %s vs %s", j2.ID, j1.ID)
	}
	// Same key, different body: conflict.
	e.submit(tokDev, e.implementTask("T-2"), 409, hdr)
	if len(e.engine.started) != 2 || e.engine.started[0] != j1.ID {
		t.Fatalf("started = %v", e.engine.started)
	}

	// Persona enforcement: a BA cannot run implementation commands, and a
	// viewer cannot run anything; exact command casing is required.
	e.submit(tokBA, e.implementTask("T-3"), 403)
	e.submit(tokView, e.implementTask("T-3"), 403)
	e.submit(tokDev, map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/Implement-Task",
		"inputs": map[string]any{"taskId": "T"}}, 422)
	// Input validation: missing required input, unknown field.
	e.submit(tokDev, map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/implement-task"}, 422)
	e.submit(tokDev, map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/implement-task",
		"inputs": map[string]any{"taskId": "T", "rm": "-rf"}}, 422)
	// Command names are case-sensitive; a near miss gets a hint, never a match.
	var cerr struct {
		Error struct{ Message, Field string }
	}
	e.do(tokBA, "POST", "/api/v1/jobs", map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/plan-migrate",
		"inputs": map[string]any{}}, &cerr, 422)
	if cerr.Error.Field != "command" || !strings.Contains(cerr.Error.Message, `did you mean "/Plan-migrate"`) {
		t.Fatalf("case near-miss: %+v", cerr.Error)
	}
	// ...but only for commands the caller can see (QA cannot see Plan commands).
	e.do(tokQA, "POST", "/api/v1/jobs", map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/plan-migrate",
		"inputs": map[string]any{}}, &cerr, 422)
	if strings.Contains(cerr.Error.Message, "did you mean") {
		t.Fatalf("hint leaked a command the caller cannot see: %+v", cerr.Error)
	}
	// BA can run /write-spec.
	e.submit(tokBA, map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/write-spec",
		"inputs": map[string]any{"specName": "Checkout"}}, 202)
	// Selecting a specific pool requires provider:select.
	body := e.implementTask("T-9")
	body["provider"] = "local"
	e.submit(tokDev, body, 403)
	e.submit(tokAdmin, map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/diagnose",
		"provider": "local", "inputs": map[string]any{"symptom": "slow"}}, 403) // admin has no execute rights
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t)
	j := e.submit(tokDev, e.implementTask("T-1"), 202)
	e.do(tokOther, "GET", "/api/v1/jobs/"+j.ID, nil, nil, 404)
	e.do(tokOther, "POST", "/api/v1/jobs/"+j.ID+"/cancel", nil, nil, 404)
	e.do(tokOther, "GET", "/api/v1/workspaces/"+e.boot.WorkspaceID, nil, nil, 404)
	var jobs struct{ Items []domain.Job }
	e.do(tokOther, "GET", "/api/v1/jobs", nil, &jobs, 200)
	if len(jobs.Items) != 0 {
		t.Fatalf("other tenant sees %d jobs", len(jobs.Items))
	}
	// Cross-tenant artifact references are rejected at intake.
	e.do(tokOther, "POST", "/api/v1/jobs", map[string]any{"workspaceId": e.boot.WorkspaceID, "command": "/build-context"}, nil, 404)
}

func TestCancelAndRetryRules(t *testing.T) {
	e := setup(t)
	j := e.submit(tokDev, e.implementTask("T-1"), 202)
	e.do(tokQA, "POST", "/api/v1/jobs/"+j.ID+"/cancel", nil, nil, 403) // not the initiator
	var got domain.Job
	e.do(tokDev, "POST", "/api/v1/jobs/"+j.ID+"/cancel", nil, &got, 202)
	if got.CancelRequestedAt == nil {
		t.Fatal("cancel not recorded")
	}
	if !strings.Contains(strings.Join(e.engine.nudges, ","), j.ID+":cancel") {
		t.Fatalf("workflow not nudged: %v", e.engine.nudges)
	}
	// Retry is only for terminal unsuccessful jobs.
	e.do(tokDev, "POST", "/api/v1/jobs/"+j.ID+"/retry", nil, nil, 409)
	forceState(t, e.store, j.ID, domain.JobFailed)
	var retry domain.Job
	e.do(tokDev, "POST", "/api/v1/jobs/"+j.ID+"/retry", nil, &retry, 202)
	if retry.RetryOf != j.ID || retry.ID == j.ID {
		t.Fatalf("retry = %+v", retry)
	}
	var again domain.Job
	e.do(tokDev, "POST", "/api/v1/jobs/"+j.ID+"/retry", nil, &again, 202)
	if again.ID != retry.ID {
		t.Fatal("retry must be idempotent per user and job")
	}
	// Operator actions only apply to NEEDS_INTERVENTION.
	e.do(tokDev, "POST", "/api/v1/jobs/"+retry.ID+"/operator-actions", map[string]any{"action": "retry"}, nil, 409)
}

// forceState drives a job through legal transitions to a terminal state,
// standing in for the workflow.
func forceState(t *testing.T, s *store.Store, jobID string, to domain.JobState) {
	t.Helper()
	ctx := context.Background()
	for _, st := range []domain.JobState{domain.JobValidating, domain.JobCleaningUp} {
		if _, err := s.TransitionJob(ctx, store.Transition{JobID: jobID, To: st}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TransitionJob(ctx, store.Transition{JobID: jobID, To: to, FailureClass: domain.FailureAgent,
		CleanupStatus: domain.CleanupCompleted}); err != nil {
		t.Fatal(err)
	}
}

// newExecution creates an execution with a known token, as Provision would.
func newExecution(t *testing.T, s *store.Store, job *domain.Job, token string) *domain.ExecutionEnvironment {
	t.Helper()
	e := &domain.ExecutionEnvironment{TenantID: job.TenantID, JobID: job.ID, Attempt: 1, Provider: "local",
		InteractionMode: domain.ModeCheckpointResume, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := s.CreateExecution(context.Background(), e, store.HashToken(token), time.Hour); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHITLRoundTripThroughExecutionAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	j := e.submit(tokDev, e.implementTask("T-1"), 202)
	for _, st := range []domain.JobState{domain.JobValidating, domain.JobQueued, domain.JobProvisioning, domain.JobPreparingWorkspace, domain.JobRunning} {
		if _, err := e.store.TransitionJob(ctx, store.Transition{JobID: j.ID, To: st}); err != nil {
			t.Fatal(err)
		}
	}
	const execTok = "exec-token-0123456789abcdef"
	ex := newExecution(t, e.store, j, execTok)
	base := "/internal/v1/executions/" + ex.ID

	// Wrong token, and user tokens, cannot use the execution API.
	e.do("nope-nope-nope", "GET", base+"/spec", nil, nil, 401)
	e.do(tokDev, "GET", base+"/spec", nil, nil, 401)

	var spec struct {
		Command      string
		Repositories []struct{ CheckoutPath, WorkingBranch string }
	}
	e.do(execTok, "GET", base+"/spec", nil, &spec, 200)
	if spec.Command != "/implement-task" || len(spec.Repositories) != 1 || spec.Repositories[0].WorkingBranch != "astrion/"+j.ID {
		t.Fatalf("spec = %+v", spec)
	}

	// Agent asks a question: persisted as RECORDED, invisible to users.
	var created struct{ InteractionID string }
	q := map[string]any{"id": "q1", "kind": "MULTIPLE_CHOICE", "prompt": "Which database?",
		"options": []map[string]string{{"value": "pg", "label": "PostgreSQL"}, {"value": "my", "label": "MySQL"}}}
	e.do(execTok, "POST", base+"/interactions", q, &created, 201)
	var dup struct{ InteractionID string }
	e.do(execTok, "POST", base+"/interactions", q, &dup, 201)
	if dup.InteractionID != created.InteractionID {
		t.Fatal("interaction creation must be idempotent per agent request id")
	}
	e.do(tokDev, "GET", "/api/v1/interactions/"+created.InteractionID, nil, nil, 404)

	// The workflow opens it (state change + notification in one transaction).
	if _, err := e.store.TransitionJob(ctx, store.Transition{JobID: j.ID, To: domain.JobWaitingForInput,
		Hook: store.OpenInteractionHook(created.InteractionID, nil)}); err != nil {
		t.Fatal(err)
	}
	var open struct{ Items []map[string]any }
	e.do(tokDev, "GET", "/api/v1/interactions?status=OPEN&mine=true", nil, &open, 200)
	if len(open.Items) != 1 || open.Items[0]["canRespond"] != true {
		t.Fatalf("open interactions = %v", open.Items)
	}

	path := "/api/v1/interactions/" + created.InteractionID
	e.do(tokQA, "POST", path+"/responses", map[string]any{"values": []string{"pg"}}, nil, 403)      // not initiator
	e.do(tokOther, "POST", path+"/responses", map[string]any{"values": []string{"pg"}}, nil, 404)   // other tenant
	e.do(tokDev, "POST", path+"/responses", map[string]any{"values": []string{"oracle"}}, nil, 422) // not an option
	e.do(tokDev, "POST", path+"/approve", nil, nil, 422)                                            // wrong kind
	key := map[string]string{"Idempotency-Key": "answer-000001"}
	e.do(tokDev, "POST", path+"/responses", map[string]any{"values": []string{"pg"}, "comment": "standard"}, nil, 200, key)
	e.do(tokDev, "POST", path+"/responses", map[string]any{"values": []string{"pg"}, "comment": "standard"}, nil, 200, key) // replay
	e.do(tokDev, "POST", path+"/responses", map[string]any{"values": []string{"my"}}, nil, 409)                             // already answered

	var st struct {
		Status   domain.InteractionStatus
		Response *domain.InteractionResponse
	}
	e.do(execTok, "GET", base+"/interactions/"+created.InteractionID, nil, &st, 200)
	if st.Status != domain.InteractionResolved || st.Response == nil || st.Response.Values[0] != "pg" {
		t.Fatalf("supervisor view = %+v", st)
	}
	var audit struct{ Items []domain.AuditEvent }
	e.do(tokAdmin, "GET", "/api/v1/audit", nil, &audit, 200)
	found := false
	for _, a := range audit.Items {
		found = found || a.Action == "interaction.respond" && a.TargetID == created.InteractionID
	}
	if !found {
		t.Fatal("response not audited")
	}
	e.do(tokDev, "GET", "/api/v1/audit", nil, nil, 403)
}

func TestExecutionArtifactsAndCompletion(t *testing.T) {
	e := setup(t)
	j := e.submit(tokDev, e.implementTask("T-1"), 202)
	const execTok = "exec-token-artifacts-000001"
	ex := newExecution(t, e.store, j, execTok)
	base := "/internal/v1/executions/" + ex.ID

	put := func(q, ctype string, body []byte, want int) map[string]any {
		r, _ := http.NewRequest("PUT", e.srv.URL+base+"/artifacts?"+q, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+execTok)
		r.Header.Set("Content-Type", ctype)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("upload %s: %d %s", q, resp.StatusCode, b)
		}
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out
	}
	a1 := put("uploadId=u1&type=diff&name=../../etc/change.diff&size=5", "text/html", []byte("hello"), 201)
	a1b := put("uploadId=u1&type=diff&name=change.diff&size=5", "text/plain", []byte("hello"), 200)
	if a1["artifactId"] != a1b["artifactId"] {
		t.Fatal("retried upload must map to the same artifact")
	}
	put("uploadId=u2&type=diff&name=big.bin", "application/octet-stream", make([]byte, 2<<20), 422)
	put("uploadId=u3&type=diff&name=x&size=10", "text/plain", []byte("short"), 422)
	cp := put("uploadId=cp&type=checkpoint&name=cp.tar.gz&classification=INTERNAL", "application/gzip", []byte("ckpt"), 201)

	var arts struct{ Items []domain.Artifact }
	e.do(tokDev, "GET", "/api/v1/jobs/"+j.ID+"/artifacts", nil, &arts, 200)
	if len(arts.Items) != 1 || arts.Items[0].Name != "change.diff" || arts.Items[0].ContentType != "application/octet-stream" {
		t.Fatalf("artifacts = %+v", arts.Items)
	}
	e.do(tokDev, "GET", "/api/v1/artifacts/"+cp["artifactId"].(string)+"/content", nil, nil, 404) // internal
	resp := e.req(tokDev, "GET", "/api/v1/artifacts/"+arts.Items[0].ID+"/content", nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %q %v", body, resp.Header)
	}

	// Logs are redacted server-side as defense in depth and deduplicated.
	batch := map[string]any{"seq": 1, "lines": []map[string]any{{"ts": time.Now(), "stream": "stdout",
		"message": "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789"}}}
	e.do(execTok, "POST", base+"/logs", batch, nil, 204)
	e.do(execTok, "POST", base+"/logs", batch, nil, 204)
	var logs struct{ Items []struct{ Message string } }
	e.do(tokDev, "GET", "/api/v1/jobs/"+j.ID+"/logs", nil, &logs, 200)
	if len(logs.Items) != 1 || strings.Contains(logs.Items[0].Message, "ghp_abc") {
		t.Fatalf("logs = %+v", logs.Items)
	}

	// Supervisors cannot claim control-plane failure classes.
	e.do(execTok, "POST", base+"/complete", map[string]any{"disposition": "FAILED", "failureClass": "POLICY"}, nil, 422)
	e.do(execTok, "POST", base+"/complete", map[string]any{"disposition": "SUSPENDED"}, nil, 422)
	e.do(execTok, "POST", base+"/complete", map[string]any{"disposition": "FAILED", "exitCode": 2,
		"failureClass": "WORKSPACE_PREPARATION", "error": "clone failed"}, nil, 204)
	got, err := e.store.GetExecution(context.Background(), ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ExecFailed || got.FailureClass != domain.FailureWorkspace {
		t.Fatalf("execution = %+v", got)
	}
	// After completion the environment may not keep writing.
	e.do(execTok, "POST", base+"/logs", batch, nil, 410)
	e.do(execTok, "POST", base+"/complete", map[string]any{"disposition": "FAILED"}, nil, 204) // replay ok
}

func TestSSEReplaysFromLastEventID(t *testing.T) {
	e := setup(t)
	j := e.submit(tokDev, e.implementTask("T-1"), 202)
	forceState(t, e.store, j.ID, domain.JobFailed)

	read := func(lastID string) []string {
		hdr := map[string]string{}
		if lastID != "" {
			hdr["Last-Event-ID"] = lastID
		}
		resp := e.req(tokDev, "GET", "/api/v1/jobs/"+j.ID+"/events", nil, hdr)
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("content type %q", ct)
		}
		var ids []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "id: "); ok {
				ids = append(ids, v)
			}
			if line == "event: end" {
				break
			}
		}
		return ids
	}
	all := read("")
	if len(all) < 4 {
		t.Fatalf("expected status events, got %v", all)
	}
	rest := read(all[1])
	if len(rest) != len(all)-2 || rest[0] != all[2] {
		t.Fatalf("replay after %s = %v (all %v)", all[1], rest, all)
	}
	// Tokens in the query string are never accepted, including for SSE.
	for _, path := range []string{"/events", ""} {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/jobs/%s%s?access_token=%s", e.srv.URL, j.ID, path, tokDev))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("query token on %q: got %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestWorkspaceAndCommandsVisibility(t *testing.T) {
	e := setup(t)
	e.do(tokView, "POST", "/api/v1/workspaces", map[string]any{"name": "x"}, nil, 403)
	var ws domain.Workspace
	e.do(tokAdmin, "POST", "/api/v1/workspaces", map[string]any{"name": "Payments", "dataClassification": "CONFIDENTIAL"}, &ws, 201)
	e.do(tokAdmin, "POST", "/api/v1/workspaces", map[string]any{"name": "Payments"}, nil, 409)
	// PATCH is a partial update: changing the description keeps everything else.
	var patched domain.Workspace
	e.do(tokAdmin, "PATCH", "/api/v1/workspaces/"+ws.ID, map[string]any{"description": "card payments"}, &patched, 200)
	if patched.Description != "card payments" || patched.Name != "Payments" || patched.DataClassification != domain.ClassConfidential {
		t.Fatalf("PATCH reset unrelated fields: %+v", patched)
	}
	e.do(tokAdmin, "PATCH", "/api/v1/workspaces/"+ws.ID, map[string]any{"name": ""}, nil, 422)
	e.do(tokAdmin, "POST", "/api/v1/workspaces/"+ws.ID+"/repositories", map[string]any{"url": "https://user:pw@github.com/a/b"}, nil, 422)
	e.do(tokAdmin, "POST", "/api/v1/workspaces/"+ws.ID+"/repositories", map[string]any{"url": "ssh://github.com/a/b"}, nil, 422)

	var cmds struct{ Items []struct{ Canonical string } }
	e.do(tokBA, "GET", "/api/v1/frameworks/"+e.boot.FrameworkID+"/commands?phase=PLAN", nil, &cmds, 200)
	names := []string{}
	for _, c := range cmds.Items {
		names = append(names, c.Canonical)
	}
	for _, want := range []string{"/shape-spec", "/write-spec", "/create-task", "/Plan-migrate", "/Create-Jira"} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Fatalf("BA plan commands %v missing %s", names, want)
		}
	}
	e.do(tokBA, "GET", "/api/v1/frameworks/"+e.boot.FrameworkID+"/commands?phase=IMPLEMENT", nil, &cmds, 200)
	if len(cmds.Items) != 0 {
		t.Fatalf("BA should not see implement commands: %v", cmds.Items)
	}
}
