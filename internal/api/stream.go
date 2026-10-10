package api

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
)

// jobEvents streams durable job events over SSE. Events are replayed from
// the job_events table after Last-Event-ID, so reconnecting clients never
// miss updates; pg_notify only wakes the stream early (SRS 21.3, UI-008).
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, domain.Errorf(domain.ErrUnavailable, "streaming unsupported"))
		return
	}
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if cursor == 0 {
		cursor = queryInt64(r, "after")
	}
	var types []string
	if t := r.URL.Query().Get("types"); t != "" {
		types = strings.Split(t, ",")
	}
	var wake <-chan struct{}
	if s.Broker != nil {
		ch, unsub := s.Broker.Subscribe(job.ID)
		defer unsub()
		wake = ch
	}
	poll := s.Config.SSEPollInterval
	if s.Broker == nil && poll > 2*time.Second {
		poll = 2 * time.Second
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()
	s.Metrics.Add("sse_streams_active", 1)
	defer s.Metrics.Add("sse_streams_active", -1)

	ctx := r.Context()
	deadline := time.NewTimer(30 * time.Minute) // clients reconnect with Last-Event-ID
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		for {
			evs, err := s.Store.ListEvents(ctx, job.TenantID, job.ID, cursor, types, 500)
			if err != nil {
				return
			}
			for _, e := range evs {
				data, _ := json.Marshal(e)
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, data)
				cursor = e.ID
			}
			flusher.Flush()
			if len(evs) < 500 {
				break
			}
		}
		// Stop once the job is terminal and fully drained.
		cur, err := s.Store.GetJob(ctx, job.TenantID, job.ID)
		if err == nil && cur.State.Terminal() && cur.CleanupStatus != domain.CleanupPending {
			if more, _ := s.Store.ListEvents(ctx, job.TenantID, job.ID, cursor, types, 1); len(more) == 0 {
				fmt.Fprintf(w, "event: end\ndata: {\"state\":%q}\n\n", cur.State)
				flusher.Flush()
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-wake:
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

type logLineView struct {
	EventID     int64     `json:"eventId"`
	ExecutionID string    `json:"executionId"`
	Attempt     int       `json:"attempt"`
	Time        time.Time `json:"ts"`
	Stream      string    `json:"stream"`
	Level       string    `json:"level,omitempty"`
	Message     string    `json:"message"`
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	cursor := queryInt64(r, "after")
	text := r.URL.Query().Get("format") == "text"
	limit := queryInt(r, "limit", 200)
	if text {
		limit = 1000
	}
	var lines []logLineView
	for {
		evs, err := s.Store.ListEvents(r.Context(), job.TenantID, job.ID, cursor, []string{domain.EventLogBatch}, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, e := range evs {
			var b struct {
				ExecutionID string            `json:"executionId"`
				Attempt     int               `json:"attempt"`
				Lines       []execapi.LogLine `json:"lines"`
			}
			if json.Unmarshal(e.Payload, &b) != nil {
				continue
			}
			for _, l := range b.Lines {
				lines = append(lines, logLineView{e.ID, b.ExecutionID, b.Attempt, l.Time, l.Stream, l.Level, l.Message})
			}
			cursor = e.ID
		}
		if !text || len(evs) < limit || len(lines) > 200000 {
			break
		}
	}
	if text {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-logs.txt"`, job.ID))
		for _, l := range lines {
			fmt.Fprintf(w, "%s [attempt %d] [%s] %s\n", l.Time.UTC().Format(time.RFC3339Nano), l.Attempt, l.Stream, l.Message)
		}
		return
	}
	if lines == nil {
		lines = []logLineView{}
	}
	writeJSON(w, 200, map[string]any{"items": lines, "next": cursor})
}

func (s *Server) jobArtifacts(w http.ResponseWriter, r *http.Request) {
	job, err := s.loadJob(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := authz.Require(principal(r), domain.PermArtifactRead); err != nil {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListArtifacts(r.Context(), job.TenantID, job.ID, false)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, list(items))
}

// userArtifact loads a user-visible artifact; internal artifacts
// (checkpoints) are indistinguishable from missing ones.
func (s *Server) userArtifact(r *http.Request) (*domain.Artifact, error) {
	p := principal(r)
	if err := authz.Require(p, domain.PermArtifactRead); err != nil {
		return nil, err
	}
	a, err := s.Store.GetArtifact(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		return nil, err
	}
	if a.Classification != domain.ArtifactUser {
		return nil, domain.Errorf(domain.ErrNotFound, "artifact")
	}
	return a, nil
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := s.userArtifact(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) artifactContent(w http.ResponseWriter, r *http.Request) {
	a, err := s.userArtifact(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rc, err := s.Objects.Get(r.Context(), a.ObjectKey)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()
	p := principal(r)
	_ = s.Store.Audit(r.Context(), domain.AuditEvent{TenantID: p.TenantID, Actor: p.UserID, ActorType: "user",
		Action: "artifact.download", TargetType: "artifact", TargetID: a.ID})
	h := w.Header()
	ct := a.ContentType
	inline := r.URL.Query().Get("inline") == "true" && (strings.HasPrefix(ct, "text/") || ct == "application/json")
	if inline {
		// Text previews are always served as plain text.
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": a.Name}))
	} else {
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
	}
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	h.Set("X-Artifact-SHA256", a.SHA256)
	_, _ = io.Copy(w, rc)
}

// spaCSP restricts the web console to same-origin scripts, styles and API
// calls. The Vite build emits no inline scripts, so 'unsafe-inline' is not
// needed; agent output is rendered as text, never as HTML.
const spaCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'"

// spaHandler serves the built web UI with index.html fallback for client
// routes. API paths never fall through to the SPA.
func spaHandler(dir string) http.HandlerFunc {
	fs := http.FileServer(http.Dir(dir))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/internal/") {
			writeError(w, domain.Errorf(domain.ErrNotFound, "route"))
			return
		}
		// Clean as a URL path (forward slashes on every OS). Backslashes are
		// path separators on Windows, so a request containing one could step
		// outside dir after FromSlash; such requests get the SPA shell.
		clean := path.Clean("/" + r.URL.Path)
		if strings.ContainsRune(clean, '\\') {
			clean = "/index.html"
		}
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean))); err == nil && !st.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fs.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", spaCSP)
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	}
}
