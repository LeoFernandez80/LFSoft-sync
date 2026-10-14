package writer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

	// failed remembers rows the row-by-row fallback rejected (key → values + error). An unchanged
	// failing row is left out of the next batch, so a few thousand rejects (e.g. an FK to a row PG
	// lacks) don't force a slow row-by-row pass every cycle. Used only by the entity's own worker.
	failed map[string]failedRow
}

type failedRow struct {
	fingerprint string
	at          time.Time
	msg         string
}

func NewPG(pool *pgxpool.Pool, cfg *config.EntityConfig) *PGWriter {
	return &PGWriter{pool: pool, cfg: cfg, failed: make(map[string]failedRow)}
}

func rowFingerprint(r *mapper.MappedRow) string {
	return fmt.Sprintf("%v", r.PGVals)
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
	keyIdx := -1
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
		if c == key {
			keyIdx = i
			continue
		}
		tCols = append(tCols, fmt.Sprintf(`t."%s"`, c))
		sCols = append(sCols, fmt.Sprintf(`s."%s"`, c))
		sets = append(sets, fmt.Sprintf(`"%s" = s."%s"`, c, c))
	}
	if keyIdx < 0 {
		return 0, fmt.Errorf("sync batch: key column %q not in mapped columns", key)
	}
	colList := strings.Join(quoted, ", ")

	// Leave out rows that failed before with the same values; they would fail again.
	stillFailed := make(map[string]failedRow)
	var cachedSample []string
	fingerprints := make(map[string]string, len(rows))
	kept := make([]*mapper.MappedRow, 0, len(rows))
	for _, r := range rows {
		k := fmt.Sprintf("%v", r.PGVals[keyIdx])
		fp := rowFingerprint(r)
		if f, ok := w.failed[k]; ok && f.fingerprint == fp && time.Since(f.at) < failedRetryAfter {
			stillFailed[k] = f
			if len(cachedSample) < rowErrorSample {
				cachedSample = append(cachedSample, f.msg)
			}
			continue
		}
		fingerprints[k] = fp
		kept = append(kept, r)
	}
	rowErrsFor := func(newErrs []string, newCount int) *RowErrors {
		count := newCount + len(stillFailed)
		if count == 0 {
			return nil
		}
		sample := append(newErrs, cachedSample...)
		if len(sample) > rowErrorSample {
			sample = sample[:rowErrorSample]
		}
		return &RowErrors{Count: count, Sample: sample}
	}
	if len(kept) == 0 {
		w.failed = stillFailed
		if re := rowErrsFor(nil, 0); re != nil {
			return 0, re
		}
		return 0, nil
	}
	rows = kept

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

	// apply writes the staged rows; keyFilter restricts it to one key (row-by-row fallback).
	apply := func(keyFilter string, args ...interface{}) (int, error) {
		n := 0
		if len(sets) > 0 {
			tag, err := tx.Exec(ctx, fmt.Sprintf(
				`UPDATE %s t SET %s FROM %s s WHERE t."%s" = s."%s" AND (%s) IS DISTINCT FROM (%s)%s`,
				target, strings.Join(sets, ", "), stage, key, key,
				strings.Join(tCols, ", "), strings.Join(sCols, ", "), keyFilter), args...)
			if err != nil {
				return 0, fmt.Errorf("update changed: %w", err)
			}
			n += int(tag.RowsAffected())
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (%s) SELECT %s FROM %s s WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE t."%s" = s."%s")%s`,
			target, colList, colList, stage, target, key, key, keyFilter), args...)
		if err != nil {
			return 0, fmt.Errorf("insert new: %w", err)
		}
		return n + int(tag.RowsAffected()), nil
	}

	if _, err = tx.Exec(ctx, "SAVEPOINT sync_batch"); err != nil {
		return 0, fmt.Errorf("savepoint: %w", err)
	}
	written, batchErr := apply("")
	if batchErr != nil {
		// One bad row (e.g. an FK to a row PG doesn't have) fails the whole set-based write.
		// Retry only the pending rows one by one so the valid ones still land.
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sync_batch"); err != nil {
			return 0, fmt.Errorf("rollback savepoint: %w (batch: %v)", err, batchErr)
		}
		var failures map[string]string
		written, failures, err = w.applyRowByRow(ctx, tx, apply, stage, target, key, tCols, sCols)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit: %w", err)
		}
		var newSample []string
		for k, msg := range failures {
			stillFailed[k] = failedRow{fingerprint: fingerprints[k], at: time.Now(), msg: msg}
			if len(newSample) < rowErrorSample {
				newSample = append(newSample, msg)
			}
		}
		w.failed = stillFailed
		if re := rowErrsFor(newSample, len(failures)); re != nil {
			return written, re
		}
		return written, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	w.failed = stillFailed
	if re := rowErrsFor(nil, 0); re != nil {
		return written, re
	}
	return written, nil
}

// RowErrors reports rows skipped by SyncBatch's row-by-row fallback; the other rows were written.
type RowErrors struct {
	Count  int
	Sample []string // "key: error" for the first few failures
}

func (e *RowErrors) Error() string {
	return fmt.Sprintf("%d rows skipped: %s", e.Count, strings.Join(e.Sample, "; "))
}

const rowErrorSample = 10

// failedRetryAfter retries remembered failures, e.g. once the missing FK row was added to PG.
const failedRetryAfter = 10 * time.Minute

func (w *PGWriter) applyRowByRow(
	ctx context.Context, tx pgx.Tx,
	apply func(string, ...interface{}) (int, error),
	stage, target, key string, tCols, sCols []string,
) (int, map[string]string, error) {
	changed := ""
	if len(tCols) > 0 {
		changed = fmt.Sprintf(` OR (%s) IS DISTINCT FROM (%s)`, strings.Join(tCols, ", "), strings.Join(sCols, ", "))
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT s."%s"::text FROM %s s LEFT JOIN %s t ON t."%s" = s."%s" WHERE t."%s" IS NULL%s`,
		key, stage, target, key, key, key, changed))
	if err != nil {
		return 0, nil, fmt.Errorf("pending keys: %w", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("pending keys: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("pending keys: %w", err)
	}

	written := 0
	failures := make(map[string]string)
	filter := fmt.Sprintf(` AND s."%s"::text = $1`, key)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, "SAVEPOINT sync_row"); err != nil {
			return 0, nil, fmt.Errorf("savepoint row: %w", err)
		}
		n, err := apply(filter, k)
		if err != nil {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sync_row"); rbErr != nil {
				return 0, nil, fmt.Errorf("rollback row %s: %w", k, rbErr)
			}
			failures[k] = fmt.Sprintf("%s=%s: %v", key, k, err)
			continue
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT sync_row"); err != nil {
			return 0, nil, fmt.Errorf("release row: %w", err)
		}
		written += n
	}
	return written, failures, nil
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

// UpsertDocument writes a root row and its whole child tree in one transaction. Rows are
// matched by their key (business key for the root, ConflictColumns for children): an existing
// row is updated only when a value differs, a new one is inserted. Nothing is written for an
// unchanged document, and no identity value is burned.
func (w *PGWriter) UpsertDocument(ctx context.Context, doc *mapper.DocRow) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err = tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
		return fmt.Errorf("set sync.is_writing: %w", err)
	}

	target := fmt.Sprintf(`%s."%s"`, w.cfg.PG.Schema, w.cfg.PG.Table)
	id, err := upsertRow(ctx, tx, target, w.cfg.PG.PK, []string{w.conflictColumn()}, doc.Row.PGCols, doc.Row.PGVals)
	if err != nil {
		return fmt.Errorf("upsert pk=%s: %w", doc.Row.PKVal, err)
	}
	if err := w.writeChildren(ctx, tx, w.cfg.Children, id, doc.Children); err != nil {
		return fmt.Errorf("pk=%s: %w", doc.Row.PKVal, err)
	}
	return tx.Commit(ctx)
}

func (w *PGWriter) writeChildren(ctx context.Context, tx pgx.Tx, cfgs []config.ChildConfig, parentID int64, data [][]*mapper.DocRow) error {
	for i := range cfgs {
		ch := &cfgs[i]
		var rows []*mapper.DocRow
		if i < len(data) {
			rows = data[i]
		}
		target := fmt.Sprintf(`%s."%s"`, w.cfg.PG.Schema, ch.PG.Table)
		keys := ch.ConflictColumns()
		kept := make([]int64, 0, len(rows))
		for _, r := range rows {
			cols := append(append(make([]string, 0, len(r.Row.PGCols)+1), r.Row.PGCols...), ch.PG.ParentFK)
			vals := append(append(make([]interface{}, 0, len(r.Row.PGVals)+1), r.Row.PGVals...), parentID)
			id, err := upsertRow(ctx, tx, target, ch.PG.PK, keys, cols, vals)
			if err != nil {
				return fmt.Errorf("%s key=%s: %w", ch.Entity, r.Row.PKVal, err)
			}
			kept = append(kept, id)
			if err := w.writeChildren(ctx, tx, ch.Children, id, r.Children); err != nil {
				return err
			}
		}
		if ch.PG.DeleteMissing {
			q := fmt.Sprintf(`DELETE FROM %s WHERE "%s" = $1 AND NOT ("%s" = ANY($2))`, target, ch.PG.ParentFK, ch.PG.PK)
			if _, err := tx.Exec(ctx, q, parentID, kept); err != nil {
				return fmt.Errorf("%s delete missing: %w", ch.Entity, err)
			}
		}
	}
	return nil
}

// upsertRow finds the row by its key columns, updates it only if some value differs and
// inserts it otherwise. Returns the row's PK.
func upsertRow(ctx context.Context, tx pgx.Tx, target, pk string, keys, cols []string, vals []interface{}) (int64, error) {
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[c] = i
	}
	where := make([]string, len(keys))
	keyArgs := make([]interface{}, len(keys))
	isKey := make(map[string]bool, len(keys))
	for i, k := range keys {
		j, ok := idx[k]
		if !ok {
			return 0, fmt.Errorf("key column %q not mapped", k)
		}
		if vals[j] == nil {
			return 0, fmt.Errorf("key column %q is NULL", k)
		}
		where[i] = fmt.Sprintf(`"%s" = $%d`, k, i+1)
		keyArgs[i] = vals[j]
		isKey[k] = true
	}

	var id int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT "%s" FROM %s WHERE %s`, pk, target, strings.Join(where, " AND ")), keyArgs...).Scan(&id)
	if err == nil {
		var sets, diffs []string
		args := []interface{}{id}
		for i, c := range cols {
			if isKey[c] {
				continue
			}
			args = append(args, vals[i])
			n := len(args)
			sets = append(sets, fmt.Sprintf(`"%s" = $%d`, c, n))
			diffs = append(diffs, fmt.Sprintf(`"%s" IS DISTINCT FROM $%d`, c, n))
		}
		if len(sets) > 0 {
			q := fmt.Sprintf(`UPDATE %s SET %s WHERE "%s" = $1 AND (%s)`,
				target, strings.Join(sets, ", "), pk, strings.Join(diffs, " OR "))
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				return 0, fmt.Errorf("update: %w", err)
			}
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("find by key: %w", err)
	}

	quoted := make([]string, len(cols))
	ph := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) RETURNING "%s"`,
		target, strings.Join(quoted, ", "), strings.Join(ph, ", "), pk)
	if err := tx.QueryRow(ctx, q, vals...).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	return id, nil
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
