package reader

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
)

type Row map[string]interface{}

type AccessReader struct {
	conn *accessdb.Conn
	cfg  *config.EntityConfig
}

func New(conn *accessdb.Conn, cfg *config.EntityConfig) *AccessReader {
	return &AccessReader{conn: conn, cfg: cfg}
}

func (r *AccessReader) accessColumns() []string {
	seen := make(map[string]bool, len(r.cfg.Fields))
	cols := make([]string, 0, len(r.cfg.Fields)+2)
	for _, f := range r.cfg.Fields {
		if !seen[f.Access] {
			cols = append(cols, f.Access)
			seen[f.Access] = true
		}
		if f.TimeSource != "" && !seen[f.TimeSource] {
			cols = append(cols, f.TimeSource)
			seen[f.TimeSource] = true
		}
	}
	return cols
}

func (r *AccessReader) ReadAll(ctx context.Context) ([]Row, error) {
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(cols, ", "), r.cfg.Access.Table)
	return r.query(ctx, cols, q)
}

func (r *AccessReader) ReadSince(ctx context.Context, lastMaxPK string) ([]Row, error) {
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s > %s",
		strings.Join(cols, ", "), r.cfg.Access.Table, r.cfg.Access.PK, lastMaxPK)
	return r.query(ctx, cols, q)
}

func (r *AccessReader) ReadByPKs(ctx context.Context, pks []string) ([]Row, error) {
	if len(pks) == 0 {
		return nil, nil
	}
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)",
		strings.Join(cols, ", "), r.cfg.Access.Table, r.cfg.Access.PK,
		strings.Join(pks, ","))
	return r.query(ctx, cols, q)
}

// ReadSinceTimestamp reads rows modified since the given timestamp.
// For entities with separate date+time columns, combines both for comparison.
func (r *AccessReader) ReadSinceTimestamp(ctx context.Context, since time.Time) ([]Row, error) {
	cols := r.accessColumns()
	tsCol := r.cfg.Detection.TimestampColumn
	timeCol := r.cfg.Detection.TimestampTimeColumn
	jetDate := "#" + since.Format("1/2/2006") + "#"

	var q string
	if timeCol != "" {
		jetTime := since.Format("15:04")
		q = fmt.Sprintf("SELECT %s FROM %s WHERE (%s > %s OR (%s = %s AND %s >= '%s'))",
			strings.Join(cols, ", "), r.cfg.Access.Table,
			tsCol, jetDate, tsCol, jetDate, timeCol, jetTime)
	} else {
		jetDateTime := "#" + since.Format("1/2/2006 3:04:05 PM") + "#"
		q = fmt.Sprintf("SELECT %s FROM %s WHERE %s >= %s",
			strings.Join(cols, ", "), r.cfg.Access.Table, tsCol, jetDateTime)
	}
	return r.query(ctx, cols, q)
}

// ReadChildren reads child rows from Access by parent FK value.
func (r *AccessReader) ReadChildren(ctx context.Context, childCfg *config.ChildConfig, parentValue string) ([]Row, error) {
	cols := make([]string, len(childCfg.Fields))
	for i, f := range childCfg.Fields {
		cols[i] = f.Access
	}

	var q string
	q = fmt.Sprintf("SELECT %s FROM %s WHERE %s = %s",
		strings.Join(cols, ", "), childCfg.Access.Table,
		childCfg.Access.ParentFK, parentValue)
	return r.query(ctx, cols, q)
}

func (r *AccessReader) query(ctx context.Context, cols []string, q string) ([]Row, error) {
	rows, err := r.conn.Query(ctx, q)
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
