package registry

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
)

//go:embed defaults/*.json
var defaultsFS embed.FS

// Bundle is a framework profile with its declared commands.
type Bundle struct {
	Framework domain.FrameworkProfile `json:"framework"`
	Commands  []Command               `json:"commands"`
}

// ParseBundle decodes and validates a bundle. Unknown JSON fields are rejected
// so typos in command metadata are caught at registration time.
func ParseBundle(b []byte) (*Bundle, error) {
	var bundle Bundle
	if err := strictUnmarshal(b, &bundle); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if bundle.Framework.ID == "" || bundle.Framework.Name == "" || bundle.Framework.Revision == "" {
		return nil, domain.Invalid("framework", "id, name and immutable revision are required")
	}
	seen := map[string]bool{}
	for i := range bundle.Commands {
		c := bundle.Commands[i].WithDefaults()
		c.FrameworkID = bundle.Framework.ID
		if len(c.CompatibleAgents) == 0 {
			// Commands inherit the framework's agent compatibility unless narrowed.
			c.CompatibleAgents = append([]string(nil), bundle.Framework.CompatibleAgents...)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("command %s: %w", c.Canonical, err)
		}
		if seen[c.Canonical] {
			return nil, domain.Invalid("commands", "duplicate canonical command %s", c.Canonical)
		}
		seen[c.Canonical] = true
		bundle.Commands[i] = c
	}
	return &bundle, nil
}

// DefaultBundles returns the built-in declared registry.
func DefaultBundles() ([]*Bundle, error) {
	entries, err := defaultsFS.ReadDir("defaults")
	if err != nil {
		return nil, err
	}
	var out []*Bundle
	for _, e := range entries {
		b, err := defaultsFS.ReadFile("defaults/" + e.Name())
		if err != nil {
			return nil, err
		}
		bundle, err := ParseBundle(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, bundle)
	}
	return out, nil
}

// Filter describes the selection context for command visibility (SRS 9: the
// frontend shows only commands available to the selected workspace,
// framework, agent, persona, and permission set).
type Filter struct {
	FrameworkID   string
	AgentProvider string
	Phase         domain.Phase
	Principal     *authz.Principal
	// IncludeUnapproved is only honored for registry managers (admin views).
	IncludeUnapproved bool
}

// Visible returns commands matching the filter, ordered by phase then name.
func Visible(cmds []Command, f Filter) []Command {
	var out []Command
	for _, c := range cmds {
		if f.FrameworkID != "" && c.FrameworkID != f.FrameworkID {
			continue
		}
		if f.Phase != "" && c.Phase != f.Phase {
			continue
		}
		if f.AgentProvider != "" && !c.SupportsAgent(f.AgentProvider) {
			continue
		}
		if c.Status != StatusApproved {
			if !(f.IncludeUnapproved && f.Principal.Has(domain.PermRegistryManage)) {
				continue
			}
		} else if f.Principal != nil && !f.IncludeUnapproved && !f.Principal.HasAll(c.EffectivePermissions()...) {
			continue
		}
		out = append(out, c)
	}
	phaseOrder := map[domain.Phase]int{}
	for i, p := range domain.AllPhases {
		phaseOrder[p] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Phase != out[j].Phase {
			return phaseOrder[out[i].Phase] < phaseOrder[out[j].Phase]
		}
		return out[i].Canonical < out[j].Canonical
	})
	return out
}

// Authorize checks that the principal may execute the command. This is the
// backend enforcement point; Visible is only a convenience for UIs.
func Authorize(p *authz.Principal, c *Command) error {
	if c.Status != StatusApproved {
		return domain.Errorf(domain.ErrForbidden, "command %s is %s and not executable", c.Canonical, c.Status)
	}
	return authz.Require(p, c.EffectivePermissions()...)
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
