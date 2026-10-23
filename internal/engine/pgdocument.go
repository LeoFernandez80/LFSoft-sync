package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/detector"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/synclog"
	"github.com/lfsoft/sync/internal/writer"
)

// processDocumentChanges writes whole documents PG → Access. A save in the app touches the
// root and many child rows, and every one queues a change for the same root
// (sync.notify_document_change on the child tables), so changes are grouped by root and each
// document is written once. Returns the change IDs that are done.
func (w *PGWorker) processDocumentChanges(ctx context.Context, changes []detector.Change, entry *synclog.Entry) []int64 {
	type group struct {
		ids []int64
		del *detector.Change
	}
	var order []int64
	groups := make(map[int64]*group)
	for _, ch := range changes {
		g := groups[ch.RowID]
		if g == nil {
			g = &group{}
			groups[ch.RowID] = g
			order = append(order, ch.RowID)
		}
		g.ids = append(g.ids, ch.ID)
		if ch.Operation == "D" {
			c := ch
			g.del = &c
		}
	}

	var processed []int64
	for _, rootID := range order {
		g := groups[rootID]
		if err := w.syncDocument(ctx, rootID, g.del, entry); err != nil {
			w.log.Warnw("write document failed", "row_id", rootID, "changes", len(g.ids), "err", err)
			entry.Errors++
			continue
		}
		processed = append(processed, g.ids...)
	}
	return processed
}

// syncDocument writes the current PG state of one document to Access: the whole tree if the
// root exists, or its deletion if it was deleted.
func (w *PGWorker) syncDocument(ctx context.Context, rootID int64, del *detector.Change, entry *synclog.Entry) error {
	row, err := w.reader.ReadByID(ctx, rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		if del == nil {
			entry.Skipped++ // the root's own D change, in a later batch, handles Access
			return nil
		}
		return w.deleteDocument(ctx, *del, entry)
	}
	if err != nil {
		return fmt.Errorf("read pg row %d: %w", rootID, err)
	}

	row, accessPK, err := w.ensureRootKey(ctx, rootID, row)
	if err != nil {
		return err
	}
	if accessPK == "" {
		entry.Skipped++ // PG-only document without a legacy key
		return nil
	}
	res := mapper.Resolvers{Ctx: ctx, FK: w.fkResolver, Lookup: w.lookup}
	root, err := w.mapper.With(res).Map(row)
	if err != nil {
		return fmt.Errorf("map root: %w", err)
	}
	rootVal, _ := root.Value(w.entity.Access.PK)
	children, err := w.reverseChildren(ctx, res, w.entity.Children, rootID, rootVal, rootVal, true)
	if err != nil {
		return err
	}

	if w.dryRun {
		w.log.Infow("dry-run: would write document", "access_pk", accessPK)
		return nil
	}

	// Anti-loop: the Access worker skips this root while the write settles.
	w.guard.Mark(w.entity.Entity, accessPK)
	if err := w.writer.WriteDocument(ctx, &writer.AccessDoc{Row: root, Children: children}); err != nil {
		return fmt.Errorf("write access: %w", err)
	}
	if err := w.idmapR.Upsert(ctx, &idmap.Entry{Entity: w.entity.Entity, LegacyID: accessPK, PGID: rootID}); err != nil {
		w.log.Warnw("id_map upsert failed", "err", err)
	}
	entry.Written++
	return nil
}

func (w *PGWorker) deleteDocument(ctx context.Context, del detector.Change, entry *synclog.Entry) error {
	accessPK, err := w.resolveAccessPK(ctx, del)
	if err != nil {
		return fmt.Errorf("resolve access pk: %w", err)
	}
	if w.dryRun {
		w.log.Infow("dry-run: would delete document", "access_pk", accessPK)
		return nil
	}
	w.guard.Mark(w.entity.Entity, accessPK)
	if err := w.writer.DeleteDocumentChildren(ctx, accessPK); err != nil {
		return fmt.Errorf("delete access children: %w", err)
	}
	return w.processDelete(ctx, del, entry)
}

// reverseChildren reads each written-to-Access collection of one PG parent and maps it back.
// rootVal/parentVal are the Access keys of the root and of this parent, for key generation.
func (w *PGWorker) reverseChildren(ctx context.Context, res mapper.Resolvers, cfgs []config.ChildConfig, parentID int64, rootVal, parentVal interface{}, topLevel bool) ([][]*writer.AccessDoc, error) {
	out := make([][]*writer.AccessDoc, len(cfgs))
	for i := range cfgs {
		ch := &cfgs[i]
		if !ch.WritesToAccess() {
			continue
		}
		rows, err := w.reader.ReadChildren(ctx, ch, parentID)
		if err != nil {
			return nil, err
		}
		if ch.Access.GenerateKey != "" {
			if err := w.generateChildKeys(ctx, ch, rows, rootVal, parentVal, topLevel); err != nil {
				return nil, fmt.Errorf("%s: %w", ch.Entity, err)
			}
		}
		m := mapper.NewChildReverse(ch).With(res)
		docs := make([]*writer.AccessDoc, 0, len(rows))
		for _, r := range rows {
			id, err := toID(r[ch.PG.PK])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ch.Entity, err)
			}
			am, err := m.Map(r)
			if err != nil {
				return nil, fmt.Errorf("%s id=%d: %w", ch.Entity, id, err)
			}
			childVal, _ := am.Value(ch.Access.PK)
			sub, err := w.reverseChildren(ctx, res, ch.Children, id, rootVal, childVal, false)
			if err != nil {
				return nil, err
			}
			docs = append(docs, &writer.AccessDoc{Row: am, Children: sub})
		}
		out[i] = docs
	}
	return out, nil
}

func toID(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("unexpected pk type %T", v)
	}
}

// generateChildKeys gives the child rows created in the app (no value in the PG column mapped
// from the Access pk) the next Access key, and stores it in PG. "parent": MAX within this
// parent in Access; "global": MAX of the whole Access table. Values already used by sibling
// rows in PG count too.
func (w *PGWorker) generateChildKeys(ctx context.Context, ch *config.ChildConfig, rows []reader.Row, rootVal, parentVal interface{}, topLevel bool) error {
	pgCol := ""
	for _, f := range ch.Fields {
		if f.Access == ch.Access.PK {
			pgCol = f.PG
			break
		}
	}
	if pgCol == "" {
		return fmt.Errorf("generate_key: no field maps %s", ch.Access.PK)
	}
	var pending []reader.Row
	var max int64
	for _, r := range rows {
		if r[pgCol] == nil {
			pending = append(pending, r)
			continue
		}
		if n, err := mapper.ToInt64(r[pgCol]); err == nil && n > max {
			max = n
		}
	}
	if len(pending) == 0 {
		return nil
	}
	where := ""
	if ch.Access.GenerateKey == "parent" {
		cols := []string{ch.Access.ParentFK}
		vals := []interface{}{parentVal}
		if !topLevel && ch.Access.RootFK != "" {
			cols = append(cols, ch.Access.RootFK)
			vals = append(vals, rootVal)
		}
		where = writer.KeyWhere(cols, vals)
	}
	accessMax, err := w.writer.MaxValue(ctx, ch.Access.Table, ch.Access.PK, where)
	if err != nil {
		return fmt.Errorf("generate_key: %w", err)
	}
	if accessMax > max {
		max = accessMax
	}
	for _, r := range pending {
		max++
		id, err := toID(r[ch.PG.PK])
		if err != nil {
			return err
		}
		if err := w.setPGValue(ctx, ch.PG.Table, pgCol, ch.PG.PK, id, max); err != nil {
			return fmt.Errorf("generate_key: %w", err)
		}
		r[pgCol] = max
		w.log.Infow("legacy key generated for line created in the app", "collection", ch.Entity, "row_id", id, "key", max)
	}
	return nil
}
