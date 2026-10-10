package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/registry"
)

const agentCols = `id, version, tenant_id, name, provider, cli_name, cli_version, runtime_image, auth_method,
	interaction_mode, environment, default_args, resources, execution_policy, enabled, created_at`

func scanAgent(row pgx.Row) (*domain.AgentProfile, error) {
	var a domain.AgentProfile
	var mode string
	var env, res, pol []byte
	if err := row.Scan(&a.ID, &a.Version, &a.TenantID, &a.Name, &a.Provider, &a.CLIName, &a.CLIVersion, &a.RuntimeImage,
		&a.AuthMethod, &mode, &env, &a.DefaultArgs, &res, &pol, &a.Enabled, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.InteractionMode = domain.InteractionMode(mode)
	_ = json.Unmarshal(env, &a.Environment)
	_ = json.Unmarshal(res, &a.Resources)
	a.ExecutionPolicy = pol
	return &a, nil
}

// SaveAgentProfile inserts a new version (profiles are immutable per version
// so jobs can record exactly which configuration they ran with).
func (s *Store) SaveAgentProfile(ctx context.Context, actor string, a *domain.AgentProfile) error {
	if a.ID == "" {
		a.ID = domain.NewID(domain.PrefixAgent)
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM agent_profiles WHERE id=$1 AND tenant_id=$2`, a.ID, a.TenantID).Scan(&a.Version); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO agent_profiles (id, version, tenant_id, name, provider, cli_name, cli_version, runtime_image,
			auth_method, interaction_mode, environment, default_args, resources, execution_policy, enabled)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING created_at`,
			a.ID, a.Version, a.TenantID, a.Name, a.Provider, a.CLIName, a.CLIVersion, a.RuntimeImage, a.AuthMethod,
			string(a.InteractionMode), jsonb(a.Environment), nz(a.DefaultArgs), jsonb(a.Resources), jsonb(a.ExecutionPolicy), a.Enabled,
		).Scan(&a.CreatedAt); err != nil {
			return err
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: a.TenantID, Actor: actor, ActorType: "user", Action: "agent_profile.save",
			TargetType: "agent_profile", TargetID: a.ID, Details: AuditDetails("version", a.Version, "provider", a.Provider, "cliVersion", a.CLIVersion)})
	})
}

// GetAgentProfile returns a specific version, or the latest when version <= 0.
func (s *Store) GetAgentProfile(ctx context.Context, tenantID, id string, version int) (*domain.AgentProfile, error) {
	var row pgx.Row
	if version > 0 {
		row = s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agent_profiles WHERE tenant_id=$1 AND id=$2 AND version=$3`, tenantID, id, version)
	} else {
		row = s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agent_profiles WHERE tenant_id=$1 AND id=$2 ORDER BY version DESC LIMIT 1`, tenantID, id)
	}
	a, err := scanAgent(row)
	return a, notFound(err, "agent profile")
}

func (s *Store) ListAgentProfiles(ctx context.Context, tenantID string) ([]domain.AgentProfile, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (id) `+agentCols+` FROM agent_profiles WHERE tenant_id=$1 ORDER BY id, version DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentProfile{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpsertBundle registers a framework and its commands. A command's version is
// bumped only when its definition changes.
func (s *Store) UpsertBundle(ctx context.Context, actor string, tenantID *string, b *registry.Bundle) error {
	f := b.Framework
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO framework_profiles (id, tenant_id, name, version, source, revision, discovery_strategy,
			compatible_agents, required_tools, config) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, version=EXCLUDED.version, source=EXCLUDED.source,
			revision=EXCLUDED.revision, discovery_strategy=EXCLUDED.discovery_strategy,
			compatible_agents=EXCLUDED.compatible_agents, required_tools=EXCLUDED.required_tools, config=EXCLUDED.config`,
			f.ID, tenantID, f.Name, f.Version, f.Source, f.Revision, f.DiscoveryStrategy, nz(f.CompatibleAgents), nz(f.RequiredTools), jsonb(f.Config)); err != nil {
			return err
		}
		for _, c := range b.Commands {
			if err := upsertCommand(ctx, tx, actor, c); err != nil {
				return err
			}
		}
		return nil
	})
}

func upsertCommand(ctx context.Context, tx pgx.Tx, actor string, c registry.Command) error {
	var latest []byte
	var version int
	err := tx.QueryRow(ctx, `SELECT definition, version FROM command_definitions WHERE id=$1 ORDER BY version DESC LIMIT 1`, c.ID).Scan(&latest, &version)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	c.Version = version + 1
	if err == nil {
		var prev registry.Command
		if json.Unmarshal(latest, &prev) == nil {
			prev.Version = c.Version
			a, _ := json.Marshal(prev)
			b, _ := json.Marshal(c)
			if string(a) == string(b) {
				return nil // unchanged
			}
		}
	}
	def, _ := json.Marshal(c)
	_, err = tx.Exec(ctx, `INSERT INTO command_definitions (id, version, framework_id, canonical, phase, status, source, definition, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.ID, c.Version, c.FrameworkID, c.Canonical, string(c.Phase), string(c.Status), string(c.Source), def, actor)
	return err
}

func (s *Store) SaveCommand(ctx context.Context, actor string, c registry.Command) error {
	return s.InTx(ctx, func(tx pgx.Tx) error { return upsertCommand(ctx, tx, actor, c) })
}

// ListCommands returns the latest version of every command (optionally per framework).
func (s *Store) ListCommands(ctx context.Context, frameworkID string) ([]registry.Command, error) {
	q := `SELECT DISTINCT ON (id) definition, version FROM command_definitions`
	args := []any{}
	if frameworkID != "" {
		q += ` WHERE framework_id=$1`
		args = append(args, frameworkID)
	}
	q += ` ORDER BY id, version DESC`
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registry.Command
	for rows.Next() {
		var def []byte
		var v int
		if err := rows.Scan(&def, &v); err != nil {
			return nil, err
		}
		var c registry.Command
		if err := json.Unmarshal(def, &c); err != nil {
			return nil, err
		}
		c.Version = v
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCommand returns a specific version, or the latest when version <= 0.
func (s *Store) GetCommand(ctx context.Context, id string, version int) (*registry.Command, error) {
	var def []byte
	var v int
	var err error
	if version > 0 {
		err = s.Pool.QueryRow(ctx, `SELECT definition, version FROM command_definitions WHERE id=$1 AND version=$2`, id, version).Scan(&def, &v)
	} else {
		err = s.Pool.QueryRow(ctx, `SELECT definition, version FROM command_definitions WHERE id=$1 ORDER BY version DESC LIMIT 1`, id).Scan(&def, &v)
	}
	if err != nil {
		return nil, notFound(err, "command")
	}
	var c registry.Command
	if err := json.Unmarshal(def, &c); err != nil {
		return nil, err
	}
	c.Version = v
	return &c, nil
}

// FindCommand resolves a canonical command within a framework (latest version).
func (s *Store) FindCommand(ctx context.Context, frameworkID, canonical string) (*registry.Command, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id FROM command_definitions WHERE framework_id=$1 AND canonical=$2 ORDER BY version DESC LIMIT 1`,
		frameworkID, canonical).Scan(&id)
	if err != nil {
		return nil, notFound(err, "command "+canonical)
	}
	return s.GetCommand(ctx, id, 0)
}

func (s *Store) ListFrameworks(ctx context.Context, tenantID string) ([]domain.FrameworkProfile, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, COALESCE(tenant_id,''), name, version, source, revision, discovery_strategy,
		compatible_agents, required_tools, config, created_at FROM framework_profiles WHERE tenant_id IS NULL OR tenant_id=$1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.FrameworkProfile{}
	for rows.Next() {
		var f domain.FrameworkProfile
		var cfg []byte
		if err := rows.Scan(&f.ID, &f.TenantID, &f.Name, &f.Version, &f.Source, &f.Revision, &f.DiscoveryStrategy,
			&f.CompatibleAgents, &f.RequiredTools, &cfg, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Config = cfg
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) GetFramework(ctx context.Context, tenantID, id string) (*domain.FrameworkProfile, error) {
	all, err := s.ListFrameworks(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, domain.Errorf(domain.ErrNotFound, "framework")
}
