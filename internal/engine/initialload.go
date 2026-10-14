package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"github.com/lfsoft/sync/internal/writer"
	"go.uber.org/zap"
)

const bulkBatchSize = 500

func RunInitialLoad(
	ctx context.Context,
	pool *pgxpool.Pool,
	conn *accessdb.Conn,
	cfg *config.EntityConfig,
	stateR *state.Repository,
	synclogR *synclog.Repository,
	idmapR *idmap.Repository,
	fk mapper.FKResolver,
	lk mapper.LookupResolver,
	log *zap.SugaredLogger,
) error {
	log = log.With("entity", cfg.Entity)
	start := time.Now()
	log.Infow("initial load started")

	pgCount, err := countPG(ctx, pool, cfg)
	if err != nil {
		return fmt.Errorf("count pg: %w", err)
	}
	if pgCount > 0 {
		return fmt.Errorf("PG table %s.%s is not empty (count=%d) — initial load requires empty table",
			cfg.PG.Schema, cfg.PG.Table, pgCount)
	}

	r := reader.New(conn, cfg)
	rows, err := r.ReadAll(ctx)
	if err != nil {
		return fmt.Errorf("read access: %w", err)
	}
	log.Infow("read from access", "rows", len(rows))

	res := mapper.Resolvers{Ctx: ctx, FK: fk, Lookup: lk}
	w := writer.NewPG(pool, cfg)
	totalWritten := 0

	if len(cfg.Children) > 0 {
		// Documents go through the same path as the worker: COPY can't attach child rows to
		// the identity values of their parents.
		totalWritten, err = initialLoadDocuments(ctx, w, &docBuilder{cfg: cfg, reader: r, res: res}, rows, log)
		if err != nil {
			return err
		}
	} else {
		mapped, err := mapper.NewForward(cfg).With(res).MapBatch(rows)
		if err != nil {
			return fmt.Errorf("map: %w", err)
		}
		for i := 0; i < len(mapped); i += bulkBatchSize {
			end := i + bulkBatchSize
			if end > len(mapped) {
				end = len(mapped)
			}
			written, err := w.BulkInsert(ctx, mapped[i:end])
			if err != nil {
				return fmt.Errorf("bulk insert batch %d-%d: %w", i, end, err)
			}
			totalWritten += written
			log.Infow("batch inserted", "from", i, "to", end, "written", written)
		}
	}

	// Register id_map for legacyCode entities
	if cfg.NeedsIDMap() && idmapR != nil {
		if err := registerIDMaps(ctx, pool, cfg, idmapR, log); err != nil {
			log.Warnw("id_map registration failed", "err", err)
		}
	}

	// Update state cursors
	maxPK := findMaxPK(rows, cfg)
	count := int64(len(rows))
	now := time.Now()
	tsVal := &now
	if cfg.Detection.AccessToPG == "timestamp" {
		if mt := findMaxTimestamp(rows, cfg); mt != nil {
			tsVal = mt
		}
	}
	newState := &state.SyncState{
		Entity:        cfg.Entity,
		LastMaxPK:     &maxPK,
		LastCount:     &count,
		LastFileModTime: &now,
		LastTimestamp:  tsVal,
	}
	if err := stateR.Upsert(ctx, newState); err != nil {
		log.Warnw("state upsert failed", "err", err)
	}

	entry := &synclog.Entry{
		Entity:     cfg.Entity,
		Direction:  "initial_load",
		StartedAt:  start,
		FinishedAt: time.Now(),
		Strategy:   "bulk",
		Detected:   len(rows),
		Written:    totalWritten,
		DurationMs: int(time.Since(start).Milliseconds()),
	}
	if err := synclogR.Insert(ctx, entry); err != nil {
		log.Warnw("synclog insert failed", "err", err)
	}

	log.Infow("initial load complete",
		"read", len(rows),
		"written", totalWritten,
		"duration_ms", entry.DurationMs,
	)
	return nil
}

func registerIDMaps(ctx context.Context, pool *pgxpool.Pool, cfg *config.EntityConfig, idmapR *idmap.Repository, log *zap.SugaredLogger) error {
	q := fmt.Sprintf(`SELECT "%s", "%s" FROM %s."%s"`,
		cfg.PG.PK, cfg.PG.LegacyCodeColumn, cfg.PG.Schema, cfg.PG.Table)
	pgRows, err := pool.Query(ctx, q)
	if err != nil {
		return err
	}
	defer pgRows.Close()

	registered := 0
	for pgRows.Next() {
		var pgID int64
		var legacyID interface{}
		if err := pgRows.Scan(&pgID, &legacyID); err != nil {
			return err
		}
		if legacyID == nil {
			continue
		}
		if err := idmapR.Upsert(ctx, &idmap.Entry{
			Entity:   cfg.Entity,
			LegacyID: fmt.Sprintf("%v", legacyID),
			PGID:     pgID,
		}); err != nil {
			log.Warnw("id_map upsert failed", "legacy_id", legacyID, "pg_id", pgID, "err", err)
			continue
		}
		registered++
	}
	log.Infow("id_map entries registered", "count", registered)
	return pgRows.Err()
}

// initialLoadDocuments writes the documents in batches. A document that fails to map or write
// is logged and skipped; the worker retries it on its periodic scan.
func initialLoadDocuments(ctx context.Context, w *writer.PGWriter, b *docBuilder, rows []reader.Row, log *zap.SugaredLogger) (int, error) {
	written, failed := 0, 0
	for start := 0; start < len(rows); start += docBatchSize {
		end := start + docBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		docs, err := b.build(ctx, rows[start:end])
		if err != nil {
			return written, fmt.Errorf("read children batch %d-%d: %w", start, end, err)
		}
		for _, d := range docs {
			if d.err == nil {
				d.err = w.UpsertDocument(ctx, d.doc)
			}
			if d.err != nil {
				failed++
				log.Warnw("document skipped", "pk", d.pk, "err", d.err)
				continue
			}
			written++
		}
		log.Infow("documents loaded", "from", start, "to", end, "written", written, "failed", failed)
	}
	return written, nil
}

func countPG(ctx context.Context, pool *pgxpool.Pool, cfg *config.EntityConfig) (int64, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s."%s"`, cfg.PG.Schema, cfg.PG.Table)
	var c int64
	err := pool.QueryRow(ctx, q).Scan(&c)
	return c, err
}

func findMaxPK(rows []reader.Row, cfg *config.EntityConfig) string {
	var maxPK string
	for _, row := range rows {
		v := fmt.Sprintf("%v", row[cfg.Access.PK])
		if v > maxPK {
			maxPK = v
		}
	}
	return maxPK
}

func findMaxTimestamp(rows []reader.Row, cfg *config.EntityConfig) *time.Time {
	col := cfg.Detection.TimestampColumn
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
