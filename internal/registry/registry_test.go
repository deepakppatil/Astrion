package registry

import (
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
)

func defaults(t *testing.T) []Command {
	t.Helper()
	bundles, err := DefaultBundles()
	if err != nil {
		t.Fatal(err)
	}
	var all []Command
	for _, b := range bundles {
		all = append(all, b.Commands...)
	}
	return all
}

func find(cmds []Command, canonical string) *Command {
	for i := range cmds {
		if cmds[i].Canonical == canonical {
			return &cmds[i]
		}
	}
	return nil
}

func TestDefaultRegistryContainsSRSCommandsWithExactCasing(t *testing.T) {
	cmds := defaults(t)
	want := map[string]domain.Phase{
		"/shape-spec": domain.PhasePlan, "/write-spec": domain.PhasePlan, "/create-task": domain.PhasePlan,
		"/Plan-migrate": domain.PhasePlan, "/Create-Jira": domain.PhasePlan,
		"/build-context": domain.PhaseDesign, "/build-architecture": domain.PhaseDesign,
		"/implement-task": domain.PhaseImplement, "/orchestrate-task": domain.PhaseImplement, "/implement-jira": domain.PhaseImplement,
	}
	for canonical, phase := range want {
		c := find(cmds, canonical)
		if c == nil {
			t.Errorf("missing command %s", canonical)
			continue
		}
		if c.Phase != phase {
			t.Errorf("%s phase = %s, want %s", canonical, c.Phase, phase)
		}
		if c.Status != StatusApproved {
			t.Errorf("%s should be approved", canonical)
		}
	}
	// Every phase must have at least one command.
	for _, p := range domain.AllPhases {
		if len(Visible(cmds, Filter{Phase: p})) == 0 {
			t.Errorf("no command registered for phase %s", p)
		}
	}
}

func TestSideEffectsImplyPermissions(t *testing.T) {
	cmds := defaults(t)
	jira := find(cmds, "/Create-Jira")
	ba := &authz.Principal{UserID: "u", TenantID: "t", Roles: []domain.Role{domain.RoleBusinessAnalyst}}
	viewer := &authz.Principal{UserID: "v", TenantID: "t", Roles: []domain.Role{domain.RoleViewer}}
	if err := Authorize(ba, jira); err != nil {
		t.Fatalf("BA should create Jira issues: %v", err)
	}
	if err := Authorize(viewer, jira); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer must be forbidden, got %v", err)
	}
	if !jira.Approval.BeforeExecution {
		t.Fatal("/Create-Jira must require approval (PLAN-010)")
	}
	deploy := find(cmds, "/deploy")
	dev := &authz.Principal{UserID: "d", TenantID: "t", Roles: []domain.Role{domain.RoleDeveloper}}
	if err := Authorize(dev, deploy); err == nil {
		t.Fatal("developer must not deploy to production")
	}
	if !deploy.RequiresProductionApproval() {
		t.Fatal("/deploy must require production approval")
	}
}

func TestProductionDeployWithoutApprovalRejected(t *testing.T) {
	c := Command{Canonical: "/yolo-deploy", FrameworkID: "f", Phase: domain.PhaseDeploy,
		SideEffects: []SideEffect{EffectDeployProd}}.WithDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error for unapproved production deployment")
	}
}

func TestVisibleFiltersByPermissionAndAgent(t *testing.T) {
	cmds := defaults(t)
	ba := &authz.Principal{UserID: "u", TenantID: "t", Roles: []domain.Role{domain.RoleBusinessAnalyst}}
	vis := Visible(cmds, Filter{Principal: ba})
	for _, c := range vis {
		if c.Phase != domain.PhasePlan {
			t.Errorf("BA should only see PLAN commands, saw %s (%s)", c.Canonical, c.Phase)
		}
	}
	if len(Visible(cmds, Filter{AgentProvider: "some-unknown-agent"})) != 0 {
		t.Error("commands restricted to known agents should be hidden for unknown agents")
	}
}

func TestValidateInputs(t *testing.T) {
	cmds := defaults(t)
	jira := find(cmds, "/implement-jira")
	if _, err := jira.ValidateInputs(json.RawMessage(`{"jiraKey":"PROJ-12"}`)); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		`{}`,                                     // missing required
		`{"jiraKey":"not a key"}`,                // pattern
		`{"jiraKey":"PROJ-1","surprise":true}`,   // unknown field
		`{"jiraKey":"PROJ-1","instructions":42}`, // reserved field type
		`[1,2]`,                                  // not an object
	}
	for _, b := range bad {
		if _, err := jira.ValidateInputs(json.RawMessage(b)); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("inputs %s: expected ErrInvalid, got %v", b, err)
		}
	}
	arch := find(cmds, "/build-architecture")
	in, err := arch.ValidateInputs(json.RawMessage(`{"instructions":"focus on APIs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if in["style"] != "markdown" {
		t.Errorf("default not applied: %v", in)
	}
	if _, err := arch.ValidateInputs(json.RawMessage(`{"style":"pdf"}`)); err == nil {
		t.Error("enum violation not detected")
	}
}

func TestDiscoveredCommandsArePendingReview(t *testing.T) {
	src := fstest.MapFS{
		".claude/commands/shape-spec.md":   {Data: []byte("# shape")},
		".claude/commands/Plan-migrate.md": {Data: []byte("# migrate")},
		".claude/commands/README.txt":      {Data: []byte("ignored")},
	}
	cmds, err := MarkdownDirDiscoverer{Dir: ".claude/commands"}.Discover("fw_x", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands", len(cmds))
	}
	if find(cmds, "/Plan-migrate") == nil {
		t.Error("casing must be preserved")
	}
	ba := &authz.Principal{UserID: "u", TenantID: "t", Roles: []domain.Role{domain.RoleBusinessAnalyst}}
	for _, c := range cmds {
		if c.Status != StatusPendingReview || c.Source != SourceDiscovered {
			t.Errorf("%s: status=%s source=%s", c.Canonical, c.Status, c.Source)
		}
		if err := Authorize(ba, &c); err == nil {
			t.Errorf("%s: unreviewed command must not be executable", c.Canonical)
		}
	}
	if len(Visible(cmds, Filter{Principal: ba})) != 0 {
		t.Error("pending commands must be hidden from non-admins")
	}

	manifest := fstest.MapFS{"astrion-commands.json": {Data: []byte(`[{"canonical":"/custom-cmd","displayName":"Custom","phase":"TEST","sideEffects":["NONE"]}]`)}}
	mc, err := ManifestDiscoverer{}.Discover("fw_x", manifest)
	if err != nil || len(mc) != 1 || mc[0].Status != StatusPendingReview {
		t.Fatalf("manifest discovery: %v %+v", err, mc)
	}
}
