package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/detector"
	"github.com/lfsoft/sync/internal/guard"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"github.com/lfsoft/sync/internal/writer"
	"go.uber.org/zap"
)

type PGWorker struct {
	pool       *pgxpool.Pool
	entity     *config.EntityConfig
	pgChanges  *detector.PGChanges
	reader     *reader.PGReader
	mapper     *mapper.Reverse
	writer     *writer.AccessWriter
	stateR     *state.Repository
	synclogR   *synclog.Repository
	idmapR     *idmap.Repository
	guard      *guard.Guard
	fkResolver mapper.FKResolver
	lookup     mapper.LookupResolver
	log        *zap.SugaredLogger
	dryRun     bool
	trigger    chan struct{}

	consecutiveErrors int
}

func NewPGWorker(
	pool *pgxpool.Pool,
	conn *accessdb.Conn,
	cfg *config.EntityConfig,
	stateR *state.Repository,
	synclogR *synclog.Repository,
	idmapR *idmap.Repository,
	g *guard.Guard,
	fk mapper.FKResolver,
	lk mapper.LookupResolver,
	log *zap.SugaredLogger,
	dryRun bool,
) *PGWorker {
	return &PGWorker{
		pool:       pool,
		entity:     cfg,
		pgChanges:  detector.NewPGChanges(pool),
		reader:     reader.NewPG(pool, cfg),
		mapper:     mapper.NewReverse(cfg),
		writer:     writer.NewAccess(conn, cfg),
		stateR:     stateR,
		synclogR:   synclogR,
		idmapR:     idmapR,
		guard:      g,
		fkResolver: fk,
		lookup:     lk,
		log:        log.With("entity", cfg.Entity, "dir", "pg_to_access"),
		dryRun:     dryRun,
		trigger:    make(chan struct{}, 1),
	}
}

func (w *PGWorker) Trigger() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

func (w *PGWorker) Run(ctx context.Context) {
	interval := w.entity.Sync.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}

	w.log.Infow("pg worker started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("pg worker stopped")
			return
		case <-w.trigger:
			w.cycle(ctx)
		case <-ticker.C:
			if w.consecutiveErrors >= maxConsecutiveErrors {
				ticker.Reset(degradedInterval)
			}
			w.cycle(ctx)
		}
	}
}

func (w *PGWorker) cycle(ctx context.Context) {
	start := time.Now()
	entry := &synclog.Entry{
		Entity:    w.entity.Entity,
		Direction: "pg_to_access",
		StartedAt: start,
		Strategy:  "trigger",
	}

	changes, err := w.pgChanges.ReadPending(ctx, w.entity.Entity, 100)
	if err != nil {
		w.handleError(ctx, entry, start, fmt.Errorf("read changes: %w", err))
		return
	}
	if len(changes) == 0 {
		w.consecutiveErrors = 0
		return
	}

	entry.Detected = len(changes)
	var processedIDs []int64

	if len(w.entity.Children) > 0 {
		processedIDs = w.processDocumentChanges(ctx, changes, entry)
	} else {
		for _, ch := range changes {
			err := w.processChange(ctx, ch, entry)
			if err != nil {
				w.log.Warnw("process change failed", "change_id", ch.ID, "op", ch.Operation, "row_id", ch.RowID, "err", err)
				entry.Errors++
				continue
			}
			processedIDs = append(processedIDs, ch.ID)
		}
	}

	if len(processedIDs) > 0 {
		if err := w.pgChanges.MarkProcessed(ctx, processedIDs); err != nil {
			w.log.Warnw("mark processed failed", "err", err)
		}
	}

	// Update last change ID
	if len(changes) > 0 {
		lastID := changes[len(changes)-1].ID
		newState := &state.SyncState{
			Entity:       w.entity.Entity,
			LastChangeID: &lastID,
		}
		if err := w.stateR.Upsert(ctx, newState); err != nil {
			w.log.Warnw("state upsert failed", "err", err)
		}
	}

	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	if err := w.synclogR.Insert(ctx, entry); err != nil {
		w.log.Warnw("synclog insert failed", "err", err)
	}

	w.consecutiveErrors = 0
	w.log.Infow("cycle complete",
		"detected", entry.Detected,
		"written", entry.Written,
		"skipped", entry.Skipped,
		"errors", entry.Errors,
		"duration_ms", entry.DurationMs,
	)
}

func (w *PGWorker) revMapper(ctx context.Context) *mapper.Reverse {
	return w.mapper.With(mapper.Resolvers{Ctx: ctx, FK: w.fkResolver, Lookup: w.lookup})
}

func (w *PGWorker) processChange(ctx context.Context, ch detector.Change, entry *synclog.Entry) error {
	switch ch.Operation {
	case "I":
		return w.processInsert(ctx, ch, entry)
	case "U":
		return w.processUpdate(ctx, ch, entry)
	case "D":
		return w.processDelete(ctx, ch, entry)
	default:
		return fmt.Errorf("unknown operation: %s", ch.Operation)
	}
}

func (w *PGWorker) processInsert(ctx context.Context, ch detector.Change, entry *synclog.Entry) error {
	row, err := w.reader.ReadByID(ctx, ch.RowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row deleted in PG after the change was queued; its D change (if any) handles Access.
			w.log.Infow("pg row gone, skipping change", "change_id", ch.ID, "op", ch.Operation, "row_id", ch.RowID)
			entry.Skipped++
			return nil
		}
		return fmt.Errorf("read pg row %d: %w", ch.RowID, err)
	}

	row, accessPK, err := w.ensureRootKey(ctx, ch.RowID, row)
	if err != nil {
		return err
	}
	if accessPK == "" {
		if w.entity.PG.BusinessKey != "" {
			// Created in PG without a legacy key and the entity doesn't generate one: there is
			// no Access row for it, so it stays PG-only.
			entry.Skipped++
			return nil
		}
		return fmt.Errorf("could not determine access PK from PG row %d", ch.RowID)
	}

	mapped, err := w.revMapper(ctx).Map(row)
	if err != nil {
		return fmt.Errorf("map row: %w", err)
	}

	if w.dryRun {
		w.log.Infow("dry-run: would insert", "access_pk", accessPK)
		return nil
	}

	// Mark anti-loop BEFORE writing to Access
	w.guard.Mark(w.entity.Entity, accessPK)

	exists, err := w.writer.Exists(ctx, accessPK)
	if err != nil {
		return fmt.Errorf("check exists: %w", err)
	}

	if exists {
		if _, err := w.writer.Update(ctx, mapped); err != nil {
			return fmt.Errorf("update access: %w", err)
		}
	} else {
		if err := w.writer.Insert(ctx, mapped); err != nil {
			return fmt.Errorf("insert access: %w", err)
		}
	}

	if w.entity.NeedsIDMap() || w.entity.PG.BusinessKey != "" {
		if err := w.idmapR.Upsert(ctx, &idmap.Entry{
			Entity:   w.entity.Entity,
			LegacyID: accessPK,
			PGID:     ch.RowID,
		}); err != nil {
			w.log.Warnw("id_map upsert failed", "err", err)
		}
	}

	entry.Written++
	return nil
}

func (w *PGWorker) processUpdate(ctx context.Context, ch detector.Change, entry *synclog.Entry) error {
	row, err := w.reader.ReadByID(ctx, ch.RowID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row deleted in PG after the change was queued; its D change (if any) handles Access.
			w.log.Infow("pg row gone, skipping change", "change_id", ch.ID, "op", ch.Operation, "row_id", ch.RowID)
			entry.Skipped++
			return nil
		}
		return fmt.Errorf("read pg row %d: %w", ch.RowID, err)
	}

	row, accessPK, err := w.ensureRootKey(ctx, ch.RowID, row)
	if err != nil {
		return err
	}
	if accessPK == "" {
		if w.entity.PG.BusinessKey != "" {
			// Created in PG without a legacy key and the entity doesn't generate one: there is
			// no Access row for it, so it stays PG-only.
			entry.Skipped++
			return nil
		}
		return fmt.Errorf("could not determine access PK from PG row %d", ch.RowID)
	}

	mapped, err := w.revMapper(ctx).Map(row)
	if err != nil {
		return fmt.Errorf("map row: %w", err)
	}

	if w.dryRun {
		w.log.Infow("dry-run: would update", "access_pk", accessPK)
		return nil
	}

	w.guard.Mark(w.entity.Entity, accessPK)

	n, err := w.writer.Update(ctx, mapped)
	if err != nil {
		return fmt.Errorf("update access: %w", err)
	}
	if n == 0 {
		// ADO's RowsAffected is not reliable (0 even when the UPDATE matched): confirm first, or
		// a table without a unique index in Access would get a duplicate row.
		exists, err := w.writer.Exists(ctx, accessPK)
		if err != nil {
			return fmt.Errorf("check exists: %w", err)
		}
		if exists {
			entry.Written++
			return nil
		}
		// Row missing in Access (or PK mismatch): insert so the change isn't silently lost.
		w.log.Warnw("update matched no rows in access, inserting", "access_pk", accessPK, "row_id", ch.RowID)
		if err := w.writer.Insert(ctx, mapped); err != nil {
			return fmt.Errorf("insert access after empty update: %w", err)
		}
	}

	entry.Written++
	return nil
}

func (w *PGWorker) processDelete(ctx context.Context, ch detector.Change, entry *synclog.Entry) error {
	accessPK, err := w.resolveAccessPK(ctx, ch)
	if err != nil {
		return fmt.Errorf("resolve access pk: %w", err)
	}

	if w.dryRun {
		w.log.Infow("dry-run: would delete", "access_pk", accessPK)
		return nil
	}

	w.guard.Mark(w.entity.Entity, accessPK)

	n, err := w.writer.Delete(ctx, accessPK)
	if err != nil {
		return fmt.Errorf("delete access: %w", err)
	}
	if n == 0 {
		w.log.Warnw("delete matched no rows in access", "access_pk", accessPK, "row_id", ch.RowID)
	}

	if w.entity.NeedsIDMap() || w.entity.PG.BusinessKey != "" {
		if err := w.idmapR.Delete(ctx, w.entity.Entity, accessPK); err != nil {
			w.log.Warnw("id_map delete failed", "err", err)
		}
	}

	entry.Written++
	return nil
}

// resolveAccessPK finds the Access PK for a deleted PG row.
// For PK directo: ch.RowID IS the Access PK.
// For legacyCode: lookup id_map by PG ID.
// For business_key: the row is gone, try id_map or fail.
func (w *PGWorker) resolveAccessPK(ctx context.Context, ch detector.Change) (string, error) {
	if !w.entity.NeedsIDMap() && w.entity.PG.BusinessKey == "" {
		return fmt.Sprintf("%d", ch.RowID), nil
	}

	if w.entity.PG.BusinessKey != "" && len(ch.OldData) > 0 {
		pk, err := w.accessPKFromOldData(ch.OldData)
		if err != nil {
			return "", fmt.Errorf("old data: %w", err)
		}
		if pk != "" {
			return pk, nil
		}
	}

	entry, err := w.idmapR.GetByPGID(ctx, w.entity.Entity, ch.RowID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("no id_map entry for pg_id=%d (entity=%s)", ch.RowID, w.entity.Entity)
		}
		return "", err
	}
	return entry.LegacyID, nil
}

func (w *PGWorker) accessPKFromOldData(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var old map[string]interface{}
	if err := dec.Decode(&old); err != nil {
		return "", err
	}
	v, ok := old[w.entity.PG.BusinessKey]
	if !ok || v == nil {
		return "", nil
	}
	if s, isStr := v.(string); isStr && mapper.NeedsDatePK(w.entity) {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t, err = time.Parse("2006-01-02", s)
		}
		if err != nil {
			return "", fmt.Errorf("parse %s=%q: %w", w.entity.PG.BusinessKey, s, err)
		}
		v = t
	}
	return mapper.AccessPKFromRow(reader.Row{w.entity.PG.BusinessKey: v}, w.entity), nil
}

func (w *PGWorker) handleError(ctx context.Context, entry *synclog.Entry, start time.Time, err error) {
	w.consecutiveErrors++
	w.log.Errorw("cycle error", "err", err, "consecutive", w.consecutiveErrors)
	entry.Errors = 1
	detail := err.Error()
	entry.ErrorDetail = &detail
	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	_ = w.synclogR.Insert(ctx, entry)
}

// ensureRootKey returns the row's Access key. A row created in the app without business key
// gets one when the entity has sync.generate_key: MAX(Access PK, PG key) + 1, skipping values
// Access already has, stored in PG (sync.is_writing, so the trigger doesn't queue it again).
func (w *PGWorker) ensureRootKey(ctx context.Context, rowID int64, row reader.Row) (reader.Row, string, error) {
	accessPK := mapper.AccessPKFromRow(row, w.entity)
	if accessPK != "" || w.entity.PG.BusinessKey == "" || !w.entity.Sync.GenerateKey {
		return row, accessPK, nil
	}
	accessMax, err := w.writer.MaxValue(ctx, w.entity.Access.Table, w.entity.Access.PK, "")
	if err != nil {
		return nil, "", fmt.Errorf("generate key: access max: %w", err)
	}
	var pgMax int64
	q := fmt.Sprintf(`SELECT COALESCE(MAX("%s"), 0)::bigint FROM %s."%s"`, w.entity.PG.BusinessKey, w.entity.PG.Schema, w.entity.PG.Table)
	if err := w.pool.QueryRow(ctx, q).Scan(&pgMax); err != nil {
		return nil, "", fmt.Errorf("generate key: pg max: %w", err)
	}
	next := accessMax
	if pgMax > next {
		next = pgMax
	}
	next++
	for i := 0; i < 1000; i++ {
		exists, err := w.writer.Exists(ctx, fmt.Sprintf("%d", next))
		if err != nil {
			return nil, "", fmt.Errorf("generate key: %w", err)
		}
		if !exists {
			break
		}
		next++
	}
	if err := w.setPGValue(ctx, w.entity.PG.Table, w.entity.PG.BusinessKey, w.entity.PG.PK, rowID, next); err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}
	w.log.Infow("legacy key generated for row created in the app", "row_id", rowID, "key", next)
	row, err = w.reader.ReadByID(ctx, rowID)
	if err != nil {
		return nil, "", err
	}
	return row, mapper.AccessPKFromRow(row, w.entity), nil
}

// setPGValue writes one column of one PG row without queuing a sync change.
func (w *PGWorker) setPGValue(ctx context.Context, table, col, pkCol string, id int64, v interface{}) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
		return err
	}
	q := fmt.Sprintf(`UPDATE %s."%s" SET "%s" = $1 WHERE "%s" = $2`, w.entity.PG.Schema, table, col, pkCol)
	if _, err := tx.Exec(ctx, q, v, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
