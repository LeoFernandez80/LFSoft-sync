package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/detector"
	"github.com/lfsoft/sync/internal/guard"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"github.com/lfsoft/sync/internal/writer"
	"go.uber.org/zap"
)

type Worker struct {
	entity     *config.EntityConfig
	detector   *detector.Detector
	reader     *reader.AccessReader
	mapper     *mapper.Forward
	writer     *writer.PGWriter
	stateR     *state.Repository
	synclogR   *synclog.Repository
	guard      *guard.Guard
	fkResolver mapper.FKResolver
	lookup     mapper.LookupResolver
	log        *zap.SugaredLogger
	dryRun     bool

	consecutiveErrors int

	// lastWritten maps Access PK → fingerprint of the mapped values last handed to PG (for
	// documents: of the whole tree).
	lastWritten map[string]string
	// rootWritten maps Access PK → fingerprint of a document's root row alone, so a
	// change-triggered full scan reads children only for roots that changed.
	rootWritten map[string]string
	// failedRoots lists the document roots (Access PK) the last syncDocuments call could not
	// read, map or write; the initial sync retries just those.
	failedRoots []string
}

const (
	maxConsecutiveErrors = 10
	degradedInterval     = 5 * time.Minute
)

func NewWorker(
	pool *pgxpool.Pool,
	conn *accessdb.Conn,
	cfg *config.EntityConfig,
	mdbBasePath string,
	stateR *state.Repository,
	synclogR *synclog.Repository,
	g *guard.Guard,
	fk mapper.FKResolver,
	lk mapper.LookupResolver,
	log *zap.SugaredLogger,
	dryRun bool,
) *Worker {
	mdbPath := cfg.MDBPath(mdbBasePath)
	return &Worker{
		entity:     cfg,
		detector:   detector.New(conn, pool, cfg, stateR, mdbPath),
		reader:     reader.New(conn, cfg),
		mapper:     mapper.NewForward(cfg),
		writer:     writer.NewPG(pool, cfg),
		stateR:     stateR,
		synclogR:   synclogR,
		guard:      g,
		fkResolver: fk,
		lookup:     lk,
		log:        log.With("entity", cfg.Entity),
		dryRun:     dryRun,

		lastWritten: make(map[string]string),
		rootWritten: make(map[string]string),
	}
}

func (w *Worker) Run(ctx context.Context) {
	interval := w.entity.Sync.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}

	w.log.Infow("worker started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-ticker.C:
			if w.consecutiveErrors >= maxConsecutiveErrors {
				ticker.Reset(degradedInterval)
			}
			w.cycle(ctx)
		}
	}
}

// cycle runs one detect → read → write round. It returns the round's log entry, or nil when the
// detector found nothing to do.
func (w *Worker) cycle(ctx context.Context) *synclog.Entry {
	start := time.Now()
	entry := &synclog.Entry{
		Entity:    w.entity.Entity,
		Direction: "access_to_pg",
		StartedAt: start,
		Strategy:  "detect",
	}

	result, err := w.detector.Detect(ctx)
	if err != nil {
		w.handleError(ctx, entry, start, fmt.Errorf("detect: %w", err))
		return entry
	}

	if result.Action == detector.ActionNone {
		w.consecutiveErrors = 0
		return nil
	}

	var rows []reader.Row

	switch result.Action {
	case detector.ActionIncremental:
		entry.Strategy = "incremental"
		rows, err = w.reader.ReadSince(ctx, result.OldMaxPK)
	case detector.ActionFullScan:
		entry.Strategy = "full_scan"
		rows, err = w.reader.ReadAll(ctx)
	case detector.ActionTimestamp:
		entry.Strategy = "timestamp"
		rows, err = w.reader.ReadSinceTimestamp(ctx, result.LastTimestamp)
	}
	if err != nil {
		w.handleError(ctx, entry, start, fmt.Errorf("read: %w", err))
		return entry
	}

	entry.Detected = len(rows)
	w.log.Infow("rows detected", "strategy", entry.Strategy, "count", len(rows), "read_ms", time.Since(start).Milliseconds())
	writeStart := time.Now()

	// Filter out rows recently written by sync (anti-loop)
	var filtered []reader.Row
	for _, row := range rows {
		pk := fmt.Sprintf("%v", row[w.entity.Access.PK])
		if w.guard.IsRecentlyWritten(w.entity.Entity, pk) {
			entry.Skipped++
			continue
		}
		filtered = append(filtered, row)
	}

	var ok bool
	if len(w.entity.Children) > 0 {
		ok = w.syncDocuments(ctx, filtered, entry, result.Action == detector.ActionFullScan, result.Periodic)
	} else {
		ok = w.syncSimple(ctx, filtered, entry, result.Periodic)
	}
	if !ok {
		// Nothing was written: keep the state (modtime, last scan) so the next cycle retries.
		w.consecutiveErrors++
		entry.FinishedAt = time.Now()
		entry.DurationMs = int(time.Since(start).Milliseconds())
		if err := w.synclogR.Insert(ctx, entry); err != nil {
			w.log.Warnw("synclog insert failed", "err", err)
		}
		return entry
	}

	writeMs := time.Since(writeStart).Milliseconds()

	if col := w.entity.Sync.BumpSequence; col != "" && entry.Written > 0 && !w.dryRun {
		if err := w.writer.BumpSequence(ctx, col); err != nil {
			w.log.Warnw("bump sequence failed", "column", col, "err", err)
		}
	}

	// Handle deletes on full scan
	if result.Action == detector.ActionFullScan && !w.dryRun && w.entity.DeletesMissing() {
		deleteStart := time.Now()
		accessPKs := make([]string, 0, len(rows))
		for _, row := range rows {
			accessPKs = append(accessPKs, fmt.Sprintf("%v", row[w.entity.Access.PK]))
		}
		recheck := func(cands []string) ([]string, error) {
			found, err := w.reader.ReadByPKs(ctx, cands)
			if err != nil {
				return nil, err
			}
			present := make(map[string]bool, len(found))
			for _, r := range found {
				present[fmt.Sprintf("%v", r[w.entity.Access.PK])] = true
			}
			var gone []string
			for _, k := range cands {
				if !present[k] {
					gone = append(gone, k)
				}
			}
			return gone, nil
		}
		deleted, err := w.writer.DeleteMissing(ctx, accessPKs, recheck)
		if err != nil {
			w.log.Warnw("delete missing failed", "err", err)
		} else if deleted > 0 {
			w.log.Infow("deleted missing rows from PG", "count", deleted)
		}
		w.log.Infow("full scan timing", "write_ms", writeMs, "delete_ms", time.Since(deleteStart).Milliseconds())
	}

	// Update state
	now := time.Now()
	newState := &state.SyncState{
		Entity:    w.entity.Entity,
		LastMaxPK: &result.NewMaxPK,
		LastCount: &result.NewCount,
	}
	// Only a full scan sees edits to existing rows, so the modtime is consumed only by one;
	// incremental/timestamp cycles leave it pending and the detector schedules the full scan.
	if result.Action == detector.ActionFullScan {
		newState.LastFileModTime = &result.FileModTime
	}
	if result.Action == detector.ActionTimestamp || result.Action == detector.ActionFullScan {
		tsVal := now
		if w.entity.Detection.AccessToPG == "timestamp" {
			if mt := maxTimestampFromRows(rows, w.entity.Detection.TimestampColumn); mt != nil {
				tsVal = *mt
			}
		}
		newState.LastTimestamp = &tsVal
	}
	if result.Action == detector.ActionFullScan {
		newState.LastHashScan = &now
	}
	if err := w.stateR.Upsert(ctx, newState); err != nil {
		w.log.Warnw("state upsert failed", "err", err)
	}

	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	if err := w.synclogR.Insert(ctx, entry); err != nil {
		w.log.Warnw("synclog insert failed", "err", err)
	}

	w.consecutiveErrors = 0
	w.log.Infow("cycle complete",
		"strategy", entry.Strategy,
		"detected", entry.Detected,
		"skipped", entry.Skipped,
		"written", entry.Written,
		"duration_ms", entry.DurationMs,
	)
	return entry
}

func (w *Worker) resolvers(ctx context.Context) mapper.Resolvers {
	return mapper.Resolvers{Ctx: ctx, FK: w.fkResolver, Lookup: w.lookup}
}

func (w *Worker) fwMapper(ctx context.Context) *mapper.Forward {
	return w.mapper.With(w.resolvers(ctx))
}

// syncSimple reports false when the batch write failed as a whole (nothing was written).
// Unless periodic, rows whose mapped values equal the last ones handed to PG are left out, so a
// change-triggered full scan writes only what was edited instead of staging the whole table.
func (w *Worker) syncSimple(ctx context.Context, filtered []reader.Row, entry *synclog.Entry, periodic bool) bool {
	m := w.fwMapper(ctx)
	// Map row by row: one unconvertible Access row must not block the rest of the batch.
	mapped := make([]*mapper.MappedRow, 0, len(filtered))
	fingerprints := make(map[string]string, len(filtered))
	var firstMapErr error
	for _, row := range filtered {
		mr, err := m.Map(row)
		if err != nil {
			pk := fmt.Sprintf("%v", row[w.entity.Access.PK])
			w.log.Warnw("map row failed", "pk", pk, "err", err)
			entry.Errors++
			if firstMapErr == nil {
				firstMapErr = fmt.Errorf("map pk=%s: %w", pk, err)
			}
			continue
		}
		fp := fmt.Sprintf("%v", mr.PGVals)
		fingerprints[mr.PKVal] = fp
		if !periodic && mr.PKVal != "" && w.lastWritten[mr.PKVal] == fp {
			continue
		}
		mapped = append(mapped, mr)
	}
	if firstMapErr != nil {
		setErrorDetail(entry, firstMapErr)
	}
	// Remember values once PG has them (or has rejected them: the writer retries rejects on
	// periodic scans). A failed batch keeps the old map so those rows are sent again.
	remember := func() {
		if periodic {
			w.lastWritten = fingerprints
			return
		}
		for _, mr := range mapped {
			w.lastWritten[mr.PKVal] = fingerprints[mr.PKVal]
		}
	}
	if len(mapped) == 0 {
		return true
	}

	if w.dryRun {
		w.log.Infow("dry-run: would write", "count", len(mapped))
		return true
	}

	written, err := w.writer.SyncBatch(ctx, mapped)
	var rowErrs *writer.RowErrors
	if errors.As(err, &rowErrs) {
		w.log.Warnw("rows skipped on write", "count", rowErrs.Count, "written", written, "sample", rowErrs.Sample)
		entry.Errors += rowErrs.Count
		setErrorDetail(entry, err)
		entry.Written = written
		remember()
		return true
	}
	if err != nil {
		w.log.Errorw("write batch failed", "err", err)
		entry.Errors++
		setErrorDetail(entry, fmt.Errorf("write batch: %w", err))
		return false
	}
	entry.Written = written
	remember()
	return true
}

func setErrorDetail(entry *synclog.Entry, err error) {
	if entry.ErrorDetail != nil {
		return
	}
	detail := err.Error()
	entry.ErrorDetail = &detail
}

// syncDocuments writes roots with their child trees. Reading children is the expensive part, so:
//   - a change-triggered full scan (fullScan && !periodic) only completes roots whose own row
//     changed since it was last written; child-only edits wait for the periodic scan or a
//     timestamp read;
//   - unless periodic, a document whose whole tree equals the last one written is skipped.
//
// It reports false when an Access read failed, so the cycle keeps its state and retries.
func (w *Worker) syncDocuments(ctx context.Context, rows []reader.Row, entry *synclog.Entry, fullScan, periodic bool) bool {
	if fullScan && !periodic {
		m := w.fwMapper(ctx)
		changed := rows[:0:0]
		for _, row := range rows {
			mr, err := m.Map(row)
			if err != nil {
				changed = append(changed, row) // the builder reports the error
				continue
			}
			if w.rootWritten[mr.PKVal] != mapper.HashValues(mr.PGVals) {
				changed = append(changed, row)
			}
		}
		rows = changed
	}

	b := &docBuilder{cfg: w.entity, reader: w.reader, res: w.resolvers(ctx)}
	ok := true
	w.failedRoots = w.failedRoots[:0]
	for start := 0; start < len(rows); start += docBatchSize {
		end := start + docBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		docs, err := b.build(ctx, rows[start:end])
		if err != nil {
			w.log.Errorw("read document children failed", "err", err)
			for _, row := range rows[start:end] {
				w.failedRoots = append(w.failedRoots, fmt.Sprintf("%v", row[w.entity.Access.PK]))
			}
			entry.Errors++
			setErrorDetail(entry, fmt.Errorf("read children: %w", err))
			ok = false
			continue
		}
		for _, d := range docs {
			if d.err != nil {
				w.log.Warnw("map document failed", "pk", d.pk, "err", d.err)
				w.failedRoots = append(w.failedRoots, d.pk)
				entry.Errors++
				setErrorDetail(entry, fmt.Errorf("map pk=%s: %w", d.pk, d.err))
				continue
			}
			fp := d.doc.Fingerprint()
			if !periodic && w.lastWritten[d.pk] == fp {
				w.rememberRoot(d)
				continue
			}
			if w.dryRun {
				w.log.Infow("dry-run: would write document", "pk", d.pk, "children", countChildren(d.doc))
				continue
			}
			// The PG -> Access worker may have written this document after it was read here (a
			// line added in the app): writing the older Access copy would delete that line in PG.
			// The next scan reads it again.
			if w.guard.IsRecentlyWritten(w.entity.Entity, d.pk) {
				entry.Skipped++
				continue
			}
			if err := w.writer.UpsertDocument(ctx, d.doc); err != nil {
				w.log.Warnw("write document failed", "pk", d.pk, "err", err)
				w.failedRoots = append(w.failedRoots, d.pk)
				entry.Errors++
				setErrorDetail(entry, fmt.Errorf("write pk=%s: %w", d.pk, err))
				continue
			}
			w.lastWritten[d.pk] = fp
			w.rememberRoot(d)
			entry.Written++
		}
	}
	return ok
}

// retryRoots syncs again only the given document roots (Access PKs), always writing them. The
// initial sync uses it for the documents that failed its first pass.
func (w *Worker) retryRoots(ctx context.Context, pks []string) *synclog.Entry {
	start := time.Now()
	entry := &synclog.Entry{Entity: w.entity.Entity, Direction: "access_to_pg", StartedAt: start, Strategy: "retry"}
	rows, err := w.reader.ReadByPKs(ctx, pks)
	if err != nil {
		w.handleError(ctx, entry, start, fmt.Errorf("read: %w", err))
		return entry
	}
	entry.Detected = len(rows)
	w.syncDocuments(ctx, rows, entry, false, true)
	if col := w.entity.Sync.BumpSequence; col != "" && entry.Written > 0 && !w.dryRun {
		if err := w.writer.BumpSequence(ctx, col); err != nil {
			w.log.Warnw("bump sequence failed", "column", col, "err", err)
		}
	}
	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	if err := w.synclogR.Insert(ctx, entry); err != nil {
		w.log.Warnw("synclog insert failed", "err", err)
	}
	return entry
}

func (w *Worker) rememberRoot(d built) {
	w.rootWritten[d.pk] = mapper.HashValues(d.doc.Row.PGVals)
}

func countChildren(d *mapper.DocRow) int {
	n := 0
	for _, ch := range d.Children {
		for _, c := range ch {
			n += 1 + countChildren(c)
		}
	}
	return n
}

func (w *Worker) handleError(ctx context.Context, entry *synclog.Entry, start time.Time, err error) {
	w.consecutiveErrors++
	w.log.Errorw("cycle error",
		"err", err,
		"consecutive", w.consecutiveErrors,
	)
	entry.Errors = 1
	detail := err.Error()
	entry.ErrorDetail = &detail
	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	_ = w.synclogR.Insert(ctx, entry)
}

func maxTimestampFromRows(rows []reader.Row, col string) *time.Time {
	if col == "" {
		return nil
	}
	var max *time.Time
	for _, row := range rows {
		v, ok := row[col]
		if !ok || v == nil {
			continue
		}
		if t, ok := v.(time.Time); ok && !t.IsZero() {
			if max == nil || t.After(*max) {
				cp := t
				max = &cp
			}
		}
	}
	return max
}
