package mapper

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/reader"
)

type fakeLookup struct {
	table map[string]interface{}
	calls [][]interface{}
}

func (f *fakeLookup) Lookup(_ context.Context, l *config.Lookup, args []interface{}) (interface{}, bool, error) {
	f.calls = append(f.calls, args)
	key := ""
	for _, a := range args {
		key += "|" + toString(a)
	}
	v, ok := f.table[key]
	return v, ok, nil
}

func (f *fakeLookup) ReverseLookup(_ context.Context, l *config.Lookup, v interface{}) (interface{}, bool, error) {
	for k, tv := range f.table {
		if toString(tv) == toString(v) {
			return k[1:], true, nil
		}
	}
	return nil, false, nil
}

func valueOf(t *testing.T, mr *MappedRow, col string) interface{} {
	t.Helper()
	for i, c := range mr.PGCols {
		if c == col {
			return mr.PGVals[i]
		}
	}
	t.Fatalf("column %s not mapped", col)
	return nil
}

func TestForwardSplitMapOffsetLookup(t *testing.T) {
	cfg := &config.EntityConfig{
		Access: config.AccessConfig{PK: "Id"},
		Fields: []config.FieldMap{
			{Access: "Id", PG: "id_legacy", Type: "int"},
			{Access: "Cumple", PG: "day", Type: "int", Nullable: true, Split: &config.Split{Sep: "/", Index: 0}},
			{Access: "Cumple", PG: "month", Type: "int", Nullable: true, Split: &config.Split{Sep: "/", Index: 1}},
			{Access: "Activo", PG: "active", Type: "access_bool", Map: map[string]interface{}{"SI": true, "NO": false}},
			{Access: "Moneda", PG: "currency", Type: "int", Offset: 1},
			{Access: "Cliente", PG: "customer_id", Type: "int",
				Lookup: &config.Lookup{Table: "sales.customers", Match: "customer_legacyCode", Select: "customer_id"}},
			{Access: "Sub", PG: "sub_id", Type: "int", Nullable: true, NullSentinels: []string{"0"},
				Lookup: &config.Lookup{Query: "SELECT ...", Args: []string{"Cliente", "Sub"}}},
		},
	}
	lk := &fakeLookup{table: map[string]interface{}{"|42": int32(7), "|42|3": int32(99)}}
	m := NewForward(cfg).With(Resolvers{Lookup: lk})

	row := reader.Row{"Id": int32(1), "Cumple": "05/11", "Activo": "si ", "Moneda": int16(0), "Cliente": float64(42), "Sub": float64(3)}
	mr, err := m.Map(row)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]interface{}{"day": int64(5), "month": int64(11), "active": true, "currency": int64(1), "customer_id": int32(7), "sub_id": int32(99)}
	for col, want := range checks {
		if got := valueOf(t, mr, col); got != want {
			t.Errorf("%s = %#v, want %#v", col, got, want)
		}
	}
	// Query args are normalized: Jet doubles become int64.
	if a := lk.calls[len(lk.calls)-1]; a[0] != int64(42) || a[1] != int64(3) {
		t.Errorf("query args = %#v", a)
	}

	// Empty birthday mask, sub-client 0 (sentinel) and an unknown map value.
	row = reader.Row{"Id": int32(2), "Cumple": "__/__", "Activo": "SI", "Moneda": int16(2), "Cliente": float64(42), "Sub": float64(0)}
	mr, err = m.Map(row)
	if err != nil {
		t.Fatal(err)
	}
	if valueOf(t, mr, "day") != nil || valueOf(t, mr, "month") != nil || valueOf(t, mr, "sub_id") != nil {
		t.Errorf("expected NULLs, got %v", mr.PGVals)
	}

	row["Activo"] = "QUIZAS"
	if _, err := m.Map(row); err == nil {
		t.Error("expected error for value missing from a non-nullable map")
	}

	row["Activo"] = "SI"
	row["Cliente"] = float64(5)
	if _, err := m.Map(row); err == nil {
		t.Error("expected error for a non-nullable lookup without match")
	}
}

func TestReverseDedupesAndInverts(t *testing.T) {
	cfg := &config.EntityConfig{
		Access: config.AccessConfig{PK: "Id"},
		Fields: []config.FieldMap{
			{Access: "Id", PG: "id_legacy", Type: "int"},
			{Access: "Cliente", PG: "customer_id", Type: "int",
				Lookup: &config.Lookup{Table: "sales.customers", Match: "customer_legacyCode", Select: "customer_id"}},
			{Access: "Cliente", PG: "customer_name", Type: "string",
				Lookup: &config.Lookup{Table: "sales.customers", Match: "customer_legacyCode", Select: "customer_businessName"}},
			{Access: "Moneda", PG: "currency", Type: "int", Offset: 1},
			{Access: "Activo", PG: "active", Type: "access_bool", Map: map[string]interface{}{"SI": true, "NO": false}},
			{Access: "Cumple", PG: "day", Type: "int", Split: &config.Split{Sep: "/", Index: 0}},
		},
	}
	lk := &fakeLookup{table: map[string]interface{}{"|42": int32(7)}}
	m := NewReverse(cfg).With(Resolvers{Lookup: lk})
	am, err := m.Map(reader.Row{"id_legacy": int32(1), "customer_id": int32(7), "customer_name": "ACME", "currency": int16(2), "active": false, "day": int16(5)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]interface{}{}
	for i, c := range am.AccessCols {
		if _, dup := got[c]; dup {
			t.Fatalf("access column %s written twice", c)
		}
		got[c] = am.AccessVals[i]
	}
	if _, ok := got["Cumple"]; ok {
		t.Error("split field must not be written back")
	}
	if got["Cliente"] != int64(42) || got["Moneda"] != int64(1) || got["Activo"] != "NO" {
		t.Errorf("reverse values = %#v", got)
	}
}

// queryLookup answers query lookups by query text + args.
type queryLookup map[string]interface{}

func (q queryLookup) Lookup(_ context.Context, l *config.Lookup, args []interface{}) (interface{}, bool, error) {
	key := l.Query
	for _, a := range args {
		key += "|" + toString(a)
	}
	v, ok := q[key]
	return v, ok, nil
}

func (q queryLookup) ReverseLookup(context.Context, *config.Lookup, interface{}) (interface{}, bool, error) {
	return nil, false, nil
}

func TestReverseOnlyCompositeKey(t *testing.T) {
	cfg := &config.EntityConfig{
		Access: config.AccessConfig{PK: "Codigo"},
		Fields: []config.FieldMap{
			{Access: "Codigo", PG: "codigo", Type: "string"},
			{Access: "SubGrupo", PG: "criterio_id", Type: "int", Nullable: true, NullSentinels: []string{"0"},
				Lookup: &config.Lookup{Query: "fwd", Args: []string{"Lista", "SubGrupo"}, ReverseQuery: "sg"}},
			{Access: "Lista", PG: "criterio_id", Type: "int", Nullable: true, NullSentinels: []string{"0"},
				ReverseOnly: true, Lookup: &config.Lookup{ReverseQuery: "lista"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	lk := queryLookup{"fwd|2|5": int32(32), "sg|32": int32(5), "lista|32": int32(2)}

	fw := NewForward(cfg).With(Resolvers{Lookup: lk})
	mr, err := fw.Map(reader.Row{"Codigo": "A1", "Lista": int32(2), "SubGrupo": int16(5)})
	if err != nil {
		t.Fatal(err)
	}
	if len(mr.PGCols) != 2 || valueOf(t, mr, "criterio_id") != int32(32) {
		t.Errorf("forward = %v %v", mr.PGCols, mr.PGVals)
	}
	mr, err = fw.Map(reader.Row{"Codigo": "A2", "Lista": int32(0), "SubGrupo": int16(0)})
	if err != nil || valueOf(t, mr, "criterio_id") != nil {
		t.Errorf("forward 0/0 = %v, %v", mr, err)
	}

	rv := NewReverse(cfg).With(Resolvers{Lookup: lk})
	for _, tc := range []struct {
		id             interface{}
		lista, subGrup interface{}
	}{{int32(32), int64(2), int64(5)}, {nil, int64(0), int64(0)}} {
		am, err := rv.Map(reader.Row{"codigo": "A1", "criterio_id": tc.id})
		if err != nil {
			t.Fatal(err)
		}
		l, _ := am.Value("Lista")
		s, _ := am.Value("SubGrupo")
		if toString(l) != toString(tc.lista) || toString(s) != toString(tc.subGrup) {
			t.Errorf("reverse %v = Lista %#v, SubGrupo %#v", tc.id, l, s)
		}
	}

	bad := config.EntityConfig{Fields: []config.FieldMap{{Access: "X", PG: "x", ReverseOnly: true}}}
	if err := bad.Validate(); err == nil {
		t.Error("reverse_only without reverse_query must fail validation")
	}
}

func TestIntBoolRoundTrip(t *testing.T) {
	f := config.FieldMap{Access: "Articulo_PrecioControlado", PG: "isPriceControlled", Type: "int_bool"}
	for raw, want := range map[int16]bool{1: true, 0: false} {
		v, err := convertValue(raw, f)
		if err != nil || v != want {
			t.Errorf("forward %d = %v, %v", raw, v, err)
		}
	}
	for pg, want := range map[bool]int16{true: 1, false: 0} {
		v, err := convertToAccess(pg, f)
		if err != nil || v != want {
			t.Errorf("reverse %v = %#v, want %d (legacy 0/1 code, not Jet -1)", pg, v, want)
		}
	}
}

func TestChildReverseKeepsKeyColumn(t *testing.T) {
	ch := &config.ChildConfig{
		Access: config.ChildAccess{Table: "Detalles", PK: "SubItem_Numero", ParentFK: "Item_Numero", RootFK: "Presupuesto_Numero"},
		Fields: []config.FieldMap{
			{Access: "SubItem_Numero", PG: "lineNumber", Type: "int"},
			{Access: "Articulo_Codigo", PG: "articleCode", Type: "string"},
			{Access: "Articulo_Codigo", PG: "article_id", Type: "int",
				Lookup: &config.Lookup{Table: "articulos.articulos", Match: "articulo_codigo", Select: "articulo_id"}},
		},
	}
	am, err := NewChildReverse(ch).Map(reader.Row{"detail_id": int32(9), "lineNumber": int32(3), "articleCode": "ABC", "article_id": int32(77)})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := am.Value("SubItem_Numero"); !ok || v != int64(3) {
		t.Errorf("SubItem_Numero = %#v", v)
	}
	if v, _ := am.Value("Articulo_Codigo"); v != "ABC" {
		t.Errorf("Articulo_Codigo = %#v: the plain code must win over the id lookup", v)
	}
	if len(am.AccessCols) != 2 {
		t.Errorf("cols = %v", am.AccessCols)
	}
}

func TestSplitFullName(t *testing.T) {
	cases := []struct{ in, first, last string }{
		{"Lucas Gonzalo de la Torre", "Lucas Gonzalo", "de la Torre"},
		{" Juan Perez ", "Juan", "Perez"},
		{"Hidramic", "", "Hidramic"},
		{"", "", ""},
	}
	for _, c := range cases {
		first, last := splitFullName(c.in)
		if first != c.first || last != c.last {
			t.Errorf("splitFullName(%q) = %q, %q; want %q, %q", c.in, first, last, c.first, c.last)
		}
	}
}

func TestTimeOfDayRoundTrip(t *testing.T) {
	f := config.FieldMap{Access: "Hora", PG: "hora", Type: "time", Nullable: true}
	jet := time.Date(1899, 12, 30, 14, 35, 10, 0, time.UTC)
	v, err := convertRaw(jet, f)
	if err != nil {
		t.Fatal(err)
	}
	pt, ok := v.(pgtype.Time)
	if !ok || !pt.Valid || pt.Microseconds != (14*3600+35*60+10)*1_000_000 {
		t.Fatalf("forward = %#v", v)
	}
	back, err := convertToAccess(pt, f)
	if err != nil {
		t.Fatal(err)
	}
	bt := back.(time.Time)
	if bt.Year() != 1899 || bt.Hour() != 14 || bt.Minute() != 35 || bt.Second() != 10 {
		t.Fatalf("reverse = %v", bt)
	}
	if v, _ := convertRaw(0.5, f); v.(pgtype.Time).Microseconds != 12*3600*1_000_000 {
		t.Fatalf("day fraction = %#v", v)
	}
}

func TestDateNullBefore(t *testing.T) {
	f := config.FieldMap{Access: "F", PG: "f", Type: "date", Nullable: true, NullBefore: "1900-01-01"}
	if v, _ := convertRaw(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC), f); v != nil {
		t.Fatalf("sentinel date = %v, want nil", v)
	}
	d := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	if v, _ := convertRaw(d, f); v != d {
		t.Fatalf("real date = %v", v)
	}
}
