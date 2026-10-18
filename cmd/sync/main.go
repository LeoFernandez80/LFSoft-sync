package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	"github.com/lfsoft/sync/internal/lookup"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	initialSync := flag.Bool("initial-sync", false, "run one full Access → PG scan per enabled entity in load_order and exit")
	only := flag.String("entities", "", "with --initial-sync: comma-separated entities to scan (default: all enabled)")
	dryRun := flag.Bool("dry-run", false, "detect and log changes without writing")
	flag.Parse()

	if *initialSync {
		os.Exit(runInitialSync(*cfgPath, *only))
	}

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
	cfg, cfgErr := config.Load(cfgPath)
	logFile := ""
	if cfgErr == nil {
		logFile = cfg.LogFile
	}
	logger, err := newLogger(logFile)
	if err != nil {
		logger, _ = zap.NewProduction()
		logger.Sugar().Warnw("log file unavailable, logging to stderr only", "path", logFile, "err", err)
	}
	defer logger.Sync()
	log := logger.Sugar()

	if cfgErr != nil {
		log.Fatalf("config: %v", cfgErr)
	}
	log.Infow("config loaded", "entities", len(cfg.Entities), "port", cfg.SyncPort, "log_file", logFile)

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
		if err := engine.RunInitialLoad(ctx, pool, conn, entityCfg, stateRepo, synclogRepo, idmapRepo, fk, lookup.New(pool), log); err != nil {
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

// runInitialSync is the --initial-sync mode: the first full scan of every entity, one after the
// other in load_order, then a summary table on stdout. Exit code 1 if any entity had errors.
func runInitialSync(cfgPath, only string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 2
	}
	logger, err := newLogger(cfg.LogFile)
	if err != nil {
		logger, _ = zap.NewProduction()
	}
	defer logger.Sync()
	log := logger.Sugar()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.PgDSN)
	if err != nil {
		log.Errorw("pg connect", "err", err)
		return 2
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Errorw("pg ping", "err", err)
		return 2
	}
	mdbConns, err := openMDBConnections(cfg)
	if err != nil {
		log.Errorw("mdb connections", "err", err)
		return 2
	}
	defer closeMDBConnections(mdbConns)

	var entities []string
	if only != "" {
		entities = strings.Split(only, ",")
	}
	results, runErr := engine.RunInitialSync(ctx, pool, mdbConns, cfg,
		state.NewRepository(pool), synclog.NewRepository(pool), idmap.NewRepository(pool), log, entities)

	fmt.Printf("\n%-22s %10s %10s %8s %12s %10s %9s\n", "ENTIDAD", "LEIDAS", "ESCRITAS", "ERRORES", "ACCESS", "PG", "SEG")
	code := 0
	for _, r := range results {
		mark := ""
		if r.Errors > 0 {
			code = 1
			mark = "  <-- ver log"
		}
		fmt.Printf("%-22s %10d %10d %8d %12d %10d %9.1f%s\n", r.Entity, r.Detected, r.Written, r.Errors,
			r.AccessCount, r.PGCount, r.Duration.Seconds(), mark)
		if r.ErrorDetail != "" {
			// ASCII: la consola clasica de Windows puede cortar al script que lee esta salida.
			detail := strings.ReplaceAll(r.ErrorDetail, "→", "->")
			if len(detail) > 300 {
				detail = detail[:300] + "..."
			}
			fmt.Printf("    %s\n", detail)
		}
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "initial sync: %v\n", runErr)
		return 2
	}
	return code
}

// newLogger writes JSON logs to stderr and, if path is set, appends to that file.
// The file is opened directly: zap output paths parse "C:\..." as a URL scheme.
func newLogger(path string) (*zap.Logger, error) {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	enc := zapcore.NewJSONEncoder(encCfg)
	level := zap.NewAtomicLevelAt(zap.InfoLevel)

	cores := []zapcore.Core{zapcore.NewCore(enc, zapcore.Lock(os.Stderr), level)}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		cores = append(cores, zapcore.NewCore(enc, zapcore.AddSync(f), level))
	}
	return zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel)), nil
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
