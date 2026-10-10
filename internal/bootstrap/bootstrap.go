// Package bootstrap performs idempotent startup seeding: the configured
// tenant, the built-in command registry bundles and (in development) a mock
// agent profile and demo workspace so the vertical slice works end to end
// without external services.
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
)

type Options struct {
	TenantID   string
	TenantName string
	Quotas     domain.TenantQuotas
	// Demo seeds a mock agent profile and a demo workspace (dev only).
	Demo bool
	// DemoRepoURL is attached to the demo workspace when set.
	DemoRepoURL string
}

type Result struct {
	FrameworkID    string
	AgentProfileID string
	WorkspaceID    string
}

func Run(ctx context.Context, s *store.Store, o Options) (*Result, error) {
	if o.TenantID == "" {
		return nil, errors.New("bootstrap: tenant id required")
	}
	if o.TenantName == "" {
		o.TenantName = o.TenantID
	}
	if err := s.CreateTenant(ctx, &domain.Tenant{ID: o.TenantID, Name: o.TenantName, Quotas: o.Quotas}); err != nil {
		return nil, fmt.Errorf("bootstrap tenant: %w", err)
	}
	bundles, err := registry.DefaultBundles()
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, b := range bundles {
		if err := s.UpsertBundle(ctx, "system:bootstrap", nil, b); err != nil {
			return nil, fmt.Errorf("bootstrap registry %s: %w", b.Framework.ID, err)
		}
		if res.FrameworkID == "" {
			res.FrameworkID = b.Framework.ID
		}
	}
	if !o.Demo {
		return res, nil
	}
	profiles, err := s.ListAgentProfiles(ctx, o.TenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.Provider == "mock" {
			res.AgentProfileID = p.ID
		}
	}
	if res.AgentProfileID == "" {
		p := &domain.AgentProfile{TenantID: o.TenantID, Name: "Mock SDD agent", Provider: "mock", CLIName: "mock-agent",
			CLIVersion: "dev", InteractionMode: domain.ModeCheckpointResume, Enabled: true,
			Resources: domain.Resources{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi"}}
		if err := s.SaveAgentProfile(ctx, "system:bootstrap", p); err != nil {
			return nil, err
		}
		res.AgentProfileID = p.ID
	}
	wss, err := s.ListWorkspaces(ctx, o.TenantID)
	if err != nil {
		return nil, err
	}
	for _, w := range wss {
		if w.Name == "Demo workspace" {
			res.WorkspaceID = w.ID
		}
	}
	if res.WorkspaceID == "" {
		w := &domain.Workspace{TenantID: o.TenantID, Name: "Demo workspace",
			Description: "Seeded for local development: mock agent, local execution pool.",
			Type:        domain.WorkspaceMultiRepo, LifecycleMode: domain.LifecycleEphemeralCheckout,
			DataClassification: domain.ClassInternal, DefaultAgentProfileID: res.AgentProfileID,
			DefaultFrameworkID: res.FrameworkID, DefaultProvider: "AUTO", CreatedBy: "system:bootstrap"}
		if err := s.CreateWorkspace(ctx, w); err != nil && !errors.Is(err, domain.ErrConflict) {
			return nil, err
		}
		res.WorkspaceID = w.ID
	}
	// Attach the demo repository whenever it is configured and missing, so
	// setting ASTRION_DEMO_REPO_URL after the first start still takes effect.
	if o.DemoRepoURL != "" {
		repos, err := s.ListRepositories(ctx, o.TenantID, res.WorkspaceID)
		if err != nil {
			return nil, err
		}
		present := false
		for _, r := range repos {
			present = present || r.URL == o.DemoRepoURL
		}
		if !present {
			r := &domain.Repository{TenantID: o.TenantID, WorkspaceID: res.WorkspaceID, Provider: domain.RepoGeneric, URL: o.DemoRepoURL,
				DefaultBranch: "main", CheckoutPolicy: "SHALLOW", PermittedOperations: []string{"read", "write"}}
			if err := s.AddRepository(ctx, "system:bootstrap", r); err != nil && !errors.Is(err, domain.ErrConflict) {
				return nil, err
			}
		}
	}
	return res, nil
}
