package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/workflow"
)

type jobRepoRequest struct {
	RepositoryID string `json:"repositoryId"`
	Revision     string `json:"revision"`
}

type submitJobRequest struct {
	WorkspaceID    string            `json:"workspaceId"`
	FrameworkID    string            `json:"frameworkId"`
	Command        string            `json:"command"` // canonical, exact casing
	AgentProfileID string            `json:"agentProfileId"`
	Provider       string            `json:"provider"` // AUTO or a pool name
	Inputs         json.RawMessage   `json:"inputs"`
	Repositories   []jobRepoRequest  `json:"repositories"`
	ParentJobID    string            `json:"parentJobId"`
	Links          map[string]string `json:"links"`
}

var revisionRe = regexp.MustCompile(`^[A-Za-z0-9._/\-]{1,200}$`)
var idemKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:\-]{8,128}$`)

// submitJob durably accepts a job and returns its ID (SRS JOB-001). All
// checks that can be made synchronously are made here so users get immediate
// feedback; the workflow re-validates authoritative state before executing.
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	body, err := readBody(r, 256<<10)
	if err != nil {
		writeError(w, err)
		return
	}
	var q submitJobRequest
	if err := strictJSON(body, &q); err != nil {
		writeError(w, err)
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" && !idemKeyRe.MatchString(idemKey) {
		writeError(w, domain.Invalid("Idempotency-Key", "must be 8-128 characters of [A-Za-z0-9._:-]"))
		return
	}
	sum := sha256.Sum256(body)
	job, created, err := s.acceptJob(r.Context(), p, q, idemKey, hex.EncodeToString(sum[:]), "")
	if err != nil {
		writeError(w, err)
		return
	}
	if created {
		s.Metrics.Add("jobs_submitted_total", 1)
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) acceptJob(ctx context.Context, p *authz.Principal, q submitJobRequest, idemKey, reqHash, retryOf string) (*domain.Job, bool, error) {
	if q.WorkspaceID == "" {
		return nil, false, domain.Invalid("workspaceId", "required")
	}
	if q.Command == "" {
		return nil, false, domain.Invalid("command", "required")
	}
	ws, err := s.Store.GetWorkspace(ctx, p.TenantID, q.WorkspaceID)
	if err != nil {
		return nil, false, err
	}
	if q.FrameworkID == "" {
		q.FrameworkID = ws.DefaultFrameworkID
	}
	if q.FrameworkID == "" {
		return nil, false, domain.Invalid("frameworkId", "required (workspace has no default framework)")
	}
	fw, err := s.Store.GetFramework(ctx, p.TenantID, q.FrameworkID)
	if err != nil {
		return nil, false, err
	}
	cmd, err := s.Store.FindCommand(ctx, fw.ID, q.Command)
	if err != nil {
		return nil, false, domain.Invalid("command", "unknown command %q for framework %s", q.Command, fw.Name)
	}
	if cmd.Status != registry.StatusApproved {
		return nil, false, domain.Invalid("command", "command %s is %s", cmd.Canonical, cmd.Status)
	}
	if err := registry.Authorize(p, cmd); err != nil {
		return nil, false, err
	}
	inputs, err := cmd.ValidateInputs(q.Inputs)
	if err != nil {
		return nil, false, err
	}
	// Artifact references must resolve to user-visible artifacts in the
	// caller's tenant; otherwise a job could read another tenant's output.
	for _, f := range cmd.Inputs {
		if f.Type != registry.FieldArtifactRef {
			continue
		}
		if ref, ok := inputs[f.Name].(string); ok && ref != "" {
			a, err := s.Store.GetArtifact(ctx, p.TenantID, ref)
			if err != nil || a.Classification != domain.ArtifactUser {
				return nil, false, domain.Invalid("inputs."+f.Name, "unknown artifact %q", ref)
			}
		}
	}
	if q.AgentProfileID == "" {
		q.AgentProfileID = ws.DefaultAgentProfileID
	}
	if q.AgentProfileID == "" {
		return nil, false, domain.Invalid("agentProfileId", "required (workspace has no default agent)")
	}
	if len(ws.PermittedAgentProfiles) > 0 && !contains(ws.PermittedAgentProfiles, q.AgentProfileID) {
		return nil, false, domain.Invalid("agentProfileId", "agent profile is not permitted in this workspace")
	}
	profile, err := s.Store.GetAgentProfile(ctx, p.TenantID, q.AgentProfileID, 0)
	if err != nil {
		return nil, false, domain.Invalid("agentProfileId", "unknown agent profile")
	}
	if !profile.Enabled {
		return nil, false, domain.Invalid("agentProfileId", "agent profile %s is disabled", profile.Name)
	}
	if !cmd.SupportsAgent(profile.Provider) {
		return nil, false, domain.Invalid("agentProfileId", "command %s does not support agent %s", cmd.Canonical, profile.Provider)
	}
	if q.Provider == "" {
		q.Provider = ws.DefaultProvider
	}
	if q.Provider == "" {
		q.Provider = "AUTO"
	}
	if q.Provider != "AUTO" {
		if q.Provider != ws.DefaultProvider && !p.Has(domain.PermProviderSelect) {
			return nil, false, domain.Errorf(domain.ErrForbidden, "selecting an execution pool requires %s", domain.PermProviderSelect)
		}
		if _, err := s.Pools.Get(q.Provider); err != nil {
			return nil, false, domain.Invalid("provider", "unknown execution pool %q", q.Provider)
		}
	}

	// Repositories: must belong to the workspace; default to all of them.
	wsRepos, err := s.Store.ListRepositories(ctx, p.TenantID, ws.ID)
	if err != nil {
		return nil, false, err
	}
	byID := map[string]domain.Repository{}
	for _, rp := range wsRepos {
		byID[rp.ID] = rp
	}
	var repos []domain.JobRepository
	if len(q.Repositories) == 0 && cmd.RepositoryAccess != registry.RepoNone {
		for _, rp := range wsRepos {
			rev := rp.PinnedRevision
			if rev == "" {
				rev = rp.DefaultBranch
			}
			repos = append(repos, domain.JobRepository{RepositoryID: rp.ID, URL: rp.URL, RequestedRevision: rev})
		}
	}
	seen := map[string]bool{}
	for i, rq := range q.Repositories {
		rp, ok := byID[rq.RepositoryID]
		if !ok {
			return nil, false, domain.Invalid("repositories", "repository %d is not part of the workspace", i)
		}
		if seen[rp.ID] {
			return nil, false, domain.Invalid("repositories", "repository %s listed twice", rp.ID)
		}
		seen[rp.ID] = true
		rev := rq.Revision
		if rev == "" {
			rev = rp.PinnedRevision
		}
		if rev == "" {
			rev = rp.DefaultBranch
		}
		if !revisionRe.MatchString(rev) || strings.HasPrefix(rev, "-") || strings.Contains(rev, "..") {
			return nil, false, domain.Invalid("repositories", "invalid revision %q", rev)
		}
		repos = append(repos, domain.JobRepository{RepositoryID: rp.ID, URL: rp.URL, RequestedRevision: rev})
	}
	if cmd.RepositoryAccess != registry.RepoNone && len(repos) == 0 {
		return nil, false, domain.Invalid("repositories", "command %s requires at least one repository", cmd.Canonical)
	}
	if len(repos) > 20 {
		return nil, false, domain.Invalid("repositories", "at most 20 repositories per job")
	}
	if q.ParentJobID != "" {
		if _, err := s.Store.GetJob(ctx, p.TenantID, q.ParentJobID); err != nil {
			return nil, false, domain.Invalid("parentJobId", "unknown parent job")
		}
	}
	if len(q.Links) > 20 {
		return nil, false, domain.Invalid("links", "too many links")
	}

	// Admission control (SRS 18.2): per-user rate and per-tenant queue depth.
	// Idempotent replays are answered by CreateJob before these limits matter
	// only if the key exists, so check the replay path first.
	if idemKey == "" || !s.isReplay(ctx, p, idemKey) {
		if err := s.admit(ctx, p); err != nil {
			return nil, false, err
		}
	}

	inputJSON, _ := json.Marshal(inputs)
	job := &domain.Job{TenantID: p.TenantID, WorkspaceID: ws.ID, InitiatedBy: p.UserID, Phase: cmd.Phase, Command: cmd.Canonical,
		CommandID: cmd.ID, AgentProfileID: profile.ID, AgentProfileVersion: profile.Version, FrameworkID: fw.ID,
		FrameworkRevision: fw.Revision, RequestedProvider: q.Provider, Inputs: inputJSON, Repositories: repos,
		RetryOf: retryOf, ParentJobID: q.ParentJobID, Links: q.Links}
	job, created, err := s.Store.CreateJob(ctx, store.NewJob{Job: job, CommandVersion: cmd.Version, IdempotencyKey: idemKey, RequestHash: reqHash})
	if err != nil {
		return nil, false, err
	}
	// Starting the workflow is best effort here: the job is already durably
	// accepted and the dispatcher starts any workflow we fail to start.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Engine.StartJob(sctx, job.ID); err != nil {
		s.log().Warn("workflow start deferred to dispatcher", "job", job.ID, "err", err)
	}
	return job, created, nil
}

func (s *Server) isReplay(ctx context.Context, p *authz.Principal, key string) bool {
	var n int
	_ = s.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE tenant_id=$1 AND initiated_by=$2 AND idempotency_key=$3`,
		p.TenantID, p.UserID, key).Scan(&n)
	return n > 0
}

func (s *Server) admit(ctx context.Context, p *authz.Principal) error {
	t, err := s.Store.GetTenant(ctx, p.TenantID)
	if err != nil {
		return err
	}
	rate := t.Quotas.MaxSubmissionsPerUserPerMinute
	if rate == 0 {
		rate = s.Config.MaxSubmissionsPerMinute
	}
	if rate > 0 {
		n, err := s.Store.CountRecentSubmissions(ctx, p.UserID, time.Minute)
		if err != nil {
			return err
		}
		if n >= rate {
			return domain.Errorf(domain.ErrRateLimited, "submission rate limit of %d per minute exceeded", rate)
		}
	}
	queued := t.Quotas.MaxQueuedJobs
	if queued == 0 {
		queued = s.Config.MaxQueuedPerTenant
	}
	if queued > 0 {
		n, err := s.Store.CountQueued(ctx, p.TenantID)
		if err != nil {
			return err
		}
		if n >= queued {
			return domain.Errorf(domain.ErrRateLimited, "tenant queue limit of %d jobs reached", queued)
		}
	}
	return nil
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermJobRead); err != nil {
		writeError(w, err)
		return
	}
	f := store.JobFilter{TenantID: p.TenantID, WorkspaceID: r.URL.Query().Get("workspaceId"), Limit: queryInt(r, "limit", 50)}
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 50
	}
	for _, st := range r.URL.Query()["state"] {
		for _, v := range strings.Split(st, ",") {
			js := domain.JobState(strings.TrimSpace(v))
			if !js.Valid() {
				writeError(w, domain.Invalid("state", "unknown state %q", v))
				return
			}
			f.States = append(f.States, js)
		}
	}
	if r.URL.Query().Get("mine") == "true" {
		f.InitiatedBy = p.UserID
	}
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeError(w, domain.Invalid("before", "must be RFC3339"))
			return
		}
		f.Before = &t
	}
	jobs, err := s.Store.ListJobs(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := list(jobs)
	if len(jobs) == f.Limit {
		resp["nextBefore"] = jobs[len(jobs)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, 200, resp)
}

// loadJob fetches a job in the caller's tenant (other tenants see 404).
func (s *Server) loadJob(r *http.Request) (*domain.Job, error) {
	p := principal(r)
	if err := authz.Require(p, domain.PermJobRead); err != nil {
		return nil, err
	}
	return s.Store.GetJob(r.Context(), p.TenantID, chi.URLParam(r, "id"))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"job": job, "allowedTransitions": job.State.NextStates(),
		"actions": s.jobActions(principal(r), job)})
}

// jobActions tells the UI which controls to show (the server re-checks).
func (s *Server) jobActions(p *authz.Principal, j *domain.Job) map[string]bool {
	owner := j.InitiatedBy == p.UserID
	admin := p.Has(domain.PermJobCancelAny)
	return map[string]bool{
		"cancel":   !j.State.Terminal() && j.CancelRequestedAt == nil && (owner || admin),
		"retry":    j.State.Terminal() && j.State != domain.JobSucceeded && (owner || admin),
		"operator": j.State == domain.JobNeedsIntervention && (owner || admin),
	}
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if job.InitiatedBy != p.UserID && !p.Has(domain.PermJobCancelAny) {
		writeError(w, domain.Errorf(domain.ErrForbidden, "only the initiator or an administrator can cancel this job"))
		return
	}
	if job.State.Terminal() {
		writeError(w, domain.Errorf(domain.ErrConflict, "job is already %s", job.State))
		return
	}
	job, err = s.Store.RequestCancel(r.Context(), p.TenantID, job.ID, p.UserID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.Engine.Nudge(r.Context(), job.ID, "cancel")
	writeJSON(w, http.StatusAccepted, job)
}

// retryJob creates a new job with the same parameters, linked by RetryOf.
// The original job stays immutable (its history is the audit record). The
// command is re-resolved at its current version and re-authorized.
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	orig, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if orig.InitiatedBy != p.UserID && !p.Has(domain.PermJobCancelAny) {
		writeError(w, domain.Errorf(domain.ErrForbidden, "only the initiator or an administrator can retry this job"))
		return
	}
	if !orig.State.Terminal() || orig.State == domain.JobSucceeded {
		writeError(w, domain.Errorf(domain.ErrConflict, "only failed, cancelled or timed-out jobs can be retried (job is %s)", orig.State))
		return
	}
	q := submitJobRequest{WorkspaceID: orig.WorkspaceID, FrameworkID: orig.FrameworkID, Command: orig.Command,
		AgentProfileID: orig.AgentProfileID, Provider: orig.RequestedProvider, Inputs: orig.Inputs,
		ParentJobID: orig.ParentJobID, Links: orig.Links}
	for _, rp := range orig.Repositories {
		q.Repositories = append(q.Repositories, jobRepoRequest{RepositoryID: rp.RepositoryID, Revision: rp.RequestedRevision})
	}
	idem := r.Header.Get("Idempotency-Key")
	if idem == "" {
		idem = "retry:" + orig.ID + ":" + p.UserID
	}
	job, _, err := s.acceptJob(r.Context(), p, q, idem, "retry:"+orig.ID, orig.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

type operatorRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// operatorAction resolves NEEDS_INTERVENTION (retry the attempt, reopen the
// timed-out interaction, or cancel). Restricted to the initiator and admins.
func (s *Server) operatorAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if job.InitiatedBy != p.UserID && !p.Has(domain.PermJobCancelAny) {
		writeError(w, domain.Errorf(domain.ErrForbidden, "operator actions require the initiator or an administrator"))
		return
	}
	var q operatorRequest
	if err := decode(r, &q, 0); err != nil {
		writeError(w, err)
		return
	}
	switch q.Action {
	case workflow.OperatorRetry, workflow.OperatorReopen, workflow.OperatorCancel:
	default:
		writeError(w, domain.Invalid("action", "must be retry, reopen or cancel"))
		return
	}
	if job.State != domain.JobNeedsIntervention {
		writeError(w, domain.Errorf(domain.ErrConflict, "job is %s, not NEEDS_INTERVENTION", job.State))
		return
	}
	if err := s.Store.Audit(r.Context(), domain.AuditEvent{TenantID: p.TenantID, Actor: p.UserID, ActorType: "user",
		Action: "job.operator." + q.Action, TargetType: "job", TargetID: job.ID, Details: store.AuditDetails("reason", q.Reason)}); err != nil {
		writeError(w, err)
		return
	}
	if err := s.Engine.Operator(r.Context(), job.ID, workflow.OperatorSignal{Action: q.Action, Actor: p.UserID, Reason: q.Reason}); err != nil {
		writeError(w, domain.Errorf(domain.ErrUnavailable, "could not deliver operator action: %v", err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) jobExecutions(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListExecutions(r.Context(), job.TenantID, job.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

func (s *Server) jobInteractions(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListInteractions(r.Context(), job.TenantID, job.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(visibleInteractions(items)))
}

// visibleInteractions hides RECORDED interactions: they are persisted but not
// yet opened by the workflow, so users cannot answer them yet.
func visibleInteractions(items []domain.Interaction) []domain.Interaction {
	out := items[:0:0]
	for _, it := range items {
		if it.Status != domain.InteractionRecorded {
			out = append(out, it)
		}
	}
	return out
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	b, err := readAllLimit(r, limit)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.Errorf(domain.ErrInvalid, "malformed request body: %v", err)
	}
	return nil
}

var errTooLarge = errors.New("request body too large")
