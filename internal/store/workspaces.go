package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

const workspaceCols = `id, tenant_id, name, description, type, lifecycle_mode, data_classification,
	default_agent_profile_id, default_framework_id, default_provider, permitted_agent_profiles,
	permitted_providers, created_by, created_at, updated_at`

func scanWorkspace(row pgx.Row) (*domain.Workspace, error) {
	var w domain.Workspace
	var typ, mode, class string
	err := row.Scan(&w.ID, &w.TenantID, &w.Name, &w.Description, &typ, &mode, &class,
		&w.DefaultAgentProfileID, &w.DefaultFrameworkID, &w.DefaultProvider, &w.PermittedAgentProfiles,
		&w.PermittedProviders, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt)
	w.Type, w.LifecycleMode, w.DataClassification = domain.WorkspaceType(typ), domain.WorkspaceLifecycle(mode), domain.DataClassification(class)
	return &w, err
}

func (s *Store) CreateWorkspace(ctx context.Context, w *domain.Workspace) error {
	if w.ID == "" {
		w.ID = domain.NewID(domain.PrefixWorkspace)
	}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO workspaces (id, tenant_id, name, description, type, lifecycle_mode, data_classification,
			default_agent_profile_id, default_framework_id, default_provider, permitted_agent_profiles, permitted_providers, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING created_at, updated_at`,
			w.ID, w.TenantID, w.Name, w.Description, string(w.Type), string(w.LifecycleMode), string(w.DataClassification),
			w.DefaultAgentProfileID, w.DefaultFrameworkID, w.DefaultProvider, nz(w.PermittedAgentProfiles), nz(w.PermittedProviders), w.CreatedBy,
		).Scan(&w.CreatedAt, &w.UpdatedAt)
		if err != nil {
			return err
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: w.TenantID, Actor: w.CreatedBy, ActorType: "user", Action: "workspace.create",
			TargetType: "workspace", TargetID: w.ID, Details: AuditDetails("name", w.Name, "classification", w.DataClassification)})
	})
	if isUniqueViolation(err) {
		return domain.Errorf(domain.ErrConflict, "workspace %q already exists", w.Name)
	}
	return err
}

func (s *Store) UpdateWorkspace(ctx context.Context, actor string, w *domain.Workspace) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE workspaces SET name=$3, description=$4, type=$5, lifecycle_mode=$6, data_classification=$7,
			default_agent_profile_id=$8, default_framework_id=$9, default_provider=$10, permitted_agent_profiles=$11,
			permitted_providers=$12, updated_at=now() WHERE tenant_id=$1 AND id=$2`,
			w.TenantID, w.ID, w.Name, w.Description, string(w.Type), string(w.LifecycleMode), string(w.DataClassification),
			w.DefaultAgentProfileID, w.DefaultFrameworkID, w.DefaultProvider, nz(w.PermittedAgentProfiles), nz(w.PermittedProviders))
		if err != nil {
			if isUniqueViolation(err) {
				return domain.Errorf(domain.ErrConflict, "workspace name already in use")
			}
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.ErrNotFound, "workspace")
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: w.TenantID, Actor: actor, ActorType: "user", Action: "workspace.update",
			TargetType: "workspace", TargetID: w.ID})
	})
}

func (s *Store) GetWorkspace(ctx context.Context, tenantID, id string) (*domain.Workspace, error) {
	w, err := scanWorkspace(s.Pool.QueryRow(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if err != nil {
		return nil, notFound(err, "workspace")
	}
	w.Repositories, err = s.ListRepositories(ctx, tenantID, id)
	return w, err
}

func (s *Store) ListWorkspaces(ctx context.Context, tenantID string) ([]domain.Workspace, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id=$1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, err
	}
	var out []domain.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *w)
	}
	rows.Close()
	for i := range out {
		if out[i].Repositories, err = s.ListRepositories(ctx, tenantID, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

const repoCols = `id, tenant_id, workspace_id, provider, url, namespace, default_branch, credential_ref, checkout_policy,
	permitted_operations, pinned_revision, last_validated_at, validation_error, created_at`

func scanRepo(row pgx.Row) (*domain.Repository, error) {
	var r domain.Repository
	var prov string
	err := row.Scan(&r.ID, &r.TenantID, &r.WorkspaceID, &prov, &r.URL, &r.Namespace, &r.DefaultBranch, &r.CredentialRef,
		&r.CheckoutPolicy, &r.PermittedOperations, &r.PinnedRevision, &r.LastValidatedAt, &r.ValidationError, &r.CreatedAt)
	r.Provider = domain.RepoProvider(prov)
	return &r, err
}

func (s *Store) ListRepositories(ctx context.Context, tenantID, workspaceID string) ([]domain.Repository, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+repoCols+` FROM repositories WHERE tenant_id=$1 AND workspace_id=$2 ORDER BY created_at`, tenantID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Repository{}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) AddRepository(ctx context.Context, actor string, r *domain.Repository) error {
	if r.ID == "" {
		r.ID = domain.NewID(domain.PrefixRepository)
	}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE tenant_id=$1 AND id=$2)`, r.TenantID, r.WorkspaceID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return domain.Errorf(domain.ErrNotFound, "workspace")
		}
		if err := tx.QueryRow(ctx, `INSERT INTO repositories (id, tenant_id, workspace_id, provider, url, namespace, default_branch,
			credential_ref, checkout_policy, permitted_operations, pinned_revision) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING created_at`,
			r.ID, r.TenantID, r.WorkspaceID, string(r.Provider), r.URL, r.Namespace, r.DefaultBranch, r.CredentialRef,
			r.CheckoutPolicy, nz(r.PermittedOperations), r.PinnedRevision).Scan(&r.CreatedAt); err != nil {
			return err
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: r.TenantID, Actor: actor, ActorType: "user", Action: "repository.add",
			TargetType: "workspace", TargetID: r.WorkspaceID, Details: AuditDetails("repositoryId", r.ID, "url", r.URL)})
	})
	if isUniqueViolation(err) {
		return domain.Errorf(domain.ErrConflict, "repository already in workspace")
	}
	return err
}

func (s *Store) RemoveRepository(ctx context.Context, actor, tenantID, workspaceID, repoID string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM repositories WHERE tenant_id=$1 AND workspace_id=$2 AND id=$3`, tenantID, workspaceID, repoID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.ErrNotFound, "repository")
		}
		return Audit(ctx, tx, domain.AuditEvent{TenantID: tenantID, Actor: actor, ActorType: "user", Action: "repository.remove",
			TargetType: "workspace", TargetID: workspaceID, Details: AuditDetails("repositoryId", repoID)})
	})
}

func (s *Store) MarkRepositoryValidated(ctx context.Context, tenantID, repoID string, validationErr string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE repositories SET last_validated_at=$3, validation_error=$4 WHERE tenant_id=$1 AND id=$2`,
		tenantID, repoID, time.Now().UTC(), validationErr)
	return err
}
