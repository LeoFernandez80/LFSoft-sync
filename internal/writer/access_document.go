package writer

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/lookup"
	"github.com/lfsoft/sync/internal/mapper"
)

// AccessDoc is a PG document mapped back to Access: the root (or child) row and, per child
// collection of the config (same order), its rows. Collections not written to Access are nil.
type AccessDoc struct {
	Row      *mapper.AccessMappedRow
	Children [][]*AccessDoc
}

// WriteDocument writes a root row and its child tree to Access. Access has no reliable
// transactions with this driver, so the write is idempotent instead: every row is updated by
// its key (inserted when missing) and, for collections with delete_missing, rows PG no longer
// has are deleted. If it fails halfway, writing the same document again completes it. It runs
// under WithLock so no other statement interleaves.
//
// Only mapped columns are written: columns that exist only in Access keep their values.
func (w *AccessWriter) WriteDocument(ctx context.Context, doc *AccessDoc) error {
	rootVal, ok := doc.Row.Value(w.cfg.Access.PK)
	if !ok || rootVal == nil {
		return fmt.Errorf("document has no %s", w.cfg.Access.PK)
	}
	return w.conn.WithLock(func(db *sql.DB) error {
		keys := []keyCol{{w.cfg.Access.PK, rootVal}}
		if err := upsertAccessRow(ctx, db, w.cfg.Access.Table, keys, doc.Row); err != nil {
			return fmt.Errorf("%s %v: %w", w.cfg.Access.Table, rootVal, err)
		}
		return writeAccessChildren(ctx, db, w.cfg.Children, rootVal, rootVal, true, doc.Children)
	})
}

// DeleteDocumentChildren deletes in Access the child rows of a root deleted in PG. Collections
// owned by the legacy app (direction access_to_pg) are left alone.
func (w *AccessWriter) DeleteDocumentChildren(ctx context.Context, accessPK string) error {
	rootVal := w.pkValue(accessPK)
	return w.conn.WithLock(func(db *sql.DB) error {
		for i := range w.cfg.Children {
			if err := deleteSubtree(ctx, db, &w.cfg.Children[i], rootVal, rootVal, true, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// pkValue turns the root PK string back into a value Jet compares correctly.
func (w *AccessWriter) pkValue(accessPK string) interface{} {
	if mapper.NeedsNumericPK(w.cfg) {
		var n int64
		if _, err := fmt.Sscanf(accessPK, "%d", &n); err == nil {
			return n
		}
	}
	if mapper.NeedsDatePK(w.cfg) {
		if t, err := mapper.ParsePKTime(accessPK); err == nil {
			return t.In(time.Local)
		}
	}
	return accessPK
}

type keyCol struct {
	col string
	val interface{}
}

// parentKeys are the columns that tie a child row to its parent (and root).
func parentKeys(ch *config.ChildConfig, rootVal, parentVal interface{}, topLevel bool) []keyCol {
	keys := []keyCol{{ch.Access.ParentFK, parentVal}}
	if !topLevel && ch.Access.RootFK != "" {
		keys = append(keys, keyCol{ch.Access.RootFK, rootVal})
	}
	return keys
}

func writeAccessChildren(ctx context.Context, db *sql.DB, cfgs []config.ChildConfig, rootVal, parentVal interface{}, topLevel bool, data [][]*AccessDoc) error {
	for i := range cfgs {
		ch := &cfgs[i]
		if !ch.WritesToAccess() {
			continue
		}
		var rows []*AccessDoc
		if i < len(data) {
			rows = data[i]
		}
		pk := parentKeys(ch, rootVal, parentVal, topLevel)
		kept := make(map[string]bool, len(rows))
		for _, r := range rows {
			pkVal, ok := r.Row.Value(ch.Access.PK)
			if !ok || pkVal == nil {
				return fmt.Errorf("%s: row without %s", ch.Access.Table, ch.Access.PK)
			}
			keys := append(append([]keyCol{}, pk...), keyCol{ch.Access.PK, pkVal})
			if err := upsertAccessRow(ctx, db, ch.Access.Table, keys, r.Row); err != nil {
				return fmt.Errorf("%s %v/%v: %w", ch.Access.Table, parentVal, pkVal, err)
			}
			kept[lookup.Key(pkVal, true)] = true
			if err := writeAccessChildren(ctx, db, ch.Children, rootVal, pkVal, false, r.Children); err != nil {
				return err
			}
		}
		if ch.PG.DeleteMissing {
			if err := deleteSubtree(ctx, db, ch, rootVal, parentVal, topLevel, kept); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteSubtree deletes the parent's rows of ch whose key is not in keep (all of them when
// keep is nil), together with their own written-to-Access descendants.
func deleteSubtree(ctx context.Context, db *sql.DB, ch *config.ChildConfig, rootVal, parentVal interface{}, topLevel bool, keep map[string]bool) error {
	if !ch.WritesToAccess() {
		return nil
	}
	pk := parentKeys(ch, rootVal, parentVal, topLevel)
	where := keyWhere(pk)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s", bracket(ch.Access.PK), bracket(ch.Access.Table), where)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("%s: list for delete: %w", ch.Access.Table, err)
	}
	var stale []interface{}
	for rows.Next() {
		var v interface{}
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		if keep == nil || !keep[lookup.Key(v, true)] {
			stale = append(stale, v)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, v := range stale {
		for i := range ch.Children {
			if err := deleteSubtree(ctx, db, &ch.Children[i], rootVal, v, false, nil); err != nil {
				return err
			}
		}
		keys := append(append([]keyCol{}, pk...), keyCol{ch.Access.PK, v})
		where := keyWhere(keys)
		if _, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", bracket(ch.Access.Table), where)); err != nil {
			return fmt.Errorf("%s: delete %v: %w", ch.Access.Table, v, err)
		}
	}
	return nil
}

// upsertAccessRow updates the row matched by keys with the mapped values, or inserts it.
// Key values are forced into the row so an insert carries them even when not mapped.
func upsertAccessRow(ctx context.Context, db *sql.DB, table string, keys []keyCol, row *mapper.AccessMappedRow) error {
	isKey := make(map[string]bool, len(keys))
	for _, k := range keys {
		isKey[strings.ToLower(k.col)] = true
	}
	var sets []string
	for i, c := range row.AccessCols {
		if isKey[strings.ToLower(c)] {
			continue
		}
		sets = append(sets, fmt.Sprintf("%s = %s", bracket(c), mapper.FormatAccessValue(row.AccessVals[i])))
	}

	where := keyWhere(keys)
	if len(sets) > 0 {
		res, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", bracket(table), strings.Join(sets, ", "), where))
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			return nil
		}
	} else {
		var n int64
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", bracket(table), where)
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return fmt.Errorf("exists: %w", err)
		}
		if n > 0 {
			return nil
		}
	}

	cols := make([]string, 0, len(keys)+len(row.AccessCols))
	vals := make([]string, 0, cap(cols))
	for _, k := range keys {
		cols = append(cols, bracket(k.col))
		vals = append(vals, mapper.FormatAccessValue(k.val))
	}
	for i, c := range row.AccessCols {
		if isKey[strings.ToLower(c)] {
			continue
		}
		cols = append(cols, bracket(c))
		vals = append(vals, mapper.FormatAccessValue(row.AccessVals[i]))
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", bracket(table), strings.Join(cols, ", "), strings.Join(vals, ", "))
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

// keyWhere builds "[a] = literal AND ..."; values are inlined like the rest of the Access
// writer (the ADODB driver's parameter typing is unreliable for Jet).
func keyWhere(keys []keyCol) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s = %s", bracket(k.col), mapper.FormatAccessValue(k.val))
	}
	return strings.Join(parts, " AND ")
}

// bracket quotes a Jet identifier; legacy names carry spaces ([Jurisdicciones de IIBB]).
func bracket(name string) string {
	if strings.HasPrefix(name, "[") {
		return name
	}
	return "[" + name + "]"
}
