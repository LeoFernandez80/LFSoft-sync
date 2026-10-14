package detector

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
)

type Action int

const (
	ActionNone        Action = iota
	ActionIncremental        // new rows only (MAX(PK) increased)
	ActionFullScan           // full scan needed (count decreased or hash interval)
	ActionTimestamp          // read rows modified since last timestamp
)

type Result struct {
	Action        Action
	NewMaxPK      string
	OldMaxPK      string
	NewCount      int64
	OldCount      int64
	FileModTime   time.Time
	LastTimestamp time.Time
	// Periodic marks the full_scan_interval scan: the worker sends every row to PG for a full
	// compare instead of only rows whose values changed since its last write.
	Periodic bool
}

type Detector struct {
	conn     *accessdb.Conn
	pool     *pgxpool.Pool
	cfg      *config.EntityConfig
	stateR   *state.Repository
	mdbPath  string

	// lastPeriodic is in memory on purpose: zero at startup, so the first scan after a restart
	// is a full compare. ss_lastHashScan also moves on change-triggered scans.
	lastPeriodic time.Time
}

func New(conn *accessdb.Conn, pool *pgxpool.Pool, cfg *config.EntityConfig, stateR *state.Repository, mdbPath string) *Detector {
	return &Detector{conn: conn, pool: pool, cfg: cfg, stateR: stateR, mdbPath: mdbPath}
}

func (d *Detector) Detect(ctx context.Context) (*Result, error) {
	st, err := d.stateR.Get(ctx, d.cfg.Entity)
	if err != nil {
		st = &state.SyncState{Entity: d.cfg.Entity}
	}

	r := &Result{}

	info, err := os.Stat(d.mdbPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", d.mdbPath, err)
	}
	// PG stores the modtime with microsecond precision; NTFS has 100ns. Without truncating, the
	// stored value is always "older" and the gate below never closes.
	r.FileModTime = info.ModTime().Truncate(time.Microsecond)

	sinceScan := time.Duration(1<<63 - 1)
	if st.LastHashScan != nil {
		sinceScan = time.Since(*st.LastHashScan)
	}

	// Periodic full scan runs before the modtime gate: the .mdb modtime can stay stale while the
	// VB6 app holds the file open.
	if d.lastPeriodic.IsZero() ||
		(d.cfg.Detection.FullScanInterval > 0 && time.Since(d.lastPeriodic) >= d.cfg.Detection.FullScanInterval) {
		d.lastPeriodic = time.Now()
		r.Periodic = true
		return d.fullScan(ctx, st, r)
	}

	// Layer 0: file modtime — skip if .mdb unchanged
	if st.LastFileModTime != nil && !r.FileModTime.After(*st.LastFileModTime) {
		r.Action = ActionNone
		return r, nil
	}

	// The file changed. Edits to existing rows (no MAX/COUNT change, or a timestamp before the
	// last one) are only visible to a full scan. The worker persists the modtime only after a
	// full scan, so until the gap allows one, every cycle lands here and runs the cheap checks.
	if sinceScan >= d.cfg.Detection.ChangeScanGap() {
		return d.fullScan(ctx, st, r)
	}

	// Strategy branch: timestamp vs max_pk
	if d.cfg.Detection.AccessToPG == "timestamp" && d.cfg.Detection.TimestampColumn != "" {
		return d.detectTimestamp(ctx, st, r)
	}
	return d.detectMaxPK(ctx, st, r)
}

func (d *Detector) detectTimestamp(ctx context.Context, st *state.SyncState, r *Result) (*Result, error) {
	// Layer 3: COUNT — detect deletes
	count, err := d.countAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	r.NewCount = count
	if st.LastCount != nil {
		r.OldCount = *st.LastCount
		if count < *st.LastCount {
			r.Action = ActionFullScan
			return r, nil
		}
	}

	// First run: no timestamp yet → full scan
	if st.LastTimestamp == nil {
		r.Action = ActionFullScan
		return r, nil
	}

	// File modtime changed → read by timestamp
	r.LastTimestamp = *st.LastTimestamp
	r.Action = ActionTimestamp
	return r, nil
}

func (d *Detector) detectMaxPK(ctx context.Context, st *state.SyncState, r *Result) (*Result, error) {
	// Layer 2: MAX(PK)
	maxPK, err := d.maxAccessPK(ctx)
	if err != nil {
		return nil, fmt.Errorf("max pk: %w", err)
	}
	r.NewMaxPK = maxPK
	r.OldMaxPK = ""
	if st.LastMaxPK != nil {
		r.OldMaxPK = *st.LastMaxPK
	}

	if r.OldMaxPK != "" && r.NewMaxPK != r.OldMaxPK {
		oldN, _ := strconv.ParseInt(r.OldMaxPK, 10, 64)
		newN, _ := strconv.ParseInt(r.NewMaxPK, 10, 64)
		if newN > oldN {
			r.Action = ActionIncremental
		}
	} else if r.OldMaxPK == "" && r.NewMaxPK != "" {
		r.Action = ActionFullScan
	}

	// Layer 3: COUNT — detect deletes
	count, err := d.countAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	r.NewCount = count
	if st.LastCount != nil {
		r.OldCount = *st.LastCount
		if count < *st.LastCount {
			r.Action = ActionFullScan
		}
		// Text PKs never trigger ActionIncremental (MAX can't be compared numerically), so inserts surface here.
		if r.Action == ActionNone && count > *st.LastCount {
			r.Action = ActionFullScan
		}
	}

	if r.Action == ActionNone && r.OldMaxPK == "" {
		r.Action = ActionFullScan
	}

	return r, nil
}

// fullScan returns ActionFullScan with the max PK / count the worker persists afterwards.
func (d *Detector) fullScan(ctx context.Context, st *state.SyncState, r *Result) (*Result, error) {
	r.Action = ActionFullScan
	if st.LastCount != nil {
		r.OldCount = *st.LastCount
	}
	count, err := d.countAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	r.NewCount = count

	if d.cfg.Detection.AccessToPG == "timestamp" && d.cfg.Detection.TimestampColumn != "" {
		return r, nil
	}
	if st.LastMaxPK != nil {
		r.OldMaxPK = *st.LastMaxPK
	}
	maxPK, err := d.maxAccessPK(ctx)
	if err != nil {
		return nil, fmt.Errorf("max pk: %w", err)
	}
	r.NewMaxPK = maxPK
	return r, nil
}

func (d *Detector) maxAccessPK(ctx context.Context) (string, error) {
	q := fmt.Sprintf("SELECT MAX(%s) FROM %s%s", d.cfg.Access.PK, d.cfg.Access.Table, reader.WhereClause("", d.cfg.Access.Filter))
	row := d.conn.QueryRow(ctx, q)
	var v interface{}
	if err := row.Scan(&v); err != nil {
		return "", err
	}
	if v == nil {
		return "0", nil
	}
	switch val := v.(type) {
	case int64:
		return strconv.FormatInt(val, 10), nil
	case float64:
		return strconv.FormatInt(int64(val), 10), nil
	case int32:
		return strconv.FormatInt(int64(val), 10), nil
	case []byte:
		return string(val), nil
	case string:
		return val, nil
	default:
		return fmt.Sprintf("%v", val), nil
	}
}

func (d *Detector) countAccess(ctx context.Context) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", d.cfg.Access.Table, reader.WhereClause("", d.cfg.Access.Filter))
	row := d.conn.QueryRow(ctx, q)
	var c int64
	if err := row.Scan(&c); err != nil {
		return 0, err
	}
	return c, nil
}
