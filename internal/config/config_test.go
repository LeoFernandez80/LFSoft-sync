package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The deployed entity files must parse with no unknown keys (a typo would silently disable an
// option) and pass the same validation Load applies to enabled entities.
func TestDeployedEntities(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "config", "entities", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no entity files: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var ec EntityConfig
		if err := dec.Decode(&ec); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if err := ec.Validate(); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
		checkKeysMapped(t, filepath.Base(f), ec.Children)
		checkCountryLookups(t, filepath.Base(f), ec.Fields, ec.Children)
	}
}

// Pais_Codigo 0 is ARGENTINA (paises.yaml): a "0" null sentinel on a country lookup leaves
// every Argentine row without country (issue #8).
func checkCountryLookups(t *testing.T, file string, fields []FieldMap, children []ChildConfig) {
	for _, f := range fields {
		if f.Lookup == nil || f.Lookup.Table != "utilities.countries" || f.Lookup.Match != "country_legacyCode" {
			continue
		}
		for _, s := range f.NullSentinels {
			if s == "0" {
				t.Errorf("%s: %s -> %s: country code 0 (ARGENTINA) must not be a null sentinel", file, f.Access, f.PG)
			}
		}
	}
	for _, c := range children {
		checkCountryLookups(t, file, c.Fields, c.Children)
	}
}

// Every conflict column must be mapped or be the injected parent FK, or upserts fail at runtime.
func checkKeysMapped(t *testing.T, file string, children []ChildConfig) {
	for _, c := range children {
		mapped := map[string]bool{c.PG.ParentFK: true}
		for _, f := range c.Fields {
			mapped[f.PG] = true
		}
		for _, k := range c.ConflictColumns() {
			if !mapped[k] {
				t.Errorf("%s: child %s: conflict column %s is not mapped", file, c.Entity, k)
			}
		}
		checkKeysMapped(t, file, c.Children)
	}
}
