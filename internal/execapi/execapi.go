// Package execapi defines the contract between the in-environment
// supervisor and the control plane (the "internal execution API"). Every
// call is authenticated with a short-lived, per-execution bearer token whose
// SHA-256 hash is stored server-side; the token can only act on its own
// execution, job, and tenant (SEC-001, SRS 22).
//
// Endpoints (all under /internal/v1/executions/{executionId}):
//
//	GET  /spec                     -> Spec
//	POST /status                   StatusReport
//	POST /logs                     LogBatch
//	POST /repositories             RepositoryReport
//	POST /interactions             InteractionRequest -> InteractionCreated
//	GET  /interactions/{id}        -> InteractionState
//	PUT  /artifacts?...            raw bytes -> ArtifactCreated
//	GET  /checkpoint               -> checkpoint archive bytes
//	POST /complete                 Completion
package execapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/domain"
)

// RepoSpec describes a repository to prepare in the workspace.
type RepoSpec struct {
	RepositoryID string `json:"repositoryId"`
	Provider     string `json:"provider"`
	URL          string `json:"url"`
	Revision     string `json:"revision"`
	CheckoutPath string `json:"checkoutPath"` // relative to the workspace repos dir
	// WorkingBranch is created when the command may modify repositories.
	WorkingBranch string `json:"workingBranch,omitempty"`
	// CredentialFile names a file in the secrets directory holding a
	// short-lived token for this repository, if any.
	CredentialFile string `json:"credentialFile,omitempty"`
}

// Spec is everything the supervisor needs to run one execution attempt.
type Spec struct {
	ExecutionID       string                 `json:"executionId"`
	JobID             string                 `json:"jobId"`
	TenantID          string                 `json:"tenantId"`
	Attempt           int                    `json:"attempt"`
	Command           string                 `json:"command"`
	Inputs            map[string]any         `json:"inputs"`
	ExpectedArtifacts []string               `json:"expectedArtifacts"`
	Profile           domain.AgentProfile    `json:"profile"`
	Mode              domain.InteractionMode `json:"mode"`
	Repositories      []RepoSpec             `json:"repositories"`
	Resume            *agent.ResumeRequest   `json:"resume,omitempty"`
	HasCheckpoint     bool                   `json:"hasCheckpoint"`
	MaxArtifactBytes  int64                  `json:"maxArtifactBytes"`
	TimeoutSeconds    int                    `json:"timeoutSeconds"`
	// SecretValues lists names of files in the secrets dir whose contents
	// must be redacted from logs (values are never sent over this API).
	SecretFiles []string `json:"secretFiles,omitempty"`
}

type StatusReport struct {
	Status  domain.ExecutionStatus `json:"status"`
	Step    string                 `json:"step,omitempty"`
	Message string                 `json:"message,omitempty"`
}

type LogLine struct {
	Time    time.Time `json:"ts"`
	Stream  string    `json:"stream"` // stdout | stderr | supervisor
	Level   string    `json:"level,omitempty"`
	Message string    `json:"message"`
}

type LogBatch struct {
	Seq   int64     `json:"seq"` // supervisor-local batch sequence, for dedupe
	Lines []LogLine `json:"lines"`
}

type RepositoryReport struct {
	RepositoryID   string `json:"repositoryId"`
	ResolvedCommit string `json:"resolvedCommit,omitempty"`
	CheckoutPath   string `json:"checkoutPath,omitempty"`
	WorkingBranch  string `json:"workingBranch,omitempty"`
	FinalCommit    string `json:"finalCommit,omitempty"`
}

type InteractionCreated struct {
	InteractionID string `json:"interactionId"`
}

type InteractionState struct {
	InteractionID string                      `json:"interactionId"`
	Status        domain.InteractionStatus    `json:"status"`
	Response      *domain.InteractionResponse `json:"response,omitempty"`
}

type ArtifactCreated struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
}

// Completion is the supervisor's final report for this attempt.
type Completion struct {
	Disposition          agent.Disposition `json:"disposition"`
	ExitCode             int               `json:"exitCode"`
	ResultStatus         string            `json:"resultStatus,omitempty"`
	Summary              string            `json:"summary,omitempty"`
	Error                string            `json:"error,omitempty"`
	FailureClass         string            `json:"failureClass,omitempty"`
	PendingInteractionID string            `json:"pendingInteractionId,omitempty"`
	CheckpointArtifactID string            `json:"checkpointArtifactId,omitempty"`
	Artifacts            []string          `json:"artifacts,omitempty"` // artifact types produced
}

// ---------------------------------------------------------------------------
// Client

// ErrGone means the execution is no longer active (cancelled, finished, or
// token revoked). The supervisor must stop work.
var ErrGone = errors.New("execution is no longer active")

type Client struct {
	BaseURL     string
	ExecutionID string
	Token       string
	HTTP        *http.Client
}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Body) }

func (c *Client) url(p string, q url.Values) string {
	u := c.BaseURL + "/internal/v1/executions/" + url.PathEscape(c.ExecutionID) + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (c *Client) do(ctx context.Context, method, p string, q url.Values, body []byte, ctype string, out any) error {
	var lastErr error
	backoff := 250 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(p, q), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		hc := c.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 2 * time.Minute}
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusGone:
			return ErrGone
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			lastErr = &httpError{resp.StatusCode, string(b)}
			continue
		case resp.StatusCode >= 400:
			return &httpError{resp.StatusCode, string(b)}
		}
		if out != nil {
			if raw, ok := out.(*[]byte); ok {
				*raw = b
				return nil
			}
			return json.Unmarshal(b, out)
		}
		return nil
	}
	return lastErr
}

func (c *Client) postJSON(ctx context.Context, p string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, p, nil, b, "application/json", out)
}

func (c *Client) Spec(ctx context.Context) (*Spec, error) {
	var s Spec
	return &s, c.do(ctx, http.MethodGet, "/spec", nil, nil, "", &s)
}

func (c *Client) Status(ctx context.Context, r StatusReport) error {
	return c.postJSON(ctx, "/status", r, nil)
}

func (c *Client) Logs(ctx context.Context, b LogBatch) error { return c.postJSON(ctx, "/logs", b, nil) }

func (c *Client) Repository(ctx context.Context, r RepositoryReport) error {
	return c.postJSON(ctx, "/repositories", r, nil)
}

func (c *Client) CreateInteraction(ctx context.Context, r agent.InteractionRequest) (string, error) {
	var out InteractionCreated
	err := c.postJSON(ctx, "/interactions", r, &out)
	return out.InteractionID, err
}

func (c *Client) GetInteraction(ctx context.Context, id string) (*InteractionState, error) {
	var out InteractionState
	return &out, c.do(ctx, http.MethodGet, "/interactions/"+url.PathEscape(id), nil, nil, "", &out)
}

// ArtifactUpload describes an upload. UploadID makes retries idempotent.
type ArtifactUpload struct {
	UploadID       string
	Type           string
	Name           string
	ContentType    string
	Classification domain.ArtifactClass
	Step           string
	RepositoryID   string
}

func (c *Client) UploadArtifact(ctx context.Context, u ArtifactUpload, data []byte) (*ArtifactCreated, error) {
	q := url.Values{"uploadId": {u.UploadID}, "type": {u.Type}, "name": {u.Name},
		"classification": {string(u.Classification)}, "step": {u.Step}, "repositoryId": {u.RepositoryID},
		"size": {strconv.Itoa(len(data))}}
	ct := u.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	var out ArtifactCreated
	return &out, c.do(ctx, http.MethodPut, "/artifacts", q, data, ct, &out)
}

func (c *Client) DownloadCheckpoint(ctx context.Context) ([]byte, error) {
	var b []byte
	return b, c.do(ctx, http.MethodGet, "/checkpoint", nil, nil, "", &b)
}

func (c *Client) Complete(ctx context.Context, r Completion) error {
	return c.postJSON(ctx, "/complete", r, nil)
}
