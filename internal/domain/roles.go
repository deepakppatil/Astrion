package domain

// Role is a persona-aligned role (SRS section 4).
type Role string

const (
	RoleBusinessAnalyst Role = "business_analyst" // also product manager
	RoleArchitect       Role = "architect"
	RoleDeveloper       Role = "developer"
	RoleQA              Role = "qa_engineer"
	RoleReleaseEngineer Role = "release_engineer"
	RolePlatformAdmin   Role = "platform_admin"
	RoleViewer          Role = "viewer"
)

var AllRoles = []Role{RoleBusinessAnalyst, RoleArchitect, RoleDeveloper, RoleQA, RoleReleaseEngineer, RolePlatformAdmin, RoleViewer}

func (r Role) Valid() bool {
	for _, x := range AllRoles {
		if x == r {
			return true
		}
	}
	return false
}

// Permission is a fine-grained capability checked by the backend. Commands
// declare the permissions they require; roles grant permissions.
type Permission string

const (
	PermWorkspaceRead  Permission = "workspace:read"
	PermWorkspaceWrite Permission = "workspace:write"
	PermJobRead        Permission = "job:read"
	PermJobCancelAny   Permission = "job:cancel:any"
	PermInteractAny    Permission = "interaction:respond:any"
	PermArtifactRead   Permission = "artifact:read"
	PermAgentConfigure Permission = "agent:configure"
	PermRegistryManage Permission = "registry:manage"
	PermProviderManage Permission = "provider:manage"
	PermTenantAdmin    Permission = "tenant:admin"
	PermAuditRead      Permission = "audit:read"
	PermProviderSelect Permission = "provider:select" // choose a provider explicitly instead of AUTO

	// Phase execution.
	PermExecPlan      Permission = "execute:plan"
	PermExecDesign    Permission = "execute:design"
	PermExecImplement Permission = "execute:implement"
	PermExecTest      Permission = "execute:test"
	PermExecDeploy    Permission = "execute:deploy"
	PermExecMaintain  Permission = "execute:maintain"

	// External side effects (separately grantable, SRS 21.4 / AUTH-007).
	PermJiraWrite         Permission = "integration:jira:write"
	PermRepoPush          Permission = "repository:push"
	PermChangeRequest     Permission = "repository:change_request"
	PermDeployNonProd     Permission = "deploy:non_production"
	PermDeployProduction  Permission = "deploy:production"
	PermApproveProduction Permission = "approve:production"
)

// PhasePermission returns the permission required to execute commands in a phase.
func PhasePermission(p Phase) Permission {
	switch p {
	case PhasePlan:
		return PermExecPlan
	case PhaseDesign:
		return PermExecDesign
	case PhaseImplement:
		return PermExecImplement
	case PhaseTest:
		return PermExecTest
	case PhaseDeploy:
		return PermExecDeploy
	case PhaseMaintain:
		return PermExecMaintain
	}
	return Permission("execute:unknown")
}
