package synclog

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	Entity      string
	Direction   string
	StartedAt   time.Time
	FinishedAt  time.Time
	Strategy    string
	Detected    int
	Skipped     int
	Written     int
	Conflicts   int
	Errors      int
	ErrorDetail *string
	DurationMs  int
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Insert(ctx context.Context, e *Entry) error {
	const q = `INSERT INTO sync.sync_log
		(sl_entity, sl_direction, sl_startedAt, sl_finishedAt, sl_strategy,
		 sl_detected, sl_skipped, sl_written, sl_conflicts, sl_errors,
		 sl_errorDetail, sl_durationMs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err := r.pool.Exec(ctx, q,
		e.Entity, e.Direction, e.StartedAt, e.FinishedAt, e.Strategy,
		e.Detected, e.Skipped, e.Written, e.Conflicts, e.Errors,
		e.ErrorDetail, e.DurationMs,
	)
	return err
}
