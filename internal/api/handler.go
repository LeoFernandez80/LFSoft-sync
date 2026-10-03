package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
)

type Handler struct {
	pool     *pgxpool.Pool
	conns    map[string]*accessdb.Conn // mdb filename -> conn
	entities map[string]*config.EntityConfig
	mu       map[string]*sync.Mutex
}

func NewHandler(pool *pgxpool.Pool, conns map[string]*accessdb.Conn, entities []config.EntityConfig) *Handler {
	h := &Handler{
		pool:     pool,
		conns:    conns,
		entities: make(map[string]*config.EntityConfig, len(entities)),
		mu:       make(map[string]*sync.Mutex, len(entities)),
	}
	for i := range entities {
		e := &entities[i]
		h.entities[e.Entity] = e
		h.mu[e.Entity] = &sync.Mutex{}
	}
	return h
}

type NextIDResponse struct {
	Entity  string          `json:"entity"`
	NextID  int64           `json:"nextId"`
	Sources NextIDSources   `json:"sources"`
}

type NextIDSources struct {
	Access   int64 `json:"access"`
	Postgres int64 `json:"postgres"`
}

func (h *Handler) HandleNextID(w http.ResponseWriter, r *http.Request) {
	entity := r.PathValue("entity")
	ec, ok := h.entities[entity]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown entity: %s", entity), http.StatusNotFound)
		return
	}

	if !ec.UsesIDCoord() {
		http.Error(w, fmt.Sprintf("entity %s does not use ID coordination", entity), http.StatusBadRequest)
		return
	}

	h.mu[entity].Lock()
	defer h.mu[entity].Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	maxAccess, err := h.maxAccessPK(ctx, ec)
	if err != nil {
		http.Error(w, fmt.Sprintf("access query failed: %v", err), http.StatusInternalServerError)
		return
	}

	maxPG, err := h.maxPGValue(ctx, ec)
	if err != nil {
		http.Error(w, fmt.Sprintf("pg query failed: %v", err), http.StatusInternalServerError)
		return
	}

	nextID := maxAccess
	if maxPG > nextID {
		nextID = maxPG
	}
	nextID++

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(NextIDResponse{
		Entity: entity,
		NextID: nextID,
		Sources: NextIDSources{
			Access:   maxAccess,
			Postgres: maxPG,
		},
	})
}

func (h *Handler) maxAccessPK(ctx context.Context, ec *config.EntityConfig) (int64, error) {
	conn, ok := h.conns[ec.Access.MDB]
	if !ok {
		return 0, fmt.Errorf("no connection for %s", ec.Access.MDB)
	}
	q := fmt.Sprintf("SELECT MAX(%s) FROM %s", ec.Access.PK, ec.Access.Table)
	row := conn.QueryRow(ctx, q)
	var max *int64
	if err := row.Scan(&max); err != nil {
		return 0, err
	}
	if max == nil {
		return 0, nil
	}
	return *max, nil
}

func (h *Handler) maxPGValue(ctx context.Context, ec *config.EntityConfig) (int64, error) {
	col := ec.SyncColumn()
	q := fmt.Sprintf(`SELECT COALESCE(MAX("%s"), 0) FROM %s."%s"`, col, ec.PG.Schema, ec.PG.Table)
	var max int64
	err := h.pool.QueryRow(ctx, q).Scan(&max)
	return max, err
}

func (h *Handler) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sync/next-id/{entity}", h.HandleNextID)
	mux.HandleFunc("GET /sync/health", h.HandleHealth)
}
