package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/api"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/conflict"
	"github.com/lfsoft/sync/internal/engine"
	"github.com/lfsoft/sync/internal/guard"
	"github.com/lfsoft/sync/internal/idmap"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"go.uber.org/zap"
)

func main() {
	if isWindowsService() {
		if err := runAsService("LFSoftSync"); err != nil {
			fmt.Fprintf(os.Stderr, "service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfgPath := flag.String("config", "config/sync.yaml", "path to sync config file")
	initialLoad := flag.String("initial-load", "", "run initial load for the given entity (Access → PG)")
	dryRun := flag.Bool("dry-run", false, "detect and log changes without writing")
	flag.Parse()

	stop := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		close(stop)
	}()

	runSync(*cfgPath, *initialLoad, *dryRun, stop)
}

func runSync(cfgPath, initialLoad string, dryRun bool, stop chan struct{}) {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	log := logger.Sugar()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Infow("config loaded", "entities", len(cfg.Entities), "port", cfg.SyncPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-stop
		cancel()
	}()

	pool, err := pgxpool.New(ctx, cfg.PgDSN)
	if err != nil {
		log.Fatalf("pg connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("pg ping: %v", err)
	}
	log.Info("pg connected")

	mdbConns, err := openMDBConnections(cfg)
	if err != nil {
		log.Fatalf("mdb connections: %v", err)
	}
	defer closeMDBConnections(mdbConns)
	log.Infow("mdb connections open", "count", len(mdbConns))

	stateRepo := state.NewRepository(pool)
	idmapRepo := idmap.NewRepository(pool)
	synclogRepo := synclog.NewRepository(pool)
	conflictRepo := conflict.NewRepository(pool)
	antiLoop := guard.New()

	_ = conflictRepo

	if initialLoad != "" {
		log.Infow("running initial load", "entity", initialLoad)
		var entityCfg *config.EntityConfig
		for i := range cfg.Entities {
			if cfg.Entities[i].Entity == initialLoad {
				entityCfg = &cfg.Entities[i]
				break
			}
		}
		if entityCfg == nil {
			log.Fatalf("entity %q not found in config", initialLoad)
		}
		conn, ok := mdbConns[entityCfg.Access.MDB]
		if !ok {
			log.Fatalf("no mdb connection for %s", entityCfg.Access.MDB)
		}
		var fk mapper.FKResolver
		if idmapRepo != nil {
			fk = mapper.NewIDMapFKResolver(idmapRepo)
		}
		if err := engine.RunInitialLoad(ctx, pool, conn, entityCfg, stateRepo, synclogRepo, idmapRepo, fk, log); err != nil {
			log.Fatalf("initial load: %v", err)
		}
		log.Infow("initial load complete", "entity", initialLoad)
		return
	}

	handler := api.NewHandler(pool, mdbConns, cfg.Entities)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", cfg.SyncPort),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Infow("HTTP server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	eng := engine.New(pool, mdbConns, cfg, stateRepo, synclogRepo, idmapRepo, antiLoop, log)
	go eng.Start(ctx, dryRun)

	<-stop
	log.Info("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	cancel()
	log.Info("sync service stopped")
}

func openMDBConnections(cfg *config.Config) (map[string]*accessdb.Conn, error) {
	seen := make(map[string]bool)
	conns := make(map[string]*accessdb.Conn)
	for _, e := range cfg.Entities {
		if seen[e.Access.MDB] {
			continue
		}
		seen[e.Access.MDB] = true
		path := e.MDBPath(cfg.MdbBasePath)
		conn, err := accessdb.Open(path)
		if err != nil {
			closeMDBConnections(conns)
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		conns[e.Access.MDB] = conn
	}
	return conns, nil
}

func closeMDBConnections(conns map[string]*accessdb.Conn) {
	for _, c := range conns {
		c.Close()
	}
}
