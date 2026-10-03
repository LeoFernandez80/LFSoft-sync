package engine

import (
	"context"
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
	log        *zap.SugaredLogger
	dryRun     bool

	consecutiveErrors int
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
		log:        log.With("entity", cfg.Entity),
		dryRun:     dryRun,
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

func (w *Worker) cycle(ctx context.Context) {
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
		return
	}

	if result.Action == detector.ActionNone {
		w.consecutiveErrors = 0
		return
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
		return
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

	if len(w.entity.Children) > 0 {
		w.syncDocuments(ctx, filtered, entry)
	} else {
		w.syncSimple(ctx, filtered, entry)
	}

	writeMs := time.Since(writeStart).Milliseconds()

	// Handle deletes on full scan
	if result.Action == detector.ActionFullScan && !w.dryRun {
		deleteStart := time.Now()
		accessPKs := make([]string, 0, len(rows))
		for _, row := range rows {
			accessPKs = append(accessPKs, fmt.Sprintf("%v", row[w.entity.Access.PK]))
		}
		deleted, err := w.writer.DeleteMissing(ctx, accessPKs)
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
		Entity:        w.entity.Entity,
		LastMaxPK:     &result.NewMaxPK,
		LastCount:     &result.NewCount,
		LastFileModTime: &result.FileModTime,
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
}

func (w *Worker) fwMapper(ctx context.Context) *mapper.Forward {
	if w.fkResolver != nil {
		return w.mapper.WithFK(ctx, w.fkResolver)
	}
	return w.mapper
}

func (w *Worker) syncSimple(ctx context.Context, filtered []reader.Row, entry *synclog.Entry) {
	m := w.fwMapper(ctx)
	mapped, err := m.MapBatch(filtered)
	if err != nil {
		w.log.Errorw("map batch failed", "err", err)
		entry.Errors++
		return
	}

	if w.dryRun {
		w.log.Infow("dry-run: would write", "count", len(mapped))
		return
	}

	written, err := w.writer.SyncBatch(ctx, mapped)
	if err != nil {
		w.log.Errorw("write batch failed", "err", err)
		entry.Errors++
		return
	}
	entry.Written = written
}

func (w *Worker) syncDocuments(ctx context.Context, parentRows []reader.Row, entry *synclog.Entry) {
	m := w.fwMapper(ctx)
	for _, parentRow := range parentRows {
		parentPK := fmt.Sprintf("%v", parentRow[w.entity.Access.PK])

		parentMapped, err := m.Map(parentRow)
		if err != nil {
			w.log.Warnw("map parent failed", "pk", parentPK, "err", err)
			entry.Errors++
			continue
		}

		childrenData := make(map[string][]*mapper.MappedRow)
		for i := range w.entity.Children {
			childCfg := &w.entity.Children[i]
			childRows, err := w.reader.ReadChildren(ctx, childCfg, parentPK)
			if err != nil {
				w.log.Warnw("read children failed", "child", childCfg.Entity, "parent_pk", parentPK, "err", err)
				entry.Errors++
				continue
			}

			childMapper := mapper.NewChildForward(childCfg)
			for _, cr := range childRows {
				cm, err := childMapper.Map(cr)
				if err != nil {
					w.log.Warnw("map child failed", "child", childCfg.Entity, "err", err)
					continue
				}
				childrenData[childCfg.Entity] = append(childrenData[childCfg.Entity], cm)
			}
		}

		if w.dryRun {
			total := 0
			for _, ch := range childrenData {
				total += len(ch)
			}
			w.log.Infow("dry-run: would write document", "pk", parentPK, "children", total)
			continue
		}

		if err := w.writer.UpsertDocument(ctx, parentMapped, w.entity.Children, childrenData); err != nil {
			w.log.Warnw("write document failed", "pk", parentPK, "err", err)
			entry.Errors++
			continue
		}
		entry.Written++
	}
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
