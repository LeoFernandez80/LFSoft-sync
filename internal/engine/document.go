package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/lfsoft/sync/internal/config"
	"github.com/lfsoft/sync/internal/lookup"
	"github.com/lfsoft/sync/internal/mapper"
	"github.com/lfsoft/sync/internal/reader"
)

// docBatchSize is how many root rows are completed with their children per round: each child
// collection is read once per batch (WHERE fk IN (...)), not once per document.
const docBatchSize = 100

// docBuilder maps root Access rows to documents: the mapped root plus its child tree.
// childReader is the part of reader.AccessReader the builder uses.
type childReader interface {
	ReadChildrenIn(ctx context.Context, childCfg *config.ChildConfig, filterCol string, values []interface{}) ([]reader.Row, error)
}

type docBuilder struct {
	cfg    *config.EntityConfig
	reader childReader
	res    mapper.Resolvers
}

// docNode is a row being attached to the tree; rootKey/key are normalized Access keys.
type docNode struct {
	doc     *mapper.DocRow
	pkRaw   interface{} // the row's own Access PK value
	rootRaw interface{}
	rootKey string
	key     string
}

// built is the outcome for one root row: a complete document, or why it couldn't be built.
type built struct {
	pk  string
	doc *mapper.DocRow
	err error
}

// build maps roots and their children. A root whose own row or any child row fails to map is
// returned with err set and no document: writing it partially, with delete_missing on, would
// delete the PG rows of the children that failed. An Access read error fails the whole batch.
func (b *docBuilder) build(ctx context.Context, roots []reader.Row) ([]built, error) {
	m := mapper.NewForward(b.cfg).With(b.res)
	out := make([]built, 0, len(roots))
	failed := make(map[string]error)
	var nodes []*docNode
	for _, row := range roots {
		raw := row[b.cfg.Access.PK]
		pk := fmt.Sprintf("%v", raw)
		mr, err := m.Map(row)
		if err != nil {
			out = append(out, built{pk: pk, err: err})
			continue
		}
		key := lookup.Key(raw, true)
		n := &docNode{
			doc:     &mapper.DocRow{Row: mr, Children: make([][]*mapper.DocRow, len(b.cfg.Children))},
			pkRaw:   raw,
			rootRaw: raw,
			rootKey: key,
			key:     key,
		}
		nodes = append(nodes, n)
	}

	if err := b.fillChildren(ctx, b.cfg.Children, nodes, true, failed); err != nil {
		return nil, err
	}

	for _, n := range nodes {
		pk := n.doc.Row.PKVal
		if err := failed[n.rootKey]; err != nil {
			out = append(out, built{pk: pk, err: err})
			continue
		}
		out = append(out, built{pk: pk, doc: n.doc})
	}
	return out, nil
}

func (b *docBuilder) fillChildren(ctx context.Context, cfgs []config.ChildConfig, parents []*docNode, topLevel bool, failed map[string]error) error {
	if len(parents) == 0 {
		return nil
	}
	for ci := range cfgs {
		ch := &cfgs[ci]

		// Which Access column filters the child rows, and how a child row finds its parent.
		byRoot := !topLevel && ch.Access.RootFK != ""
		filterCol := ch.Access.ParentFK
		parentIndex := make(map[string]*docNode, len(parents))
		var values []interface{}
		seenVal := make(map[string]bool)
		for _, p := range parents {
			idxKey := p.key
			v, vKey := p.pkRaw, p.key
			if byRoot {
				idxKey = p.rootKey + "|" + p.key
				v, vKey = p.rootRaw, p.rootKey
			}
			parentIndex[idxKey] = p
			if !seenVal[vKey] {
				seenVal[vKey] = true
				values = append(values, v)
			}
		}
		if byRoot {
			filterCol = ch.Access.RootFK
		}

		rows, err := b.reader.ReadChildrenIn(ctx, ch, filterCol, values)
		if err != nil {
			return err
		}

		m := mapper.NewChildForward(ch).With(b.res)
		var children []*docNode
		// sum_on_duplicate: first node per (parent, conflict key) absorbs the repeats.
		var merged map[string]*docNode
		if len(ch.PG.SumOnDuplicate) > 0 {
			merged = make(map[string]*docNode)
		}
		for _, row := range rows {
			idxKey := lookup.Key(row[ch.Access.ParentFK], true)
			if byRoot {
				idxKey = lookup.Key(row[ch.Access.RootFK], true) + "|" + idxKey
			}
			p := parentIndex[idxKey]
			if p == nil {
				continue // belongs to a parent outside this batch (or an orphan)
			}
			if failed[p.rootKey] != nil {
				continue
			}
			mr, err := m.Map(row)
			if err != nil {
				failed[p.rootKey] = fmt.Errorf("%s key=%v: %w", ch.Entity, row[ch.Access.PK], err)
				continue
			}
			if merged != nil {
				key := idxKey + "|" + conflictKey(ch, mr)
				if first := merged[key]; first != nil {
					if err := addColumns(first.doc.Row, mr, ch.PG.SumOnDuplicate); err != nil {
						failed[p.rootKey] = fmt.Errorf("%s key=%v: %w", ch.Entity, row[ch.Access.PK], err)
					}
					continue
				}
			}
			n := &docNode{
				doc:     &mapper.DocRow{Row: mr, Children: make([][]*mapper.DocRow, len(ch.Children))},
				pkRaw:   row[ch.Access.PK],
				rootRaw: p.rootRaw,
				rootKey: p.rootKey,
				key:     lookup.Key(row[ch.Access.PK], true),
			}
			p.doc.Children[ci] = append(p.doc.Children[ci], n.doc)
			children = append(children, n)
			if merged != nil {
				merged[idxKey+"|"+conflictKey(ch, mr)] = n
			}
		}

		if err := b.fillChildren(ctx, ch.Children, children, false, failed); err != nil {
			return err
		}
	}
	return nil
}

// conflictKey is the row's conflict key without the parent FK (the caller scopes it to the
// parent), compared case-insensitively as Jet does.
func conflictKey(ch *config.ChildConfig, mr *mapper.MappedRow) string {
	var parts []string
	for _, k := range ch.ConflictColumns() {
		if k == ch.PG.ParentFK {
			continue
		}
		for i, c := range mr.PGCols {
			if c == k {
				parts = append(parts, lookup.Key(mr.PGVals[i], true))
			}
		}
	}
	return strings.Join(parts, "|")
}

// addColumns adds src's numeric values of cols into dst.
func addColumns(dst, src *mapper.MappedRow, cols []string) error {
	for _, col := range cols {
		di, si := -1, -1
		for i, c := range dst.PGCols {
			if c == col {
				di = i
			}
		}
		for i, c := range src.PGCols {
			if c == col {
				si = i
			}
		}
		if di < 0 || si < 0 {
			return fmt.Errorf("sum_on_duplicate: column %s not mapped", col)
		}
		a, okA := dst.PGVals[di].(float64)
		b, okB := src.PGVals[si].(float64)
		if !okA || !okB {
			return fmt.Errorf("sum_on_duplicate: column %s is not decimal", col)
		}
		dst.PGVals[di] = a + b
	}
	return nil
}
