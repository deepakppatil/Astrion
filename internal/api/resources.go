package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, 200, map[string]any{"user": p, "permissions": authz.Permissions(p.Roles)})
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermJobRead); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.Store.Dashboard(r.Context(), p.TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, st)
}

// --- Workspaces ---

type workspaceRequest struct {
	Name                   string                    `json:"name"`
	Description            string                    `json:"description"`
	Type                   domain.WorkspaceType      `json:"type"`
	LifecycleMode          domain.WorkspaceLifecycle `json:"lifecycleMode"`
	DataClassification     domain.DataClassification `json:"dataClassification"`
	DefaultAgentProfileID  string                    `json:"defaultAgentProfileId"`
	DefaultFrameworkID     string                    `json:"defaultFrameworkId"`
	DefaultProvider        string                    `json:"defaultProvider"`
	PermittedAgentProfiles []string                  `json:"permittedAgentProfiles"`
	PermittedProviders     []string                  `json:"permittedProviders"`
}

func (q *workspaceRequest) validate() error {
	q.Name = strings.TrimSpace(q.Name)
	if q.Name == "" || len(q.Name) > 120 {
		return domain.Invalid("name", "required, at most 120 characters")
	}
	switch q.Type {
	case "":
		q.Type = domain.WorkspaceMultiRepo
	case domain.WorkspaceSingleRepo, domain.WorkspaceMultiRepo, domain.WorkspaceManaged, domain.WorkspaceEphemeral, domain.WorkspaceCustom:
	default:
		return domain.Invalid("type", "unknown workspace type %q", q.Type)
	}
	switch q.LifecycleMode {
	case "":
		q.LifecycleMode = domain.LifecycleEphemeralCheckout
	case domain.LifecycleEphemeralCheckout, domain.LifecyclePersistent:
	default:
		return domain.Invalid("lifecycleMode", "unknown lifecycle mode %q", q.LifecycleMode)
	}
	switch q.DataClassification {
	case "":
		q.DataClassification = domain.ClassInternal
	case domain.ClassPublic, domain.ClassInternal, domain.ClassConfidential, domain.ClassRestricted:
	default:
		return domain.Invalid("dataClassification", "unknown classification %q", q.DataClassification)
	}
	return nil
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceRead); err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListWorkspaces(r.Context(), p.TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceWrite); err != nil {
		writeError(w, err)
		return
	}
	var q workspaceRequest
	if err := decode(r, &q, 0); err != nil {
		writeError(w, err)
		return
	}
	if err := q.validate(); err != nil {
		writeError(w, err)
		return
	}
	ws := &domain.Workspace{TenantID: p.TenantID, Name: q.Name, Description: q.Description, Type: q.Type,
		LifecycleMode: q.LifecycleMode, DataClassification: q.DataClassification, DefaultAgentProfileID: q.DefaultAgentProfileID,
		DefaultFrameworkID: q.DefaultFrameworkID, DefaultProvider: q.DefaultProvider,
		PermittedAgentProfiles: q.PermittedAgentProfiles, PermittedProviders: q.PermittedProviders, CreatedBy: p.UserID}
	if err := s.Store.CreateWorkspace(r.Context(), ws); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceRead); err != nil {
		writeError(w, err)
		return
	}
	ws, err := s.Store.GetWorkspace(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, ws)
}

func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceWrite); err != nil {
		writeError(w, err)
		return
	}
	ws, err := s.Store.GetWorkspace(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	var q workspaceRequest
	if err := decode(r, &q, 0); err != nil {
		writeError(w, err)
		return
	}
	if err := q.validate(); err != nil {
		writeError(w, err)
		return
	}
	ws.Name, ws.Description, ws.Type, ws.LifecycleMode, ws.DataClassification = q.Name, q.Description, q.Type, q.LifecycleMode, q.DataClassification
	ws.DefaultAgentProfileID, ws.DefaultFrameworkID, ws.DefaultProvider = q.DefaultAgentProfileID, q.DefaultFrameworkID, q.DefaultProvider
	ws.PermittedAgentProfiles, ws.PermittedProviders = q.PermittedAgentProfiles, q.PermittedProviders
	if err := s.Store.UpdateWorkspace(r.Context(), p.UserID, ws); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, ws)
}

func (s *Server) listRepositories(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceRead); err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListRepositories(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

type repositoryRequest struct {
	Provider            domain.RepoProvider `json:"provider"`
	URL                 string              `json:"url"`
	Namespace           string              `json:"namespace"`
	DefaultBranch       string              `json:"defaultBranch"`
	CredentialRef       string              `json:"credentialRef"`
	CheckoutPolicy      string              `json:"checkoutPolicy"`
	PermittedOperations []string            `json:"permittedOperations"`
	PinnedRevision      string              `json:"pinnedRevision"`
}

// validateRepoURL accepts https URLs (and file URLs outside production for
// local development). Embedded credentials are rejected: credentials are
// always references into the secret manager.
func validateRepoURL(raw string, production bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" && u.Scheme != "file" {
		return domain.Invalid("url", "must be an absolute repository URL")
	}
	if u.User != nil {
		return domain.Invalid("url", "must not contain credentials; use credentialRef")
	}
	switch u.Scheme {
	case "https":
	case "file":
		if production {
			return domain.Invalid("url", "file repositories are not allowed in production")
		}
	default:
		return domain.Invalid("url", "scheme %q is not allowed (use https)", u.Scheme)
	}
	return nil
}

func (s *Server) addRepository(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceWrite); err != nil {
		writeError(w, err)
		return
	}
	var q repositoryRequest
	if err := decode(r, &q, 0); err != nil {
		writeError(w, err)
		return
	}
	if err := validateRepoURL(q.URL, s.Config.Production); err != nil {
		writeError(w, err)
		return
	}
	switch q.Provider {
	case "":
		q.Provider = domain.RepoGeneric
		if strings.Contains(q.URL, "github.com") {
			q.Provider = domain.RepoGitHub
		} else if strings.Contains(q.URL, "gitlab") {
			q.Provider = domain.RepoGitLab
		}
	case domain.RepoGitHub, domain.RepoGitLab, domain.RepoGeneric:
	default:
		writeError(w, domain.Invalid("provider", "must be GITHUB, GITLAB or GIT"))
		return
	}
	if q.DefaultBranch == "" {
		q.DefaultBranch = "main"
	}
	if q.CheckoutPolicy == "" {
		q.CheckoutPolicy = "SHALLOW"
	}
	if len(q.PermittedOperations) == 0 {
		q.PermittedOperations = []string{"read"}
	}
	repo := &domain.Repository{TenantID: p.TenantID, WorkspaceID: chi.URLParam(r, "id"), Provider: q.Provider, URL: q.URL,
		Namespace: q.Namespace, DefaultBranch: q.DefaultBranch, CredentialRef: q.CredentialRef, CheckoutPolicy: q.CheckoutPolicy,
		PermittedOperations: q.PermittedOperations, PinnedRevision: q.PinnedRevision}
	if err := s.Store.AddRepository(r.Context(), p.UserID, repo); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) removeRepository(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermWorkspaceWrite); err != nil {
		writeError(w, err)
		return
	}
	if err := s.Store.RemoveRepository(r.Context(), p.UserID, p.TenantID, chi.URLParam(r, "id"), chi.URLParam(r, "repoId")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Agents, frameworks, pools, integrations ---

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	items, err := s.Store.ListAgentProfiles(r.Context(), p.TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	type agentView struct {
		domain.AgentProfile
		Capabilities any `json:"capabilities,omitempty"`
	}
	out := make([]agentView, 0, len(items))
	for _, a := range items {
		v := agentView{AgentProfile: a}
		if ad, err := s.Agents.Get(a.Provider); err == nil {
			v.Capabilities = ad.Capabilities()
		}
		if !p.Has(domain.PermAgentConfigure) {
			v.Environment = nil // configuration details are admin-only
		}
		out = append(out, v)
	}
	writeJSON(w, 200, list(out))
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Store.GetAgentProfile(r.Context(), p.TenantID, chi.URLParam(r, "id"), queryInt(r, "version", 0))
	if err != nil {
		writeError(w, err)
		return
	}
	if !p.Has(domain.PermAgentConfigure) {
		a.Environment = nil
	}
	writeJSON(w, 200, a)
}

func (s *Server) saveAgent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermAgentConfigure); err != nil {
		writeError(w, err)
		return
	}
	var a domain.AgentProfile
	if err := decode(r, &a, 0); err != nil {
		writeError(w, err)
		return
	}
	a.TenantID = p.TenantID
	ad, err := s.Agents.Get(a.Provider)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := ad.ValidateProfile(a); err != nil {
		writeError(w, domain.Invalid("profile", "%v", err))
		return
	}
	for k := range a.Environment {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") || strings.Contains(lk, "key") {
			writeError(w, domain.Invalid("environment", "%s looks like a secret; configure credentials via the secret manager", k))
			return
		}
	}
	if err := s.Store.SaveAgentProfile(r.Context(), p.UserID, &a); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) listFrameworks(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListFrameworks(r.Context(), principal(r).TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

func (s *Server) getFramework(w http.ResponseWriter, r *http.Request) {
	f, err := s.Store.GetFramework(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, f)
}

// listCommands returns commands visible to the caller. Visibility is a UX
// convenience; submission re-authorizes server-side.
func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	fw, err := s.Store.GetFramework(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	cmds, err := s.Store.ListCommands(r.Context(), fw.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	f := registry.Filter{FrameworkID: fw.ID, Phase: domain.Phase(r.URL.Query().Get("phase")),
		AgentProvider: r.URL.Query().Get("agent"), Principal: p,
		IncludeUnapproved: r.URL.Query().Get("all") == "true"}
	writeJSON(w, 200, list(registry.Visible(cmds, f)))
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	type poolView struct {
		Name              string                    `json:"name"`
		Type              string                    `json:"type"`
		Network           string                    `json:"network"`
		Region            string                    `json:"region,omitempty"`
		MaxClassification domain.DataClassification `json:"maxClassification"`
		Enabled           bool                      `json:"enabled"`
	}
	t, err := s.Store.GetTenant(r.Context(), p.TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := []poolView{}
	for _, pl := range s.Pools.All() {
		if len(t.PermittedProviders) > 0 && !contains(t.PermittedProviders, pl.Name) {
			continue
		}
		out = append(out, poolView{pl.Name, pl.Type, pl.Network, pl.Region, pl.MaxClassification, pl.Enabled})
	}
	writeJSON(w, 200, map[string]any{"items": out, "canSelect": p.Has(domain.PermProviderSelect)})
}

// listIntegrations reports integration status. Integrations beyond the
// vertical slice are registered but not yet configured (see roadmap).
func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, list([]map[string]any{
		{"id": "github", "name": "GitHub", "category": "repository", "status": "available", "notes": "Clone/branch via short-lived tokens; PR creation planned (Release 2)."},
		{"id": "gitlab", "name": "GitLab", "category": "repository", "status": "available", "notes": "Clone/branch via short-lived tokens; MR creation planned (Release 2)."},
		{"id": "jira", "name": "Jira", "category": "issue-tracking", "status": "not_configured", "notes": "Jira writes are approval-gated; connector planned (Release 2)."},
		{"id": "oidc", "name": "OIDC identity provider", "category": "identity", "status": "available"},
	}))
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermAuditRead); err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListAudit(r.Context(), p.TenantID, queryInt64(r, "before"), queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
