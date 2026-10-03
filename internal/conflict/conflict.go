package conflict

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type IDConflict struct {
	ID         int64
	Entity     string
	PGID       int64
	OriginalID int64
	NewID      *int64
	Status     string
	DetectedAt time.Time
	ResolvedAt *time.Time
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Insert(ctx context.Context, c *IDConflict) error {
	const q = `INSERT INTO sync.id_conflicts
		(ic_entity, ic_pgId, ic_originalId, ic_newId, ic_status)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := r.pool.Exec(ctx, q, c.Entity, c.PGID, c.OriginalID, c.NewID, c.Status)
	return err
}

func (r *Repository) Resolve(ctx context.Context, id int64, newID int64) error {
	const q = `UPDATE sync.id_conflicts
		SET ic_newId = $2, ic_status = 'resolved', ic_resolvedAt = NOW()
		WHERE ic_id = $1`
	_, err := r.pool.Exec(ctx, q, id, newID)
	return err
}

func (r *Repository) ListPending(ctx context.Context, entity string) ([]IDConflict, error) {
	const q = `SELECT ic_id, ic_entity, ic_pgId, ic_originalId, ic_newId,
		ic_status, ic_detectedAt, ic_resolvedAt
		FROM sync.id_conflicts
		WHERE ic_entity = $1 AND ic_status = 'pending'
		ORDER BY ic_detectedAt DESC`
	rows, err := r.pool.Query(ctx, q, entity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IDConflict
	for rows.Next() {
		var c IDConflict
		if err := rows.Scan(&c.ID, &c.Entity, &c.PGID, &c.OriginalID,
			&c.NewID, &c.Status, &c.DetectedAt, &c.ResolvedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
