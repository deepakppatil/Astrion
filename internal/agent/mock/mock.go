// Package mock is a deterministic agent adapter used for local development,
// CI, and the end-to-end vertical slice. It drives cmd/mock-agent, which
// emulates an SDD framework CLI and speaks a JSON-lines protocol.
package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/domain"
)

// ExitAwaitingInput is the exit code the mock agent uses after writing a
// checkpoint while waiting for a human (EX_TEMPFAIL).
const ExitAwaitingInput = 75

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return "mock" }

func (*Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		InteractionModes:      []domain.InteractionMode{domain.ModeCheckpointResume, domain.ModeLiveSession},
		StructuredEvents:      true,
		ToolPermissionControl: false,
		NonInteractive:        true,
		GracefulCancellation:  true,
		SupportedAuthMethods:  []string{"none"},
	}
}

func (*Adapter) ValidateProfile(p domain.AgentProfile) error {
	if p.Provider != "mock" {
		return domain.Invalid("provider", "expected mock, got %q", p.Provider)
	}
	if p.AuthMethod != "" && p.AuthMethod != "none" {
		return domain.Invalid("authMethod", "mock agent supports only 'none'")
	}
	return nil
}

func (*Adapter) ValidateAuth(context.Context, domain.AgentProfile, agent.Credentials) error {
	return nil
}

type invocationFile struct {
	Command           string                 `json:"command"`
	Inputs            map[string]any         `json:"inputs"`
	ExpectedArtifacts []string               `json:"expectedArtifacts"`
	Repositories      []domain.JobRepository `json:"repositories"`
}

func (a *Adapter) Invocation(req agent.InvocationRequest) (agent.Invocation, error) {
	cli := req.Profile.CLIName
	if cli == "" {
		cli = "mock-agent"
	}
	inv := invocationFile{Command: req.Command, Inputs: req.Inputs, ExpectedArtifacts: req.ExpectedArtifacts, Repositories: req.Repositories}
	b, err := json.Marshal(inv)
	if err != nil {
		return agent.Invocation{}, err
	}
	mode := "checkpoint"
	if req.Mode == domain.ModeLiveSession {
		mode = "live"
	}
	invFile := filepath.Join(req.CheckpointDir, "..", "invocation.json")
	args := []string{
		"run",
		"--invocation", invFile,
		"--workdir", req.WorkDir,
		"--output-dir", req.OutputDir,
		"--checkpoint-dir", req.CheckpointDir,
		"--mode", mode,
	}
	args = append(args, req.Profile.DefaultArgs...)
	files := map[string][]byte{invFile: b}
	if req.Resume != nil {
		ans, err := json.Marshal(req.Resume)
		if err != nil {
			return agent.Invocation{}, err
		}
		ansFile := filepath.Join(req.CheckpointDir, "..", "answer.json")
		files[ansFile] = ans
		args = append(args, "--resume-answer", ansFile)
	}
	env := map[string]string{}
	for k, v := range req.Profile.Environment {
		env[k] = v
	}
	return agent.Invocation{Path: cli, Args: args, Env: env, Files: files, KeepStdinOpen: req.Mode == domain.ModeLiveSession}, nil
}

type decoder struct{}

func (*Adapter) NewDecoder() agent.Decoder { return decoder{} }

// wire is the mock agent's JSON-lines wire format.
type wire struct {
	Type         string                     `json:"type"`
	Level        string                     `json:"level"`
	Message      string                     `json:"message"`
	Step         string                     `json:"step"`
	ID           string                     `json:"id"`
	Kind         domain.InteractionKind     `json:"kind"`
	Prompt       string                     `json:"prompt"`
	Options      []domain.InteractionOption `json:"options"`
	Schema       domain.InputSchema         `json:"schema"`
	Role         domain.Role                `json:"requestedRole"`
	Path         string                     `json:"path"`
	ArtifactType string                     `json:"artifactType"`
	ContentType  string                     `json:"contentType"`
	Status       string                     `json:"status"`
	Summary      string                     `json:"summary"`
}

func (decoder) Decode(line []byte) []agent.Event {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var w wire
	if line[0] != '{' || json.Unmarshal(line, &w) != nil || w.Type == "" {
		return []agent.Event{{Type: agent.EventLog, Level: "info", Message: string(line)}}
	}
	switch agent.EventType(w.Type) {
	case agent.EventLog:
		return []agent.Event{{Type: agent.EventLog, Level: w.Level, Message: w.Message}}
	case agent.EventProgress:
		return []agent.Event{{Type: agent.EventProgress, Step: w.Step, Message: w.Message}}
	case agent.EventInteraction:
		return []agent.Event{{Type: agent.EventInteraction, Interaction: &agent.InteractionRequest{
			AgentRequestID: w.ID, Kind: w.Kind, Prompt: w.Prompt, Options: w.Options, Schema: w.Schema,
			RequestedRole: w.Role, Step: w.Step,
		}}}
	case agent.EventArtifact:
		return []agent.Event{{Type: agent.EventArtifact, Artifact: &agent.ArtifactOutput{
			Path: w.Path, ArtifactType: w.ArtifactType, ContentType: w.ContentType,
		}}}
	case agent.EventCheckpoint:
		return []agent.Event{{Type: agent.EventCheckpoint, Message: w.Message}}
	case agent.EventResult:
		return []agent.Event{{Type: agent.EventResult, Result: &agent.Result{Status: w.Status, Summary: w.Summary}}}
	}
	return []agent.Event{{Type: agent.EventLog, Level: "info", Message: string(line)}}
}

func (*Adapter) EncodeLiveResponse(_ agent.InteractionRequest, resp domain.InteractionResponse) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"decision": resp.Decision, "values": resp.Values, "text": resp.Text, "comment": resp.Comment,
	})
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return append(b, '\n'), nil
}

func (*Adapter) Disposition(exitCode int, result *agent.Result, awaitingInput bool) agent.Disposition {
	switch {
	case exitCode == ExitAwaitingInput && awaitingInput:
		return agent.DispositionSuspended
	case exitCode == 0 && result != nil && result.Status == "success":
		return agent.DispositionCompleted
	default:
		return agent.DispositionFailed
	}
}
