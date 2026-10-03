package engine

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/guard"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"go.uber.org/zap"
)

type Engine struct {
	pool      *pgxpool.Pool
	conns     map[string]*accessdb.Conn
	cfg       *config.Config
	stateR    *state.Repository
	synclogR  *synclog.Repository
	idmapR    *idmap.Repository
	guard     *guard.Guard
	log       *zap.SugaredLogger
	workers   []*Worker
	pgWorkers map[string]*PGWorker
}

func New(
	pool *pgxpool.Pool,
	conns map[string]*accessdb.Conn,
	cfg *config.Config,
	stateR *state.Repository,
	synclogR *synclog.Repository,
	idmapR *idmap.Repository,
	g *guard.Guard,
	log *zap.SugaredLogger,
) *Engine {
	return &Engine{
		pool:      pool,
		conns:     conns,
		cfg:       cfg,
		stateR:    stateR,
		synclogR:  synclogR,
		idmapR:    idmapR,
		guard:     g,
		log:       log,
		pgWorkers: make(map[string]*PGWorker),
	}
}

func (e *Engine) Start(ctx context.Context, dryRun bool) {
	var wg sync.WaitGroup
	var fk mapper.FKResolver
	if e.idmapR != nil {
		fk = mapper.NewIDMapFKResolver(e.idmapR)
	}

	for i := range e.cfg.Entities {
		ec := &e.cfg.Entities[i]
		conn, ok := e.conns[ec.Access.MDB]
		if !ok {
			e.log.Warnw("no mdb connection for entity, skipping", "entity", ec.Entity, "mdb", ec.Access.MDB)
			continue
		}

		// Access → PG worker
		w := NewWorker(e.pool, conn, ec, e.cfg.MdbBasePath, e.stateR, e.synclogR, e.guard, fk, e.log, dryRun)
		e.workers = append(e.workers, w)
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			w.Run(ctx)
		}(w)

		// PG → Access worker
		if ec.Sync.Direction == "both" || ec.Sync.Direction == "pg_to_access" {
			pw := NewPGWorker(e.pool, conn, ec, e.stateR, e.synclogR, e.idmapR, e.guard, fk, e.log, dryRun)
			e.pgWorkers[ec.Entity] = pw
			wg.Add(1)
			go func(pw *PGWorker) {
				defer wg.Done()
				pw.Run(ctx)
			}(pw)
		}
	}

	// LISTEN sync_change → trigger PG workers immediately
	listener := NewListener(e.pool, e.log)
	go listener.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case entity := <-listener.C():
				if pw, ok := e.pgWorkers[entity]; ok {
					pw.Trigger()
				}
			}
		}
	}()

	// Guard cleanup goroutine
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.guard.Cleanup()
			}
		}
	}()

	wg.Wait()
}
