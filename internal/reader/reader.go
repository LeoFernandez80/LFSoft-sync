package reader

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
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
	return columnsFor(r.cfg.Fields, r.cfg.Access.PK)
}

// columnsFor lists each Access column the fields need once: mapped columns, time sources,
// lookup query arguments, plus the given key columns.
func columnsFor(fields []config.FieldMap, keys ...string) []string {
	seen := make(map[string]bool, len(fields)+len(keys))
	cols := make([]string, 0, len(fields)+len(keys))
	add := func(c string) {
		if c != "" && !seen[c] {
			cols = append(cols, c)
			seen[c] = true
		}
	}
	for _, k := range keys {
		add(k)
	}
	for _, f := range fields {
		add(f.Access)
		add(f.TimeSource)
		if f.Lookup != nil {
			for _, a := range f.Lookup.Args {
				add(a)
			}
		}
	}
	return cols
}

func (r *AccessReader) ReadAll(ctx context.Context) ([]Row, error) {
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s%s", strings.Join(bracketed(cols), ", "), r.cfg.Access.Table, r.where(""))
	return r.query(ctx, cols, q)
}

func (r *AccessReader) ReadSince(ctx context.Context, lastMaxPK string) ([]Row, error) {
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s%s",
		strings.Join(bracketed(cols), ", "), r.cfg.Access.Table, r.where(fmt.Sprintf("%s > %s", r.cfg.Access.PK, lastMaxPK)))
	return r.query(ctx, cols, q)
}

func (r *AccessReader) ReadByPKs(ctx context.Context, pks []string) ([]Row, error) {
	if len(pks) == 0 {
		return nil, nil
	}
	cols := r.accessColumns()
	q := fmt.Sprintf("SELECT %s FROM %s%s",
		strings.Join(bracketed(cols), ", "), r.cfg.Access.Table,
		r.where(fmt.Sprintf("%s IN (%s)", r.cfg.Access.PK, strings.Join(pks, ","))))
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
		q = fmt.Sprintf("SELECT %s FROM %s%s",
			strings.Join(bracketed(cols), ", "), r.cfg.Access.Table,
			r.where(fmt.Sprintf("(%s > %s OR (%s = %s AND %s >= '%s'))", tsCol, jetDate, tsCol, jetDate, timeCol, jetTime)))
	} else {
		jetDateTime := "#" + since.Format("1/2/2006 3:04:05 PM") + "#"
		q = fmt.Sprintf("SELECT %s FROM %s%s",
			strings.Join(bracketed(cols), ", "), r.cfg.Access.Table, r.where(fmt.Sprintf("%s >= %s", tsCol, jetDateTime)))
	}
	return r.query(ctx, cols, q)
}

// where builds the WHERE clause of a root query: cond plus the entity's access.filter.
func (r *AccessReader) where(cond string) string {
	return WhereClause(cond, r.cfg.Access.Filter)
}

// WhereClause joins a condition with an optional filter into " WHERE ..." (or "").
func WhereClause(cond, filter string) string {
	switch {
	case cond != "" && filter != "":
		return " WHERE " + cond + " AND (" + filter + ")"
	case cond != "":
		return " WHERE " + cond
	case filter != "":
		return " WHERE (" + filter + ")"
	}
	return ""
}

// childInChunk bounds the IN list of one child query; Jet handles a few hundred values fine.
const childInChunk = 200

// ReadChildrenIn reads the child rows whose filterCol (parent_fk or root_fk) is one of values.
// Besides the mapped columns it always returns the child's PK, parent_fk and root_fk, which
// the caller needs to attach rows to their parents.
func (r *AccessReader) ReadChildrenIn(ctx context.Context, childCfg *config.ChildConfig, filterCol string, values []interface{}) ([]Row, error) {
	cols := columnsFor(childCfg.Fields, childCfg.Access.PK, childCfg.Access.ParentFK, childCfg.Access.RootFK)
	var result []Row
	for start := 0; start < len(values); start += childInChunk {
		end := start + childInChunk
		if end > len(values) {
			end = len(values)
		}
		list := make([]string, 0, end-start)
		for _, v := range values[start:end] {
			list = append(list, jetLiteral(v))
		}
		q := fmt.Sprintf("SELECT %s FROM [%s] WHERE %s IN (%s)",
			strings.Join(bracketed(cols), ", "), childCfg.Access.Table,
			bracket(filterCol), strings.Join(list, ", "))
		rows, err := r.query(ctx, cols, q)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", childCfg.Access.Table, err)
		}
		result = append(result, rows...)
	}
	return result, nil
}

// Child tables and columns are bracketed: legacy names carry spaces and accents
// ([Horarios Atencion de Contactos], [FechaCumpleaños]).
func bracket(c string) string {
	if strings.HasPrefix(c, "[") {
		return c
	}
	return "[" + c + "]"
}

func bracketed(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = bracket(c)
	}
	return out
}

// jetLiteral formats a key value read from Access back into a Jet SQL literal.
func jetLiteral(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(val, "'", "''") + "'"
	case []byte:
		return "'" + strings.ReplaceAll(string(val), "'", "''") + "'"
	case time.Time:
		return "#" + val.Format("1/2/2006 3:04:05 PM") + "#"
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(val), 'f', -1, 32)
	case bool:
		if val {
			return "-1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", val)
	}
}

// query reads the whole result inside one job on the Access worker thread. Going through
// Conn.Query would hop to that thread twice per row (Next + Scan), which dominated full scans.
func (r *AccessReader) query(ctx context.Context, cols []string, q string) ([]Row, error) {
	var result []Row
	err := r.conn.WithLock(func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()

		dest := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			row := make(Row, len(cols))
			for i, col := range cols {
				row[col] = dest[i]
			}
			result = append(result, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
