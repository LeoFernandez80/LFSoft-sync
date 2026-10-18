package mapper

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/reader"
)

type MappedRow struct {
	PGCols []string
	PGVals []interface{}
	PKVal  string
}

type FKResolver interface {
	ResolveLegacyToPG(ctx context.Context, entity, legacyID string) (int64, error)
	ResolvePGToLegacy(ctx context.Context, entity string, pgID int64) (string, error)
}

// LookupResolver resolves the lookup option of a field (implemented by internal/lookup).
type LookupResolver interface {
	Lookup(ctx context.Context, l *config.Lookup, args []interface{}) (interface{}, bool, error)
	ReverseLookup(ctx context.Context, l *config.Lookup, v interface{}) (interface{}, bool, error)
}

// Resolvers is what field mapping needs beyond the row itself. Any of them may be nil.
type Resolvers struct {
	Ctx    context.Context
	FK     FKResolver
	Lookup LookupResolver
}

func (r Resolvers) ctx() context.Context {
	if r.Ctx != nil {
		return r.Ctx
	}
	return context.Background()
}

// Forward maps Access rows to PG columns, for a root entity or for a child collection.
type Forward struct {
	fields []config.FieldMap
	pkCol  string
	res    Resolvers
}

func NewForward(cfg *config.EntityConfig) *Forward {
	return &Forward{fields: cfg.Fields, pkCol: cfg.Access.PK}
}

func NewChildForward(cfg *config.ChildConfig) *Forward {
	return &Forward{fields: cfg.Fields, pkCol: cfg.Access.PK}
}

func (m *Forward) With(res Resolvers) *Forward {
	return &Forward{fields: m.fields, pkCol: m.pkCol, res: res}
}

func (m *Forward) Map(row reader.Row) (*MappedRow, error) {
	cols := make([]string, 0, len(m.fields))
	vals := make([]interface{}, 0, len(m.fields))
	var pkVal string

	for _, f := range m.fields {
		raw, ok := row[f.Access]
		if !ok {
			continue
		}

		if f.Access == m.pkCol {
			pkVal = fmt.Sprintf("%v", raw)
		}

		v, err := m.field(f, raw, row)
		if err != nil {
			return nil, fmt.Errorf("field %s → %s: %w", f.Access, f.PG, err)
		}
		cols = append(cols, f.PG)
		vals = append(vals, v)
	}

	return &MappedRow{PGCols: cols, PGVals: vals, PKVal: pkVal}, nil
}

// field converts one Access value: split → map → type conversion → offset → fk_resolve → lookup.
func (m *Forward) field(f config.FieldMap, raw interface{}, row reader.Row) (interface{}, error) {
	if f.Type == "access_datetime" {
		return combineDateTime(raw, row, f)
	}

	if f.Split != nil {
		raw = splitPart(raw, f.Split)
	}
	if f.Map != nil && raw != nil {
		mv, ok := mapValue(f.Map, raw)
		if !ok {
			if f.Nullable {
				return nil, nil
			}
			return nil, fmt.Errorf("value %q not in map", toString(raw))
		}
		raw = mv
	}

	v, err := convertValue(raw, f)
	if err != nil {
		// A split part that isn't a number ("__" of an empty "__/__" mask) is no data.
		if f.Split != nil && f.Nullable {
			return nil, nil
		}
		return nil, err
	}

	if f.Offset != 0 {
		if n, ok := v.(int64); ok {
			v = n + f.Offset
		}
	}

	if f.FKResolve != nil && m.res.FK != nil {
		legacyID := fmt.Sprintf("%v", v)
		if legacyID != "" && legacyID != "0" && legacyID != "<nil>" {
			pgID, err := m.res.FK.ResolveLegacyToPG(m.res.ctx(), f.FKResolve.Entity, legacyID)
			if err != nil {
				return nil, fmt.Errorf("fk_resolve %s: %w", f.FKResolve.Entity, err)
			}
			v = pgID
		} else if f.Nullable {
			v = nil
		}
	}

	if f.Lookup != nil {
		return m.lookup(f, v, row)
	}
	return v, nil
}

func (m *Forward) lookup(f config.FieldMap, v interface{}, row reader.Row) (interface{}, error) {
	l := f.Lookup
	// A NULL (or null sentinel) value has nothing to look up.
	if v == nil {
		return nil, nil
	}
	if m.res.Lookup == nil {
		return nil, fmt.Errorf("lookup: no resolver configured")
	}
	args := []interface{}{v}
	if l.Query != "" {
		args = make([]interface{}, len(l.Args))
		for i, col := range l.Args {
			a := NormalizeArg(row[col])
			if a == nil {
				return nil, nil
			}
			args[i] = a
		}
	}
	res, found, err := m.res.Lookup.Lookup(m.res.ctx(), l, args)
	if err != nil {
		return nil, err
	}
	if !found {
		if f.Nullable {
			return nil, nil
		}
		target := l.Table
		if target == "" {
			target = "query"
		}
		return nil, fmt.Errorf("lookup %s: no match for %v", target, args)
	}
	return res, nil
}

// NormalizeArg turns an Access value into a query parameter PG accepts for integer and text
// columns: integral floats (Jet Double codes) become int64, text is trimmed.
func NormalizeArg(v interface{}) interface{} {
	switch val := v.(type) {
	case float64:
		if val == math.Trunc(val) {
			return int64(val)
		}
	case float32:
		if float64(val) == math.Trunc(float64(val)) {
			return int64(val)
		}
	case int16:
		return int64(val)
	case int32:
		return int64(val)
	case int:
		return int64(val)
	case uint8:
		return int64(val)
	case []byte:
		return strings.TrimSpace(string(val))
	case string:
		return strings.TrimSpace(val)
	}
	return v
}

func splitPart(raw interface{}, s *config.Split) interface{} {
	if raw == nil {
		return nil
	}
	parts := strings.Split(toString(raw), s.Sep)
	if s.Index < 0 || s.Index >= len(parts) {
		return nil
	}
	return strings.TrimSpace(parts[s.Index])
}

// mapValue finds raw among the map keys, trimmed and case-insensitively (Jet compares so).
func mapValue(m map[string]interface{}, raw interface{}) (interface{}, bool) {
	key := strings.TrimSpace(toString(raw))
	for k, v := range m {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return v, true
		}
	}
	return nil, false
}

// DocRow is a mapped root or child row with its mapped children, one slice per configured
// child collection (same order as the config). An empty slice means "Access has none".
type DocRow struct {
	Row      *MappedRow
	Children [][]*DocRow
}

// Fingerprint identifies the mapped values of the whole document, children included. It is a
// hash: workers keep one per document, and a quote with its lines prints to kilobytes.
func (d *DocRow) Fingerprint() string {
	h := fnv.New128a()
	d.writeFingerprint(h)
	return string(h.Sum(nil))
}

func (d *DocRow) writeFingerprint(w io.Writer) {
	fmt.Fprintf(w, "%v", d.Row.PGVals)
	for _, ch := range d.Children {
		io.WriteString(w, "[")
		for _, c := range ch {
			c.writeFingerprint(w)
		}
		io.WriteString(w, "]")
	}
}

func (m *Forward) MapBatch(rows []reader.Row) ([]*MappedRow, error) {
	result := make([]*MappedRow, 0, len(rows))
	for _, row := range rows {
		mr, err := m.Map(row)
		if err != nil {
			return nil, err
		}
		result = append(result, mr)
	}
	return result, nil
}

func convertValue(raw interface{}, f config.FieldMap) (interface{}, error) {
	v, err := convertRaw(raw, f)
	if err != nil || f.ZeroDefault == nil {
		return v, err
	}
	// zero_default: Access 0/NULL in a column PG requires (NOT NULL FK) becomes a fixed value.
	if v == nil || v == int64(0) {
		return *f.ZeroDefault, nil
	}
	return v, nil
}

func convertRaw(raw interface{}, f config.FieldMap) (interface{}, error) {
	if raw == nil {
		if f.Nullable {
			return nil, nil
		}
		return zeroForType(f.Type), nil
	}

	if f.Nullable && isNullSentinel(raw, f.NullSentinels) {
		return nil, nil
	}

	switch f.Type {
	case "int":
		return toInt64(raw)
	case "string":
		s := toString(raw)
		switch f.Transform {
		case "trim":
			s = strings.TrimSpace(s)
		case "first_name":
			s, _ = splitFullName(s)
		case "last_name":
			_, s = splitFullName(s)
		}
		return s, nil
	case "access_bool", "int_bool":
		return toAccessBool(raw)
	case "access_bool_inverse":
		b, err := toAccessBool(raw)
		if err != nil {
			return false, err
		}
		return !b, nil
	case "float", "decimal":
		return toFloat64(raw)
	case "date":
		d, err := toDate(raw)
		if err == nil && f.NullBefore != "" {
			if t, ok := d.(time.Time); ok {
				if limit, perr := time.Parse("2006-01-02", f.NullBefore); perr == nil && t.Before(limit) {
					return nil, nil
				}
			}
		}
		return d, err
	case "time":
		return toTimeOfDay(raw)
	default:
		return raw, nil
	}
}

func toInt64(v interface{}) (int64, error) {
	switch val := v.(type) {
	case int64:
		return val, nil
	case int32:
		return int64(val), nil
	case int16:
		return int64(val), nil
	case int8:
		return int64(val), nil
	case int:
		return int64(val), nil
	case float64:
		return int64(val), nil
	case float32:
		return int64(val), nil
	case uint8:
		return int64(val), nil
	case []byte:
		s := string(val)
		var n int64
		_, err := fmt.Sscanf(s, "%d", &n)
		return n, err
	case string:
		var n int64
		_, err := fmt.Sscanf(val, "%d", &n)
		return n, err
	case bool:
		if val {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}

func toFloat64(v interface{}) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case float32:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case int32:
		return float64(val), nil
	case int16:
		return float64(val), nil
	case int:
		return float64(val), nil
	case uint8:
		return float64(val), nil
	case pgtype.Numeric:
		f, err := val.Float64Value()
		if err != nil {
			return 0, fmt.Errorf("pgtype.Numeric to float64: %w", err)
		}
		return f.Float64, nil
	case []byte:
		var f float64
		_, err := fmt.Sscanf(string(val), "%f", &f)
		return f, err
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// toAccessBool converts Access boolean: -1 = true, 0 = false.
func toAccessBool(v interface{}) (bool, error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	case int64:
		return val != 0, nil
	case int32:
		return val != 0, nil
	case int16:
		return val != 0, nil
	case uint8: // Byte columns used as flags
		return val != 0, nil
	case int8:
		return val != 0, nil
	case float64:
		return val != 0, nil
	case float32:
		return val != 0, nil
	case int:
		return val != 0, nil
	default:
		return false, fmt.Errorf("cannot convert %T to bool", v)
	}
}

func isNullSentinel(v interface{}, sentinels []string) bool {
	if len(sentinels) == 0 {
		return false
	}
	s := toString(v)
	for _, sentinel := range sentinels {
		if s == sentinel {
			return true
		}
	}
	return false
}

func combineDateTime(dateVal interface{}, row reader.Row, f config.FieldMap) (interface{}, error) {
	if dateVal == nil {
		if f.Nullable {
			return nil, nil
		}
		return nil, nil
	}

	var dt time.Time
	switch v := dateVal.(type) {
	case time.Time:
		if v.IsZero() {
			return nil, nil
		}
		dt = v
	case float64:
		dt = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).Add(
			time.Duration(v*24) * time.Hour)
	default:
		return dateVal, nil
	}

	if f.TimeSource != "" {
		if timeRaw, ok := row[f.TimeSource]; ok && timeRaw != nil {
			timeStr := toString(timeRaw)
			timeStr = strings.TrimSpace(timeStr)
			if timeStr != "" && timeStr != "__:__" {
				parts := strings.Split(timeStr, ":")
				if len(parts) >= 2 {
					var h, m int
					fmt.Sscanf(parts[0], "%d", &h)
					fmt.Sscanf(parts[1], "%d", &m)
					dt = time.Date(dt.Year(), dt.Month(), dt.Day(), h, m, 0, 0, time.UTC)
				}
			}
		}
	}

	return dt, nil
}

func toDate(v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case time.Time:
		if val.IsZero() {
			return nil, nil
		}
		return val, nil
	case float64:
		return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).Add(
			time.Duration(val*24) * time.Hour), nil
	default:
		return v, nil
	}
}

// toTimeOfDay keeps the clock part of a Jet time value (a DateTime on 1899-12-30, or the day
// fraction of a Double) for a PG time column.
func toTimeOfDay(v interface{}) (interface{}, error) {
	var h, m, s int
	switch val := v.(type) {
	case time.Time:
		if val.IsZero() {
			return nil, nil
		}
		h, m, s = val.Clock()
	case float64:
		secs := int(math.Round((val - math.Floor(val)) * 86400))
		h, m, s = secs/3600%24, secs/60%60, secs%60
	default:
		return nil, fmt.Errorf("cannot convert %T to time of day", v)
	}
	us := (int64(h)*3600 + int64(m)*60 + int64(s)) * 1_000_000
	return pgtype.Time{Microseconds: us, Valid: true}, nil
}

// lastNameRe takes the last word of a full name plus the particles before it ("de la Torre").
var lastNameRe = regexp.MustCompile(`(?i)((?:\b(?:de|del|la|las|los|van|von|di|da|dos|san|santa)\b\s+)*\S+)$`)

// splitFullName splits the legacy single-column name ("Lucas Gonzalo de la Torre") into first
// and last name, as the utilities ETL did. Joining them with a space gives the trimmed input.
func splitFullName(full string) (first, last string) {
	full = strings.TrimSpace(full)
	last = lastNameRe.FindString(full)
	first = strings.TrimSpace(full[:len(full)-len(last)])
	return first, last
}

func zeroForType(t string) interface{} {
	switch t {
	case "int":
		return int64(0)
	case "string":
		return ""
	case "access_bool", "access_bool_inverse", "int_bool":
		return false
	case "float", "decimal":
		return float64(0)
	case "date":
		return nil
	default:
		return nil
	}
}

// HashValues fingerprints one row's mapped values.
func HashValues(vals []interface{}) string {
	h := fnv.New128a()
	fmt.Fprintf(h, "%v", vals)
	return string(h.Sum(nil))
}

// ToInt64 converts a value read from Access or PG to int64.
func ToInt64(v interface{}) (int64, error) { return toInt64(v) }
