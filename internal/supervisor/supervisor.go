// Package supervisor is the in-environment process that runs one execution
// attempt (SRS 5.7, 9.3, 15). It prepares the workspace, launches the agent
// CLI through its adapter, streams redacted logs, relays HITL requests,
// uploads artifacts and checkpoints, and reports a final Completion. It
// never decides the job outcome; the control plane verifies completion.
//
// Security notes: the execution token is held only by the supervisor and is
// never placed in the agent process environment; agent inputs are written to
// files (0600) rather than argv; artifact paths are confined to the output
// directory; checkpoint archives are extracted with path and symlink checks.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/redact"
)

// API is the subset of the execution API used by the supervisor.
type API interface {
	Spec(ctx context.Context) (*execapi.Spec, error)
	Status(ctx context.Context, r execapi.StatusReport) error
	Logs(ctx context.Context, b execapi.LogBatch) error
	Repository(ctx context.Context, r execapi.RepositoryReport) error
	CreateInteraction(ctx context.Context, r agent.InteractionRequest) (string, error)
	GetInteraction(ctx context.Context, id string) (*execapi.InteractionState, error)
	UploadArtifact(ctx context.Context, u execapi.ArtifactUpload, data []byte) (*execapi.ArtifactCreated, error)
	DownloadCheckpoint(ctx context.Context) ([]byte, error)
	Complete(ctx context.Context, r execapi.Completion) error
}

type Config struct {
	API          API
	Adapters     *agent.Registry
	WorkspaceDir string
	SecretsDir   string
	Log          *slog.Logger
	PollInterval time.Duration // live-session interaction polling
	KillGrace    time.Duration
	Git          string
}

type Supervisor struct {
	cfg  Config
	spec *execapi.Spec
	red  *redact.Redactor
	logs *logShipper

	reposDir, outDir, stateDir, cpDir, homeDir string

	mu            sync.Mutex
	result        *agent.Result
	pending       *pendingInteraction
	artifactTypes []string
	uploadSeq     int
	failure       *failure
}

type pendingInteraction struct {
	req agent.InteractionRequest
	id  string
}

type failure struct {
	class string
	msg   string
}

func New(cfg Config) *Supervisor {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 10 * time.Second
	}
	if cfg.Git == "" {
		cfg.Git = "git"
	}
	return &Supervisor{cfg: cfg, red: redact.New()}
}

// Run executes the attempt. It returns nil when a Completion was delivered
// (whatever the disposition) and an error only when it could not report.
func (s *Supervisor) Run(ctx context.Context) error {
	api := s.cfg.API
	spec, err := api.Spec(ctx)
	if err != nil {
		return fmt.Errorf("fetch execution spec: %w", err)
	}
	s.spec = spec
	s.loadSecretsForRedaction()

	shipCtx, stopShip := context.WithCancel(context.Background())
	s.logs = newLogShipper(api, s.red, s.cfg.Log)
	shipDone := make(chan struct{})
	go func() { s.logs.run(shipCtx); close(shipDone) }()
	defer func() { stopShip(); <-shipDone }()

	comp := s.execute(ctx)
	s.logs.flush(context.Background())

	// Report with a fresh context: cancellation of ctx (SIGTERM) must still
	// produce a final report when possible.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := api.Complete(rctx, comp); err != nil {
		return fmt.Errorf("report completion: %w", err)
	}
	return nil
}

func (s *Supervisor) fail(class domain.FailureClass, format string, args ...any) execapi.Completion {
	msg := s.red.String(fmt.Sprintf(format, args...))
	s.logs.add("supervisor", "error", msg)
	return execapi.Completion{Disposition: agent.DispositionFailed, ExitCode: -1, Error: msg, FailureClass: string(class),
		Artifacts: s.producedTypes()}
}

func (s *Supervisor) execute(ctx context.Context) execapi.Completion {
	spec, api := s.spec, s.cfg.API
	adapter, err := s.cfg.Adapters.Get(spec.Profile.Provider)
	if err != nil {
		return s.fail(domain.FailureValidation, "%v", err)
	}
	ws := s.cfg.WorkspaceDir
	s.reposDir = filepath.Join(ws, "repos")
	s.outDir = filepath.Join(ws, "out")
	s.stateDir = filepath.Join(ws, ".astrion")
	s.cpDir = filepath.Join(s.stateDir, "checkpoint")
	s.homeDir = filepath.Join(s.stateDir, "home")
	for _, d := range []string{s.reposDir, s.outDir, s.cpDir, s.homeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return s.fail(domain.FailureWorkspace, "create workspace: %v", err)
		}
	}

	_ = api.Status(ctx, execapi.StatusReport{Status: domain.ExecPreparing, Step: "prepare-workspace"})
	repos, err := s.prepareRepositories(ctx)
	if err != nil {
		return s.fail(domain.FailureWorkspace, "%v", err)
	}
	if spec.Resume != nil {
		if !spec.HasCheckpoint {
			return s.fail(domain.FailureEnvironmentLost, "resume requested but no checkpoint is available")
		}
		data, err := api.DownloadCheckpoint(ctx)
		if err != nil {
			return s.fail(domain.FailureEnvironmentLost, "download checkpoint: %v", err)
		}
		if err := extractArchive(data, s.cpDir, 512<<20); err != nil {
			return s.fail(domain.FailureEnvironmentLost, "restore checkpoint: %v", err)
		}
		s.logs.add("supervisor", "info", "checkpoint restored; resuming with response to "+spec.Resume.AgentRequestID)
	}

	workDir := s.reposDir
	if len(repos) == 1 {
		workDir = repos[0].CheckoutPath
	}
	inv, err := adapter.Invocation(agent.InvocationRequest{
		Profile: spec.Profile, Command: spec.Command, Inputs: spec.Inputs, ExpectedArtifacts: spec.ExpectedArtifacts,
		WorkDir: workDir, OutputDir: s.outDir, CheckpointDir: s.cpDir, Mode: spec.Mode, Resume: spec.Resume,
		Repositories: repos,
	})
	if err != nil {
		return s.fail(domain.FailureValidation, "build invocation: %v", err)
	}
	for p, b := range inv.Files {
		if !within(s.stateDir, p) && !within(ws, p) {
			return s.fail(domain.FailureInternal, "adapter attempted to write outside the workspace: %s", p)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return s.fail(domain.FailureWorkspace, "write invocation file: %v", err)
		}
	}

	_ = api.Status(ctx, execapi.StatusReport{Status: domain.ExecRunning, Step: "agent"})
	exitCode, runErr := s.runAgent(ctx, adapter, inv, workDir)
	if f := s.getFailure(); f != nil {
		return s.fail(domain.FailureClass(f.class), "%s", f.msg)
	}
	if ctx.Err() != nil {
		return s.fail(domain.FailureCancelled, "execution cancelled")
	}
	if runErr != nil {
		return s.fail(domain.FailureAgent, "agent process: %v", runErr)
	}

	s.mu.Lock()
	result, pending := s.result, s.pending
	s.mu.Unlock()
	awaiting := pending != nil && spec.Mode == domain.ModeCheckpointResume
	disp := adapter.Disposition(exitCode, result, awaiting)
	comp := execapi.Completion{Disposition: disp, ExitCode: exitCode}
	if result != nil {
		comp.ResultStatus, comp.Summary = result.Status, s.red.String(result.Summary)
	}
	switch disp {
	case agent.DispositionSuspended:
		data, err := createArchive(s.cpDir)
		if err != nil {
			return s.fail(domain.FailureInternal, "archive checkpoint: %v", err)
		}
		a, err := s.upload(ctx, execapi.ArtifactUpload{Type: "checkpoint", Name: "checkpoint.tar.gz",
			ContentType: "application/gzip", Classification: domain.ArtifactInternal, Step: "checkpoint"}, data)
		if err != nil {
			return s.fail(domain.FailureInternal, "upload checkpoint: %v", err)
		}
		comp.CheckpointArtifactID = a.ArtifactID
		comp.PendingInteractionID = pending.id
		s.logs.add("supervisor", "info", "checkpoint saved; compute will be released while waiting for input")
	case agent.DispositionCompleted:
		if err := s.collectRepositoryChanges(ctx, repos); err != nil {
			return s.fail(domain.FailureOutputValidation, "collect repository changes: %v", err)
		}
	default:
		if comp.Error == "" {
			comp.Error = fmt.Sprintf("agent exited with code %d", exitCode)
			if result != nil && result.Summary != "" {
				comp.Error += ": " + s.red.String(result.Summary)
			}
		}
		comp.FailureClass = string(domain.FailureAgent)
	}
	comp.Artifacts = s.producedTypes()
	return comp
}

func (s *Supervisor) setFailure(class domain.FailureClass, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = &failure{class: string(class), msg: msg}
	}
}

func (s *Supervisor) getFailure() *failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *Supervisor) producedTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.artifactTypes...)
}

func (s *Supervisor) loadSecretsForRedaction() {
	if s.cfg.SecretsDir == "" {
		return
	}
	entries, err := os.ReadDir(s.cfg.SecretsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.cfg.SecretsDir, e.Name()))
		if err == nil && len(b) < 64<<10 {
			s.red.Add(strings.TrimSpace(string(b)))
		}
	}
}

// runAgent launches the CLI in its own process group and processes its
// output until it exits or ctx is cancelled.
func (s *Supervisor) runAgent(ctx context.Context, adapter agent.Adapter, inv agent.Invocation, workDir string) (int, error) {
	cmd := exec.Command(inv.Path, inv.Args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.homeDir,
		"TMPDIR=" + os.TempDir(),
		"LANG=C.UTF-8",
		"ASTRION_OUTPUT_DIR=" + s.outDir,
	}
	for k, v := range inv.Env {
		if strings.HasPrefix(k, "ASTRION_EXECUTION_TOKEN") {
			continue
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	var stdin io.WriteCloser
	if inv.KeepStdinOpen {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return -1, err
		}
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			s.logs.add("stderr", "warn", sc.Text())
		}
	}()
	go func() {
		defer wg.Done()
		dec := adapter.NewDecoder()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			for _, ev := range dec.Decode(sc.Bytes()) {
				s.handleEvent(runCtx, adapter, ev, stdin, cancelRun)
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		wg.Wait() // drain pipes before Wait closes them
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		s.terminate(cmd)
		waitErr = <-done
	}
	if stdin != nil {
		stdin.Close()
	}
	if cmd.ProcessState == nil {
		return -1, waitErr
	}
	code := cmd.ProcessState.ExitCode()
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return code, waitErr
	}
	return code, nil
}

func (s *Supervisor) terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	t := time.AfterFunc(s.cfg.KillGrace, func() { _ = syscall.Kill(pgid, syscall.SIGKILL) })
	go func() {
		// Ensure the timer is released once the process is gone.
		for cmd.ProcessState == nil {
			time.Sleep(100 * time.Millisecond)
		}
		t.Stop()
	}()
}

func (s *Supervisor) handleEvent(ctx context.Context, adapter agent.Adapter, ev agent.Event, stdin io.Writer, abort func()) {
	api := s.cfg.API
	switch ev.Type {
	case agent.EventLog:
		s.logs.add("stdout", ev.Level, ev.Message)
	case agent.EventProgress:
		s.logs.add("supervisor", "info", "step: "+ev.Step)
		if err := api.Status(ctx, execapi.StatusReport{Status: domain.ExecRunning, Step: ev.Step, Message: s.red.String(ev.Message)}); errors.Is(err, execapi.ErrGone) {
			s.setFailure(domain.FailureCancelled, "execution is no longer active")
			abort()
		}
	case agent.EventInteraction:
		req := *ev.Interaction
		req.Prompt = s.red.String(req.Prompt)
		id, err := api.CreateInteraction(ctx, req)
		if err != nil {
			s.setFailure(domain.FailureInternal, "record interaction: "+err.Error())
			abort()
			return
		}
		s.mu.Lock()
		s.pending = &pendingInteraction{req: req, id: id}
		s.mu.Unlock()
		s.logs.add("supervisor", "info", "agent requested input ("+string(req.Kind)+"); interaction "+id)
		if s.spec.Mode == domain.ModeLiveSession {
			if stdin == nil {
				s.setFailure(domain.FailureInternal, "live session without stdin")
				abort()
				return
			}
			_ = api.Status(ctx, execapi.StatusReport{Status: domain.ExecAwaitingInput, Step: req.Step})
			// Block the event loop: a live-session agent is waiting for us.
			if err := s.awaitAndDeliver(ctx, adapter, req, id, stdin); err != nil {
				s.setFailure(domain.FailureHITLTimeout, err.Error())
				abort()
				return
			}
			s.mu.Lock()
			s.pending = nil
			s.mu.Unlock()
			_ = api.Status(ctx, execapi.StatusReport{Status: domain.ExecRunning, Step: req.Step})
		}
	case agent.EventArtifact:
		if err := s.uploadOutput(ctx, ev.Artifact); err != nil {
			s.logs.add("supervisor", "error", "artifact upload failed: "+s.red.String(err.Error()))
		}
	case agent.EventResult:
		s.mu.Lock()
		s.result = ev.Result
		s.mu.Unlock()
	case agent.EventCheckpoint:
		s.logs.add("supervisor", "info", "agent checkpoint: "+ev.Message)
	}
}

func (s *Supervisor) awaitAndDeliver(ctx context.Context, adapter agent.Adapter, req agent.InteractionRequest, id string, stdin io.Writer) error {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		st, err := s.cfg.API.GetInteraction(ctx, id)
		switch {
		case errors.Is(err, execapi.ErrGone):
			return errors.New("execution is no longer active")
		case err != nil:
			s.cfg.Log.Warn("poll interaction", "err", err)
		case st.Status == domain.InteractionResolved && st.Response != nil:
			b, err := adapter.EncodeLiveResponse(req, *st.Response)
			if err != nil {
				return err
			}
			if _, err := stdin.Write(b); err != nil {
				return fmt.Errorf("deliver response to agent: %w", err)
			}
			s.logs.add("supervisor", "info", "response delivered to live session for interaction "+id)
			return nil
		case st.Status == domain.InteractionExpired || st.Status == domain.InteractionCancelled:
			return fmt.Errorf("interaction %s %s", id, strings.ToLower(string(st.Status)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Supervisor) nextUploadID(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadSeq++
	return fmt.Sprintf("%s-%d", kind, s.uploadSeq)
}

func (s *Supervisor) upload(ctx context.Context, u execapi.ArtifactUpload, data []byte) (*execapi.ArtifactCreated, error) {
	if u.UploadID == "" {
		u.UploadID = s.nextUploadID(u.Type)
	}
	if max := s.spec.MaxArtifactBytes; max > 0 && int64(len(data)) > max {
		return nil, fmt.Errorf("artifact %s exceeds size limit (%d > %d bytes)", u.Name, len(data), max)
	}
	a, err := s.cfg.API.UploadArtifact(ctx, u, data)
	if err != nil {
		return nil, err
	}
	if u.Classification != domain.ArtifactInternal {
		s.mu.Lock()
		s.artifactTypes = append(s.artifactTypes, u.Type)
		s.mu.Unlock()
		s.logs.add("supervisor", "info", fmt.Sprintf("artifact uploaded: %s (%s, %d bytes)", u.Name, u.Type, a.SizeBytes))
	}
	return a, nil
}

func (s *Supervisor) uploadOutput(ctx context.Context, out *agent.ArtifactOutput) error {
	p := filepath.Join(s.outDir, filepath.FromSlash(out.Path))
	if !within(s.outDir, p) {
		return fmt.Errorf("artifact path %q escapes the output directory", out.Path)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("artifact %q is not a regular file", out.Path)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	name := out.Name
	if name == "" {
		name = filepath.Base(p)
	}
	ct := out.ContentType
	if ct == "" {
		ct = guessContentType(name)
	}
	_, err = s.upload(ctx, execapi.ArtifactUpload{UploadID: "out-" + out.Path, Type: out.ArtifactType, Name: name,
		ContentType: ct, Classification: domain.ArtifactUser}, data)
	return err
}

func guessContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".diff", ".patch":
		return "text/x-diff"
	case ".txt", ".log":
		return "text/plain"
	case ".xml":
		return "application/xml"
	case ".html":
		return "text/html"
	}
	return "application/octet-stream"
}

// within reports whether p is inside dir (after cleaning).
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
