package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SyncPort    int            `yaml:"sync_port"`
	PgDSN       string         `yaml:"pg_dsn"`
	MdbBasePath string         `yaml:"mdb_base_path"`
	EntitiesDir string         `yaml:"entities_dir"`
	LogFile     string         `yaml:"log_file"`
	Entities    []EntityConfig `yaml:"-"`
}

type EntityConfig struct {
	Entity  string       `yaml:"entity"`
	Enabled bool         `yaml:"enabled"`
	Access  AccessConfig `yaml:"access"`
	PG      PGConfig     `yaml:"postgres"`

	Detection DetectionConfig `yaml:"detection"`
	Fields    []FieldMap      `yaml:"fields"`
	Children  []ChildConfig   `yaml:"children"`
	Sync      SyncConfig      `yaml:"sync"`
}

type AccessConfig struct {
	MDB    string `yaml:"mdb"`
	Table  string `yaml:"table"`
	PK     string `yaml:"pk"`
	PKType string `yaml:"pk_type"`
	// Filter is a Jet condition that limits which root rows are synced, e.g. leaving out old
	// documents whose lines have no key. Rows outside it are neither read nor deleted from PG.
	Filter string `yaml:"filter"`
}

type PGConfig struct {
	Schema           string `yaml:"schema"`
	Table            string `yaml:"table"`
	PK               string `yaml:"pk"`
	LegacyCodeColumn string `yaml:"legacy_code_column"`
	BusinessKey      string `yaml:"business_key"`
}

type DetectionConfig struct {
	AccessToPG          string        `yaml:"access_to_pg"`
	TimestampColumn     string        `yaml:"timestamp_column"`
	TimestampTimeColumn string        `yaml:"timestamp_time_column"`
	PGToAccess          string        `yaml:"pg_to_access"`
	FullScanInterval    time.Duration `yaml:"full_scan_interval"`
	// ChangeScanInterval is the minimum gap between full scans triggered by a .mdb modtime change
	// (edits to existing rows are only visible to a full scan). Zero means DefaultChangeScanInterval.
	ChangeScanInterval time.Duration `yaml:"change_scan_interval"`
}

const DefaultChangeScanInterval = 30 * time.Second

func (d DetectionConfig) ChangeScanGap() time.Duration {
	if d.ChangeScanInterval > 0 {
		return d.ChangeScanInterval
	}
	return DefaultChangeScanInterval
}

type FieldMap struct {
	Access        string     `yaml:"access"`
	PG            string     `yaml:"pg"`
	Type          string     `yaml:"type"`
	Nullable      bool       `yaml:"nullable"`
	Transform     string     `yaml:"transform"`
	NullSentinels []string   `yaml:"null_sentinels"`
	TimeSource    string     `yaml:"time_source"`
	FKResolve     *FKResolve `yaml:"fk_resolve"`
	// ZeroDefault replaces 0 or NULL from Access (int fields only), e.g. a default unit of
	// measure for a NOT NULL FK column. Access → PG only.
	ZeroDefault *int64 `yaml:"zero_default"`
	// Split takes one part of a delimited Access text ("dd/mm" → day) before conversion.
	// Access → PG only: the field is left out of PG → Access writes.
	Split *Split `yaml:"split"`
	// Map translates Access values (compared trimmed, case-insensitively) before conversion,
	// e.g. "SI"/"NO" → true/false. A value missing from the map is NULL, or an error if not nullable.
	Map map[string]interface{} `yaml:"map"`
	// Offset is added to int values going to PG and subtracted going back (0-based → 1-based codes).
	Offset int64 `yaml:"offset"`
	// Lookup replaces the converted value with one read from a PG table (legacy code → id,
	// code → snapshot text). Unlike fk_resolve it needs no sync.id_map entries.
	Lookup *Lookup `yaml:"lookup"`
}

type FKResolve struct {
	Entity string `yaml:"entity"`
}

type Split struct {
	Sep   string `yaml:"sep"`
	Index int    `yaml:"index"`
}

// Lookup resolves a value against PG. Table form: SELECT select FROM table WHERE match = value,
// cached per table and reversible (PG → Access). Query form: an arbitrary SELECT whose $1..$n
// are the Access columns listed in Args (for composite keys); not reversible.
type Lookup struct {
	Table           string   `yaml:"table"` // schema.table
	Match           string   `yaml:"match"`
	Select          string   `yaml:"select"`
	CaseInsensitive bool     `yaml:"case_insensitive"`
	Query           string   `yaml:"query"`
	Args            []string `yaml:"args"`
}

type ChildConfig struct {
	Entity   string        `yaml:"entity"`
	Access   ChildAccess   `yaml:"access"`
	PG       ChildPG       `yaml:"postgres"`
	Fields   []FieldMap    `yaml:"fields"`
	Children []ChildConfig `yaml:"children"`
	// Direction "access_to_pg" keeps the collection out of PG → Access document writes (the
	// legacy app owns it, e.g. stock withdrawals). Empty: follows the entity.
	Direction string `yaml:"direction"`
}

// WritesToAccess reports whether PG → Access document writes include this collection.
func (c *ChildConfig) WritesToAccess() bool {
	return c.Direction != "access_to_pg"
}

type ChildAccess struct {
	Table string `yaml:"table"`
	PK    string `yaml:"pk"`
	// ParentFK is the child column holding the parent's Access PK.
	ParentFK string `yaml:"parent_fk"`
	// RootFK is the child column holding the root's Access PK, for nested children whose
	// parent key is only unique within the root (Detalles: Presupuesto_Numero + Item_Numero).
	RootFK string `yaml:"root_fk"`
}

type ChildPG struct {
	Table    string `yaml:"table"`
	PK       string `yaml:"pk"`
	ParentFK string `yaml:"parent_fk"`
	// Conflict is the unique key that identifies an existing child row. Default: parent_fk plus
	// the PG column mapped from the Access PK.
	Conflict []string `yaml:"conflict"`
	// DeleteMissing removes the parent's PG child rows that are no longer in Access.
	DeleteMissing bool `yaml:"delete_missing"`
	// SumOnDuplicate merges Access rows of one parent that share the conflict key (compared
	// case-insensitively, like Jet): these PG columns are added up, the rest come from the
	// first row. For tables without a unique key in Access where repeats mean "more of the
	// same" (stock withdrawals). Without it the last repeated row wins.
	SumOnDuplicate []string `yaml:"sum_on_duplicate"`
}

type SyncConfig struct {
	Interval   time.Duration `yaml:"interval"`
	BatchSize  int           `yaml:"batch_size"`
	Direction  string        `yaml:"direction"`
	UseIDCoord *bool         `yaml:"use_idcoord"`
	// DeleteMissing (default true) deletes PG rows missing from Access on full scans. Turn it off
	// when PG also holds rows created by the app that never reach Access.
	DeleteMissing *bool `yaml:"delete_missing"`
}

func (ec *EntityConfig) DeletesMissing() bool {
	return ec.Sync.DeleteMissing == nil || *ec.Sync.DeleteMissing
}

// ConflictColumns returns the PG columns that identify an existing child row.
func (c *ChildConfig) ConflictColumns() []string {
	if len(c.PG.Conflict) > 0 {
		return c.PG.Conflict
	}
	for _, f := range c.Fields {
		if f.Access == c.Access.PK {
			return []string{c.PG.ParentFK, f.PG}
		}
	}
	return []string{c.PG.ParentFK}
}

func (ec *EntityConfig) UsesIDCoord() bool {
	if ec.Sync.UseIDCoord != nil {
		return *ec.Sync.UseIDCoord
	}
	return ec.PG.BusinessKey == ""
}

func (ec *EntityConfig) SyncColumn() string {
	if ec.PG.BusinessKey != "" {
		return ec.PG.BusinessKey
	}
	if ec.PG.LegacyCodeColumn != "" {
		return ec.PG.LegacyCodeColumn
	}
	return ec.PG.PK
}

func (ec *EntityConfig) NeedsIDMap() bool {
	return ec.PG.LegacyCodeColumn != ""
}

func (ec *EntityConfig) MDBPath(basePath string) string {
	return filepath.Join(basePath, ec.Access.MDB)
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.SyncPort == 0 {
		cfg.SyncPort = 9090
	}
	if cfg.EntitiesDir == "" {
		cfg.EntitiesDir = "config/entities"
	}
	entities, err := loadEntities(cfg.EntitiesDir)
	if err != nil {
		return nil, err
	}
	cfg.Entities = entities
	return &cfg, nil
}

func loadEntities(dir string) ([]EntityConfig, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("config: glob entities: %w", err)
	}
	var entities []EntityConfig
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", f, err)
		}
		var ec EntityConfig
		if err := yaml.Unmarshal(data, &ec); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", f, err)
		}
		if ec.Enabled {
			if err := ec.Validate(); err != nil {
				return nil, fmt.Errorf("config: %s: %w", f, err)
			}
			entities = append(entities, ec)
		}
	}
	return entities, nil
}

// Validate checks the options the engine cannot run with. Load applies it to enabled entities.
func (ec *EntityConfig) Validate() error {
	if err := validateFields(ec.Fields); err != nil {
		return err
	}
	// Rows left out by the filter would look deleted to the full-scan delete.
	if ec.Access.Filter != "" && ec.DeletesMissing() {
		return fmt.Errorf("access.filter requires sync.delete_missing: false")
	}
	return validateChildren(ec.Children)
}

func validateFields(fields []FieldMap) error {
	for _, f := range fields {
		if l := f.Lookup; l != nil {
			if l.Query == "" && (l.Table == "" || l.Match == "" || l.Select == "") {
				return fmt.Errorf("field %s: lookup needs table, match and select (or query and args)", f.PG)
			}
			if l.Query != "" && len(l.Args) == 0 {
				return fmt.Errorf("field %s: lookup query needs args", f.PG)
			}
		}
		if f.Split != nil && f.Split.Sep == "" {
			return fmt.Errorf("field %s: split needs sep", f.PG)
		}
	}
	return nil
}

// Nested children may omit root_fk only when the parent's Access PK is unique across its whole
// table; otherwise rows of different roots would be mixed. That can't be checked from here.
func validateChildren(children []ChildConfig) error {
	for i := range children {
		c := &children[i]
		if c.Access.Table == "" || c.Access.PK == "" || c.Access.ParentFK == "" {
			return fmt.Errorf("child %s: access.table, access.pk and access.parent_fk are required", c.Entity)
		}
		if c.PG.Table == "" || c.PG.PK == "" || c.PG.ParentFK == "" {
			return fmt.Errorf("child %s: postgres.table, postgres.pk and postgres.parent_fk are required", c.Entity)
		}
		if err := validateFields(c.Fields); err != nil {
			return fmt.Errorf("child %s: %w", c.Entity, err)
		}
		if err := validateChildren(c.Children); err != nil {
			return err
		}
	}
	return nil
}
