package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/guard"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/lookup"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/stock"
	"github.com/lfsoft/sync/internal/synclog"
	"go.uber.org/zap"
)

// InitialSyncResult is the outcome of one entity's first full scan.
type InitialSyncResult struct {
	Entity      string
	Detected    int
	Written     int
	Errors      int
	ErrorDetail string
	AccessCount int64 // root rows in Access (within access.filter)
	PGCount     int64 // root rows in PG that carry a sync key
	Duration    time.Duration
}

// RunInitialSync runs one full Access → PG scan per enabled entity, in load order, through the
// same worker the service uses: rows are upserted by their sync key, so tables already seeded
// by migrations are completed and corrected instead of rejected. It also leaves sync.sync_state
// set, so the service starts from there. only (optional) restricts it to those entities.
func RunInitialSync(
	ctx context.Context,
	pool *pgxpool.Pool,
	conns map[string]*accessdb.Conn,
	cfg *config.Config,
	stateR *state.Repository,
	synclogR *synclog.Repository,
	idmapR *idmap.Repository,
	log *zap.SugaredLogger,
	only []string,
) ([]InitialSyncResult, error) {
	want := make(map[string]bool, len(only))
	for _, e := range only {
		want[strings.TrimSpace(e)] = true
	}
	var fk mapper.FKResolver
	if idmapR != nil {
		fk = mapper.NewIDMapFKResolver(idmapR)
	}

	var entities []*config.EntityConfig
	for _, ec := range cfg.InLoadOrder() {
		if len(want) == 0 || want[ec.Entity] {
			entities = append(entities, ec)
		}
	}

	// run scans one entity with a fresh worker and a fresh lookup cache, so catalogs written by
	// earlier entities resolve right away. retry nil: the first pass, a full scan (a new worker's
	// first Detect is always the periodic one). retry set: only those document roots.
	run := func(ec *config.EntityConfig, retry []string) (InitialSyncResult, []string, error) {
		res := InitialSyncResult{Entity: ec.Entity}
		conn, ok := conns[ec.Access.MDB]
		if !ok {
			return res, nil, fmt.Errorf("%s: no mdb connection for %s", ec.Entity, ec.Access.MDB)
		}
		start := time.Now()
		log.Infow("initial sync: entity started", "entity", ec.Entity, "load_order", ec.Order(), "retry_roots", len(retry))
		w := NewWorker(pool, conn, ec, cfg.MdbBasePath, stateR, synclogR, guard.New(), fk, lookup.New(pool), log, false)
		var entry *synclog.Entry
		if ec.Kind == "stock_movements" {
			entry = stock.New(pool, conn, ec, cfg.MdbBasePath, stateR, synclogR, log, false).Cycle(ctx, true)
		} else if retry != nil {
			entry = w.retryRoots(ctx, retry)
		} else {
			entry = w.cycle(ctx)
		}
		if entry != nil {
			res.Detected = entry.Detected
			res.Written = entry.Written
			res.Errors = entry.Errors
			if entry.ErrorDetail != nil {
				res.ErrorDetail = *entry.ErrorDetail
			}
		}
		if err := ctx.Err(); err != nil {
			return res, nil, err
		}
		var err error
		if res.AccessCount, err = countAccessRoots(ctx, conn, ec); err != nil {
			log.Warnw("initial sync: access count failed", "entity", ec.Entity, "err", err)
		}
		if res.PGCount, err = countPGRoots(ctx, pool, ec); err != nil {
			log.Warnw("initial sync: pg count failed", "entity", ec.Entity, "err", err)
		}
		res.Duration = time.Since(start)
		log.Infow("initial sync: entity done",
			"entity", ec.Entity, "detected", res.Detected, "written", res.Written, "errors", res.Errors,
			"access_rows", res.AccessCount, "pg_rows", res.PGCount, "duration_ms", res.Duration.Milliseconds())
		return res, append([]string(nil), w.failedRoots...), nil
	}

	results := make([]InitialSyncResult, 0, len(entities))
	failed := make([][]string, 0, len(entities))
	for _, ec := range entities {
		res, roots, err := run(ec, nil)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		failed = append(failed, roots)
	}

	// Second pass for entities with errors: rows that pointed at a catalog loaded later in the
	// first pass (a province of a country that came after it) resolve now. Documents retry only
	// the roots that failed; simple entities are cheap enough to scan again.
	for i, ec := range entities {
		if results[i].Errors == 0 {
			continue
		}
		var retry []string
		if len(ec.Children) > 0 {
			if len(failed[i]) == 0 {
				continue
			}
			retry = failed[i]
		}
		log.Infow("initial sync: retrying entity with errors", "entity", ec.Entity, "errors", results[i].Errors)
		res, _, err := run(ec, retry)
		if err != nil {
			return results, err
		}
		if retry != nil {
			// Report the first pass, with what the retry recovered.
			recovered := len(retry) - res.Errors
			res.Detected = results[i].Detected
			res.Errors = results[i].Errors - recovered
			if res.Errors == 0 {
				res.ErrorDetail = ""
			}
		}
		res.Written += results[i].Written
		res.Duration += results[i].Duration
		results[i] = res
	}
	return results, nil
}

func countAccessRoots(ctx context.Context, conn *accessdb.Conn, ec *config.EntityConfig) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", ec.Access.Table, reader.WhereClause("", ec.Access.Filter))
	if ec.Kind == "stock_movements" {
		q = "SELECT COUNT(*) FROM (SELECT DISTINCT [Movimiento_Numero] FROM [Stock])"
	}
	var n int64
	err := reader.QueryOne(ctx, conn, q, &n)
	return n, err
}

func countPGRoots(ctx context.Context, pool *pgxpool.Pool, ec *config.EntityConfig) (int64, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s."%s" WHERE "%s" IS NOT NULL`, ec.PG.Schema, ec.PG.Table, ec.SyncColumn())
	var n int64
	err := pool.QueryRow(ctx, q).Scan(&n)
	return n, err
}
