// Package api implements the control-plane HTTP API (SRS 21): the public
// /api/v1 surface used by the web UI and the internal execution API used by
// supervisors. All authorization is enforced here, server-side.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/auth"
	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/events"
	"github.com/deepakppatil/astrion/internal/objectstore"
	"github.com/deepakppatil/astrion/internal/orchestrator"
	"github.com/deepakppatil/astrion/internal/provider"
	"github.com/deepakppatil/astrion/internal/store"
)

type Config struct {
	Production bool
	// MaxArtifactBytes caps a single uploaded artifact.
	MaxArtifactBytes int64
	// MaxSubmissionsPerMinute is the default per-user submission rate limit.
	MaxSubmissionsPerMinute int
	// MaxQueuedPerTenant is the default queued-job limit per tenant.
	MaxQueuedPerTenant int
	// SSEPollInterval is the fallback poll interval for event streams.
	SSEPollInterval time.Duration
	// StaticDir, if set, serves the built web UI.
	StaticDir string
}

type Server struct {
	Store   *store.Store
	Engine  orchestrator.Engine
	Auth    auth.Authenticator
	Objects objectstore.Store
	Broker  *events.Broker
	Agents  *agent.Registry
	Pools   *provider.Registry
	Config  Config
	Log     *slog.Logger
	Metrics *Metrics
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Metrics == nil {
		s.Metrics = NewMetrics()
	}
	if s.Config.MaxArtifactBytes == 0 {
		s.Config.MaxArtifactBytes = 50 << 20
	}
	if s.Config.SSEPollInterval == 0 {
		s.Config.SSEPollInterval = 15 * time.Second
	}
	r := chi.NewRouter()
	r.Use(s.recoverer, s.requestLog, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/readyz", s.ready)
	r.Get("/metrics", s.Metrics.ServeHTTP)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authenticate)
		r.Get("/me", s.me)
		r.Get("/dashboard", s.dashboard)

		r.Get("/workspaces", s.listWorkspaces)
		r.Post("/workspaces", s.createWorkspace)
		r.Get("/workspaces/{id}", s.getWorkspace)
		r.Patch("/workspaces/{id}", s.updateWorkspace)
		r.Get("/workspaces/{id}/repositories", s.listRepositories)
		r.Post("/workspaces/{id}/repositories", s.addRepository)
		r.Delete("/workspaces/{id}/repositories/{repoId}", s.removeRepository)

		r.Get("/agents", s.listAgents)
		r.Post("/agents", s.saveAgent)
		r.Get("/agents/{id}", s.getAgent)
		r.Get("/frameworks", s.listFrameworks)
		r.Get("/frameworks/{id}", s.getFramework)
		r.Get("/frameworks/{id}/commands", s.listCommands)
		r.Get("/execution-pools", s.listPools)
		r.Get("/integrations", s.listIntegrations)

		r.Get("/jobs", s.listJobs)
		r.Post("/jobs", s.submitJob)
		r.Get("/jobs/{id}", s.getJob)
		r.Post("/jobs/{id}/cancel", s.cancelJob)
		r.Post("/jobs/{id}/retry", s.retryJob)
		r.Post("/jobs/{id}/operator-actions", s.operatorAction)
		r.Get("/jobs/{id}/events", s.jobEvents)
		r.Get("/jobs/{id}/logs", s.jobLogs)
		r.Get("/jobs/{id}/artifacts", s.jobArtifacts)
		r.Get("/jobs/{id}/interactions", s.jobInteractions)
		r.Get("/jobs/{id}/executions", s.jobExecutions)

		r.Get("/artifacts/{id}", s.getArtifact)
		r.Get("/artifacts/{id}/content", s.artifactContent)

		r.Get("/interactions", s.listInteractions)
		r.Get("/interactions/{id}", s.getInteraction)
		r.Post("/interactions/{id}/responses", s.respond)
		r.Post("/interactions/{id}/approve", s.approve)
		r.Post("/interactions/{id}/reject", s.reject)

		r.Get("/audit", s.listAudit)
	})

	r.Route("/internal/v1/executions/{execId}", func(r chi.Router) {
		r.Use(s.authenticateExecution)
		r.Get("/spec", s.execSpec)
		r.Post("/status", s.execStatus)
		r.Post("/logs", s.execLogs)
		r.Post("/repositories", s.execRepository)
		r.Post("/interactions", s.execCreateInteraction)
		r.Get("/interactions/{iid}", s.execGetInteraction)
		r.Put("/artifacts", s.execUploadArtifact)
		r.Get("/checkpoint", s.execCheckpoint)
		r.Post("/complete", s.execComplete)
	})

	if s.Config.StaticDir != "" {
		r.NotFound(spaHandler(s.Config.StaticDir))
	}
	return r
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

// ---------------------------------------------------------------------------
// Middleware

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		s.Metrics.ObserveRequest(r.Method, route, sw.status, time.Since(start))
		if strings.HasSuffix(route, "/events") || route == "/healthz" || route == "/metrics" {
			return
		}
		s.log().Debug("http", "method", r.Method, "route", route, "status", sw.status, "dur", time.Since(start))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log().Error("panic", "err", v, "stack", string(debug.Stack()))
				writeError(w, errors.New("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the caller to a tenant user. Role assignments from
// an authoritative IdP (group mapping) are synchronized on each request.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tokens are accepted only in the Authorization header, never in the
		// URL (where they would leak into proxy logs, history and Referer).
		// The web console consumes SSE with fetch() so it can set headers.
		id, err := s.Auth.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="astrion"`)
			writeError(w, domain.ErrUnauthorized)
			return
		}
		p, err := s.resolvePrincipal(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(authz.WithPrincipal(r.Context(), p)))
	})
}

func (s *Server) resolvePrincipal(ctx context.Context, id *auth.Identity) (*authz.Principal, error) {
	if _, err := s.Store.GetTenant(ctx, id.TenantID); err != nil {
		return nil, domain.Errorf(domain.ErrForbidden, "tenant is not provisioned")
	}
	u, err := s.Store.GetUserBySubject(ctx, id.TenantID, id.Subject)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		u = &domain.User{TenantID: id.TenantID, Subject: id.Subject, Email: id.Email, DisplayName: id.Name}
		if id.RolesAuthoritative {
			u.Roles = id.Roles
		}
		if err := s.Store.UpsertUser(ctx, u); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case id.RolesAuthoritative && !sameRoles(u.Roles, id.Roles):
		u.Roles, u.Email, u.DisplayName = id.Roles, id.Email, id.Name
		if err := s.Store.UpsertUser(ctx, u); err != nil {
			return nil, err
		}
	}
	return &authz.Principal{UserID: u.ID, TenantID: u.TenantID, Subject: u.Subject, Email: u.Email,
		Name: u.DisplayName, Roles: u.Roles}, nil
}

func sameRoles(a, b []domain.Role) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[domain.Role]bool{}
	for _, r := range a {
		set[r] = true
	}
	for _, r := range b {
		if !set[r] {
			return false
		}
	}
	return true
}

func principal(r *http.Request) *authz.Principal {
	p, _ := authz.FromContext(r.Context())
	return p
}

// ---------------------------------------------------------------------------
// Helpers

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	var ve *domain.ValidationError
	var te *domain.InvalidTransitionError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]apiError{"error": {Code: "invalid", Message: ve.Message, Field: ve.Field}})
		return
	case errors.As(err, &te):
		status, code = http.StatusConflict, "invalid_transition"
	case errors.Is(err, domain.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid"
	case errors.Is(err, domain.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, domain.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, domain.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, domain.ErrExpired):
		status, code = http.StatusGone, "expired"
	case errors.Is(err, domain.ErrRateLimited):
		status, code = http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, domain.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, "unavailable"
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		slog.Default().Error("internal error", "err", err)
		msg = "internal server error"
	}
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// decode reads a size-limited JSON body, rejecting unknown fields.
func decode(r *http.Request, v any, limit int64) error {
	if limit == 0 {
		limit = 1 << 20
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.Errorf(domain.ErrInvalid, "malformed request body: %v", err)
	}
	return nil
}

func queryInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func queryInt64(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return n
}

func list[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items}
}

func ptr[T any](v T) *T { return &v }
