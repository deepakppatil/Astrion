package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/objectstore"
	"github.com/deepakppatil/astrion/internal/redact"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
)

// The internal execution API is used only by supervisors running inside
// execution environments. A per-execution bearer token scopes every call to
// one execution, its job and its tenant. These handlers only update
// execution-scoped records and nudge the workflow: the workflow remains the
// single writer of job state.

type execCtxKey struct{}

type execContext struct {
	Exec *domain.ExecutionEnvironment
	Job  *domain.Job
}

func execFrom(r *http.Request) *execContext {
	return r.Context().Value(execCtxKey{}).(*execContext)
}

func (s *Server) authenticateExecution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		execID := chi.URLParam(r, "execId")
		if !ok || tok == "" || execID == "" {
			writeError(w, domain.ErrUnauthorized)
			return
		}
		e, err := s.Store.AuthenticateExecution(r.Context(), execID, tok)
		if err != nil {
			writeError(w, domain.ErrUnauthorized)
			return
		}
		job, err := s.Store.GetJobInternal(r.Context(), e.JobID)
		if err != nil {
			writeError(w, err)
			return
		}
		// Once the attempt has finished or the job is ending, the
		// environment must stop: answer 410 so the supervisor exits.
		// Completion replays and interaction polling stay allowed.
		// (The full route pattern is not resolved yet inside a sub-router
		// middleware, so match on the path.)
		readOnly := r.Method == http.MethodGet
		if !strings.HasSuffix(r.URL.Path, "/complete") && !readOnly {
			if e.Status.Finished() || job.State.Terminal() || job.CancelRequestedAt != nil {
				writeError(w, domain.Errorf(domain.ErrExpired, "execution is no longer active"))
				return
			}
		}
		ctx := context.WithValue(r.Context(), execCtxKey{}, &execContext{Exec: e, Job: job})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) loadJobCommand(ctx context.Context, job *domain.Job) (*registry.Command, error) {
	ver, err := s.Store.JobCommandVersion(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	cmd, err := s.Store.GetCommand(ctx, job.CommandID, ver)
	if err != nil {
		return nil, err
	}
	c := cmd.WithDefaults()
	return &c, nil
}

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._\-]+`)

func checkoutPath(i int, url string) string {
	base := strings.TrimSuffix(path.Base(strings.TrimRight(url, "/")), ".git")
	base = strings.Trim(unsafePath.ReplaceAllString(base, "-"), ".-")
	if base == "" {
		base = "repo"
	}
	return fmt.Sprintf("%02d-%s", i+1, base)
}

func (s *Server) execSpec(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	ctx := r.Context()
	cmd, err := s.loadJobCommand(ctx, ec.Job)
	if err != nil {
		writeError(w, err)
		return
	}
	profile, err := s.Store.GetAgentProfile(ctx, ec.Job.TenantID, ec.Job.AgentProfileID, ec.Job.AgentProfileVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	inputs, err := cmd.ValidateInputs(ec.Job.Inputs)
	if err != nil {
		writeError(w, err)
		return
	}
	spec := execapi.Spec{ExecutionID: ec.Exec.ID, JobID: ec.Job.ID, TenantID: ec.Job.TenantID, Attempt: ec.Exec.Attempt,
		Command: ec.Job.Command, Inputs: inputs, Profile: *profile, Mode: ec.Exec.InteractionMode,
		HasCheckpoint: ec.Exec.CheckpointArtifactID != "", MaxArtifactBytes: s.Config.MaxArtifactBytes,
		TimeoutSeconds: cmd.TimeoutSeconds}
	for _, a := range cmd.ExpectedArtifacts {
		if a.Required {
			spec.ExpectedArtifacts = append(spec.ExpectedArtifacts, a.Type)
		}
	}
	repoProviders := map[string]string{}
	if repos, err := s.Store.ListRepositories(ctx, ec.Job.TenantID, ec.Job.WorkspaceID); err == nil {
		for _, rp := range repos {
			repoProviders[rp.ID] = string(rp.Provider)
		}
	}
	for i, rp := range ec.Job.Repositories {
		rs := execapi.RepoSpec{RepositoryID: rp.RepositoryID, Provider: repoProviders[rp.RepositoryID], URL: rp.URL,
			Revision: rp.RequestedRevision, CheckoutPath: checkoutPath(i, rp.URL)}
		if rp.ResolvedCommit != "" && ec.Exec.Attempt > 1 {
			rs.Revision = rp.ResolvedCommit // resumes must see the same code
		}
		if cmd.RepositoryAccess == registry.RepoWrite {
			rs.WorkingBranch = "astrion/" + ec.Job.ID
		}
		spec.Repositories = append(spec.Repositories, rs)
	}
	if ec.Exec.ResumeInteractionID != "" {
		it, err := s.Store.GetInteractionInternal(ctx, ec.Exec.ResumeInteractionID)
		if err != nil {
			writeError(w, err)
			return
		}
		if it.JobID != ec.Job.ID || it.Response == nil {
			writeError(w, domain.Errorf(domain.ErrConflict, "resume interaction is not resolved"))
			return
		}
		spec.Resume = &agent.ResumeRequest{InteractionID: it.ID, AgentRequestID: it.AgentRequestID, Response: *it.Response}
	}
	writeJSON(w, 200, spec)
}

func (s *Server) execStatus(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	var q execapi.StatusReport
	if err := decode(r, &q, 64<<10); err != nil {
		writeError(w, err)
		return
	}
	switch q.Status {
	case domain.ExecPreparing, domain.ExecRunning, domain.ExecAwaitingInput, "":
	default:
		// Final statuses are only accepted through /complete.
		writeError(w, domain.Invalid("status", "status %q cannot be reported here", q.Status))
		return
	}
	if q.Status != "" {
		if _, err := s.Store.UpdateExecution(r.Context(), ec.Exec.ID, store.ExecutionUpdate{Status: q.Status}); err != nil {
			writeError(w, err)
			return
		}
	}
	if q.Step != "" {
		if err := s.Store.SetJobStep(r.Context(), ec.Job.ID, truncate(q.Step, 200)); err != nil {
			writeError(w, err)
			return
		}
	}
	s.Engine.Nudge(r.Context(), ec.Job.ID, "exec-status")
	w.WriteHeader(http.StatusNoContent)
}

// serverRedactor applies pattern-based redaction as defense in depth; the
// supervisor already redacted exact secret values before sending.
var serverRedactor = redact.New()

func (s *Server) execLogs(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	var q execapi.LogBatch
	if err := decode(r, &q, 2<<20); err != nil {
		writeError(w, err)
		return
	}
	if len(q.Lines) > 1000 {
		writeError(w, domain.Invalid("lines", "at most 1000 lines per batch"))
		return
	}
	lines := make([]execapi.LogLine, 0, len(q.Lines))
	for _, l := range q.Lines {
		switch l.Stream {
		case "stdout", "stderr", "supervisor":
		default:
			l.Stream = "stdout"
		}
		l.Message = serverRedactor.String(truncate(strings.ToValidUTF8(l.Message, "\uFFFD"), 8192))
		lines = append(lines, l)
	}
	payload := map[string]any{"executionId": ec.Exec.ID, "attempt": ec.Exec.Attempt, "seq": q.Seq, "lines": lines}
	if _, err := s.Store.AppendLogBatch(r.Context(), ec.Job.TenantID, ec.Job.ID, ec.Exec.ID, q.Seq, payload); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) execRepository(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	var q execapi.RepositoryReport
	if err := decode(r, &q, 64<<10); err != nil {
		writeError(w, err)
		return
	}
	found := false
	for _, rp := range ec.Job.Repositories {
		found = found || rp.RepositoryID == q.RepositoryID
	}
	if !found {
		writeError(w, domain.Errorf(domain.ErrNotFound, "repository is not part of this job"))
		return
	}
	if err := s.Store.UpdateJobRepository(r.Context(), ec.Job.ID, domain.JobRepository{RepositoryID: q.RepositoryID,
		ResolvedCommit: truncate(q.ResolvedCommit, 64), CheckoutPath: truncate(q.CheckoutPath, 300),
		WorkingBranch: truncate(q.WorkingBranch, 200), FinalCommit: truncate(q.FinalCommit, 64)}); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// execCreateInteraction persists an agent's question (RECORDED) and nudges
// the workflow, which opens it and moves the job to a waiting state before
// any user is notified (HITL-001).
func (s *Server) execCreateInteraction(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	var q agent.InteractionRequest
	if err := decode(r, &q, 256<<10); err != nil {
		writeError(w, err)
		return
	}
	if q.AgentRequestID == "" || len(q.AgentRequestID) > 200 {
		writeError(w, domain.Invalid("id", "agent request id is required (max 200 chars)"))
		return
	}
	if strings.TrimSpace(q.Prompt) == "" {
		writeError(w, domain.Invalid("prompt", "required"))
		return
	}
	if len(q.Options) > 50 {
		writeError(w, domain.Invalid("options", "at most 50 options"))
		return
	}
	if q.Kind == domain.InteractionMultipleChoice && len(q.Options) == 0 {
		writeError(w, domain.Invalid("options", "multiple choice requires options"))
		return
	}
	it := &domain.Interaction{TenantID: ec.Job.TenantID, JobID: ec.Job.ID, ExecutionID: ec.Exec.ID, StepID: truncate(q.Step, 200),
		AgentRequestID: q.AgentRequestID, Kind: q.Kind, Prompt: serverRedactor.String(q.Prompt), Options: q.Options,
		Schema: q.Schema, RequestedRole: q.RequestedRole}
	if it.RequestedRole != "" && !it.RequestedRole.Valid() {
		writeError(w, domain.Invalid("requestedRole", "unknown role"))
		return
	}
	saved, err := s.Store.RecordInteraction(r.Context(), it)
	if err != nil {
		writeError(w, err)
		return
	}
	s.Engine.Nudge(r.Context(), ec.Job.ID, "interaction-recorded")
	writeJSON(w, http.StatusCreated, execapi.InteractionCreated{InteractionID: saved.ID})
}

func (s *Server) execGetInteraction(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	it, err := s.Store.GetInteractionInternal(r.Context(), chi.URLParam(r, "iid"))
	if err != nil || it.JobID != ec.Job.ID || (it.ExecutionID != ec.Exec.ID && it.ID != ec.Exec.ResumeInteractionID) {
		writeError(w, domain.Errorf(domain.ErrNotFound, "interaction"))
		return
	}
	writeJSON(w, 200, execapi.InteractionState{InteractionID: it.ID, Status: it.Status, Response: it.Response})
}

var artifactTypeRe = regexp.MustCompile(`^[a-z][a-z0-9\-]{0,63}$`)

// uploadArtifactID derives a stable ID so a retried upload maps to the same
// artifact (exactly-once metadata).
func uploadArtifactID(execID, uploadID string) string {
	sum := sha256.Sum256([]byte(execID + "\x00" + uploadID))
	return string(domain.PrefixArtifact) + "_" + hex.EncodeToString(sum[:12])
}

func (s *Server) execUploadArtifact(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	q := r.URL.Query()
	uploadID, typ, name := q.Get("uploadId"), q.Get("type"), q.Get("name")
	if uploadID == "" || len(uploadID) > 200 {
		writeError(w, domain.Invalid("uploadId", "required"))
		return
	}
	if !artifactTypeRe.MatchString(typ) {
		writeError(w, domain.Invalid("type", "must match %s", artifactTypeRe))
		return
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == "/" || len(name) > 200 || !utf8.ValidString(name) {
		writeError(w, domain.Invalid("name", "invalid file name"))
		return
	}
	class := domain.ArtifactClass(q.Get("classification"))
	switch class {
	case "":
		class = domain.ArtifactUser
	case domain.ArtifactUser, domain.ArtifactInternal:
	default:
		writeError(w, domain.Invalid("classification", "must be USER or INTERNAL"))
		return
	}
	declared, _ := strconv.ParseInt(q.Get("size"), 10, 64)
	if declared > s.Config.MaxArtifactBytes || r.ContentLength > s.Config.MaxArtifactBytes {
		writeError(w, domain.Invalid("size", "artifact exceeds the %d byte limit", s.Config.MaxArtifactBytes))
		return
	}
	repoID := q.Get("repositoryId")
	if repoID != "" {
		found := false
		for _, rp := range ec.Job.Repositories {
			found = found || rp.RepositoryID == repoID
		}
		if !found {
			writeError(w, domain.Invalid("repositoryId", "repository is not part of this job"))
			return
		}
	}
	id := uploadArtifactID(ec.Exec.ID, uploadID)
	if existing, err := s.Store.GetArtifactInternal(r.Context(), id); err == nil {
		writeJSON(w, 200, execapi.ArtifactCreated{ArtifactID: existing.ID, SHA256: existing.SHA256, SizeBytes: existing.SizeBytes})
		return
	}
	ctype := sanitizeContentType(r.Header.Get("Content-Type"))
	key := objectstore.Key(ec.Job.TenantID, ec.Job.ID, id, name)
	body := &limitedReader{r: r.Body, n: s.Config.MaxArtifactBytes}
	sha, size, err := s.Objects.Put(r.Context(), key, body, ctype)
	if body.exceeded {
		_ = s.Objects.Delete(context.WithoutCancel(r.Context()), key)
		writeError(w, domain.Invalid("size", "artifact exceeds the %d byte limit", s.Config.MaxArtifactBytes))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if declared > 0 && declared != size {
		_ = s.Objects.Delete(context.WithoutCancel(r.Context()), key)
		writeError(w, domain.Invalid("size", "received %d bytes, declared %d", size, declared))
		return
	}
	a := &domain.Artifact{ID: id, TenantID: ec.Job.TenantID, JobID: ec.Job.ID, ExecutionID: ec.Exec.ID, Type: typ, Name: name,
		ObjectKey: key, ContentType: ctype, SizeBytes: size, SHA256: sha, Classification: class,
		SourceStep: truncate(q.Get("step"), 200), RepositoryID: repoID}
	if err := s.Store.CreateArtifact(r.Context(), a); err != nil {
		if existing, gerr := s.Store.GetArtifactInternal(r.Context(), id); gerr == nil {
			a = existing // concurrent retry won
		} else {
			writeError(w, err)
			return
		}
	}
	s.Metrics.Add("artifacts_uploaded_total", 1)
	writeJSON(w, http.StatusCreated, execapi.ArtifactCreated{ArtifactID: a.ID, SHA256: a.SHA256, SizeBytes: a.SizeBytes})
}

func (s *Server) execCheckpoint(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	if ec.Exec.CheckpointArtifactID == "" {
		writeError(w, domain.Errorf(domain.ErrNotFound, "no checkpoint for this execution"))
		return
	}
	a, err := s.Store.GetArtifactInternal(r.Context(), ec.Exec.CheckpointArtifactID)
	if err != nil || a.JobID != ec.Job.ID || a.Classification != domain.ArtifactInternal {
		writeError(w, domain.Errorf(domain.ErrNotFound, "checkpoint"))
		return
	}
	rc, err := s.Objects.Get(r.Context(), a.ObjectKey)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	_, _ = io.Copy(w, rc)
}

func (s *Server) execComplete(w http.ResponseWriter, r *http.Request) {
	ec := execFrom(r)
	var q execapi.Completion
	if err := decode(r, &q, 256<<10); err != nil {
		writeError(w, err)
		return
	}
	u := store.ExecutionUpdate{ExitCode: ptr(q.ExitCode), ResultStatus: truncate(q.ResultStatus, 50),
		Summary: serverRedactor.String(truncate(q.Summary, 4000)), ErrorMessage: serverRedactor.String(truncate(q.Error, 4000))}
	switch q.Disposition {
	case agent.DispositionCompleted:
		u.Status = domain.ExecCompleted
	case agent.DispositionSuspended:
		if q.PendingInteractionID == "" || q.CheckpointArtifactID == "" {
			writeError(w, domain.Invalid("disposition", "SUSPENDED requires pendingInteractionId and checkpointArtifactId"))
			return
		}
		it, err := s.Store.GetInteractionInternal(r.Context(), q.PendingInteractionID)
		if err != nil || it.ExecutionID != ec.Exec.ID {
			writeError(w, domain.Invalid("pendingInteractionId", "unknown interaction"))
			return
		}
		cp, err := s.Store.GetArtifactInternal(r.Context(), q.CheckpointArtifactID)
		if err != nil || cp.ExecutionID != ec.Exec.ID || cp.Classification != domain.ArtifactInternal {
			writeError(w, domain.Invalid("checkpointArtifactId", "unknown checkpoint artifact"))
			return
		}
		u.Status = domain.ExecSuspended
		u.PendingInteractionID = &q.PendingInteractionID
		u.CheckpointArtifactID = q.CheckpointArtifactID
	case agent.DispositionFailed:
		u.Status = domain.ExecFailed
		u.FailureClass = domain.FailureAgent
		if fc := domain.FailureClass(q.FailureClass); fc != "" {
			if !supervisorFailureClasses[fc] {
				writeError(w, domain.Invalid("failureClass", "failure class %q cannot be reported by a supervisor", fc))
				return
			}
			u.FailureClass = fc
		}
	default:
		writeError(w, domain.Invalid("disposition", "must be COMPLETED, SUSPENDED or FAILED"))
		return
	}
	if _, err := s.Store.UpdateExecution(r.Context(), ec.Exec.ID, u); err != nil {
		writeError(w, err)
		return
	}
	s.Engine.Nudge(r.Context(), ec.Job.ID, "exec-complete")
	w.WriteHeader(http.StatusNoContent)
}

// supervisorFailureClasses are the classes an environment may claim; policy,
// quota and authorization failures are decided by the control plane only.
var supervisorFailureClasses = map[domain.FailureClass]bool{
	domain.FailureWorkspace: true, domain.FailureAgent: true, domain.FailureOutputValidation: true,
	domain.FailureTimeout: true, domain.FailureInternal: true,
}

// ---------------------------------------------------------------------------

type limitedReader struct {
	r        io.Reader
	n        int64
	exceeded bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			l.exceeded = true
			return 0, errors.New("artifact too large")
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

func readAllLimit(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, domain.Errorf(domain.ErrInvalid, "read body: %v", err)
	}
	if int64(len(b)) > limit {
		return nil, domain.Errorf(domain.ErrInvalid, "%v", errTooLarge)
	}
	return b, nil
}

var safeTypes = map[string]bool{
	"text/plain": true, "text/markdown": true, "text/x-diff": true, "text/csv": true, "application/json": true,
	"application/xml": true, "application/yaml": true, "application/zip": true, "application/gzip": true,
	"application/x-tar": true, "application/octet-stream": true, "application/pdf": true, "image/png": true,
	"application/junit+xml": true, "application/x-ndjson": true,
}

// sanitizeContentType keeps known media types and downgrades everything
// else (notably text/html and image/svg+xml) to application/octet-stream.
func sanitizeContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || !safeTypes[mt] {
		return "application/octet-stream"
	}
	return mt
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
