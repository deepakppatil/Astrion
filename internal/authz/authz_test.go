package authz

import (
	"errors"
	"testing"

	"github.com/deepakppatil/astrion/internal/domain"
)

func TestAdminDoesNotImplicitlyDeployOrExecute(t *testing.T) {
	admin := &Principal{UserID: "u1", TenantID: "t1", Roles: []domain.Role{domain.RolePlatformAdmin}}
	for _, p := range []domain.Permission{domain.PermDeployProduction, domain.PermExecImplement, domain.PermJiraWrite} {
		if admin.Has(p) {
			t.Errorf("platform admin unexpectedly has %s", p)
		}
	}
}

func TestDeployPermissionSeparateFromImplementation(t *testing.T) {
	dev := &Principal{UserID: "u1", TenantID: "t1", Roles: []domain.Role{domain.RoleDeveloper}}
	if !dev.Has(domain.PermExecImplement) {
		t.Fatal("developer should implement")
	}
	if dev.Has(domain.PermExecDeploy) || dev.Has(domain.PermDeployProduction) {
		t.Fatal("developer must not deploy (DEPLOY-003)")
	}
}

func TestSameTenantHidesOtherTenants(t *testing.T) {
	p := &Principal{UserID: "u1", TenantID: "t1"}
	if err := SameTenant(p, "t2"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant access must look like not-found, got %v", err)
	}
	if err := SameTenant(p, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := SameTenant(&Principal{UserID: "u"}, ""); err == nil {
		t.Fatal("empty tenant must never match")
	}
}

func TestCanRespond(t *testing.T) {
	it := &domain.Interaction{TenantID: "t1"}
	owner := &Principal{UserID: "owner", TenantID: "t1", Roles: []domain.Role{domain.RoleDeveloper}}
	other := &Principal{UserID: "other", TenantID: "t1", Roles: []domain.Role{domain.RoleDeveloper}}
	admin := &Principal{UserID: "admin", TenantID: "t1", Roles: []domain.Role{domain.RolePlatformAdmin}}
	foreign := &Principal{UserID: "owner", TenantID: "t2", Roles: []domain.Role{domain.RolePlatformAdmin}}

	if err := CanRespond(owner, it, "owner", false); err != nil {
		t.Errorf("initiator should respond: %v", err)
	}
	if err := CanRespond(other, it, "owner", false); err == nil {
		t.Error("unrelated user must not respond")
	}
	if err := CanRespond(admin, it, "owner", false); err != nil {
		t.Errorf("admin with respond:any should respond: %v", err)
	}
	if err := CanRespond(foreign, it, "owner", false); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant must be not found, got %v", err)
	}

	roleReq := &domain.Interaction{TenantID: "t1", RequestedRole: domain.RoleArchitect}
	arch := &Principal{UserID: "arch", TenantID: "t1", Roles: []domain.Role{domain.RoleArchitect}}
	if err := CanRespond(arch, roleReq, "owner", false); err != nil {
		t.Errorf("requested role should respond: %v", err)
	}
	if err := CanRespond(owner, roleReq, "owner", false); err == nil {
		t.Error("initiator without requested role must not respond to role-targeted request")
	}

	rel := &Principal{UserID: "rel", TenantID: "t1", Roles: []domain.Role{domain.RoleReleaseEngineer}}
	if err := CanRespond(owner, it, "owner", true); err == nil {
		t.Error("production approval requires approve:production")
	}
	if err := CanRespond(rel, &domain.Interaction{TenantID: "t1", RequestedRole: domain.RoleReleaseEngineer}, "owner", true); err != nil {
		t.Errorf("release engineer should approve production: %v", err)
	}
}
