// Package authz implements backend-enforced authorization. Frontend visibility
// is never treated as a security boundary (SRS 4.6).
package authz

import (
	"context"
	"sort"

	"github.com/deepakppatil/astrion/internal/domain"
)

// Principal is the authenticated caller.
type Principal struct {
	UserID   string        `json:"userId"`
	TenantID string        `json:"tenantId"`
	Subject  string        `json:"subject"`
	Email    string        `json:"email"`
	Name     string        `json:"name"`
	Roles    []domain.Role `json:"roles"`
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok && p != nil
}

// rolePermissions is the default role->permission policy. It is deliberately
// static code (reviewed via PR) for the vertical slice; tenant-customizable
// policies are planned (see docs/plan/implementation-plan.md).
var rolePermissions = map[domain.Role][]domain.Permission{
	domain.RoleViewer: {
		domain.PermWorkspaceRead, domain.PermJobRead, domain.PermArtifactRead,
	},
	domain.RoleBusinessAnalyst: {
		domain.PermWorkspaceRead, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermExecPlan, domain.PermJiraWrite,
	},
	domain.RoleArchitect: {
		domain.PermWorkspaceRead, domain.PermWorkspaceWrite, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermExecPlan, domain.PermExecDesign,
	},
	domain.RoleDeveloper: {
		domain.PermWorkspaceRead, domain.PermWorkspaceWrite, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermExecDesign, domain.PermExecImplement, domain.PermExecTest, domain.PermExecMaintain,
		domain.PermRepoPush, domain.PermChangeRequest,
	},
	domain.RoleQA: {
		domain.PermWorkspaceRead, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermExecTest,
	},
	domain.RoleReleaseEngineer: {
		domain.PermWorkspaceRead, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermExecTest, domain.PermExecDeploy, domain.PermExecMaintain,
		domain.PermDeployNonProd, domain.PermDeployProduction, domain.PermApproveProduction,
		domain.PermProviderSelect,
	},
	domain.RolePlatformAdmin: {
		domain.PermWorkspaceRead, domain.PermWorkspaceWrite, domain.PermJobRead, domain.PermArtifactRead,
		domain.PermJobCancelAny, domain.PermInteractAny,
		domain.PermAgentConfigure, domain.PermRegistryManage, domain.PermProviderManage,
		domain.PermTenantAdmin, domain.PermAuditRead, domain.PermProviderSelect,
		// Note: platform admins do NOT implicitly get phase execution, deploy, or
		// Jira write permissions. Administration is separate from delivery.
	},
}

// Permissions returns the effective, de-duplicated permission set for roles.
func Permissions(roles []domain.Role) []domain.Permission {
	set := map[domain.Permission]bool{}
	for _, r := range roles {
		for _, p := range rolePermissions[r] {
			set[p] = true
		}
	}
	out := make([]domain.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Has reports whether the principal holds a permission.
func (p *Principal) Has(perm domain.Permission) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		for _, x := range rolePermissions[r] {
			if x == perm {
				return true
			}
		}
	}
	return false
}

// HasAll reports whether the principal holds every permission.
func (p *Principal) HasAll(perms ...domain.Permission) bool {
	for _, perm := range perms {
		if !p.Has(perm) {
			return false
		}
	}
	return true
}

// HasRole reports whether the principal holds a role.
func (p *Principal) HasRole(r domain.Role) bool {
	if p == nil {
		return false
	}
	for _, x := range p.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// Require returns domain.ErrForbidden unless the principal holds all permissions.
func Require(p *Principal, perms ...domain.Permission) error {
	if p == nil {
		return domain.ErrUnauthorized
	}
	for _, perm := range perms {
		if !p.Has(perm) {
			return domain.Errorf(domain.ErrForbidden, "missing permission %s", perm)
		}
	}
	return nil
}

// SameTenant enforces the tenant boundary. Resources from another tenant are
// reported as not found to avoid leaking their existence.
func SameTenant(p *Principal, tenantID string) error {
	if p == nil {
		return domain.ErrUnauthorized
	}
	if p.TenantID == "" || p.TenantID != tenantID {
		return domain.ErrNotFound
	}
	return nil
}

// CanRespond decides whether a principal may answer an interaction:
// the initiating user, the explicitly requested user, a holder of the requested
// role, or an admin holding interaction:respond:any. Production approvals
// additionally require approve:production.
func CanRespond(p *Principal, it *domain.Interaction, jobInitiator string, requiresProductionApproval bool) error {
	if err := SameTenant(p, it.TenantID); err != nil {
		return err
	}
	if requiresProductionApproval && !p.Has(domain.PermApproveProduction) {
		return domain.Errorf(domain.ErrForbidden, "production approval requires %s", domain.PermApproveProduction)
	}
	switch {
	case it.RequestedUser != "" && it.RequestedUser == p.UserID:
		return nil
	case it.RequestedRole != "" && p.HasRole(it.RequestedRole):
		return nil
	case it.RequestedUser == "" && it.RequestedRole == "" && jobInitiator == p.UserID:
		return nil
	case p.Has(domain.PermInteractAny):
		return nil
	}
	return domain.Errorf(domain.ErrForbidden, "not authorized to respond to this interaction")
}
