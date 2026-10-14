package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/lookup"
	"github.com/lfsoft/sync/internal/reader"
)

// fakeChildren serves child tables from memory, filtering like WHERE col IN (...).
type fakeChildren struct {
	tables  map[string][]reader.Row
	queries []string
}

func (f *fakeChildren) ReadChildrenIn(_ context.Context, ch *config.ChildConfig, col string, values []interface{}) ([]reader.Row, error) {
	f.queries = append(f.queries, fmt.Sprintf("%s.%s%v", ch.Access.Table, col, values))
	want := make(map[string]bool, len(values))
	for _, v := range values {
		want[lookup.Key(v, true)] = true
	}
	var out []reader.Row
	for _, r := range f.tables[ch.Access.Table] {
		if want[lookup.Key(r[col], true)] {
			out = append(out, r)
		}
	}
	return out, nil
}

func quoteConfig() *config.EntityConfig {
	return &config.EntityConfig{
		Access: config.AccessConfig{Table: "Presupuestos", PK: "Presupuesto_Numero"},
		Fields: []config.FieldMap{{Access: "Presupuesto_Numero", PG: "quote_number", Type: "int"}},
		Children: []config.ChildConfig{{
			Entity: "items",
			Access: config.ChildAccess{Table: "Items", PK: "Item_Numero", ParentFK: "Presupuesto_Numero"},
			PG:     config.ChildPG{Table: "quoteItems", PK: "quoteItem_id", ParentFK: "quoteItem_quote_id"},
			Fields: []config.FieldMap{{Access: "Item_Numero", PG: "quoteItem_number", Type: "int"}},
			Children: []config.ChildConfig{{
				Entity: "details",
				Access: config.ChildAccess{Table: "Detalles", PK: "SubItem_Numero", ParentFK: "Item_Numero", RootFK: "Presupuesto_Numero"},
				PG:     config.ChildPG{Table: "quoteItemDetails", PK: "quoteItemDetail_id", ParentFK: "quoteItemDetail_quoteItem_id"},
				Fields: []config.FieldMap{
					{Access: "SubItem_Numero", PG: "quoteItemDetail_lineNumber", Type: "int"},
					{Access: "Articulo_Cantidad", PG: "quoteItemDetail_quantity", Type: "decimal"},
				},
			}},
		}},
	}
}

func TestDocBuilderNestsByRootAndParent(t *testing.T) {
	src := &fakeChildren{tables: map[string][]reader.Row{
		"Items": {
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(1)},
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(2)},
			{"Presupuesto_Numero": int32(20), "Item_Numero": int16(1)},
		},
		// Item 1 exists in both quotes: details must follow (quote, item), not the item alone.
		"Detalles": {
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(1), "SubItem_Numero": int16(1), "Articulo_Cantidad": 1.0},
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(1), "SubItem_Numero": int16(2), "Articulo_Cantidad": 2.0},
			{"Presupuesto_Numero": int32(20), "Item_Numero": int16(1), "SubItem_Numero": int16(1), "Articulo_Cantidad": 3.0},
		},
	}}
	b := &docBuilder{cfg: quoteConfig(), reader: src}
	docs, err := b.build(context.Background(), []reader.Row{
		{"Presupuesto_Numero": int32(10)},
		{"Presupuesto_Numero": int32(20)},
		{"Presupuesto_Numero": int32(30)}, // no items
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("got %d docs", len(docs))
	}
	if n := len(src.queries); n != 2 {
		t.Errorf("expected one query per child table, got %v", src.queries)
	}

	byPK := map[string]int{}
	for i, d := range docs {
		if d.err != nil {
			t.Fatalf("pk %s: %v", d.pk, d.err)
		}
		byPK[d.pk] = i
	}
	q10 := docs[byPK["10"]].doc
	if len(q10.Children[0]) != 2 {
		t.Fatalf("quote 10 items = %d", len(q10.Children[0]))
	}
	if got := len(q10.Children[0][0].Children[0]); got != 2 {
		t.Errorf("quote 10 item 1 details = %d, want 2", got)
	}
	if got := len(q10.Children[0][1].Children[0]); got != 0 {
		t.Errorf("quote 10 item 2 details = %d, want 0", got)
	}
	q20 := docs[byPK["20"]].doc
	if got := len(q20.Children[0][0].Children[0]); got != 1 {
		t.Errorf("quote 20 item 1 details = %d, want 1", got)
	}
	q30 := docs[byPK["30"]].doc
	if len(q30.Children) != 1 || len(q30.Children[0]) != 0 {
		t.Errorf("quote 30 must carry an empty item collection, got %v", q30.Children)
	}
}

func TestDocBuilderFailsWholeDocumentOnChildError(t *testing.T) {
	cfg := quoteConfig()
	// A bad value in one detail line must fail its quote, not just drop the line: with
	// delete_missing the dropped line would be deleted from PG.
	cfg.Children[0].Children[0].Fields[1].Type = "int"
	src := &fakeChildren{tables: map[string][]reader.Row{
		"Items": {
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(1)},
			{"Presupuesto_Numero": int32(20), "Item_Numero": int16(1)},
		},
		"Detalles": {
			{"Presupuesto_Numero": int32(10), "Item_Numero": int16(1), "SubItem_Numero": int16(1), "Articulo_Cantidad": struct{}{}},
			{"Presupuesto_Numero": int32(20), "Item_Numero": int16(1), "SubItem_Numero": int16(1), "Articulo_Cantidad": int32(1)},
		},
	}}
	b := &docBuilder{cfg: cfg, reader: src}
	docs, err := b.build(context.Background(), []reader.Row{{"Presupuesto_Numero": int32(10)}, {"Presupuesto_Numero": int32(20)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		switch d.pk {
		case "10":
			if d.err == nil || d.doc != nil {
				t.Errorf("quote 10 should fail, got doc=%v err=%v", d.doc, d.err)
			}
		case "20":
			if d.err != nil {
				t.Errorf("quote 20 should build: %v", d.err)
			}
		}
	}
}

func TestConflictColumnsDefault(t *testing.T) {
	ch := quoteConfig().Children[0]
	got := ch.ConflictColumns()
	if len(got) != 2 || got[0] != "quoteItem_quote_id" || got[1] != "quoteItem_number" {
		t.Errorf("ConflictColumns = %v", got)
	}
}

func TestDocBuilderSumsDuplicateRows(t *testing.T) {
	cfg := quoteConfig()
	cfg.Children[0].Children = []config.ChildConfig{{
		Entity: "withdrawals",
		Access: config.ChildAccess{Table: "Movimientos", PK: "Articulo_Codigo", ParentFK: "Item_Numero", RootFK: "Presupuesto_Numero"},
		PG: config.ChildPG{Table: "quoteItemWithdrawals", PK: "quoteItemWithdrawal_id", ParentFK: "quoteItemWithdrawal_quoteItem_id",
			Conflict:       []string{"quoteItemWithdrawal_quoteItem_id", "quoteItemWithdrawal_articleCode"},
			SumOnDuplicate: []string{"quoteItemWithdrawal_quantity"}},
		Fields: []config.FieldMap{
			{Access: "Articulo_Codigo", PG: "quoteItemWithdrawal_articleCode", Type: "string", Transform: "trim"},
			{Access: "Articulo_Cantidad", PG: "quoteItemWithdrawal_quantity", Type: "decimal"},
		},
	}}
	// Same rows as Presup.mdb quote 51904: one article twice with different case.
	src := &fakeChildren{tables: map[string][]reader.Row{
		"Items": {{"Presupuesto_Numero": int32(51904), "Item_Numero": int16(1)}},
		"Movimientos": {
			{"Presupuesto_Numero": int32(51904), "Item_Numero": int16(1), "Articulo_Codigo": "000000000Z101006", "Articulo_Cantidad": 2.0},
			{"Presupuesto_Numero": int32(51904), "Item_Numero": int16(1), "Articulo_Codigo": "000000000z101006", "Articulo_Cantidad": 6.0},
			{"Presupuesto_Numero": int32(51904), "Item_Numero": int16(1), "Articulo_Codigo": "00000000ZVT21006", "Articulo_Cantidad": 4.0},
		},
	}}
	docs, err := (&docBuilder{cfg: cfg, reader: src}).build(context.Background(), []reader.Row{{"Presupuesto_Numero": int32(51904)}})
	if err != nil || docs[0].err != nil {
		t.Fatal(err, docs[0].err)
	}
	w := docs[0].doc.Children[0][0].Children[0]
	if len(w) != 2 {
		t.Fatalf("withdrawals = %d, want 2 (case-insensitive repeats merged)", len(w))
	}
	if q := w[0].Row.PGVals[1]; q != 8.0 {
		t.Errorf("merged quantity = %v, want 8", q)
	}
}
