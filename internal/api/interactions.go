package api

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
)

type interactionView struct {
	domain.Interaction
	JobCommand string `json:"jobCommand,omitempty"`
	JobState   string `json:"jobState,omitempty"`
	CanRespond bool   `json:"canRespond"`
	Platform   bool   `json:"platform"` // raised by the platform (approval gate), not the agent
	Production bool   `json:"productionApproval,omitempty"`
}

// interactionContext resolves the job and whether production approval
// rights are required to answer.
func (s *Server) interactionContext(ctx context.Context, it *domain.Interaction) (*domain.Job, bool, error) {
	job, err := s.Store.GetJob(ctx, it.TenantID, it.JobID)
	if err != nil {
		return nil, false, err
	}
	prod := false
	if it.ExecutionID == "" && it.Kind == domain.InteractionApproval {
		if cmd, err := s.loadJobCommand(ctx, job); err == nil {
			prod = cmd.RequiresProductionApproval()
		}
	}
	return job, prod, nil
}

func (s *Server) view(ctx context.Context, p *authz.Principal, it *domain.Interaction) interactionView {
	v := interactionView{Interaction: *it, Platform: it.ExecutionID == ""}
	if job, prod, err := s.interactionContext(ctx, it); err == nil {
		v.JobCommand, v.JobState, v.Production = job.Command, string(job.State), prod
		v.CanRespond = it.Status == domain.InteractionOpen && authz.CanRespond(p, it, job.InitiatedBy, prod) == nil
	}
	return v
}

func (s *Server) listInteractions(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermJobRead); err != nil {
		writeError(w, err)
		return
	}
	if st := r.URL.Query().Get("status"); st != "" && st != string(domain.InteractionOpen) {
		writeError(w, domain.Invalid("status", "only status=OPEN is supported; use /jobs/{id}/interactions for history"))
		return
	}
	items, err := s.Store.ListOpenInteractions(r.Context(), p.TenantID, queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, err)
		return
	}
	mine := r.URL.Query().Get("mine") == "true"
	out := []interactionView{}
	for i := range items {
		v := s.view(r.Context(), p, &items[i])
		if mine && !v.CanRespond {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, 200, list(out))
}

func (s *Server) getInteraction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := authz.Require(p, domain.PermJobRead); err != nil {
		writeError(w, err)
		return
	}
	it, err := s.Store.GetInteraction(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil || it.Status == domain.InteractionRecorded {
		writeError(w, domain.Errorf(domain.ErrNotFound, "interaction"))
		return
	}
	writeJSON(w, 200, s.view(r.Context(), p, it))
}

type responseRequest struct {
	Decision domain.Decision `json:"decision"`
	Values   []string        `json:"values"`
	Text     string          `json:"text"`
	Comment  string          `json:"comment"`
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request) { s.resolve(w, r, "") }
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	s.resolve(w, r, domain.DecisionApprove)
}
func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	s.resolve(w, r, domain.DecisionReject)
}

// resolve validates, authorizes, durably records and audits a response, and
// only then nudges the workflow to resume (HITL-003..006).
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, forced domain.Decision) {
	p := principal(r)
	it, err := s.Store.GetInteraction(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil || it.Status == domain.InteractionRecorded {
		writeError(w, domain.Errorf(domain.ErrNotFound, "interaction"))
		return
	}
	var q responseRequest
	if r.ContentLength != 0 {
		if err := decode(r, &q, 128<<10); err != nil {
			writeError(w, err)
			return
		}
	}
	if forced != "" {
		if q.Decision != "" && q.Decision != forced {
			writeError(w, domain.Invalid("decision", "conflicts with endpoint"))
			return
		}
		q.Decision = forced
	}
	job, prod, err := s.interactionContext(r.Context(), it)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := authz.CanRespond(p, it, job.InitiatedBy, prod); err != nil {
		writeError(w, err)
		return
	}
	if err := validateResponse(it, &q); err != nil {
		writeError(w, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !idemKeyRe.MatchString(key) {
		writeError(w, domain.Invalid("Idempotency-Key", "must be 8-128 characters of [A-Za-z0-9._:-]"))
		return
	}
	resp := &domain.InteractionResponse{Decision: q.Decision, Values: q.Values, Text: q.Text, Comment: q.Comment,
		RespondedBy: p.UserID, IdempotencyKey: key}
	saved, replayed, err := s.Store.ResolveInteraction(r.Context(), p.TenantID, it.ID, resp)
	if err != nil {
		writeError(w, err)
		return
	}
	s.Engine.Nudge(r.Context(), it.JobID, "interaction-resolved")
	status := http.StatusOK
	if !replayed {
		s.Metrics.Add("interactions_resolved_total", 1)
	}
	writeJSON(w, status, s.view(r.Context(), p, saved))
}

// validateResponse checks a response against the interaction kind, options
// and schema. Values are treated as data, never as commands.
func validateResponse(it *domain.Interaction, q *responseRequest) error {
	if utf8.RuneCountInString(q.Comment) > 5000 {
		return domain.Invalid("comment", "at most 5000 characters")
	}
	if it.Kind.IsDecision() {
		switch q.Decision {
		case domain.DecisionApprove, domain.DecisionReject:
		default:
			return domain.Invalid("decision", "must be APPROVE or REJECT for %s", it.Kind)
		}
		if len(q.Values) > 0 || q.Text != "" {
			return domain.Invalid("text", "decision interactions take a decision and optional comment only")
		}
		return nil
	}
	if q.Decision == "" {
		q.Decision = domain.DecisionAnswer
	}
	if q.Decision != domain.DecisionAnswer {
		return domain.Invalid("decision", "%s interactions require an answer, not %s", it.Kind, q.Decision)
	}
	if it.Kind == domain.InteractionMultipleChoice || len(it.Options) > 0 && q.Text == "" {
		if len(q.Values) == 0 {
			return domain.Invalid("values", "select at least one option")
		}
		if len(q.Values) > 1 && !it.Schema.Multiple {
			return domain.Invalid("values", "only one option may be selected")
		}
		allowed := map[string]bool{}
		for _, o := range it.Options {
			allowed[o.Value] = true
		}
		seen := map[string]bool{}
		for _, v := range q.Values {
			if !allowed[v] {
				return domain.Invalid("values", "%q is not one of the offered options", v)
			}
			if seen[v] {
				return domain.Invalid("values", "%q selected twice", v)
			}
			seen[v] = true
		}
		return nil
	}
	if len(q.Values) > 0 {
		return domain.Invalid("values", "this interaction takes a text answer")
	}
	n := utf8.RuneCountInString(strings.TrimSpace(q.Text))
	max := it.Schema.MaxLength
	if max <= 0 || max > 50000 {
		max = 50000
	}
	switch {
	case n == 0 && (it.Schema.Required || it.Kind == domain.InteractionMissingInput):
		return domain.Invalid("text", "an answer is required")
	case n < it.Schema.MinLength:
		return domain.Invalid("text", "at least %d characters", it.Schema.MinLength)
	case n > max:
		return domain.Invalid("text", "at most %d characters", max)
	}
	return nil
}
