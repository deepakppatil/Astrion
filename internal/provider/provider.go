// Package provider defines the infrastructure provider abstraction (SRS
// section 14). Providers allocate disposable, isolated execution
// environments. The workflow never branches on Rancher, EKS, or any other
// specific infrastructure; it only uses this contract.
package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
)

// Ownership metadata is attached to every environment (LIFE-008) and is what
// the reconciler uses to find abandoned resources. Cleanup is always scoped by
// these labels so it can never delete another tenant's resources.
type Ownership struct {
	TenantID    string    `json:"tenantId"`
	JobID       string    `json:"jobId"`
	WorkflowID  string    `json:"workflowId"`
	ExecutionID string    `json:"executionId"`
	Attempt     int       `json:"attempt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Label keys (Kubernetes-compatible).
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelTenant    = "astrion.io/tenant"
	LabelJob       = "astrion.io/job"
	LabelExecution = "astrion.io/execution"
	LabelAttempt   = "astrion.io/attempt"
	AnnotWorkflow  = "astrion.io/workflow"
	AnnotExpiresAt = "astrion.io/expires-at"
	ManagedByValue = "astrion"
	ComponentValue = "execution"
)

func (o Ownership) Labels() map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelComponent: ComponentValue,
		LabelTenant:    o.TenantID,
		LabelJob:       o.JobID,
		LabelExecution: o.ExecutionID,
		LabelAttempt:   strconv.Itoa(o.Attempt),
	}
}

func (o Ownership) Annotations() map[string]string {
	return map[string]string{
		AnnotWorkflow:  o.WorkflowID,
		AnnotExpiresAt: o.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// Spec describes an execution environment to allocate.
type Spec struct {
	Owner          Ownership         `json:"owner"`
	Image          string            `json:"image"`
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env"` // non-secret configuration
	Secrets        map[string][]byte `json:"-"`   // delivered via provider secret mechanism, mounted as files
	Resources      domain.Resources  `json:"resources"`
	ActiveDeadline time.Duration     `json:"activeDeadline"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	// NetworkProfile selects a pre-provisioned network policy (e.g. "restricted-egress").
	NetworkProfile string `json:"networkProfile"`
}

// Ref identifies an allocated environment within a provider.
type Ref struct {
	Provider string `json:"provider"`
	ID       string `json:"id"` // e.g. "namespace/job-name"
}

type Phase string

const (
	PhasePending   Phase = "PENDING"
	PhaseRunning   Phase = "RUNNING"
	PhaseSucceeded Phase = "SUCCEEDED"
	PhaseFailed    Phase = "FAILED"
	PhaseNotFound  Phase = "NOT_FOUND"
)

type Status struct {
	Ref     Ref       `json:"ref"`
	Phase   Phase     `json:"phase"`
	Message string    `json:"message,omitempty"`
	Owner   Ownership `json:"owner"`
	Created time.Time `json:"created"`
}

// Capacity is a coarse admission signal.
type Capacity struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Provider is the infrastructure contract. All mutating operations must be
// idempotent: Allocate keyed by ExecutionID, Terminate tolerating NotFound.
type Provider interface {
	Name() string
	Validate(ctx context.Context) error
	CheckCapacity(ctx context.Context, spec Spec) (Capacity, error)
	Allocate(ctx context.Context, spec Spec) (Ref, error)
	Status(ctx context.Context, ref Ref) (Status, error)
	Terminate(ctx context.Context, ref Ref) error
	// ListOwned returns environments managed by Astrion (optionally per tenant)
	// for orphan reconciliation (LIFE-004).
	ListOwned(ctx context.Context, tenantID string) ([]Status, error)
}

// Pool is a configured execution pool backed by a provider.
type Pool struct {
	Name    string `json:"name"`
	Type    string `json:"type"`    // kubernetes | local
	Network string `json:"network"` // PRIVATE | PUBLIC
	Region  string `json:"region,omitempty"`
	// MaxClassification is the most sensitive data this pool may process.
	MaxClassification domain.DataClassification `json:"maxClassification"`
	Tools             []string                  `json:"tools,omitempty"`
	CostWeight        int                       `json:"costWeight"`
	Enabled           bool                      `json:"enabled"`
	Provider          Provider                  `json:"-"`
}

// Registry holds configured pools.
type Registry struct {
	mu    sync.RWMutex
	pools map[string]*Pool
}

func NewRegistry() *Registry { return &Registry{pools: map[string]*Pool{}} }

func (r *Registry) Add(p *Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[p.Name] = p
}

func (r *Registry) Get(name string) (*Pool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pools[name]
	if !ok {
		return nil, domain.Errorf(domain.ErrNotFound, "execution pool %q", name)
	}
	return p, nil
}

func (r *Registry) All() []*Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Pool, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SelectionRequest is the policy input for choosing a pool (SRS 14.4).
type SelectionRequest struct {
	Requested          string // "AUTO" or a pool name
	DataClassification domain.DataClassification
	TenantPermitted    []string
	WorkspacePermitted []string
	RequiredTools      []string
	PrivateNetworkRepo bool // repository only reachable from private networks
	Region             string
}

// Select chooses an execution pool. It fails safe: it never moves data to a
// pool whose MaxClassification is below the workspace classification, even
// when compliant pools lack capacity — capacity is checked later and results
// in queueing, not in falling back to a less restricted pool.
func Select(pools []*Pool, req SelectionRequest) (*Pool, error) {
	var candidates []*Pool
	var reasons []string
	for _, p := range pools {
		if !p.Enabled {
			continue
		}
		if req.Requested != "" && req.Requested != "AUTO" && p.Name != req.Requested {
			continue
		}
		if len(req.TenantPermitted) > 0 && !contains(req.TenantPermitted, p.Name) {
			reasons = append(reasons, p.Name+": not permitted for tenant")
			continue
		}
		if len(req.WorkspacePermitted) > 0 && !contains(req.WorkspacePermitted, p.Name) {
			reasons = append(reasons, p.Name+": not permitted for workspace")
			continue
		}
		if req.DataClassification.Rank() > p.MaxClassification.Rank() {
			reasons = append(reasons, fmt.Sprintf("%s: classification %s exceeds pool maximum %s", p.Name, req.DataClassification, p.MaxClassification))
			continue
		}
		if req.PrivateNetworkRepo && p.Network != "PRIVATE" {
			reasons = append(reasons, p.Name+": repository requires private network")
			continue
		}
		if req.Region != "" && p.Region != "" && p.Region != req.Region {
			reasons = append(reasons, p.Name+": region restriction")
			continue
		}
		missing := false
		for _, t := range req.RequiredTools {
			if len(p.Tools) > 0 && !contains(p.Tools, t) {
				missing = true
			}
		}
		if missing {
			reasons = append(reasons, p.Name+": missing required tools")
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		if req.Requested != "" && req.Requested != "AUTO" && len(reasons) == 0 {
			return nil, domain.Errorf(domain.ErrInvalid, "execution pool %q not available", req.Requested)
		}
		return nil, domain.Errorf(domain.ErrForbidden, "no compliant execution pool: %v", reasons)
	}
	// Prefer the most restrictive compliant pool (private over public), then cost.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Network == "PRIVATE") != (b.Network == "PRIVATE") {
			return a.Network == "PRIVATE"
		}
		if a.CostWeight != b.CostWeight {
			return a.CostWeight < b.CostWeight
		}
		return a.Name < b.Name
	})
	return candidates[0], nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
