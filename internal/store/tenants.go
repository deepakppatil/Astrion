package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

func (s *Store) CreateTenant(ctx context.Context, t *domain.Tenant) error {
	if t.ID == "" {
		t.ID = domain.NewID(domain.PrefixTenant)
	}
	if t.Status == "" {
		t.Status = "ACTIVE"
	}
	if t.RetentionDays == 0 {
		t.RetentionDays = 90
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO tenants (id, name, status, quotas, retention_days, permitted_providers, security_policy)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Name, t.Status, jsonb(t.Quotas), t.RetentionDays, nz(t.PermittedProviders), jsonb(t.SecurityPolicy))
	return err
}

func scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var t domain.Tenant
	var quotas, sec []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Status, &quotas, &t.RetentionDays, &t.PermittedProviders, &sec, &t.CreatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(quotas, &t.Quotas)
	t.SecurityPolicy = sec
	return &t, nil
}

func (s *Store) GetTenant(ctx context.Context, id string) (*domain.Tenant, error) {
	t, err := scanTenant(s.Pool.QueryRow(ctx, `SELECT id, name, status, quotas, retention_days, permitted_providers, security_policy, created_at FROM tenants WHERE id=$1`, id))
	return t, notFound(err, "tenant")
}

func (s *Store) UpdateTenantQuotas(ctx context.Context, id string, q domain.TenantQuotas) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tenants SET quotas=$2 WHERE id=$1`, id, jsonb(q))
	return err
}

// UpsertUser creates or updates a user and replaces role assignments.
func (s *Store) UpsertUser(ctx context.Context, u *domain.User) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if u.ID == "" {
			u.ID = domain.NewID(domain.PrefixUser)
		}
		err := tx.QueryRow(ctx, `INSERT INTO users (id, tenant_id, subject, email, display_name) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (tenant_id, subject) DO UPDATE SET email=EXCLUDED.email, display_name=EXCLUDED.display_name
			RETURNING id, created_at`, u.ID, u.TenantID, u.Subject, u.Email, u.DisplayName).Scan(&u.ID, &u.CreatedAt)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_assignments WHERE user_id=$1`, u.ID); err != nil {
			return err
		}
		for _, r := range u.Roles {
			if !r.Valid() {
				return domain.Invalid("roles", "unknown role %q", r)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO role_assignments (tenant_id, user_id, role) VALUES ($1,$2,$3)`, u.TenantID, u.ID, string(r)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) userRoles(ctx context.Context, userID string) ([]domain.Role, error) {
	rows, err := s.Pool.Query(ctx, `SELECT role FROM role_assignments WHERE user_id=$1 ORDER BY role`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []domain.Role
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		roles = append(roles, domain.Role(r))
	}
	return roles, rows.Err()
}

func (s *Store) GetUserBySubject(ctx context.Context, tenantID, subject string) (*domain.User, error) {
	var u domain.User
	err := s.Pool.QueryRow(ctx, `SELECT id, tenant_id, subject, email, display_name, created_at FROM users WHERE tenant_id=$1 AND subject=$2`,
		tenantID, subject).Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt)
	if err != nil {
		return nil, notFound(err, "user")
	}
	u.Roles, err = s.userRoles(ctx, u.ID)
	return &u, err
}

func (s *Store) GetUser(ctx context.Context, tenantID, id string) (*domain.User, error) {
	var u domain.User
	err := s.Pool.QueryRow(ctx, `SELECT id, tenant_id, subject, email, display_name, created_at FROM users WHERE tenant_id=$1 AND id=$2`,
		tenantID, id).Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt)
	if err != nil {
		return nil, notFound(err, "user")
	}
	u.Roles, err = s.userRoles(ctx, u.ID)
	return &u, err
}

func (s *Store) ListUsers(ctx context.Context, tenantID string) ([]domain.User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, tenant_id, subject, email, display_name, created_at FROM users WHERE tenant_id=$1 ORDER BY display_name, id`, tenantID)
	if err != nil {
		return nil, err
	}
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	rows.Close()
	for i := range out {
		if out[i].Roles, err = s.userRoles(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
