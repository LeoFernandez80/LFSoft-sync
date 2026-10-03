package idmap

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	Entity   string
	LegacyID string
	PGID     int64
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) GetByLegacy(ctx context.Context, entity, legacyID string) (*Entry, error) {
	const q = `SELECT id_map_entity, id_map_legacyId, id_map_pgId
		FROM sync.id_map WHERE id_map_entity = $1 AND id_map_legacyId = $2`
	var e Entry
	err := r.pool.QueryRow(ctx, q, entity, legacyID).Scan(&e.Entity, &e.LegacyID, &e.PGID)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *Repository) GetByPGID(ctx context.Context, entity string, pgID int64) (*Entry, error) {
	const q = `SELECT id_map_entity, id_map_legacyId, id_map_pgId
		FROM sync.id_map WHERE id_map_entity = $1 AND id_map_pgId = $2`
	var e Entry
	err := r.pool.QueryRow(ctx, q, entity, pgID).Scan(&e.Entity, &e.LegacyID, &e.PGID)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *Repository) Upsert(ctx context.Context, e *Entry) error {
	const q = `INSERT INTO sync.id_map (id_map_entity, id_map_legacyId, id_map_pgId)
		VALUES ($1, $2, $3)
		ON CONFLICT (id_map_entity, id_map_legacyId) DO UPDATE SET
			id_map_pgId = EXCLUDED.id_map_pgId`
	_, err := r.pool.Exec(ctx, q, e.Entity, e.LegacyID, e.PGID)
	return err
}

func (r *Repository) Delete(ctx context.Context, entity, legacyID string) error {
	const q = `DELETE FROM sync.id_map WHERE id_map_entity = $1 AND id_map_legacyId = $2`
	_, err := r.pool.Exec(ctx, q, entity, legacyID)
	return err
}
