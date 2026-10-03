package reader

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/sync/internal/config"
)

type PGReader struct {
	pool *pgxpool.Pool
	cfg  *config.EntityConfig
}

func NewPG(pool *pgxpool.Pool, cfg *config.EntityConfig) *PGReader {
	return &PGReader{pool: pool, cfg: cfg}
}

func (r *PGReader) pgColumns() []string {
	cols := make([]string, len(r.cfg.Fields))
	for i, f := range r.cfg.Fields {
		cols[i] = f.PG
	}
	return cols
}

func (r *PGReader) ReadByID(ctx context.Context, id int64) (Row, error) {
	cols := r.pgColumns()
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s."%s" WHERE "%s" = $1`,
		strings.Join(quoted, ", "),
		r.cfg.PG.Schema, r.cfg.PG.Table, r.cfg.PG.PK)

	pgRow := r.pool.QueryRow(ctx, q, id)

	dest := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := pgRow.Scan(ptrs...); err != nil {
		return nil, err
	}

	row := make(Row, len(cols))
	for i, col := range cols {
		row[col] = dest[i]
	}
	return row, nil
}

func (r *PGReader) ReadByIDs(ctx context.Context, ids []int64) ([]Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cols := r.pgColumns()
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s."%s" WHERE "%s" = ANY($1)`,
		strings.Join(quoted, ", "),
		r.cfg.PG.Schema, r.cfg.PG.Table, r.cfg.PG.PK)

	rows, err := r.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Row
	for rows.Next() {
		dest := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(Row, len(cols))
		for i, col := range cols {
			row[col] = dest[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
