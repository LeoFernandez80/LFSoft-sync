package writer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
)

type AccessWriter struct {
	conn *accessdb.Conn
	cfg  *config.EntityConfig
}

func NewAccess(conn *accessdb.Conn, cfg *config.EntityConfig) *AccessWriter {
	return &AccessWriter{conn: conn, cfg: cfg}
}

func (w *AccessWriter) formatPKWhere(accessPK string) string {
	if mapper.NeedsNumericPK(w.cfg) {
		return fmt.Sprintf("%s = %s", w.cfg.Access.PK, accessPK)
	}
	if mapper.NeedsDatePK(w.cfg) {
		if t, err := mapper.ParsePKTime(accessPK); err == nil {
			return fmt.Sprintf("%s = %s", w.cfg.Access.PK, mapper.FormatJetDate(t.In(time.Local)))
		}
	}
	return fmt.Sprintf("%s = '%s'", w.cfg.Access.PK, strings.ReplaceAll(accessPK, "'", "''"))
}

func (w *AccessWriter) Insert(ctx context.Context, row *mapper.AccessMappedRow) error {
	values := make([]string, len(row.AccessCols))
	for i, v := range row.AccessVals {
		values[i] = mapper.FormatAccessValue(v)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		w.cfg.Access.Table,
		strings.Join(row.AccessCols, ", "),
		strings.Join(values, ", "),
	)
	_, err := w.conn.Exec(ctx, q)
	return err
}

// Update returns the number of Access rows matched by the PK.
func (w *AccessWriter) Update(ctx context.Context, row *mapper.AccessMappedRow) (int64, error) {
	if row.AccessPK == "" {
		return 0, fmt.Errorf("no PK value for update")
	}

	var setClauses []string
	for i, col := range row.AccessCols {
		if col == w.cfg.Access.PK {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = %s", col, mapper.FormatAccessValue(row.AccessVals[i])))
	}
	if len(setClauses) == 0 {
		return 0, nil
	}

	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		w.cfg.Access.Table,
		strings.Join(setClauses, ", "),
		w.formatPKWhere(row.AccessPK),
	)
	res, err := w.conn.Exec(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (w *AccessWriter) Delete(ctx context.Context, accessPK string) (int64, error) {
	if accessPK == "" {
		return 0, fmt.Errorf("no PK value for delete")
	}
	q := fmt.Sprintf("DELETE FROM %s WHERE %s",
		w.cfg.Access.Table, w.formatPKWhere(accessPK))
	res, err := w.conn.Exec(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (w *AccessWriter) Exists(ctx context.Context, accessPK string) (bool, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s",
		w.cfg.Access.Table, w.formatPKWhere(accessPK))
	var c int64
	if err := reader.QueryOne(ctx, w.conn, q, &c); err != nil {
		return false, err
	}
	return c > 0, nil
}

// MaxValue returns MAX(col) of an Access table (0 when empty), optionally filtered by where.
func (w *AccessWriter) MaxValue(ctx context.Context, table, col, where string) (int64, error) {
	q := fmt.Sprintf("SELECT MAX(%s) FROM %s", bracket(col), bracket(table))
	if where != "" {
		q += " WHERE " + where
	}
	var v interface{}
	if err := reader.QueryOne(ctx, w.conn, q, &v); err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return mapper.ToInt64(v)
}

// KeyWhere builds "[a] = literal AND ..." for the given column/value pairs.
func KeyWhere(cols []string, vals []interface{}) string {
	keys := make([]keyCol, len(cols))
	for i := range cols {
		keys[i] = keyCol{cols[i], vals[i]}
	}
	return keyWhere(keys)
}
