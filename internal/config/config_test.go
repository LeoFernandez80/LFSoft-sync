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
