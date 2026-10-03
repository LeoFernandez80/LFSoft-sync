package detector

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Change struct {
	ID        int64
	Entity    string
	RowID     int64
	Operation string // "I", "U", "D"
	OldData   []byte // JSON of the deleted row (only for "D")
}

type PGChanges struct {
	pool *pgxpool.Pool
}

func NewPGChanges(pool *pgxpool.Pool) *PGChanges {
	return &PGChanges{pool: pool}
}

func (d *PGChanges) ReadPending(ctx context.Context, entity string, limit int) ([]Change, error) {
	const q = `SELECT ch_id, ch_entity, ch_row_id, ch_operation, ch_old_data
		FROM sync.pg_changes
		WHERE ch_entity = $1 AND NOT ch_processed
		ORDER BY ch_id
		LIMIT $2`
	rows, err := d.pool.Query(ctx, q, entity, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.Entity, &c.RowID, &c.Operation, &c.OldData); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (d *PGChanges) MarkProcessed(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE sync.pg_changes SET ch_processed = TRUE WHERE ch_id = ANY($1)`
	_, err := d.pool.Exec(ctx, q, ids)
	return err
}
