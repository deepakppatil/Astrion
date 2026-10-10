package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/agent/mock"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
)

// fakeAPI records calls and simulates the control plane.
type fakeAPI struct {
	mu           sync.Mutex
	spec         execapi.Spec
	statuses     []execapi.StatusReport
	logs         []execapi.LogLine
	repos        []execapi.RepositoryReport
	interactions []agent.InteractionRequest
	artifacts    map[string][]byte // type -> data
	classes      map[string]domain.ArtifactClass
	checkpoint   []byte
	completion   *execapi.Completion
	resolveAfter int // live mode: resolve on the Nth poll
	polls        int
}

func (f *fakeAPI) Spec(context.Context) (*execapi.Spec, error) { s := f.spec; return &s, nil }
func (f *fakeAPI) Status(_ context.Context, r execapi.StatusReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, r)
	return nil
}
func (f *fakeAPI) Logs(_ context.Context, b execapi.LogBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, b.Lines...)
	return nil
}
func (f *fakeAPI) Repository(_ context.Context, r execapi.RepositoryReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, r)
	return nil
}
func (f *fakeAPI) CreateInteraction(_ context.Context, r agent.InteractionRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactions = append(f.interactions, r)
	return "int_1", nil
}
func (f *fakeAPI) GetInteraction(context.Context, string) (*execapi.InteractionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.polls < f.resolveAfter {
		return &execapi.InteractionState{InteractionID: "int_1", Status: domain.InteractionOpen}, nil
	}
	return &execapi.InteractionState{InteractionID: "int_1", Status: domain.InteractionResolved,
		Response: &domain.InteractionResponse{Decision: domain.Decision("ANSWER"), Values: []string{"exhaustive"}}}, nil
}
func (f *fakeAPI) UploadArtifact(_ context.Context, u execapi.ArtifactUpload, data []byte) (*execapi.ArtifactCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.artifacts == nil {
		f.artifacts, f.classes = map[string][]byte{}, map[string]domain.ArtifactClass{}
	}
	f.artifacts[u.Type] = data
	f.classes[u.Type] = u.Classification
	return &execapi.ArtifactCreated{ArtifactID: "art_" + u.Type, SizeBytes: int64(len(data))}, nil
}
func (f *fakeAPI) DownloadCheckpoint(context.Context) ([]byte, error) { return f.checkpoint, nil }
func (f *fakeAPI) Complete(_ context.Context, c execapi.Completion) error {
	f.completion = &c
	return nil
}

func (f *fakeAPI) allLogs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, l := range f.logs {
		b.WriteString(l.Message + "\n")
	}
	return b.String()
}

var mockAgentPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mockagent")
	if err != nil {
		panic(err)
	}
	mockAgentPath = filepath.Join(dir, "mock-agent")
	out, err := exec.Command("go", "build", "-o", mockAgentPath, "../../cmd/mock-agent").CombinedOutput()
	if err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func baseSpec(mode domain.InteractionMode, instructions string, expected ...string) execapi.Spec {
	return execapi.Spec{ExecutionID: "exe_1", JobID: "job_1", TenantID: "ten_1", Attempt: 1, Command: "/write-spec",
		Inputs:            map[string]any{"instructions": instructions},
		ExpectedArtifacts: expected,
		Profile:           domain.AgentProfile{Provider: "mock", CLIName: mockAgentPath},
		Mode:              mode, MaxArtifactBytes: 10 << 20}
}

func run(t *testing.T, api *fakeAPI) *execapi.Completion {
	t.Helper()
	s := New(Config{API: api, Adapters: agent.NewRegistry(mock.New()), WorkspaceDir: t.TempDir(),
		PollInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if api.completion == nil {
		t.Fatal("no completion reported")
	}
	return api.completion
}

func TestCheckpointSuspendAndResume(t *testing.T) {
	api := &fakeAPI{spec: baseSpec(domain.ModeCheckpointResume, "Build login", "specification")}
	c := run(t, api)
	if c.Disposition != agent.DispositionSuspended || c.PendingInteractionID != "int_1" || c.CheckpointArtifactID == "" {
		t.Fatalf("unexpected completion %+v", c)
	}
	if api.classes["checkpoint"] != domain.ArtifactInternal {
		t.Fatal("checkpoint must be an INTERNAL artifact")
	}
	if len(api.interactions) != 1 || api.interactions[0].Kind != domain.InteractionMultipleChoice {
		t.Fatalf("interactions: %+v", api.interactions)
	}
	if _, ok := api.artifacts["specification"]; ok {
		t.Fatal("specification must not exist before the question is answered")
	}

	// New attempt (fresh workspace) resumes from the checkpoint.
	spec2 := baseSpec(domain.ModeCheckpointResume, "Build login", "specification")
	spec2.Attempt = 2
	spec2.HasCheckpoint = true
	spec2.Resume = &agent.ResumeRequest{InteractionID: "int_1", AgentRequestID: "q-1",
		Response: domain.InteractionResponse{Decision: "ANSWER", Values: []string{"concise"}}}
	api2 := &fakeAPI{spec: spec2, checkpoint: api.artifacts["checkpoint"]}
	c2 := run(t, api2)
	if c2.Disposition != agent.DispositionCompleted {
		t.Fatalf("resume did not complete: %+v\nlogs:\n%s", c2, api2.allLogs())
	}
	if !strings.Contains(string(api2.artifacts["specification"]), "concise detail") {
		t.Fatalf("answer not applied: %s", api2.artifacts["specification"])
	}
	if !strings.Contains(api2.allLogs(), "resumed session") {
		t.Fatal("expected resume log")
	}
}

func TestResumeWithoutCheckpointFails(t *testing.T) {
	spec := baseSpec(domain.ModeCheckpointResume, "x", "specification")
	spec.Resume = &agent.ResumeRequest{InteractionID: "int_1", AgentRequestID: "q-1"}
	api := &fakeAPI{spec: spec}
	c := run(t, api)
	if c.Disposition != agent.DispositionFailed || c.FailureClass != string(domain.FailureEnvironmentLost) {
		t.Fatalf("%+v", c)
	}
}

func TestLiveSession(t *testing.T) {
	api := &fakeAPI{spec: baseSpec(domain.ModeLiveSession, "Build login", "specification"), resolveAfter: 3}
	c := run(t, api)
	if c.Disposition != agent.DispositionCompleted {
		t.Fatalf("%+v\n%s", c, api.allLogs())
	}
	if !strings.Contains(string(api.artifacts["specification"]), "exhaustive detail") {
		t.Fatal("live response not delivered")
	}
	sawAwaiting := false
	for _, s := range api.statuses {
		if s.Status == domain.ExecAwaitingInput {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Fatal("expected AWAITING_INPUT status")
	}
}

func TestFailureAndRedaction(t *testing.T) {
	api := &fakeAPI{spec: baseSpec(domain.ModeCheckpointResume, "[mock:no-hitl] [mock:fail] [mock:secret-leak]", "specification")}
	c := run(t, api)
	if c.Disposition != agent.DispositionFailed || c.ExitCode != 1 || c.FailureClass != string(domain.FailureAgent) {
		t.Fatalf("%+v", c)
	}
	logs := api.allLogs()
	if strings.Contains(logs, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("secret leaked into logs")
	}
	if !strings.Contains(logs, "[REDACTED]") {
		t.Fatal("expected redaction marker")
	}
}

func TestRepositoryCheckoutAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", origin}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	spec := baseSpec(domain.ModeCheckpointResume, "[mock:no-hitl]", "implementation-notes", "diff")
	spec.Command = "/implement-task"
	spec.Repositories = []execapi.RepoSpec{{RepositoryID: "repo_1", URL: "file://" + origin, Revision: "main",
		CheckoutPath: "app", WorkingBranch: "astrion/job_1"}}
	api := &fakeAPI{spec: spec}
	c := run(t, api)
	if c.Disposition != agent.DispositionCompleted {
		t.Fatalf("%+v\n%s", c, api.allLogs())
	}
	if len(api.repos) < 2 || len(api.repos[0].ResolvedCommit) != 40 || api.repos[1].FinalCommit == "" {
		t.Fatalf("repository reports: %+v", api.repos)
	}
	if !strings.Contains(string(api.artifacts["diff"]), "ASTRION_MOCK_CHANGE.md") {
		t.Fatalf("diff artifact: %q", api.artifacts["diff"])
	}

	// A bad revision fails workspace preparation and names the repository.
	spec.Repositories[0].Revision = "does-not-exist"
	api2 := &fakeAPI{spec: spec}
	c2 := run(t, api2)
	if c2.Disposition != agent.DispositionFailed || c2.FailureClass != string(domain.FailureWorkspace) || !strings.Contains(c2.Error, "repo_1") {
		t.Fatalf("%+v", c2)
	}
}

func TestArchiveRejectsTraversal(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "state.json"), []byte("{}"), 0o600)
	data, err := createArchive(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := extractArchive(data, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "state.json")); err != nil {
		t.Fatal(err)
	}
	if !within("/a/b", "/a/b/c") || within("/a/b", "/a/bc") || within("/a/b", "/a/b/../c") {
		t.Fatal("within() is wrong")
	}
}
