// Package lookup resolves field values against PG tables (legacy code → id, code → snapshot
// text) for the lookup option of a field mapping.
package lookup

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/sync/internal/config"
)

const (
	// tableTTL bounds how stale a cached table can get; rows added in PG show up within it.
	tableTTL = 10 * time.Minute
	// missReload is the minimum gap between reloads triggered by a miss, so a value that is
	// genuinely absent doesn't reload the table on every row.
	missReload = 30 * time.Second
	queryTTL   = 10 * time.Minute
	missTTL    = time.Minute
)

// Resolver caches whole lookup tables (they are catalogs: customers, articles, activities) and
// single query results. Safe for concurrent use by all workers.
type Resolver struct {
	pool *pgxpool.Pool

	mu      sync.Mutex
	tables  map[string]*table
	queries map[string]queryEntry
}

type table struct {
	mu       sync.Mutex
	forward  map[string]interface{} // normalized match → select
	reverse  map[string]interface{} // select → match
	loadedAt time.Time
}

type queryEntry struct {
	val   interface{}
	found bool
	at    time.Time
}

func New(pool *pgxpool.Pool) *Resolver {
	return &Resolver{pool: pool, tables: make(map[string]*table), queries: make(map[string]queryEntry)}
}

// Lookup returns the value for args (one value for the table form; the Args columns for the
// query form) and whether it was found.
func (r *Resolver) Lookup(ctx context.Context, l *config.Lookup, args []interface{}) (interface{}, bool, error) {
	if l.Query != "" {
		return r.query(ctx, l, args)
	}
	if len(args) != 1 {
		return nil, false, fmt.Errorf("lookup %s: expected 1 value, got %d", l.Table, len(args))
	}
	t := r.table(l)
	key := Key(args[0], l.CaseInsensitive)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := r.ensureLoaded(ctx, l, t, func() bool { _, ok := t.forward[key]; return ok }); err != nil {
		return nil, false, err
	}
	v, ok := t.forward[key]
	return v, ok, nil
}

// ReverseLookup maps a selected value back to its match value (PG → Access). Query lookups
// are not reversible.
func (r *Resolver) ReverseLookup(ctx context.Context, l *config.Lookup, v interface{}) (interface{}, bool, error) {
	if l.Query != "" {
		return nil, false, fmt.Errorf("lookup query is not reversible")
	}
	t := r.table(l)
	key := Key(v, false)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := r.ensureLoaded(ctx, l, t, func() bool { _, ok := t.reverse[key]; return ok }); err != nil {
		return nil, false, err
	}
	m, ok := t.reverse[key]
	return m, ok, nil
}

func (r *Resolver) table(l *config.Lookup) *table {
	id := fmt.Sprintf("%s|%s|%s|%t", l.Table, l.Match, l.Select, l.CaseInsensitive)
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[id]
	if !ok {
		t = &table{}
		r.tables[id] = t
	}
	return t
}

// ensureLoaded (re)loads t when it is stale, or when has() misses and the last load is old
// enough: a row inserted in PG after the load must resolve without waiting for the TTL.
func (r *Resolver) ensureLoaded(ctx context.Context, l *config.Lookup, t *table, has func() bool) error {
	age := time.Since(t.loadedAt)
	if t.forward != nil && age < tableTTL && (age < missReload || has()) {
		return nil
	}
	q := fmt.Sprintf(`SELECT "%s", "%s" FROM %s ORDER BY "%s"`, l.Match, l.Select, QuoteTable(l.Table), l.Select)
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("lookup load %s: %w", l.Table, err)
	}
	defer rows.Close()
	fwd := make(map[string]interface{})
	rev := make(map[string]interface{})
	for rows.Next() {
		var m, s interface{}
		if err := rows.Scan(&m, &s); err != nil {
			return fmt.Errorf("lookup load %s: %w", l.Table, err)
		}
		if m == nil || s == nil {
			continue
		}
		// Ordered by select: on duplicate keys (e.g. two spellings of a code compared
		// case-insensitively) the lowest id wins, as in the legacy ETL.
		if k := Key(m, l.CaseInsensitive); fwd[k] == nil {
			fwd[k] = s
		}
		if k := Key(s, false); rev[k] == nil {
			rev[k] = m
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lookup load %s: %w", l.Table, err)
	}
	t.forward, t.reverse, t.loadedAt = fwd, rev, time.Now()
	return nil
}

func (r *Resolver) query(ctx context.Context, l *config.Lookup, args []interface{}) (interface{}, bool, error) {
	keyParts := make([]string, len(args))
	for i, a := range args {
		keyParts[i] = Key(a, false)
	}
	key := l.Query + "\x00" + strings.Join(keyParts, "\x00")

	r.mu.Lock()
	e, ok := r.queries[key]
	r.mu.Unlock()
	if ok {
		ttl := queryTTL
		if !e.found {
			ttl = missTTL
		}
		if time.Since(e.at) < ttl {
			return e.val, e.found, nil
		}
	}

	var v interface{}
	err := r.pool.QueryRow(ctx, l.Query, args...).Scan(&v)
	found := true
	if err == pgx.ErrNoRows {
		found, err = false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup query: %w", err)
	}
	if v == nil {
		found = false
	}
	r.mu.Lock()
	r.queries[key] = queryEntry{val: v, found: found, at: time.Now()}
	r.mu.Unlock()
	return v, found, nil
}

// Key normalizes a value read from Access or PG so both sides compare equal: integral floats
// as integers, text trimmed (and lowercased when caseInsensitive, as Jet compares).
func Key(v interface{}, caseInsensitive bool) string {
	var s string
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		s = strings.TrimSpace(val)
	case []byte:
		s = strings.TrimSpace(string(val))
	case float64:
		if val == math.Trunc(val) {
			s = fmt.Sprintf("%d", int64(val))
		} else {
			s = fmt.Sprintf("%v", val)
		}
	case float32:
		return Key(float64(val), caseInsensitive)
	default:
		s = fmt.Sprintf("%v", val)
	}
	if caseInsensitive {
		s = strings.ToLower(s)
	}
	return s
}

// QuoteTable turns "schema.table" into schema."table" (PG folds unquoted names to lowercase).
func QuoteTable(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return fmt.Sprintf(`%s."%s"`, name[:i], name[i+1:])
	}
	return fmt.Sprintf(`"%s"`, name)
}
