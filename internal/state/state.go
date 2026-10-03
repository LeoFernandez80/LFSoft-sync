package state

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type SyncState struct {
	Entity          string
	LastMaxPK       *string
	LastTimestamp    *time.Time
	LastFileModTime *time.Time
	LastChangeID    *int64
	LastHashScan    *time.Time
	LastCount       *int64
	UpdatedAt       time.Time
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Get(ctx context.Context, entity string) (*SyncState, error) {
	const q = `SELECT ss_entity, ss_lastMaxPk, ss_lastTimestamp, ss_lastFileModTime,
		ss_lastChangeId, ss_lastHashScan, ss_lastCount, ss_updatedAt
		FROM sync.sync_state WHERE ss_entity = $1`
	var s SyncState
	err := r.pool.QueryRow(ctx, q, entity).Scan(
		&s.Entity, &s.LastMaxPK, &s.LastTimestamp, &s.LastFileModTime,
		&s.LastChangeID, &s.LastHashScan, &s.LastCount, &s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Repository) Upsert(ctx context.Context, s *SyncState) error {
	const q = `INSERT INTO sync.sync_state
		(ss_entity, ss_lastMaxPk, ss_lastTimestamp, ss_lastFileModTime,
		 ss_lastChangeId, ss_lastHashScan, ss_lastCount, ss_updatedAt)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (ss_entity) DO UPDATE SET
			ss_lastMaxPk = COALESCE(EXCLUDED.ss_lastMaxPk, sync_state.ss_lastMaxPk),
			ss_lastTimestamp = COALESCE(EXCLUDED.ss_lastTimestamp, sync_state.ss_lastTimestamp),
			ss_lastFileModTime = COALESCE(EXCLUDED.ss_lastFileModTime, sync_state.ss_lastFileModTime),
			ss_lastChangeId = COALESCE(EXCLUDED.ss_lastChangeId, sync_state.ss_lastChangeId),
			ss_lastHashScan = COALESCE(EXCLUDED.ss_lastHashScan, sync_state.ss_lastHashScan),
			ss_lastCount = COALESCE(EXCLUDED.ss_lastCount, sync_state.ss_lastCount),
			ss_updatedAt = NOW()`
	_, err := r.pool.Exec(ctx, q,
		s.Entity, s.LastMaxPK, s.LastTimestamp, s.LastFileModTime,
		s.LastChangeID, s.LastHashScan, s.LastCount,
	)
	return err
}
