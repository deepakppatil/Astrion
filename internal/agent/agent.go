// Package agent defines the code-agent runtime abstraction (SRS section 11).
//
// An Adapter encapsulates everything provider-specific about a CLI agent:
// how to validate it, how to invoke a command, how to decode its output into
// normalized events, how to deliver human responses, and what it can and
// cannot do (Capabilities). The core job state machine never branches on a
// provider name; it only consults Capabilities (AC-007).
package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/deepakppatil/astrion/internal/domain"
)

// Capabilities are declared, not assumed. In particular an adapter must not
// claim CHECKPOINT_RESUME unless the CLI documents a continuation mechanism
// that has passed the conformance suite (SRS 10.2, 15.3).
type Capabilities struct {
	InteractionModes      []domain.InteractionMode `json:"interactionModes"`
	StructuredEvents      bool                     `json:"structuredEvents"`
	ToolPermissionControl bool                     `json:"toolPermissionControl"`
	NonInteractive        bool                     `json:"nonInteractive"`
	GracefulCancellation  bool                     `json:"gracefulCancellation"`
	SupportedAuthMethods  []string                 `json:"supportedAuthMethods"`
}

// Supports reports whether the adapter supports an interaction mode.
func (c Capabilities) Supports(m domain.InteractionMode) bool {
	for _, x := range c.InteractionModes {
		if x == m {
			return true
		}
	}
	return false
}

// Credentials is an opaque reference bundle resolved from the secret manager.
// Values are only materialized inside the execution environment.
type Credentials struct {
	Method string            `json:"method"`
	Refs   map[string]string `json:"refs"` // logical name -> secret reference
}

// InteractionRequest is a normalized HITL prompt emitted by an agent.
type InteractionRequest struct {
	AgentRequestID string                     `json:"id"`
	Kind           domain.InteractionKind     `json:"kind"`
	Prompt         string                     `json:"prompt"`
	Options        []domain.InteractionOption `json:"options,omitempty"`
	Schema         domain.InputSchema         `json:"schema"`
	RequestedRole  domain.Role                `json:"requestedRole,omitempty"`
	Step           string                     `json:"step,omitempty"`
}

// ArtifactOutput announces a file produced by the agent.
type ArtifactOutput struct {
	Path         string `json:"path"` // relative to the output directory
	ArtifactType string `json:"artifactType"`
	ContentType  string `json:"contentType,omitempty"`
	Name         string `json:"name,omitempty"`
}

// Result is the agent's own claim about the outcome. It is evidence, not an
// authorization decision: the platform separately verifies completion criteria.
type Result struct {
	Status  string `json:"status"` // success | failure
	Summary string `json:"summary"`
}

type EventType string

const (
	EventLog         EventType = "log"
	EventProgress    EventType = "progress"
	EventInteraction EventType = "interaction"
	EventArtifact    EventType = "artifact"
	EventCheckpoint  EventType = "checkpoint"
	EventResult      EventType = "result"
)

// Event is a normalized agent event.
type Event struct {
	Type        EventType           `json:"type"`
	Level       string              `json:"level,omitempty"`
	Message     string              `json:"message,omitempty"`
	Step        string              `json:"step,omitempty"`
	Interaction *InteractionRequest `json:"interaction,omitempty"`
	Artifact    *ArtifactOutput     `json:"artifact,omitempty"`
	Result      *Result             `json:"result,omitempty"`
}

// Decoder turns raw stdout lines into events. Lines that are not structured
// are surfaced as log events so nothing is silently dropped.
type Decoder interface {
	Decode(line []byte) []Event
}

// ResumeRequest carries a resolved interaction into a new execution attempt
// (checkpoint/resume mode).
type ResumeRequest struct {
	InteractionID  string                     `json:"interactionId"`
	AgentRequestID string                     `json:"agentRequestId"`
	Response       domain.InteractionResponse `json:"response"`
}

// InvocationRequest is everything needed to launch one command.
type InvocationRequest struct {
	Profile           domain.AgentProfile    `json:"profile"`
	Command           string                 `json:"command"` // canonical, never rewritten
	Inputs            map[string]any         `json:"inputs"`
	ExpectedArtifacts []string               `json:"expectedArtifacts"`
	WorkDir           string                 `json:"workDir"`
	OutputDir         string                 `json:"outputDir"`
	CheckpointDir     string                 `json:"checkpointDir"`
	Mode              domain.InteractionMode `json:"mode"`
	Resume            *ResumeRequest         `json:"resume,omitempty"`
	Repositories      []domain.JobRepository `json:"repositories"`
}

// Invocation is a concrete process launch. Files are written (mode 0600)
// before launch so that inputs and answers never appear in argv.
type Invocation struct {
	Path  string            `json:"path"`
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Files map[string][]byte `json:"-"`
	// KeepStdinOpen is true in live-session mode so responses can be written.
	KeepStdinOpen bool `json:"keepStdinOpen"`
}

// Disposition is the supervisor-level interpretation of a finished process.
type Disposition string

const (
	DispositionCompleted Disposition = "COMPLETED"
	DispositionSuspended Disposition = "SUSPENDED" // checkpoint saved, awaiting input
	DispositionFailed    Disposition = "FAILED"
)

// Adapter is the provider-specific contract. Conceptual operations from SRS
// 11.1 map as follows: validate configuration (ValidateProfile), validate
// authentication (ValidateAuth), resolve capabilities (Capabilities), prepare
// a session and start a command (Invocation), stream logs and events and
// capture interaction requests (NewDecoder), deliver responses
// (EncodeLiveResponse or Invocation with Resume), cancel (supervisor signals
// the process group; GracefulCancellation tells it whether SIGTERM is honored),
// collect outputs (artifact events + OutputDir), and determine resumability
// (Capabilities.InteractionModes + Disposition).
type Adapter interface {
	Name() string
	Capabilities() Capabilities
	ValidateProfile(p domain.AgentProfile) error
	ValidateAuth(ctx context.Context, p domain.AgentProfile, creds Credentials) error
	Invocation(req InvocationRequest) (Invocation, error)
	NewDecoder() Decoder
	EncodeLiveResponse(req InteractionRequest, resp domain.InteractionResponse) ([]byte, error)
	Disposition(exitCode int, result *Result, awaitingInput bool) Disposition
}

// Registry holds registered adapters.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok {
		return nil, domain.Errorf(domain.ErrInvalid, "agent provider %q is not registered", name)
	}
	return a, nil
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ResolveMode picks the interaction mode for a profile, validating that the
// adapter actually supports it. It never silently upgrades to resume.
func ResolveMode(a Adapter, p domain.AgentProfile) (domain.InteractionMode, error) {
	caps := a.Capabilities()
	mode := p.InteractionMode
	if mode == "" {
		if caps.Supports(domain.ModeCheckpointResume) {
			mode = domain.ModeCheckpointResume
		} else {
			mode = domain.ModeLiveSession
		}
	}
	if !caps.Supports(mode) {
		return "", fmt.Errorf("%w: agent %s does not support interaction mode %s", domain.ErrInvalid, a.Name(), mode)
	}
	return mode, nil
}
