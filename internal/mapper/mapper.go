package mapper

import (
	"context"
	"fmt"
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

type Forward struct {
	fields []config.FieldMap
	cfg    *config.EntityConfig
	fk     FKResolver
	ctx    context.Context
}

func NewForward(cfg *config.EntityConfig) *Forward {
	return &Forward{fields: cfg.Fields, cfg: cfg}
}

func (m *Forward) WithFK(ctx context.Context, fk FKResolver) *Forward {
	return &Forward{fields: m.fields, cfg: m.cfg, fk: fk, ctx: ctx}
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

		if f.Access == m.cfg.Access.PK {
			pkVal = fmt.Sprintf("%v", raw)
		}

		if f.Type == "access_datetime" {
			v, err := combineDateTime(raw, row, f)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", f.Access, err)
			}
			cols = append(cols, f.PG)
			vals = append(vals, v)
			continue
		}

		v, err := convertValue(raw, f)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Access, err)
		}

		if f.FKResolve != nil && m.fk != nil {
			legacyID := fmt.Sprintf("%v", v)
			if legacyID != "" && legacyID != "0" && legacyID != "<nil>" {
				pgID, err := m.fk.ResolveLegacyToPG(m.ctx, f.FKResolve.Entity, legacyID)
				if err != nil {
					return nil, fmt.Errorf("field %s: fk_resolve %s: %w", f.Access, f.FKResolve.Entity, err)
				}
				v = pgID
			} else if f.Nullable {
				v = nil
			}
		}

		cols = append(cols, f.PG)
		vals = append(vals, v)
	}

	return &MappedRow{PGCols: cols, PGVals: vals, PKVal: pkVal}, nil
}

type ChildForward struct {
	fields []config.FieldMap
	cfg    *config.ChildConfig
}

func NewChildForward(cfg *config.ChildConfig) *ChildForward {
	return &ChildForward{fields: cfg.Fields, cfg: cfg}
}

func (m *ChildForward) Map(row reader.Row) (*MappedRow, error) {
	cols := make([]string, 0, len(m.fields))
	vals := make([]interface{}, 0, len(m.fields))
	var pkVal string

	for _, f := range m.fields {
		raw, ok := row[f.Access]
		if !ok {
			continue
		}

		if f.Access == m.cfg.Access.PK {
			pkVal = fmt.Sprintf("%v", raw)
		}

		v, err := convertValue(raw, f)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Access, err)
		}
		cols = append(cols, f.PG)
		vals = append(vals, v)
	}

	return &MappedRow{PGCols: cols, PGVals: vals, PKVal: pkVal}, nil
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
		if f.Transform == "trim" {
			s = strings.TrimSpace(s)
		}
		return s, nil
	case "access_bool":
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
		return toDate(raw)
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
	case float64:
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

func zeroForType(t string) interface{} {
	switch t {
	case "int":
		return int64(0)
	case "string":
		return ""
	case "access_bool", "access_bool_inverse":
		return false
	case "float", "decimal":
		return float64(0)
	case "date":
		return nil
	default:
		return nil
	}
}
