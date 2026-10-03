package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SyncPort    int           `yaml:"sync_port"`
	PgDSN       string        `yaml:"pg_dsn"`
	MdbBasePath string        `yaml:"mdb_base_path"`
	EntitiesDir string        `yaml:"entities_dir"`
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
}

type FKResolve struct {
	Entity string `yaml:"entity"`
}

type ChildConfig struct {
	Entity   string        `yaml:"entity"`
	Access   ChildAccess   `yaml:"access"`
	PG       ChildPG       `yaml:"postgres"`
	Fields   []FieldMap    `yaml:"fields"`
	Children []ChildConfig `yaml:"children"`
}

type ChildAccess struct {
	Table    string `yaml:"table"`
	PK       string `yaml:"pk"`
	ParentFK string `yaml:"parent_fk"`
}

type ChildPG struct {
	Table       string `yaml:"table"`
	PK          string `yaml:"pk"`
	ParentFK    string `yaml:"parent_fk"`
	ParentMatch string `yaml:"parent_match"`
}

type SyncConfig struct {
	Interval   time.Duration `yaml:"interval"`
	BatchSize  int           `yaml:"batch_size"`
	Direction  string        `yaml:"direction"`
	UseIDCoord *bool         `yaml:"use_idcoord"`
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
			entities = append(entities, ec)
		}
	}
	return entities, nil
}
