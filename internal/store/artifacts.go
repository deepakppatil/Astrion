package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/deepakppatil/astrion/internal/domain"
)

const artifactCols = `id, tenant_id, job_id, execution_id, type, name, object_key, content_type, size_bytes, sha256,
	classification, source_step, repository_id, revision, retain_until, created_at`

func scanArtifact(row pgx.Row) (*domain.Artifact, error) {
	var a domain.Artifact
	var class string
	err := row.Scan(&a.ID, &a.TenantID, &a.JobID, &a.ExecutionID, &a.Type, &a.Name, &a.ObjectKey, &a.ContentType, &a.SizeBytes,
		&a.SHA256, &class, &a.SourceStep, &a.RepositoryID, &a.Revision, &a.RetainUntil, &a.CreatedAt)
	a.Classification = domain.ArtifactClass(class)
	return &a, err
}

// CreateArtifact records metadata after the object has been durably stored.
// The user-facing notification is emitted in the same transaction.
func (s *Store) CreateArtifact(ctx context.Context, a *domain.Artifact) error {
	if a.ID == "" {
		a.ID = domain.NewID(domain.PrefixArtifact)
	}
	if a.Classification == "" {
		a.Classification = domain.ArtifactUser
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO artifacts (id, tenant_id, job_id, execution_id, type, name, object_key, content_type,
			size_bytes, sha256, classification, source_step, repository_id, revision, retain_until)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING created_at`,
			a.ID, a.TenantID, a.JobID, a.ExecutionID, a.Type, a.Name, a.ObjectKey, a.ContentType, a.SizeBytes, a.SHA256,
			string(a.Classification), a.SourceStep, a.RepositoryID, a.Revision, a.RetainUntil).Scan(&a.CreatedAt); err != nil {
			return err
		}
		if a.Classification == domain.ArtifactUser {
			if _, err := AppendEvent(ctx, tx, a.TenantID, a.JobID, domain.EventArtifactAvailable, a); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetArtifact(ctx context.Context, tenantID, id string) (*domain.Artifact, error) {
	a, err := scanArtifact(s.Pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	return a, notFound(err, "artifact")
}

func (s *Store) GetArtifactInternal(ctx context.Context, id string) (*domain.Artifact, error) {
	a, err := scanArtifact(s.Pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id=$1`, id))
	return a, notFound(err, "artifact")
}

func (s *Store) ListArtifacts(ctx context.Context, tenantID, jobID string, includeInternal bool) ([]domain.Artifact, error) {
	q := `SELECT ` + artifactCols + ` FROM artifacts WHERE tenant_id=$1 AND job_id=$2`
	if !includeInternal {
		q += ` AND classification='USER'`
	}
	rows, err := s.Pool.Query(ctx, q+` ORDER BY created_at`, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
