package mapper

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/reader"
)

type AccessMappedRow struct {
	AccessCols []string
	AccessVals []interface{}
	AccessPK   string
}

type Reverse struct {
	fields []config.FieldMap
	cfg    *config.EntityConfig
	res    Resolvers
}

func NewReverse(cfg *config.EntityConfig) *Reverse {
	return &Reverse{fields: cfg.Fields, cfg: cfg}
}

// NewChildReverse maps rows of a child collection back to its Access table.
func NewChildReverse(ch *config.ChildConfig) *Reverse {
	cfg := &config.EntityConfig{
		Access: config.AccessConfig{Table: ch.Access.Table, PK: ch.Access.PK},
		Fields: ch.Fields,
	}
	return &Reverse{fields: ch.Fields, cfg: cfg}
}

// Value returns the value mapped to an Access column, if the row has it.
func (r *AccessMappedRow) Value(col string) (interface{}, bool) {
	for i, c := range r.AccessCols {
		if c == col {
			return r.AccessVals[i], true
		}
	}
	return nil, false
}

func (m *Reverse) With(res Resolvers) *Reverse {
	return &Reverse{fields: m.fields, cfg: m.cfg, res: res}
}

func (m *Reverse) Map(row reader.Row) (*AccessMappedRow, error) {
	cols := make([]string, 0, len(m.fields))
	vals := make([]interface{}, 0, len(m.fields))
	var accessPK string
	seen := make(map[string]bool, len(m.fields))

	for _, f := range m.fields {
		raw, ok := row[f.PG]
		if !ok {
			continue
		}
		// Several PG columns can come from one Access column (an id and its snapshot text):
		// the first field wins, so list the reversible one (id) first. Split and query lookups
		// can't be reversed and are left out, and so are name parts (first_name/last_name).
		namepart := (f.Transform == "first_name" || f.Transform == "last_name") && len(f.ReverseFrom) == 0
		oneWayQuery := f.Lookup != nil && f.Lookup.Query != "" && f.Lookup.ReverseQuery == ""
		if seen[f.Access] || (f.Split != nil && len(f.ReverseFrom) == 0) || namepart || oneWayQuery {
			continue
		}
		seen[f.Access] = true
		if len(f.ReverseFrom) > 0 && f.ReverseFormat != "" {
			args := make([]interface{}, 0, len(f.ReverseFrom))
			empty := false
			for _, c := range f.ReverseFrom {
				v := row[c]
				if v == nil {
					empty = true
					break
				}
				if n, err := toInt64(v); err == nil {
					v = n
				}
				args = append(args, v)
			}
			cols = append(cols, f.Access)
			if empty {
				vals = append(vals, f.ReverseEmpty)
			} else {
				vals = append(vals, fmt.Sprintf(f.ReverseFormat, args...))
			}
			continue
		}
		if len(f.ReverseFrom) > 0 {
			parts := make([]string, 0, len(f.ReverseFrom))
			for _, c := range f.ReverseFrom {
				if s := strings.TrimSpace(toString(row[c])); s != "" {
					parts = append(parts, s)
				}
			}
			cols = append(cols, f.Access)
			vals = append(vals, strings.Join(parts, " "))
			continue
		}
		if f.Lookup != nil && f.Lookup.ReverseQuery != "" {
			if raw != nil {
				if m.res.Lookup == nil {
					return nil, fmt.Errorf("field %s: lookup: no resolver configured", f.PG)
				}
				l := &config.Lookup{Query: f.Lookup.ReverseQuery, Args: []string{f.PG}}
				match, found, err := m.res.Lookup.Lookup(m.res.ctx(), l, []interface{}{raw})
				if err != nil {
					return nil, fmt.Errorf("field %s: reverse query: %w", f.PG, err)
				}
				if !found {
					match = nil
				}
				raw = match
			}
		} else if f.Lookup != nil {
			if raw != nil {
				if m.res.Lookup == nil {
					return nil, fmt.Errorf("field %s: lookup: no resolver configured", f.PG)
				}
				match, found, err := m.res.Lookup.ReverseLookup(m.res.ctx(), f.Lookup, raw)
				if err != nil {
					return nil, fmt.Errorf("field %s: %w", f.PG, err)
				}
				if !found {
					return nil, fmt.Errorf("field %s: reverse lookup %s: no match for %v", f.PG, f.Lookup.Table, raw)
				}
				raw = match
			}
		}
		if f.Offset != 0 && raw != nil {
			if n, err := toInt64(raw); err == nil {
				raw = n - f.Offset
			}
		}
		if f.Map != nil {
			// The Access side holds the map key (e.g. "SI"), not a typed value.
			mv := reverseMapValue(f.Map, raw)
			// A "true"/"false" key stands for an Access Yes/No column, not for text.
			if s, ok := mv.(string); ok {
				switch strings.ToLower(s) {
				case "true":
					mv = true
				case "false":
					mv = false
				}
			}
			cols = append(cols, f.Access)
			vals = append(vals, mv)
			continue
		}

		if f.Access == m.cfg.Access.PK {
			pk := raw
			// PG date columns arrive as UTC midnight; Access stores local midnight.
			if t, isTime := raw.(time.Time); isTime && NeedsDatePK(m.cfg) {
				pk = LocalCalendarDate(t)
			}
			accessPK = fmt.Sprintf("%v", pk)
		}

		if f.Type == "access_datetime" {
			dateVal, timeVal := splitDateTime(raw)
			cols = append(cols, f.Access)
			vals = append(vals, dateVal)
			if f.TimeSource != "" {
				cols = append(cols, f.TimeSource)
				vals = append(vals, timeVal)
			}
			continue
		}

		v, err := convertToAccess(raw, f)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.PG, err)
		}

		if f.FKResolve != nil && m.res.FK != nil && v != nil {
			pgID, err := toInt64(v)
			if err == nil && pgID != 0 {
				legacyID, err := m.res.FK.ResolvePGToLegacy(m.res.ctx(), f.FKResolve.Entity, pgID)
				if err != nil {
					return nil, fmt.Errorf("field %s: fk_resolve %s: %w", f.PG, f.FKResolve.Entity, err)
				}
				v, _ = toInt64(interface{}(legacyID))
			}
		}

		cols = append(cols, f.Access)
		vals = append(vals, v)
	}

	return &AccessMappedRow{AccessCols: cols, AccessVals: vals, AccessPK: accessPK}, nil
}

// reverseMapValue returns the map key whose value equals v (nil when none does).
func reverseMapValue(m map[string]interface{}, v interface{}) interface{} {
	if v == nil {
		return nil
	}
	want := fmt.Sprintf("%v", v)
	if n, err := toInt64(v); err == nil {
		if _, isBool := v.(bool); !isBool {
			want = fmt.Sprintf("%d", n)
		}
	}
	for k, mv := range m {
		if fmt.Sprintf("%v", mv) == want {
			return k
		}
	}
	return nil
}

func (m *Reverse) MapBatch(rows []reader.Row) ([]*AccessMappedRow, error) {
	result := make([]*AccessMappedRow, 0, len(rows))
	for _, row := range rows {
		mr, err := m.Map(row)
		if err != nil {
			return nil, err
		}
		result = append(result, mr)
	}
	return result, nil
}

func convertToAccess(raw interface{}, f config.FieldMap) (interface{}, error) {
	if raw == nil {
		if f.Nullable {
			// Blank sentinels become NULL: Access text fields may reject zero-length strings.
			if len(f.NullSentinels) > 0 && strings.TrimSpace(f.NullSentinels[0]) != "" {
				s := f.NullSentinels[0]
				if f.Type == "int" {
					return toInt64(s)
				}
				return s, nil
			}
			return nil, nil
		}
		return zeroForAccessType(f.Type), nil
	}

	switch f.Type {
	case "int":
		return toAccessInt(raw, f)
	case "string":
		return toString(raw), nil
	case "access_bool":
		return toAccessBoolReverse(raw)
	case "access_bool_inverse":
		v, err := toAccessBoolReverse(raw)
		if err != nil {
			return int16(0), err
		}
		if v == int16(-1) {
			return int16(0), nil
		}
		return int16(-1), nil
	case "int_bool":
		// Numeric 0/1 code in the legacy (e.g. PrecioControlado), not a Jet boolean (-1).
		v, err := toAccessBoolReverse(raw)
		if err != nil {
			return int16(0), err
		}
		if v == int16(-1) {
			return int16(1), nil
		}
		return int16(0), nil
	case "float", "decimal":
		return toFloat64(raw)
	case "date":
		if t, ok := raw.(time.Time); ok {
			return LocalCalendarDate(t), nil
		}
		return raw, nil
	case "time":
		// Jet keeps a time of day as a DateTime on its zero date.
		if t, ok := raw.(pgtype.Time); ok {
			if !t.Valid {
				return nil, nil
			}
			return time.Date(1899, 12, 30, 0, 0, 0, 0, time.Local).Add(time.Duration(t.Microseconds) * time.Microsecond), nil
		}
		return raw, nil
	default:
		return raw, nil
	}
}

// LocalCalendarDate maps a date-only instant to local midnight of its calendar day.
// Rounds to the nearest UTC day so both UTC-midnight and local-midnight inputs resolve to the same day.
func LocalCalendarDate(t time.Time) time.Time {
	d := t.UTC().Add(12 * time.Hour)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
}

// ParsePKTime parses a date PK produced by fmt.Sprint(time.Time) or RFC3339.
func ParsePKTime(s string) (time.Time, error) {
	var err error
	for _, layout := range []string{
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700 -07",
		time.RFC3339,
	} {
		var t time.Time
		if t, err = time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

func toAccessInt(v interface{}, f config.FieldMap) (interface{}, error) {
	n, err := toInt64(v)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(f.Access) {
	default:
		return n, nil
	}
}

// toAccessBoolReverse converts Go bool → Access boolean (-1 = true, 0 = false).
func toAccessBoolReverse(v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case bool:
		if val {
			return int16(-1), nil
		}
		return int16(0), nil
	case int64:
		if val != 0 {
			return int16(-1), nil
		}
		return int16(0), nil
	default:
		return int16(0), fmt.Errorf("cannot convert %T to access bool", v)
	}
}

func splitDateTime(raw interface{}) (interface{}, interface{}) {
	if raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return nil, nil
		}
		dateOnly := time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, v.Location())
		timeStr := fmt.Sprintf("%02d:%02d", v.Hour(), v.Minute())
		return dateOnly, timeStr
	default:
		return raw, nil
	}
}

func zeroForAccessType(t string) interface{} {
	switch t {
	case "int":
		return int64(0)
	case "string":
		return ""
	case "access_bool", "access_bool_inverse", "int_bool":
		return int16(0)
	case "float", "decimal":
		return float64(0)
	case "date":
		return nil
	default:
		return nil
	}
}

// AccessPKFromRow extracts the Access PK value from a PG row.
// For PK directo: PG PK column value.
// For legacyCode: the legacy_code_column value.
// For business_key: the business_key column value.
func AccessPKFromRow(row reader.Row, cfg *config.EntityConfig) string {
	if cfg.PG.BusinessKey != "" {
		if v, ok := row[cfg.PG.BusinessKey]; ok {
			if v == nil {
				return "" // a row created in PG without a legacy key
			}
			if t, isTime := v.(time.Time); isTime && NeedsDatePK(cfg) {
				v = LocalCalendarDate(t)
			}
			return fmt.Sprintf("%v", v)
		}
	}
	if cfg.PG.LegacyCodeColumn != "" {
		if v, ok := row[cfg.PG.LegacyCodeColumn]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	if v, ok := row[cfg.PG.PK]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// NeedsNumericPK returns true if the Access PK type expects numeric values.
func NeedsNumericPK(cfg *config.EntityConfig) bool {
	switch strings.ToLower(cfg.Access.PKType) {
	case "byte", "int", "long", "integer":
		return true
	default:
		return false
	}
}

func NeedsDatePK(cfg *config.EntityConfig) bool {
	switch strings.ToLower(cfg.Access.PKType) {
	case "date", "datetime":
		return true
	default:
		return false
	}
}

// FormatJetDate formats a time.Time for Jet SQL date literal.
func FormatJetDate(t time.Time) string {
	return "#" + t.Format("1/2/2006 3:04:05 PM") + "#"
}

// FormatAccessValue formats a value for Jet SQL inline usage.
func FormatAccessValue(v interface{}) string {
	if v == nil {
		return "NULL"
	}
	switch val := v.(type) {
	case time.Time:
		if val.IsZero() {
			return "NULL"
		}
		return FormatJetDate(val)
	case string:
		escaped := strings.ReplaceAll(val, "'", "''")
		return fmt.Sprintf("'%s'", escaped)
	case []byte:
		escaped := strings.ReplaceAll(string(val), "'", "''")
		return fmt.Sprintf("'%s'", escaped)
	case bool:
		if val {
			return "-1"
		}
		return "0"
	case int16:
		return fmt.Sprintf("%d", val)
	case int32:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case int:
		return fmt.Sprintf("%d", val)
	case float64:
		if val == math.Trunc(val) {
			return fmt.Sprintf("%.0f", val)
		}
		return fmt.Sprintf("%g", val)
	case float32:
		return fmt.Sprintf("%g", val)
	default:
		escaped := strings.ReplaceAll(fmt.Sprintf("%v", val), "'", "''")
		return fmt.Sprintf("'%s'", escaped)
	}
}
