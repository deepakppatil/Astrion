// Package registry implements the versioned command and persona registry
// (SRS section 9). Commands are metadata, not hard-coded branches: adding a
// command that fits the execution contract requires no new backend code (AC-008).
package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/deepakppatil/astrion/internal/domain"
)

// RepoAccess is the repository access a command needs.
type RepoAccess string

const (
	RepoNone  RepoAccess = "NONE"
	RepoRead  RepoAccess = "READ"
	RepoWrite RepoAccess = "WRITE"
)

// SideEffect classes external effects so that policy can gate them.
type SideEffect string

const (
	EffectNone            SideEffect = "NONE"
	EffectRepositoryWrite SideEffect = "REPOSITORY_WRITE"
	EffectChangeRequest   SideEffect = "CHANGE_REQUEST"
	EffectExternalTicket  SideEffect = "EXTERNAL_TICKET"
	EffectDeployNonProd   SideEffect = "DEPLOYMENT_NON_PRODUCTION"
	EffectDeployProd      SideEffect = "DEPLOYMENT_PRODUCTION"
)

// Status of a command definition. Discovered commands start PENDING_REVIEW and
// are not executable until approved (repository-controlled definitions are untrusted).
type Status string

const (
	StatusApproved      Status = "APPROVED"
	StatusPendingReview Status = "PENDING_REVIEW"
	StatusDisabled      Status = "DISABLED"
)

type Source string

const (
	SourceDeclared   Source = "DECLARED"
	SourceDiscovered Source = "DISCOVERED"
)

// FieldType is a type in the small, safe command input schema language.
type FieldType string

const (
	FieldString      FieldType = "string"
	FieldText        FieldType = "text"
	FieldEnum        FieldType = "enum"
	FieldBoolean     FieldType = "boolean"
	FieldInteger     FieldType = "integer"
	FieldArtifactRef FieldType = "artifactRef"
	FieldJiraKey     FieldType = "jiraKey"
)

type InputField struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Type        FieldType `json:"type"`
	Required    bool      `json:"required"`
	Description string    `json:"description,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
	MaxLength   int       `json:"maxLength,omitempty"`
	Default     any       `json:"default,omitempty"`
}

type ArtifactExpectation struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type ApprovalPolicy struct {
	// BeforeExecution requires an explicit approval interaction before the
	// agent is launched (e.g. production deployment).
	BeforeExecution bool        `json:"beforeExecution"`
	ApproverRole    domain.Role `json:"approverRole,omitempty"`
	// ProductionApproval additionally requires approve:production.
	ProductionApproval bool `json:"productionApproval,omitempty"`
}

type RetryPolicy struct {
	// MaxAttempts for whole-execution retries. Agent workflows are not assumed
	// to be safely retryable, so the default is 1 (no automatic retry).
	MaxAttempts int `json:"maxAttempts"`
	// RetryOn lists failure classes eligible for automatic retry.
	RetryOn []domain.FailureClass `json:"retryOn,omitempty"`
}

type HITLTimeoutAction string

const (
	OnTimeoutFail         HITLTimeoutAction = "FAIL"
	OnTimeoutIntervention HITLTimeoutAction = "NEEDS_INTERVENTION"
	OnTimeoutPause        HITLTimeoutAction = "PAUSE" // wait indefinitely
)

type HITLPolicy struct {
	MaxWaitSeconds int               `json:"maxWaitSeconds"`
	OnTimeout      HITLTimeoutAction `json:"onTimeout"`
}

type OutputRule struct {
	ArtifactType string   `json:"artifactType"`
	MaxBytes     int64    `json:"maxBytes,omitempty"`
	ContentTypes []string `json:"contentTypes,omitempty"`
}

// Command is a versioned command definition.
type Command struct {
	ID                  string                `json:"id"`
	FrameworkID         string                `json:"frameworkId"`
	Version             int                   `json:"version"`
	DisplayName         string                `json:"displayName"`
	Canonical           string                `json:"canonical"` // exact framework identifier; never rewritten
	Description         string                `json:"description"`
	Phase               domain.Phase          `json:"phase"`
	Personas            []domain.Role         `json:"personas"`
	CompatibleAgents    []string              `json:"compatibleAgents"` // adapter names; empty = any
	Inputs              []InputField          `json:"inputs"`
	RepositoryAccess    RepoAccess            `json:"repositoryAccess"`
	RequiredPermissions []domain.Permission   `json:"requiredPermissions"`
	ExpectedArtifacts   []ArtifactExpectation `json:"expectedArtifacts"`
	SideEffects         []SideEffect          `json:"sideEffects"`
	TimeoutSeconds      int                   `json:"timeoutSeconds"`
	Resources           *domain.Resources     `json:"resources,omitempty"`
	Approval            ApprovalPolicy        `json:"approval"`
	Retry               RetryPolicy           `json:"retry"`
	HITL                HITLPolicy            `json:"hitl"`
	OutputValidation    []OutputRule          `json:"outputValidation,omitempty"`
	Source              Source                `json:"source"`
	Status              Status                `json:"status"`
}

var canonicalRe = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_\-:.]{0,63}$`)
var fieldNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)

// EffectivePermissions returns the phase permission plus declared and
// side-effect-implied permissions. Side effects always imply their permission,
// so a definition cannot "forget" to require Jira or deploy rights.
func (c *Command) EffectivePermissions() []domain.Permission {
	set := map[domain.Permission]bool{domain.PhasePermission(c.Phase): true}
	for _, p := range c.RequiredPermissions {
		set[p] = true
	}
	for _, e := range c.SideEffects {
		switch e {
		case EffectRepositoryWrite:
			set[domain.PermRepoPush] = true
		case EffectChangeRequest:
			set[domain.PermChangeRequest] = true
		case EffectExternalTicket:
			set[domain.PermJiraWrite] = true
		case EffectDeployNonProd:
			set[domain.PermDeployNonProd] = true
		case EffectDeployProd:
			set[domain.PermDeployProduction] = true
		}
	}
	out := make([]domain.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

// HasSideEffect reports whether the command declares the effect.
func (c *Command) HasSideEffect(e SideEffect) bool {
	for _, x := range c.SideEffects {
		if x == e {
			return true
		}
	}
	return false
}

// RequiresProductionApproval reports whether production policy applies.
func (c *Command) RequiresProductionApproval() bool {
	return c.HasSideEffect(EffectDeployProd) || c.Approval.ProductionApproval
}

// SupportsAgent reports agent compatibility.
func (c *Command) SupportsAgent(provider string) bool {
	if len(c.CompatibleAgents) == 0 {
		return true
	}
	for _, a := range c.CompatibleAgents {
		if a == provider || a == "*" {
			return true
		}
	}
	return false
}

// Validate checks a definition before it can be stored or approved.
func (c *Command) Validate() error {
	if !canonicalRe.MatchString(c.Canonical) {
		return domain.Invalid("canonical", "must look like a slash command (got %q)", c.Canonical)
	}
	if c.FrameworkID == "" {
		return domain.Invalid("frameworkId", "required")
	}
	if !c.Phase.Valid() {
		return domain.Invalid("phase", "unknown phase %q", c.Phase)
	}
	for _, r := range c.Personas {
		if !r.Valid() {
			return domain.Invalid("personas", "unknown role %q", r)
		}
	}
	switch c.RepositoryAccess {
	case RepoNone, RepoRead, RepoWrite:
	default:
		return domain.Invalid("repositoryAccess", "unknown value %q", c.RepositoryAccess)
	}
	seen := map[string]bool{}
	for _, f := range c.Inputs {
		if !fieldNameRe.MatchString(f.Name) {
			return domain.Invalid("inputs", "invalid field name %q", f.Name)
		}
		if seen[f.Name] {
			return domain.Invalid("inputs", "duplicate field %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case FieldString, FieldText, FieldBoolean, FieldInteger, FieldArtifactRef, FieldJiraKey:
		case FieldEnum:
			if len(f.Enum) == 0 {
				return domain.Invalid("inputs", "enum field %q has no values", f.Name)
			}
		default:
			return domain.Invalid("inputs", "field %q has unknown type %q", f.Name, f.Type)
		}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 7*24*3600 {
		return domain.Invalid("timeoutSeconds", "out of range")
	}
	switch c.HITL.OnTimeout {
	case "", OnTimeoutFail, OnTimeoutIntervention, OnTimeoutPause:
	default:
		return domain.Invalid("hitl.onTimeout", "unknown action %q", c.HITL.OnTimeout)
	}
	if c.HasSideEffect(EffectDeployProd) && !c.Approval.BeforeExecution {
		// DEPLOY-004: production deployment must require explicit authorization.
		return domain.Invalid("approval", "production deployment commands must require approval before execution")
	}
	return nil
}

// WithDefaults fills operational defaults.
func (c Command) WithDefaults() Command {
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 3600
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 1
	}
	if c.HITL.MaxWaitSeconds == 0 {
		c.HITL.MaxWaitSeconds = 24 * 3600
	}
	if c.HITL.OnTimeout == "" {
		// Default preserves workflow state and never auto-approves (SRS 10.5).
		c.HITL.OnTimeout = OnTimeoutIntervention
	}
	if c.RepositoryAccess == "" {
		c.RepositoryAccess = RepoRead
	}
	if c.Source == "" {
		c.Source = SourceDeclared
	}
	if c.Status == "" {
		c.Status = StatusApproved
	}
	if c.Version == 0 {
		c.Version = 1
	}
	return c
}

var jiraKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,9}-[0-9]{1,9}$`)

// ValidateInputs validates user-supplied inputs against the command schema.
// Unknown fields are rejected so that the agent never receives unexpected data.
func (c *Command) ValidateInputs(raw json.RawMessage) (map[string]any, error) {
	in := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, domain.Invalid("inputs", "must be a JSON object")
		}
	}
	known := map[string]InputField{}
	for _, f := range c.Inputs {
		known[f.Name] = f
	}
	// Platform-reserved optional inputs accepted by every command.
	reserved := map[string]bool{"instructions": true}
	for k := range in {
		if _, ok := known[k]; !ok && !reserved[k] {
			return nil, domain.Invalid("inputs."+k, "unknown input for %s", c.Canonical)
		}
	}
	if v, ok := in["instructions"]; ok {
		s, isStr := v.(string)
		if !isStr || len(s) > 20000 {
			return nil, domain.Invalid("inputs.instructions", "must be a string up to 20000 characters")
		}
	}
	for _, f := range c.Inputs {
		v, present := in[f.Name]
		if !present || v == nil || v == "" {
			if f.Required {
				return nil, domain.Invalid("inputs."+f.Name, "is required")
			}
			if f.Default != nil && !present {
				in[f.Name] = f.Default
			}
			continue
		}
		if err := validateField(f, v); err != nil {
			return nil, err
		}
	}
	return in, nil
}

func validateField(f InputField, v any) error {
	field := "inputs." + f.Name
	switch f.Type {
	case FieldString, FieldText, FieldArtifactRef:
		s, ok := v.(string)
		if !ok {
			return domain.Invalid(field, "must be a string")
		}
		max := f.MaxLength
		if max == 0 {
			if f.Type == FieldText {
				max = 50000
			} else {
				max = 512
			}
		}
		if len(s) > max {
			return domain.Invalid(field, "exceeds %d characters", max)
		}
	case FieldJiraKey:
		s, ok := v.(string)
		if !ok || !jiraKeyRe.MatchString(s) {
			return domain.Invalid(field, "must be a Jira issue key like PROJ-123")
		}
	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			return domain.Invalid(field, "must be one of %v", f.Enum)
		}
		for _, e := range f.Enum {
			if e == s {
				return nil
			}
		}
		return domain.Invalid(field, "must be one of %v", f.Enum)
	case FieldBoolean:
		if _, ok := v.(bool); !ok {
			return domain.Invalid(field, "must be a boolean")
		}
	case FieldInteger:
		n, ok := v.(json.Number)
		if !ok {
			return domain.Invalid(field, "must be an integer")
		}
		if _, err := n.Int64(); err != nil {
			return domain.Invalid(field, "must be an integer")
		}
	default:
		return fmt.Errorf("unsupported field type %q", f.Type)
	}
	return nil
}
