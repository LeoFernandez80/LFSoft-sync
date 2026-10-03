package writer

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/mapper"
)

// Safety brake for full-scan delete propagation: a key-format mismatch makes every row look missing.
const (
	maxDeleteMissingRatio = 0.10
	maxDeleteMissingRows  = 10
)

type PGWriter struct {
	pool *pgxpool.Pool
	cfg  *config.EntityConfig
}

func NewPG(pool *pgxpool.Pool, cfg *config.EntityConfig) *PGWriter {
	return &PGWriter{pool: pool, cfg: cfg}
}

func (w *PGWriter) conflictColumn() string {
	if w.cfg.PG.BusinessKey != "" {
		return w.cfg.PG.BusinessKey
	}
	if w.cfg.PG.LegacyCodeColumn != "" {
		return w.cfg.PG.LegacyCodeColumn
	}
	return w.cfg.PG.PK
}

func (w *PGWriter) buildUpsert(cols []string) string {
	conflict := w.conflictColumn()
	schema := w.cfg.PG.Schema
	table := w.cfg.PG.Table

	placeholders := make([]string, len(cols))
	quotedCols := make([]string, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}

	var updates []string
	for i, c := range cols {
		if c == conflict {
			continue
		}
		updates = append(updates, fmt.Sprintf(`"%s" = $%d`, c, i+1))
	}

	q := fmt.Sprintf(
		`INSERT INTO %s."%s" (%s) VALUES (%s) ON CONFLICT ("%s") DO UPDATE SET %s`,
		schema, table,
		strings.Join(quotedCols, ", "),
		strings.Join(placeholders, ", "),
		conflict,
		strings.Join(updates, ", "),
	)
	return q
}

// SyncBatch stages rows via COPY into a temp table, then updates only rows whose values
// differ and inserts only missing keys. Unchanged rows are not touched, so full scans
// neither burn identity values (INSERT ... ON CONFLICT always calls nextval) nor bloat the table.
func (w *PGWriter) SyncBatch(ctx context.Context, rows []*mapper.MappedRow) (written int, err error) {
	if len(rows) == 0 {
		return 0, nil
	}

	key := w.conflictColumn()
	cols := rows[0].PGCols
	target := fmt.Sprintf(`%s."%s"`, w.cfg.PG.Schema, w.cfg.PG.Table)

	quoted := make([]string, len(cols))
	tCols := make([]string, 0, len(cols))
	sCols := make([]string, 0, len(cols))
	sets := make([]string, 0, len(cols))
	hasKey := false
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
		if c == key {
			hasKey = true
			continue
		}
		tCols = append(tCols, fmt.Sprintf(`t."%s"`, c))
		sCols = append(sCols, fmt.Sprintf(`s."%s"`, c))
		sets = append(sets, fmt.Sprintf(`"%s" = s."%s"`, c, c))
	}
	if !hasKey {
		return 0, fmt.Errorf("sync batch: key column %q not in mapped columns", key)
	}
	colList := strings.Join(quoted, ", ")

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err = tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
		return 0, fmt.Errorf("set sync.is_writing: %w", err)
	}

	const stage = "sync_stage"
	if _, err = tx.Exec(ctx, fmt.Sprintf(`CREATE TEMP TABLE %s ON COMMIT DROP AS SELECT %s FROM %s WITH NO DATA`, stage, colList, target)); err != nil {
		return 0, fmt.Errorf("create stage: %w", err)
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{stage}, cols, &mappedRowSource{rows: rows, idx: -1}); err != nil {
		return 0, fmt.Errorf("copy stage: %w", err)
	}

	if len(sets) > 0 {
		tag, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %s t SET %s FROM %s s WHERE t."%s" = s."%s" AND (%s) IS DISTINCT FROM (%s)`,
			target, strings.Join(sets, ", "), stage, key, key,
			strings.Join(tCols, ", "), strings.Join(sCols, ", ")))
		if err != nil {
			return 0, fmt.Errorf("update changed: %w", err)
		}
		written += int(tag.RowsAffected())
	}

	tag, err := tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM %s s WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE t."%s" = s."%s")`,
		target, colList, colList, stage, target, key, key))
	if err != nil {
		return 0, fmt.Errorf("insert new: %w", err)
	}
	written += int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return written, nil
}

func (w *PGWriter) DeleteMissing(ctx context.Context, accessPKs []string) (int, error) {
	if len(accessPKs) == 0 {
		return 0, nil
	}

	col := w.conflictColumn()
	schema := w.cfg.PG.Schema
	table := w.cfg.PG.Table

	isDatePK := strings.ToLower(w.cfg.Access.PKType) == "date" || strings.ToLower(w.cfg.Access.PKType) == "datetime"

	params := make([]string, len(accessPKs))
	args := make([]interface{}, len(accessPKs))
	for i, pk := range accessPKs {
		params[i] = fmt.Sprintf("$%d", i+1)
		if isDatePK {
			if t, err := mapper.ParsePKTime(pk); err == nil {
				args[i] = t
			} else {
				return 0, fmt.Errorf("delete missing: cannot parse date pk %q", pk)
			}
		} else {
			args[i] = pk
		}
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'")
	if err != nil {
		return 0, err
	}

	colExpr := fmt.Sprintf(`"%s"::text`, col)
	if isDatePK {
		colExpr = fmt.Sprintf(`"%s"`, col)
	}
	where := fmt.Sprintf(`%s NOT IN (%s)`, colExpr, strings.Join(params, ", "))

	var total, toDelete int64
	countQ := fmt.Sprintf(`SELECT COUNT(*), COUNT(*) FILTER (WHERE %s) FROM %s."%s"`, where, schema, table)
	if err := tx.QueryRow(ctx, countQ, args...).Scan(&total, &toDelete); err != nil {
		return 0, fmt.Errorf("delete missing: count: %w", err)
	}
	if toDelete == 0 {
		return 0, nil
	}
	if toDelete > maxDeleteMissingRows && float64(toDelete) > float64(total)*maxDeleteMissingRatio {
		return 0, fmt.Errorf("delete missing aborted: would delete %d of %d rows (limit %.0f%%)",
			toDelete, total, maxDeleteMissingRatio*100)
	}

	q := fmt.Sprintf(`DELETE FROM %s."%s" WHERE %s`, schema, table, where)
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (w *PGWriter) CountPG(ctx context.Context) (int64, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s."%s"`, w.cfg.PG.Schema, w.cfg.PG.Table)
	var c int64
	err := w.pool.QueryRow(ctx, q).Scan(&c)
	return c, err
}

func (w *PGWriter) AllPGKeys(ctx context.Context) (map[string]bool, error) {
	col := w.conflictColumn()
	q := fmt.Sprintf(`SELECT "%s"::text FROM %s."%s"`, col, w.cfg.PG.Schema, w.cfg.PG.Table)
	rows, err := w.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make(map[string]bool)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys[k] = true
	}
	return keys, rows.Err()
}

// UpsertDocument writes a parent row and its children in one transaction.
// It resolves the parent PG PK for children FK via parent_match column.
func (w *PGWriter) UpsertDocument(ctx context.Context, parent *mapper.MappedRow, childConfigs []config.ChildConfig, childrenData map[string][]*mapper.MappedRow) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'")
	if err != nil {
		return fmt.Errorf("set sync.is_writing: %w", err)
	}

	// Upsert parent
	q := w.buildUpsert(parent.PGCols)
	_, err = tx.Exec(ctx, q, parent.PGVals...)
	if err != nil {
		return fmt.Errorf("upsert parent pk=%s: %w", parent.PKVal, err)
	}

	// Resolve parent PG PK for children
	for _, childCfg := range childConfigs {
		childRows := childrenData[childCfg.Entity]
		if len(childRows) == 0 {
			continue
		}

		parentPGPK, err := w.resolveParentPK(ctx, tx, childCfg, parent)
		if err != nil {
			return fmt.Errorf("resolve parent pk for %s: %w", childCfg.Entity, err)
		}

		// Inject parent FK into each child row
		for _, cr := range childRows {
			cr.PGCols = append(cr.PGCols, childCfg.PG.ParentFK)
			cr.PGVals = append(cr.PGVals, parentPGPK)
		}

		// Upsert children
		for _, cr := range childRows {
			cq := buildChildUpsert(w.cfg.PG.Schema, childCfg, cr.PGCols)
			_, err := tx.Exec(ctx, cq, cr.PGVals...)
			if err != nil {
				return fmt.Errorf("upsert child %s pk=%s: %w", childCfg.Entity, cr.PKVal, err)
			}
		}
	}

	return tx.Commit(ctx)
}

func (w *PGWriter) resolveParentPK(ctx context.Context, tx pgx.Tx, childCfg config.ChildConfig, parent *mapper.MappedRow) (int64, error) {
	matchCol := childCfg.PG.ParentMatch
	if matchCol == "" {
		matchCol = w.cfg.PG.PK
	}

	// Find the match value from the parent mapped row
	var matchVal interface{}
	for i, col := range parent.PGCols {
		if col == matchCol {
			matchVal = parent.PGVals[i]
			break
		}
	}
	if matchVal == nil {
		return 0, fmt.Errorf("parent_match column %q not found in parent row", matchCol)
	}

	var parentID int64
	q := fmt.Sprintf(`SELECT "%s" FROM %s."%s" WHERE "%s" = $1`,
		w.cfg.PG.PK, w.cfg.PG.Schema, w.cfg.PG.Table, matchCol)
	if err := tx.QueryRow(ctx, q, matchVal).Scan(&parentID); err != nil {
		return 0, fmt.Errorf("lookup parent by %s=%v: %w", matchCol, matchVal, err)
	}
	return parentID, nil
}

func buildChildUpsert(schema string, childCfg config.ChildConfig, cols []string) string {
	conflict := childCfg.PG.PK
	table := childCfg.PG.Table

	placeholders := make([]string, len(cols))
	quotedCols := make([]string, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}

	var updates []string
	for i, c := range cols {
		if c == conflict {
			continue
		}
		updates = append(updates, fmt.Sprintf(`"%s" = $%d`, c, i+1))
	}

	return fmt.Sprintf(
		`INSERT INTO %s."%s" (%s) VALUES (%s) ON CONFLICT ("%s") DO UPDATE SET %s`,
		schema, table,
		strings.Join(quotedCols, ", "),
		strings.Join(placeholders, ", "),
		conflict,
		strings.Join(updates, ", "),
	)
}

// BulkInsert uses COPY for fast initial load.
func (w *PGWriter) BulkInsert(ctx context.Context, rows []*mapper.MappedRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'")
	if err != nil {
		return 0, err
	}

	cols := rows[0].PGCols
	quotedCols := make([]string, len(cols))
	for i, c := range cols {
		quotedCols[i] = fmt.Sprintf(`"%s"`, c)
	}

	tableName := pgx.Identifier{w.cfg.PG.Schema, w.cfg.PG.Table}
	copyCount, err := tx.CopyFrom(
		ctx,
		tableName,
		cols,
		&mappedRowSource{rows: rows, idx: -1},
	)
	if err != nil {
		return 0, fmt.Errorf("copy: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(copyCount), nil
}

type mappedRowSource struct {
	rows []*mapper.MappedRow
	idx  int
}

func (s *mappedRowSource) Next() bool {
	s.idx++
	return s.idx < len(s.rows)
}

func (s *mappedRowSource) Values() ([]interface{}, error) {
	return s.rows[s.idx].PGVals, nil
}

func (s *mappedRowSource) Err() error { return nil }
