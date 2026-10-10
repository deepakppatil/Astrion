package domain

import (
	"encoding/json"
	"time"
)

// Phase is an ADLC phase. Deploy and Maintain are distinct phases even though
// the SRS groups them in a single heading.
type Phase string

const (
	PhasePlan      Phase = "PLAN"
	PhaseDesign    Phase = "DESIGN"
	PhaseImplement Phase = "IMPLEMENT"
	PhaseTest      Phase = "TEST"
	PhaseDeploy    Phase = "DEPLOY"
	PhaseMaintain  Phase = "MAINTAIN"
)

var AllPhases = []Phase{PhasePlan, PhaseDesign, PhaseImplement, PhaseTest, PhaseDeploy, PhaseMaintain}

func (p Phase) Valid() bool {
	for _, x := range AllPhases {
		if x == p {
			return true
		}
	}
	return false
}

// DataClassification drives provider-selection policy (SRS 14.4).
type DataClassification string

const (
	ClassPublic       DataClassification = "PUBLIC"
	ClassInternal     DataClassification = "INTERNAL"
	ClassConfidential DataClassification = "CONFIDENTIAL"
	ClassRestricted   DataClassification = "RESTRICTED"
)

// Rank orders classifications from least to most sensitive.
func (c DataClassification) Rank() int {
	switch c {
	case ClassPublic:
		return 0
	case ClassInternal:
		return 1
	case ClassConfidential:
		return 2
	case ClassRestricted:
		return 3
	}
	return 3 // unknown is treated as most sensitive (fail safe)
}

// Tenant is the organizational isolation boundary.
type Tenant struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Status             string          `json:"status"`
	Quotas             TenantQuotas    `json:"quotas"`
	RetentionDays      int             `json:"retentionDays"`
	PermittedProviders []string        `json:"permittedProviders"`
	SecurityPolicy     json.RawMessage `json:"securityPolicy,omitempty"`
	CreatedAt          time.Time       `json:"createdAt"`
}

type TenantQuotas struct {
	MaxConcurrentJobs              int `json:"maxConcurrentJobs"`
	MaxQueuedJobs                  int `json:"maxQueuedJobs"`
	MaxSubmissionsPerUserPerMinute int `json:"maxSubmissionsPerUserPerMinute"`
}

// User is an authenticated identity within a tenant.
type User struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenantId"`
	Subject     string    `json:"subject"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Roles       []Role    `json:"roles"`
	CreatedAt   time.Time `json:"createdAt"`
}

// WorkspaceType and lifecycle mode are modeled separately (SRS 7.3).
type WorkspaceType string

const (
	WorkspaceSingleRepo WorkspaceType = "SINGLE_REPOSITORY"
	WorkspaceMultiRepo  WorkspaceType = "MULTI_REPOSITORY"
	WorkspaceManaged    WorkspaceType = "MANAGED"
	WorkspaceEphemeral  WorkspaceType = "EPHEMERAL"
	WorkspaceCustom     WorkspaceType = "CUSTOM"
)

type WorkspaceLifecycle string

const (
	LifecycleEphemeralCheckout WorkspaceLifecycle = "EPHEMERAL_CHECKOUT"
	LifecyclePersistent        WorkspaceLifecycle = "PERSISTENT" // explicitly approved scenarios only
)

type Workspace struct {
	ID                     string             `json:"id"`
	TenantID               string             `json:"tenantId"`
	Name                   string             `json:"name"`
	Description            string             `json:"description"`
	Type                   WorkspaceType      `json:"type"`
	LifecycleMode          WorkspaceLifecycle `json:"lifecycleMode"`
	DataClassification     DataClassification `json:"dataClassification"`
	DefaultAgentProfileID  string             `json:"defaultAgentProfileId,omitempty"`
	DefaultFrameworkID     string             `json:"defaultFrameworkId,omitempty"`
	DefaultProvider        string             `json:"defaultProvider,omitempty"`
	PermittedAgentProfiles []string           `json:"permittedAgentProfiles"`
	PermittedProviders     []string           `json:"permittedProviders"`
	Repositories           []Repository       `json:"repositories,omitempty"`
	CreatedBy              string             `json:"createdBy"`
	CreatedAt              time.Time          `json:"createdAt"`
	UpdatedAt              time.Time          `json:"updatedAt"`
}

type RepoProvider string

const (
	RepoGitHub  RepoProvider = "GITHUB"
	RepoGitLab  RepoProvider = "GITLAB"
	RepoGeneric RepoProvider = "GIT"
)

// Repository is a logical repository identity — never a checkout directory.
type Repository struct {
	ID                  string       `json:"id"`
	TenantID            string       `json:"tenantId"`
	WorkspaceID         string       `json:"workspaceId"`
	Provider            RepoProvider `json:"provider"`
	URL                 string       `json:"url"`
	Namespace           string       `json:"namespace"`
	DefaultBranch       string       `json:"defaultBranch"`
	CredentialRef       string       `json:"credentialRef,omitempty"` // reference into the secret manager, never a secret
	CheckoutPolicy      string       `json:"checkoutPolicy"`
	PermittedOperations []string     `json:"permittedOperations"` // e.g. read, branch, push, merge_request
	PinnedRevision      string       `json:"pinnedRevision,omitempty"`
	LastValidatedAt     *time.Time   `json:"lastValidatedAt,omitempty"`
	ValidationError     string       `json:"validationError,omitempty"`
	CreatedAt           time.Time    `json:"createdAt"`
}

// InteractionMode is how an agent runtime handles human-in-the-loop pauses.
type InteractionMode string

const (
	// ModeLiveSession keeps the process alive while waiting (strict timeouts apply).
	ModeLiveSession InteractionMode = "LIVE_SESSION"
	// ModeCheckpointResume persists state, releases compute, and resumes in a new attempt.
	ModeCheckpointResume InteractionMode = "CHECKPOINT_RESUME"
)

// AgentProfile is a versioned, independently configurable runtime definition.
type AgentProfile struct {
	ID              string            `json:"id"`
	TenantID        string            `json:"tenantId"`
	Name            string            `json:"name"`
	Version         int               `json:"version"`
	Provider        string            `json:"provider"` // adapter name, e.g. "mock", "claude-code"
	CLIName         string            `json:"cliName"`
	CLIVersion      string            `json:"cliVersion"`
	RuntimeImage    string            `json:"runtimeImage"`
	AuthMethod      string            `json:"authMethod"`
	InteractionMode InteractionMode   `json:"interactionMode"`
	Environment     map[string]string `json:"environment,omitempty"`
	DefaultArgs     []string          `json:"defaultArgs,omitempty"`
	Resources       Resources         `json:"resources"`
	ExecutionPolicy json.RawMessage   `json:"executionPolicy,omitempty"`
	Enabled         bool              `json:"enabled"`
	CreatedAt       time.Time         `json:"createdAt"`
}

type Resources struct {
	CPURequest       string `json:"cpuRequest"`
	CPULimit         string `json:"cpuLimit"`
	MemoryRequest    string `json:"memoryRequest"`
	MemoryLimit      string `json:"memoryLimit"`
	EphemeralStorage string `json:"ephemeralStorage"`
}

// FrameworkProfile pins an SDD framework to an immutable revision.
type FrameworkProfile struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenantId"`
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	Source            string          `json:"source"`
	Revision          string          `json:"revision"`
	DiscoveryStrategy string          `json:"discoveryStrategy"` // DECLARED | ADAPTER
	CompatibleAgents  []string        `json:"compatibleAgents"`
	RequiredTools     []string        `json:"requiredTools"`
	Config            json.RawMessage `json:"config,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
}

// JobRepository is a repository resolved for one job, with the exact revision used.
type JobRepository struct {
	RepositoryID      string `json:"repositoryId"`
	URL               string `json:"url"`
	RequestedRevision string `json:"requestedRevision"`
	ResolvedCommit    string `json:"resolvedCommit,omitempty"`
	CheckoutPath      string `json:"checkoutPath,omitempty"`
	WorkingBranch     string `json:"workingBranch,omitempty"`
	FinalCommit       string `json:"finalCommit,omitempty"`
	ChangeRequestURL  string `json:"changeRequestUrl,omitempty"`
}

type Job struct {
	ID                  string            `json:"id"`
	TenantID            string            `json:"tenantId"`
	WorkspaceID         string            `json:"workspaceId"`
	InitiatedBy         string            `json:"initiatedBy"`
	Phase               Phase             `json:"phase"`
	Command             string            `json:"command"`
	CommandID           string            `json:"commandId"`
	AgentProfileID      string            `json:"agentProfileId"`
	AgentProfileVersion int               `json:"agentProfileVersion"`
	FrameworkID         string            `json:"frameworkId"`
	FrameworkRevision   string            `json:"frameworkRevision"`
	RequestedProvider   string            `json:"requestedProvider"`
	SelectedProvider    string            `json:"selectedProvider,omitempty"`
	Inputs              json.RawMessage   `json:"inputs"`
	Repositories        []JobRepository   `json:"repositories"`
	WorkflowID          string            `json:"workflowId"`
	State               JobState          `json:"state"`
	CurrentStep         string            `json:"currentStep"`
	PendingOutcome      Outcome           `json:"pendingOutcome,omitempty"`
	FailureClass        FailureClass      `json:"failureClass,omitempty"`
	FailureMessage      string            `json:"failureMessage,omitempty"`
	CleanupStatus       string            `json:"cleanupStatus"`
	Attempt             int               `json:"attempt"`
	RetryOf             string            `json:"retryOf,omitempty"`
	ParentJobID         string            `json:"parentJobId,omitempty"`
	Links               map[string]string `json:"links,omitempty"` // lineage: taskId, specArtifactId, jiraKey...
	CancelRequestedAt   *time.Time        `json:"cancelRequestedAt,omitempty"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	StartedAt           *time.Time        `json:"startedAt,omitempty"`
	FinishedAt          *time.Time        `json:"finishedAt,omitempty"`
}

// Cleanup statuses.
const (
	CleanupNotStarted = "NOT_STARTED"
	CleanupPending    = "PENDING"
	CleanupCompleted  = "COMPLETED"
	CleanupFailed     = "FAILED"
)

// ExecutionStatus is reported by the in-environment supervisor and the provider.
type ExecutionStatus string

const (
	ExecProvisioning  ExecutionStatus = "PROVISIONING"
	ExecPreparing     ExecutionStatus = "PREPARING_WORKSPACE"
	ExecRunning       ExecutionStatus = "RUNNING"
	ExecAwaitingInput ExecutionStatus = "AWAITING_INPUT" // live session waiting on stdin
	ExecSuspended     ExecutionStatus = "SUSPENDED"      // checkpoint saved, process exited
	ExecCompleted     ExecutionStatus = "COMPLETED"
	ExecFailed        ExecutionStatus = "FAILED"
	ExecLost          ExecutionStatus = "LOST"
	ExecTerminated    ExecutionStatus = "TERMINATED"
)

func (s ExecutionStatus) Finished() bool {
	switch s {
	case ExecSuspended, ExecCompleted, ExecFailed, ExecLost, ExecTerminated:
		return true
	}
	return false
}

// ExecutionEnvironment is one execution attempt of a job on a provider.
type ExecutionEnvironment struct {
	ID                   string          `json:"id"`
	TenantID             string          `json:"tenantId"`
	JobID                string          `json:"jobId"`
	Attempt              int             `json:"attempt"`
	Provider             string          `json:"provider"`
	ProviderRef          string          `json:"providerRef"`
	Status               ExecutionStatus `json:"status"`
	InteractionMode      InteractionMode `json:"interactionMode"`
	ExitCode             *int            `json:"exitCode,omitempty"`
	ResultStatus         string          `json:"resultStatus,omitempty"`
	Summary              string          `json:"summary,omitempty"`
	ErrorMessage         string          `json:"errorMessage,omitempty"`
	FailureClass         FailureClass    `json:"failureClass,omitempty"`
	PendingInteractionID string          `json:"pendingInteractionId,omitempty"`
	CheckpointArtifactID string          `json:"checkpointArtifactId,omitempty"`
	ResumeInteractionID  string          `json:"resumeInteractionId,omitempty"`
	CleanupStatus        string          `json:"cleanupStatus"`
	CleanupError         string          `json:"cleanupError,omitempty"`
	ExpiresAt            time.Time       `json:"expiresAt"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
	FinishedAt           *time.Time      `json:"finishedAt,omitempty"`
}

// InteractionKind distinguishes HITL request types (SRS 10.1).
type InteractionKind string

const (
	InteractionMissingInput      InteractionKind = "MISSING_INPUT"
	InteractionClarification     InteractionKind = "CLARIFICATION"
	InteractionMultipleChoice    InteractionKind = "MULTIPLE_CHOICE"
	InteractionFreeText          InteractionKind = "FREE_TEXT"
	InteractionApproval          InteractionKind = "APPROVAL"
	InteractionPermission        InteractionKind = "PERMISSION"
	InteractionToolAuthorization InteractionKind = "TOOL_AUTHORIZATION"
	InteractionConflict          InteractionKind = "CONFLICT_RESOLUTION"
	InteractionArtifactReview    InteractionKind = "ARTIFACT_REVIEW"
)

var AllInteractionKinds = []InteractionKind{
	InteractionMissingInput, InteractionClarification, InteractionMultipleChoice,
	InteractionFreeText, InteractionApproval, InteractionPermission,
	InteractionToolAuthorization, InteractionConflict, InteractionArtifactReview,
}

func (k InteractionKind) Valid() bool {
	for _, x := range AllInteractionKinds {
		if x == k {
			return true
		}
	}
	return false
}

// IsDecision reports whether the kind is approve/reject-shaped and therefore
// puts the job in WAITING_FOR_APPROVAL rather than WAITING_FOR_INPUT.
func (k InteractionKind) IsDecision() bool {
	switch k {
	case InteractionApproval, InteractionPermission, InteractionToolAuthorization, InteractionArtifactReview:
		return true
	}
	return false
}

// InteractionStatus lifecycle: RECORDED (persisted, workflow not yet waiting)
// -> OPEN (workflow waiting, user notified) -> RESOLVED | EXPIRED | CANCELLED.
type InteractionStatus string

const (
	InteractionRecorded  InteractionStatus = "RECORDED"
	InteractionOpen      InteractionStatus = "OPEN"
	InteractionResolved  InteractionStatus = "RESOLVED"
	InteractionExpired   InteractionStatus = "EXPIRED"
	InteractionCancelled InteractionStatus = "CANCELLED"
)

type InteractionOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// InputSchema is a deliberately small, safe schema language for HITL responses.
type InputSchema struct {
	Required  bool `json:"required"`
	MaxLength int  `json:"maxLength,omitempty"`
	MinLength int  `json:"minLength,omitempty"`
	Multiple  bool `json:"multiple,omitempty"` // multi-select for MULTIPLE_CHOICE
}

type Interaction struct {
	ID             string               `json:"id"`
	TenantID       string               `json:"tenantId"`
	JobID          string               `json:"jobId"`
	ExecutionID    string               `json:"executionId"`
	StepID         string               `json:"stepId"`
	AgentRequestID string               `json:"agentRequestId"`
	Kind           InteractionKind      `json:"kind"`
	Prompt         string               `json:"prompt"`
	Options        []InteractionOption  `json:"options,omitempty"`
	Schema         InputSchema          `json:"schema"`
	RequestedUser  string               `json:"requestedUser,omitempty"`
	RequestedRole  Role                 `json:"requestedRole,omitempty"`
	Status         InteractionStatus    `json:"status"`
	CreatedAt      time.Time            `json:"createdAt"`
	OpenedAt       *time.Time           `json:"openedAt,omitempty"`
	ExpiresAt      *time.Time           `json:"expiresAt,omitempty"`
	ResolvedAt     *time.Time           `json:"resolvedAt,omitempty"`
	Response       *InteractionResponse `json:"response,omitempty"`
}

type Decision string

const (
	DecisionApprove Decision = "APPROVE"
	DecisionReject  Decision = "REJECT"
	DecisionAnswer  Decision = "ANSWER"
)

type InteractionResponse struct {
	ID             string    `json:"id"`
	InteractionID  string    `json:"interactionId"`
	Decision       Decision  `json:"decision"`
	Values         []string  `json:"values,omitempty"`
	Text           string    `json:"text,omitempty"`
	Comment        string    `json:"comment,omitempty"`
	RespondedBy    string    `json:"respondedBy"`
	IdempotencyKey string    `json:"-"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ArtifactClass string

const (
	ArtifactUser     ArtifactClass = "USER"     // visible to users with job access
	ArtifactInternal ArtifactClass = "INTERNAL" // checkpoints etc; never downloadable by users
)

type Artifact struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenantId"`
	JobID          string        `json:"jobId"`
	ExecutionID    string        `json:"executionId"`
	Type           string        `json:"type"` // specification, architecture, diff, test-report, execution-log, checkpoint...
	Name           string        `json:"name"`
	ObjectKey      string        `json:"-"`
	ContentType    string        `json:"contentType"`
	SizeBytes      int64         `json:"sizeBytes"`
	SHA256         string        `json:"sha256"`
	Classification ArtifactClass `json:"classification"`
	SourceStep     string        `json:"sourceStep"`
	RepositoryID   string        `json:"repositoryId,omitempty"`
	Revision       string        `json:"revision,omitempty"`
	RetainUntil    *time.Time    `json:"retainUntil,omitempty"`
	CreatedAt      time.Time     `json:"createdAt"`
}

// JobEvent is a durable, ordered event used for SSE replay (and as the outbox).
type JobEvent struct {
	ID        int64           `json:"id"`
	TenantID  string          `json:"tenantId"`
	JobID     string          `json:"jobId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Job event types delivered to the frontend.
const (
	EventJobStatusChanged    = "job.status_changed"
	EventStepChanged         = "job.step_changed"
	EventLogBatch            = "job.log_batch"
	EventInputRequired       = "interaction.input_required"
	EventApprovalRequired    = "interaction.approval_required"
	EventInteractionResolved = "interaction.resolved"
	EventArtifactAvailable   = "artifact.available"
	EventJobFailed           = "job.failed"
	EventJobCompleted        = "job.completed"
	EventCleanupFailed       = "cleanup.failed"
)

type LogLine struct {
	Seq         int64     `json:"seq"`
	ExecutionID string    `json:"executionId"`
	Stream      string    `json:"stream"`
	Line        string    `json:"line"`
	Timestamp   time.Time `json:"ts"`
}

type AuditEvent struct {
	ID         int64           `json:"id"`
	TenantID   string          `json:"tenantId"`
	Actor      string          `json:"actor"`
	ActorType  string          `json:"actorType"` // user | system | execution
	Action     string          `json:"action"`
	TargetType string          `json:"targetType"`
	TargetID   string          `json:"targetId"`
	Details    json.RawMessage `json:"details,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}
