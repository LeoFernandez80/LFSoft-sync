// Package stock synchronizes stock movements between the legacy Stock table (Articulo.mdb) and
// articulos."stockMovements" + "stockMovementLines".
//
// The two models don't map field by field, so this is not a generic entity:
//   - Legacy: one Stock row per line, header fields repeated on each row, no line key. Every
//     row carries its own Movimiento_Tipo (a conversion is rows of type 6 and 57 with the same
//     Movimiento_Numero). The sign comes from the type (< 50 adds, >= 50 subtracts); a negative
//     Articulo_Cantidad (supplier credit notes) inverts it.
//   - App: a header (section, type, origin, status) and lines with type, sign, direction and a
//     quantity that is always > 0 (the sign carries the direction; a reversal inverts the sign).
//
// Neither side edits a movement once saved (the legacy only inserts or deletes whole movements;
// the app annuls with a reversal movement), so a movement is copied whole, once, in each
// direction, identified by its number:
//   - Access -> PG: numbers missing in PG are inserted (createdBy = "legacy"); legacy movements
//     deleted in Access are deleted in PG.
//   - PG -> Access: movements created in the app are inserted into Stock, one row per line. If
//     the legacy already used that number, the app movement is renumbered first.
package stock

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lfsoft/shared/accessdb"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/detector"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
	"github.com/lfsoft/sync/internal/state"
	"github.com/lfsoft/sync/internal/synclog"
	"go.uber.org/zap"
)

// LegacyCreatedBy marks PG movements that came from Access: only those are deleted when they
// disappear from Access, and they are never written back.
const LegacyCreatedBy = "legacy"

const (
	chunk                 = 200
	maxDeleteMissingRatio = 0.10
	maxDeleteMissingRows  = 10
)

var accessCols = []string{
	"Movimiento_Numero", "Movimiento_Fecha", "Movimiento_Tipo", "Movimiento_Observacion",
	"Factura_Numero", "Cliente_Proveedor", "Articulo_Codigo", "Articulo_Cantidad", "Articulo_Precio",
	"Movimiento_Responsable", "Movimiento_Controladopor", "NumeroNP", "Movimiento_ROT",
}

type Syncer struct {
	pool     *pgxpool.Pool
	conn     *accessdb.Conn
	cfg      *config.EntityConfig
	mdbPath  string
	stateR   *state.Repository
	synclogR *synclog.Repository
	changes  *detector.PGChanges
	log      *zap.SugaredLogger
	dryRun   bool
	trigger  chan struct{}

	lastPeriodic time.Time
	// unstorable remembers Access movements without a single line PG can hold (placeholder rows
	// with no article, codes missing from the article master) so change scans don't re-read them.
	unstorable map[int64]bool
}

func New(pool *pgxpool.Pool, conn *accessdb.Conn, cfg *config.EntityConfig, mdbBasePath string,
	stateR *state.Repository, synclogR *synclog.Repository, log *zap.SugaredLogger, dryRun bool) *Syncer {
	return &Syncer{
		pool: pool, conn: conn, cfg: cfg, mdbPath: cfg.MDBPath(mdbBasePath),
		stateR: stateR, synclogR: synclogR, changes: detector.NewPGChanges(pool),
		log: log.With("entity", cfg.Entity), dryRun: dryRun,
		trigger: make(chan struct{}, 1), unstorable: make(map[int64]bool),
	}
}

func (s *Syncer) table() string { return fmt.Sprintf(`%s."stockMovements"`, s.cfg.PG.Schema) }
func (s *Syncer) lines() string { return fmt.Sprintf(`%s."stockMovementLines"`, s.cfg.PG.Schema) }

// Trigger wakes the PG -> Access loop (LISTEN sync_change).
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Syncer) interval() time.Duration {
	if s.cfg.Sync.Interval > 0 {
		return s.cfg.Sync.Interval
	}
	return 10 * time.Second
}

// RunAccessToPG polls Access until ctx is done.
func (s *Syncer) RunAccessToPG(ctx context.Context) {
	s.log.Infow("stock worker started", "interval", s.interval())
	t := time.NewTicker(s.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Cycle(ctx, false)
		}
	}
}

// RunPGToAccess writes app movements to Access until ctx is done.
func (s *Syncer) RunPGToAccess(ctx context.Context) {
	s.log.Infow("stock pg worker started", "interval", s.interval())
	t := time.NewTicker(s.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			s.PGCycle(ctx)
		case <-t.C:
			s.PGCycle(ctx)
		}
	}
}

// ---------------------------------------------------------------- Access -> PG

// Cycle compares the movement numbers of both sides and copies what is missing. Unless force,
// it only runs when Articulo.mdb changed or the periodic scan is due. Returns nil when idle.
func (s *Syncer) Cycle(ctx context.Context, force bool) *synclog.Entry {
	info, err := os.Stat(s.mdbPath)
	if err != nil {
		return s.fail(ctx, time.Now(), fmt.Errorf("stat %s: %w", s.mdbPath, err))
	}
	modTime := info.ModTime().Truncate(time.Microsecond)
	st, _ := s.stateR.Get(ctx, s.cfg.Entity)
	periodic := force || s.lastPeriodic.IsZero() ||
		(s.cfg.Detection.FullScanInterval > 0 && time.Since(s.lastPeriodic) >= s.cfg.Detection.FullScanInterval)
	if !periodic {
		if st != nil && st.LastFileModTime != nil && !modTime.After(*st.LastFileModTime) {
			return nil
		}
		if st != nil && st.LastHashScan != nil && time.Since(*st.LastHashScan) < s.cfg.Detection.ChangeScanGap() {
			return nil
		}
	} else {
		s.lastPeriodic = time.Now()
		s.unstorable = make(map[int64]bool)
	}

	start := time.Now()
	entry := &synclog.Entry{Entity: s.cfg.Entity, Direction: "access_to_pg", StartedAt: start, Strategy: "full_scan"}

	accessNums, err := s.accessNumbers(ctx)
	if err != nil {
		return s.fail(ctx, start, fmt.Errorf("read access numbers: %w", err))
	}
	pgNums, err := s.pgNumbers(ctx)
	if err != nil {
		return s.fail(ctx, start, fmt.Errorf("read pg numbers: %w", err))
	}

	var missing []int64
	for n := range accessNums {
		if _, ok := pgNums[n]; !ok && !s.unstorable[n] {
			missing = append(missing, n)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	entry.Detected = len(missing)

	if len(missing) > 0 && !s.dryRun {
		lk, err := s.loadLookups(ctx)
		if err != nil {
			return s.fail(ctx, start, fmt.Errorf("load lookups: %w", err))
		}
		for i := 0; i < len(missing); i += chunk {
			end := i + chunk
			if end > len(missing) {
				end = len(missing)
			}
			rows, err := s.readMovements(ctx, missing[i:end])
			if err != nil {
				return s.fail(ctx, start, fmt.Errorf("read movements: %w", err))
			}
			movs, skippedLines := buildMovements(rows, lk)
			entry.Skipped += skippedLines
			built := make(map[int64]bool, len(movs))
			for _, m := range movs {
				built[m.Number] = true
			}
			for _, n := range missing[i:end] {
				if !built[n] {
					s.unstorable[n] = true
				}
			}
			written, err := s.insertMovements(ctx, movs)
			if err != nil {
				entry.Errors++
				setDetail(entry, err)
				s.log.Errorw("insert movements failed", "from", missing[i], "err", err)
				continue
			}
			entry.Written += written
		}
		if entry.Written > 0 {
			s.bumpSequence(ctx)
		}
	}

	// Legacy movements deleted in Access (supplier invoice / invoice deletions).
	if !s.dryRun {
		var gone []int64
		legacyCount := 0
		for n, createdBy := range pgNums {
			if createdBy != LegacyCreatedBy {
				continue
			}
			legacyCount++
			if _, ok := accessNums[n]; !ok {
				gone = append(gone, n)
			}
		}
		if len(gone) > 0 {
			if len(gone) > maxDeleteMissingRows && float64(len(gone)) > float64(legacyCount)*maxDeleteMissingRatio {
				s.log.Warnw("delete missing aborted", "would_delete", len(gone), "legacy_rows", legacyCount)
			} else if n, err := s.deleteMovements(ctx, gone); err != nil {
				s.log.Warnw("delete missing failed", "err", err)
			} else if n > 0 {
				s.log.Infow("deleted movements missing from access", "count", n)
			}
		}
	}

	now := time.Now()
	count := int64(len(accessNums))
	if err := s.stateR.Upsert(ctx, &state.SyncState{Entity: s.cfg.Entity, LastFileModTime: &modTime, LastHashScan: &now, LastCount: &count}); err != nil {
		s.log.Warnw("state upsert failed", "err", err)
	}
	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	if entry.Detected > 0 || entry.Errors > 0 {
		if err := s.synclogR.Insert(ctx, entry); err != nil {
			s.log.Warnw("synclog insert failed", "err", err)
		}
		s.log.Infow("cycle complete", "detected", entry.Detected, "written", entry.Written,
			"skipped_lines", entry.Skipped, "unstorable", len(s.unstorable), "duration_ms", entry.DurationMs)
	}
	return entry
}

func (s *Syncer) fail(ctx context.Context, start time.Time, err error) *synclog.Entry {
	s.log.Errorw("cycle error", "err", err)
	detail := err.Error()
	e := &synclog.Entry{Entity: s.cfg.Entity, Direction: "access_to_pg", StartedAt: start, FinishedAt: time.Now(),
		Strategy: "full_scan", Errors: 1, ErrorDetail: &detail, DurationMs: int(time.Since(start).Milliseconds())}
	_ = s.synclogR.Insert(ctx, e)
	return e
}

func setDetail(e *synclog.Entry, err error) {
	if e.ErrorDetail == nil {
		d := err.Error()
		e.ErrorDetail = &d
	}
}

func (s *Syncer) accessNumbers(ctx context.Context) (map[int64]bool, error) {
	out := make(map[int64]bool)
	err := s.conn.WithLock(func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, "SELECT DISTINCT [Movimiento_Numero] FROM [Stock] WHERE [Movimiento_Numero] IS NOT NULL")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v interface{}
			if err := rows.Scan(&v); err != nil {
				return err
			}
			if n, err := mapper.ToInt64(v); err == nil {
				out[n] = true
			}
		}
		return rows.Err()
	})
	return out, err
}

func (s *Syncer) pgNumbers(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT "stockMovement_number", COALESCE("stockMovement_createdBy", '') FROM %s`, s.table()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var n int64
		var by string
		if err := rows.Scan(&n, &by); err != nil {
			return nil, err
		}
		out[n] = by
	}
	return out, rows.Err()
}

func (s *Syncer) readMovements(ctx context.Context, nums []int64) ([]reader.Row, error) {
	list := make([]string, len(nums))
	for i, n := range nums {
		list[i] = fmt.Sprintf("%d", n)
	}
	sel := make([]string, len(accessCols))
	for i, c := range accessCols {
		sel[i] = "[" + c + "]"
	}
	q := fmt.Sprintf("SELECT %s FROM [Stock] WHERE [Movimiento_Numero] IN (%s)", strings.Join(sel, ", "), strings.Join(list, ","))
	var out []reader.Row
	err := s.conn.WithLock(func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		dest := make([]interface{}, len(accessCols))
		ptrs := make([]interface{}, len(accessCols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			r := make(reader.Row, len(accessCols))
			for i, c := range accessCols {
				r[c] = dest[i]
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------- lookups

type movType struct {
	ID      int64
	Code    int64
	Sign    int64
	Section string
}

type lookups struct {
	typesByCode map[int64]movType
	typesByID   map[int64]movType
	articles    map[string]int64 // lower(trim(code)) -> id
	articleCode map[int64]string
	customers   map[int64]int64 // legacyCode -> id
	customerCod map[int64]int64 // id -> legacyCode
	quotes      map[int64]int64 // number -> id
	quoteNum    map[int64]int64 // id -> number
	invs        map[int64]int64 // number -> id
	invNum      map[int64]int64 // id -> number
}

func (s *Syncer) loadLookups(ctx context.Context) (*lookups, error) {
	lk := &lookups{
		typesByCode: map[int64]movType{}, typesByID: map[int64]movType{},
		articles: map[string]int64{}, articleCode: map[int64]string{},
		customers: map[int64]int64{}, customerCod: map[int64]int64{},
		quotes: map[int64]int64{}, quoteNum: map[int64]int64{},
		invs: map[int64]int64{}, invNum: map[int64]int64{},
	}
	sch := s.cfg.PG.Schema
	load := func(q string, fn func(pgx.Rows) error) error {
		rows, err := s.pool.Query(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := load(fmt.Sprintf(`SELECT "stockMovementType_id", "stockMovementType_code", "stockMovementType_sign", "stockMovementType_section" FROM %s."stockMovementTypes"`, sch), func(r pgx.Rows) error {
		var t movType
		var sign int16
		if err := r.Scan(&t.ID, &t.Code, &sign, &t.Section); err != nil {
			return err
		}
		t.Sign = int64(sign)
		lk.typesByCode[t.Code] = t
		lk.typesByID[t.ID] = t
		return nil
	}); err != nil {
		return nil, fmt.Errorf("types: %w", err)
	}
	// Lowest id wins on codes that differ only in case (Jet compares case-insensitively).
	if err := load(fmt.Sprintf(`SELECT "articulo_id", "articulo_codigo" FROM %s."articulos" ORDER BY "articulo_id"`, sch), func(r pgx.Rows) error {
		var id int64
		var code string
		if err := r.Scan(&id, &code); err != nil {
			return err
		}
		k := strings.ToLower(strings.TrimSpace(code))
		if _, ok := lk.articles[k]; !ok {
			lk.articles[k] = id
		}
		lk.articleCode[id] = code
		return nil
	}); err != nil {
		return nil, fmt.Errorf("articulos: %w", err)
	}
	if err := load(`SELECT "customer_id", "customer_legacyCode" FROM sales."customers" WHERE "customer_legacyCode" IS NOT NULL`, func(r pgx.Rows) error {
		var id, code int64
		if err := r.Scan(&id, &code); err != nil {
			return err
		}
		lk.customers[code] = id
		lk.customerCod[id] = code
		return nil
	}); err != nil {
		return nil, fmt.Errorf("customers: %w", err)
	}
	if err := load(`SELECT "quote_id", "quote_number" FROM sales."quotes"`, func(r pgx.Rows) error {
		var id, num int64
		if err := r.Scan(&id, &num); err != nil {
			return err
		}
		lk.quotes[num] = id
		lk.quoteNum[id] = num
		return nil
	}); err != nil {
		return nil, fmt.Errorf("quotes: %w", err)
	}
	if err := load(fmt.Sprintf(`SELECT "stockInventory_id", "stockInventory_number" FROM %s."stockInventories"`, sch), func(r pgx.Rows) error {
		var id, num int64
		if err := r.Scan(&id, &num); err != nil {
			return err
		}
		lk.invs[num] = id
		lk.invNum[id] = num
		return nil
	}); err != nil {
		return nil, fmt.Errorf("inventories: %w", err)
	}
	return lk, nil
}

// ---------------------------------------------------------------- building

type line struct {
	Number    int
	Direction string
	TypeID    int64
	Sign      int64
	Articulo  int64
	Quantity  float64
	UnitCost  float64
}

type movement struct {
	Number        int64
	Date          time.Time
	Section       string
	TypeID        *int64
	Observation   *string
	SupplierCode  *int64
	CustomerID    *int64
	InvoiceNumber *int64
	Responsible   *string
	ControlledBy  *string
	Rot           *string
	Origin        string
	OriginID      *int64
	TotalCost     float64
	Lines         []line
}

var (
	supplierTypes = map[int64]bool{1: true, 11: true, 54: true}
	customerTypes = map[int64]bool{2: true, 10: true, 51: true, 52: true, 59: true}
	invoiceTypes  = map[int64]bool{1: true, 11: true, 51: true, 54: true}
	quoteTypes    = map[int64]bool{2: true, 10: true, 52: true, 59: true}
)

func num(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	if n, err := mapper.ToInt64(v); err == nil {
		return float64(n), true
	}
	return 0, false
}

func intOf(v interface{}) int64 {
	f, _ := num(v)
	return int64(f)
}

func text(v interface{}, max int) *string {
	if v == nil {
		return nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		s = fmt.Sprintf("%v", x)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return &s
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// buildMovements groups Stock rows by number and maps each group to a movement. Rows PG can't
// hold (no article, or an article or type PG doesn't have) are left out and counted; a movement
// with no line left is not returned. Zero quantities are kept (articulos V029 allows them).
func buildMovements(rows []reader.Row, lk *lookups) ([]*movement, int) {
	groups := map[int64][]reader.Row{}
	var order []int64
	for _, r := range rows {
		n := intOf(r["Movimiento_Numero"])
		if _, ok := groups[n]; !ok {
			order = append(order, n)
		}
		groups[n] = append(groups[n], r)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var out []*movement
	skipped := 0
	for _, n := range order {
		rs := groups[n]
		// Stable line order: the legacy has no line number.
		sort.SliceStable(rs, func(i, j int) bool {
			ci, cj := strings.ToLower(fmt.Sprint(rs[i]["Articulo_Codigo"])), strings.ToLower(fmt.Sprint(rs[j]["Articulo_Codigo"]))
			if ci != cj {
				return ci < cj
			}
			qi, _ := num(rs[i]["Articulo_Cantidad"])
			qj, _ := num(rs[j]["Articulo_Cantidad"])
			return qi < qj
		})
		m := &movement{Number: n, Origin: "MANUAL"}
		var headerType *movType
		conversion := false
		for _, r := range rs {
			code := intOf(r["Movimiento_Tipo"])
			if code == 6 || code == 57 {
				conversion = true
			}
			if t, ok := lk.typesByCode[code]; ok && headerType == nil {
				tt := t
				headerType = &tt
			}
			if m.Date.IsZero() {
				if d, ok := r["Movimiento_Fecha"].(time.Time); ok && !d.IsZero() {
					m.Date = d
				}
			}
			if m.Observation == nil {
				m.Observation = text(r["Movimiento_Observacion"], 0)
			}
			if m.Responsible == nil {
				m.Responsible = text(r["Movimiento_Responsable"], 3)
			}
			if m.ControlledBy == nil {
				m.ControlledBy = text(r["Movimiento_Controladopor"], 3)
			}
			if m.Rot == nil {
				m.Rot = text(r["Movimiento_ROT"], 20)
			}

			t, okType := lk.typesByCode[code]
			artCode := ""
			if c := text(r["Articulo_Codigo"], 0); c != nil {
				artCode = strings.ToLower(*c)
			}
			art, okArt := lk.articles[artCode]
			qty, _ := num(r["Articulo_Cantidad"])
			if !okType || !okArt {
				skipped++
				continue
			}
			sign := t.Sign
			if qty < 0 {
				sign, qty = -sign, -qty
			}
			dir := "IN"
			if t.Sign < 0 {
				dir = "OUT"
			}
			cost, _ := num(r["Articulo_Precio"])
			m.Lines = append(m.Lines, line{
				Number: len(m.Lines) + 1, Direction: dir, TypeID: t.ID, Sign: sign, Articulo: art,
				Quantity: round4(qty), UnitCost: round4(cost),
			})
			m.TotalCost += round4(qty) * round4(cost)
		}
		if len(m.Lines) == 0 || headerType == nil {
			continue
		}
		if m.Date.IsZero() {
			m.Date = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		first := rs[0]
		code := headerType.Code
		if conversion {
			m.Section = "CONVERSION"
		} else {
			m.Section = headerType.Section
			id := headerType.ID
			m.TypeID = &id
		}
		cp := intOf(first["Cliente_Proveedor"])
		fac := intOf(first["Factura_Numero"])
		if supplierTypes[code] && cp > 0 {
			m.SupplierCode = &cp
		}
		if customerTypes[code] && cp > 0 {
			if id, ok := lk.customers[cp]; ok {
				m.CustomerID = &id
			}
		}
		if invoiceTypes[code] && fac > 0 && fac <= math.MaxInt32 {
			m.InvoiceNumber = &fac
		}
		switch {
		case quoteTypes[code]:
			m.Origin = "QUOTE"
			if id, ok := lk.quotes[fac]; ok {
				m.OriginID = &id
			}
		case (code == 7 || code == 58) && fac > 0:
			m.Origin = "INVENTORY"
			if id, ok := lk.invs[fac]; ok {
				m.OriginID = &id
			}
		case code == 3:
			m.Origin = "ARTICLE_INITIAL"
		case code == 8 || code == 9:
			m.Origin = "FABREM"
			if id, ok := lk.quotes[fac]; ok {
				m.OriginID = &id
			}
		case code == 11 && intOf(first["NumeroNP"]) > 0:
			m.Origin = "PURCHASE_INVOICE"
		}
		m.TotalCost = round4(m.TotalCost)
		out = append(out, m)
	}
	return out, skipped
}

// ---------------------------------------------------------------- PG writes

func (s *Syncer) insertMovements(ctx context.Context, movs []*movement) (int, error) {
	if len(movs) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
		return 0, err
	}
	insert := fmt.Sprintf(`INSERT INTO %s ("stockMovement_number", "stockMovement_date", "stockMovement_section",
		"stockMovement_type_id", "stockMovement_observation", "stockMovement_supplierCode", "stockMovement_customer_id",
		"stockMovement_invoiceNumber", "stockMovement_responsible", "stockMovement_controlledBy", "stockMovement_rot",
		"stockMovement_origin", "stockMovement_originId", "stockMovement_status", "stockMovement_totalCost",
		"stockMovement_createdBy", "stockMovement_createdAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1, $14, $15, $16)
		ON CONFLICT ("stockMovement_number") DO NOTHING RETURNING "stockMovement_id"`, s.table())
	var lineRows [][]interface{}
	written := 0
	for _, m := range movs {
		var id int64
		err := tx.QueryRow(ctx, insert, m.Number, m.Date, m.Section, m.TypeID, m.Observation, m.SupplierCode,
			m.CustomerID, m.InvoiceNumber, m.Responsible, m.ControlledBy, m.Rot, m.Origin, m.OriginID,
			m.TotalCost, LegacyCreatedBy, m.Date).Scan(&id)
		if err == pgx.ErrNoRows {
			continue // created meanwhile (e.g. by the app with the same number)
		}
		if err != nil {
			return 0, fmt.Errorf("movement %d: %w", m.Number, err)
		}
		for _, l := range m.Lines {
			lineRows = append(lineRows, []interface{}{id, l.Number, l.Direction, l.TypeID, l.Sign, l.Articulo, l.Quantity, l.UnitCost})
		}
		written++
	}
	if len(lineRows) > 0 {
		_, err := tx.CopyFrom(ctx, pgx.Identifier{s.cfg.PG.Schema, "stockMovementLines"},
			[]string{"stockMovementLine_movement_id", "stockMovementLine_lineNumber", "stockMovementLine_direction",
				"stockMovementLine_type_id", "stockMovementLine_sign", "stockMovementLine_articulo_id",
				"stockMovementLine_quantity", "stockMovementLine_unitCost"},
			pgx.CopyFromRows(lineRows))
		if err != nil {
			return 0, fmt.Errorf("lines: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return written, nil
}

func (s *Syncer) deleteMovements(ctx context.Context, nums []int64) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
		return 0, err
	}
	ids := fmt.Sprintf(`SELECT "stockMovement_id" FROM %s WHERE "stockMovement_number" = ANY($1) AND "stockMovement_createdBy" = '%s'`, s.table(), LegacyCreatedBy)
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s."stockInventoryLines" SET "stockInventoryLine_adjustmentMovement_id" = NULL WHERE "stockInventoryLine_adjustmentMovement_id" IN (%s)`, s.cfg.PG.Schema, ids), nums); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE "stockMovement_id" IN (%s) AND "stockMovement_id" NOT IN (SELECT "stockMovement_reversesMovement_id" FROM %s WHERE "stockMovement_reversesMovement_id" IS NOT NULL)`, s.table(), ids, s.table()), nums)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// bumpSequence keeps the app's numbering (stock_movement_number_seq) above every number the
// legacy used, so the app doesn't hand out one that already exists.
func (s *Syncer) bumpSequence(ctx context.Context) {
	q := fmt.Sprintf(`SELECT setval(seq, m) FROM (
		SELECT pg_get_serial_sequence('%s', 'stockMovement_number')::regclass AS seq,
		       (SELECT MAX("stockMovement_number") FROM %s) AS m) x
		WHERE seq IS NOT NULL AND m IS NOT NULL AND m > COALESCE(pg_sequence_last_value(seq), 0)`, s.table(), s.table())
	if _, err := s.pool.Exec(ctx, q); err != nil {
		s.log.Warnw("bump sequence failed", "err", err)
	}
}

// ---------------------------------------------------------------- PG -> Access

// PGCycle writes to Access the movements the app created since the last cycle.
func (s *Syncer) PGCycle(ctx context.Context) {
	changes, err := s.changes.ReadPending(ctx, s.cfg.Entity, 100)
	if err != nil {
		s.log.Errorw("read changes failed", "dir", "pg_to_access", "err", err)
		return
	}
	if len(changes) == 0 {
		return
	}
	start := time.Now()
	entry := &synclog.Entry{Entity: s.cfg.Entity, Direction: "pg_to_access", StartedAt: start, Strategy: "trigger", Detected: len(changes)}
	var done []int64
	var lk *lookups
	seen := map[int64]bool{}
	for _, ch := range changes {
		if seen[ch.RowID] || ch.Operation == "D" {
			done = append(done, ch.ID)
			continue
		}
		seen[ch.RowID] = true
		if lk == nil {
			if lk, err = s.loadLookups(ctx); err != nil {
				s.log.Errorw("load lookups failed", "dir", "pg_to_access", "err", err)
				return
			}
		}
		written, err := s.writeMovement(ctx, ch.RowID, lk)
		if err != nil {
			entry.Errors++
			setDetail(entry, err)
			s.log.Warnw("write movement failed", "dir", "pg_to_access", "row_id", ch.RowID, "err", err)
			continue
		}
		if written {
			entry.Written++
		} else {
			entry.Skipped++
		}
		done = append(done, ch.ID)
	}
	if err := s.changes.MarkProcessed(ctx, done); err != nil {
		s.log.Warnw("mark processed failed", "err", err)
	}
	entry.FinishedAt = time.Now()
	entry.DurationMs = int(time.Since(start).Milliseconds())
	_ = s.synclogR.Insert(ctx, entry)
	s.log.Infow("cycle complete", "dir", "pg_to_access", "detected", entry.Detected, "written", entry.Written,
		"skipped", entry.Skipped, "errors", entry.Errors, "duration_ms", entry.DurationMs)
}

func (s *Syncer) writeMovement(ctx context.Context, id int64, lk *lookups) (bool, error) {
	var (
		number                                int64
		date                                  time.Time
		obs, resp, ctrl, rot, createdBy, orig *string
		supplier, customer, invoice, originID *int64
	)
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT "stockMovement_number", "stockMovement_date", "stockMovement_observation",
		"stockMovement_responsible", "stockMovement_controlledBy", "stockMovement_rot", "stockMovement_createdBy",
		"stockMovement_origin", "stockMovement_supplierCode", "stockMovement_customer_id", "stockMovement_invoiceNumber",
		"stockMovement_originId" FROM %s WHERE "stockMovement_id" = $1`, s.table()), id).Scan(
		&number, &date, &obs, &resp, &ctrl, &rot, &createdBy, &orig, &supplier, &customer, &invoice, &originID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if createdBy != nil && *createdBy == LegacyCreatedBy {
		return false, nil // came from Access
	}

	type pgLine struct {
		typeID, sign, articulo int64
		qty, cost              float64
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT "stockMovementLine_type_id", "stockMovementLine_sign", "stockMovementLine_articulo_id",
		"stockMovementLine_quantity"::float8, "stockMovementLine_unitCost"::float8 FROM %s
		WHERE "stockMovementLine_movement_id" = $1 ORDER BY "stockMovementLine_lineNumber"`, s.lines()), id)
	if err != nil {
		return false, err
	}
	var ls []pgLine
	for rows.Next() {
		var l pgLine
		var sign int16
		if err := rows.Scan(&l.typeID, &sign, &l.articulo, &l.qty, &l.cost); err != nil {
			rows.Close()
			return false, err
		}
		l.sign = int64(sign)
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(ls) == 0 {
		return false, nil // header without lines yet
	}

	// The legacy may have used this number meanwhile (it numbers with MAX+1): renumber.
	w := mapper.FormatAccessValue
	exists := func(n int64) (bool, error) {
		var c int64
		err := reader.QueryOne(ctx, s.conn, fmt.Sprintf("SELECT COUNT(*) FROM [Stock] WHERE [Movimiento_Numero] = %d", n), &c)
		return c > 0, err
	}
	taken, err := exists(number)
	if err != nil {
		return false, err
	}
	if taken {
		var accessMax, pgMax int64
		var v interface{}
		if err := reader.QueryOne(ctx, s.conn, "SELECT MAX([Movimiento_Numero]) FROM [Stock]", &v); err != nil {
			return false, err
		}
		accessMax = intOf(v)
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(MAX("stockMovement_number"), 0) FROM %s`, s.table())).Scan(&pgMax); err != nil {
			return false, err
		}
		next := accessMax
		if pgMax > next {
			next = pgMax
		}
		next++
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL sync.is_writing = 'true'"); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET "stockMovement_number" = $1 WHERE "stockMovement_id" = $2`, s.table()), next, id); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		s.log.Warnw("stock movement renumbered: the legacy already used its number", "old", number, "new", next)
		number = next
		s.bumpSequence(ctx)
	}

	var factura, cliente int64
	switch {
	case invoice != nil:
		factura = *invoice
	case originID != nil && orig != nil && (*orig == "QUOTE" || *orig == "FABREM"):
		factura = lk.quoteNum[*originID]
	case originID != nil && orig != nil && *orig == "INVENTORY":
		factura = lk.invNum[*originID]
	}
	switch {
	case supplier != nil:
		cliente = *supplier
	case customer != nil:
		cliente = lk.customerCod[*customer]
	}
	day := mapper.LocalCalendarDate(date)
	str := func(p *string) interface{} {
		if p == nil {
			return nil
		}
		return *p
	}

	var stmts []string
	for _, l := range ls {
		t, ok := lk.typesByID[l.typeID]
		if !ok {
			return false, fmt.Errorf("unknown movement type id %d", l.typeID)
		}
		code, ok := lk.articleCode[l.articulo]
		if !ok {
			return false, fmt.Errorf("unknown articulo id %d", l.articulo)
		}
		qty := l.qty * float64(l.sign*t.Sign) // reversal lines (inverted sign) go as negative quantities
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO [Stock] ([Movimiento_Numero], [Movimiento_Fecha], [Movimiento_Tipo],
			[Movimiento_Observacion], [Factura_Numero], [Cliente_Proveedor], [Articulo_Codigo], [Articulo_Cantidad],
			[Articulo_Precio], [Movimiento_Responsable], [Movimiento_Controladopor], [Movimiento_ROT])
			VALUES (%s, %s, %d, %s, %d, %d, %s, %s, %s, %s, %s, %s)`,
			w(number), w(day), t.Code, w(str(obs)), factura, cliente, w(code), w(qty), w(l.cost),
			w(str(resp)), w(str(ctrl)), w(str(rot))))
	}
	if s.dryRun {
		s.log.Infow("dry-run: would write movement", "number", number, "lines", len(stmts))
		return false, nil
	}
	err = s.conn.WithLock(func(db *sql.DB) error {
		for _, q := range stmts {
			if _, err := db.ExecContext(ctx, q); err != nil {
				// No transactions with this driver: undo the rows already inserted.
				_, _ = db.ExecContext(ctx, fmt.Sprintf("DELETE FROM [Stock] WHERE [Movimiento_Numero] = %d", number))
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("insert stock rows of %d: %w", number, err)
	}
	return true, nil
}
