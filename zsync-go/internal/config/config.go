package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/samber/lo"
	"gopkg.in/yaml.v3"
)

// TagValue represents the possible values of the ZFS user property used to
// mark datasets for replication on the source.
type TagValue string

const (
	TagAll     TagValue = "all"     // Replicate the dataset and all children.
	TagSubvols TagValue = "subvols" // Replicate only child datasets (not the tagged dataset itself).
	TagExclude TagValue = "exclude" // Explicitly exclude a dataset from replication.
)

// SnapshotFilter holds the list of snapshot name patterns (e.g. "hourly",
// "daily") that determine which snapshots are eligible for replication.
type SnapshotFilter []string

// Regex returns a compiled alternation pattern such as "hourly|daily|weekly".
func (sf SnapshotFilter) Regex() string {
	return strings.Join(sf, "|")
}

// SSHConfig holds connection parameters for the source host.
type SSHConfig struct {
	// Host is the SSH address (user@host). Empty means local mode.
	Host string `yaml:"host"`
	// Port is the SSH port. Defaults to 22.
	Port int `yaml:"port"`
}

// IsLocal returns true when no remote host is configured.
func (s SSHConfig) IsLocal() bool {
	return s.Host == ""
}

// CheckZFSConfig configures the optional checkzfs monitoring integration.
type CheckZFSConfig struct {
	// Enabled enables checkzfs monitoring.
	Enabled bool `yaml:"enabled"`
	// Local runs checkzfs without the --source parameter.
	Local bool `yaml:"local"`
	// Prefix is the checkzfs output prefix.
	Prefix string `yaml:"prefix"`
	// MaxAge is the maximum age of the last snapshot in minutes (warn,crit).
	MaxAge string `yaml:"max_age"`
	// MaxSnapshotCount is the maximum snapshot count per dataset (warn,crit).
	MaxSnapshotCount string `yaml:"max_snapshot_count"`
	// Spool determines where checkzfs output goes: "local" or "source".
	Spool string `yaml:"spool"`
	// SpoolMaxAge is the maximum age of spool data in seconds.
	SpoolMaxAge int `yaml:"spool_max_age"`
}

// TargetConfig groups settings for the local replication target.
type TargetConfig struct {
	// Dataset is the ZFS dataset path on the local (backup) host under which
	// replicated datasets are created. Example: "backup/replicas".
	Dataset string `yaml:"dataset"`
	// MinKeep is the minimum number of snapshots to retain per filter
	// interval on the target.
	MinKeep int `yaml:"min_keep"`
}

// SourceConfig groups settings that describe the replication source.
type SourceConfig struct {
	// SSH holds the connection parameters for the source host.
	SSH SSHConfig `yaml:"ssh"`
	// Datasets lists the root datasets on the source to process.
	// Only datasets under these roots (that carry the correct tag) will be
	// replicated. This prevents accidental overlap with the target.
	Datasets []string `yaml:"datasets"`
	// Tag is the ZFS user property on the source that marks datasets for
	// replication. Defaults to "bashclub:zsync".
	Tag string `yaml:"tag"`
	// SnapshotFilter lists snapshot name patterns that are eligible for
	// replication (e.g. ["hourly", "daily", "weekly", "monthly"]).
	SnapshotFilter SnapshotFilter `yaml:"snapshot_filter"`
}

// Config is the top-level configuration for zsync.
type Config struct {
	// Target groups settings for the local replication target.
	Target TargetConfig `yaml:"target"`
	// Source groups settings that describe the replication source.
	Source SourceConfig `yaml:"source"`
	// CheckZFS configures the optional monitoring integration.
	CheckZFS CheckZFSConfig `yaml:"checkzfs"`
}

// defaults fills in zero-value fields with sensible defaults.
func (c *Config) defaults() {
	if c.Source.Tag == "" {
		c.Source.Tag = "bashclub:zsync"
	}
	if len(c.Source.SnapshotFilter) == 0 {
		c.Source.SnapshotFilter = SnapshotFilter{"hourly", "daily", "weekly", "monthly"}
	}
	if c.Target.MinKeep == 0 {
		c.Target.MinKeep = 3
	}
	if c.Source.SSH.Port == 0 {
		c.Source.SSH.Port = 22
	}
	if c.CheckZFS.Prefix == "" {
		c.CheckZFS.Prefix = "zsync"
	}
	if c.CheckZFS.MaxAge == "" {
		c.CheckZFS.MaxAge = "1500,6000"
	}
	if c.CheckZFS.MaxSnapshotCount == "" {
		c.CheckZFS.MaxSnapshotCount = "150,165"
	}
	if c.CheckZFS.Spool == "" {
		c.CheckZFS.Spool = "local"
	}
	if c.CheckZFS.SpoolMaxAge == 0 {
		c.CheckZFS.SpoolMaxAge = 87000
	}
}

// Validate checks that all required fields are present and consistent.
func (c *Config) Validate() error {
	if c.Target.Dataset == "" {
		return fmt.Errorf("config: target.dataset must not be empty")
	}
	if strings.Contains(c.Target.Dataset, " ") {
		return fmt.Errorf("config: target.dataset %q must not contain spaces", c.Target.Dataset)
	}
	if c.Target.MinKeep < 1 {
		return fmt.Errorf("config: target.min_keep must be >= 1, got %d", c.Target.MinKeep)
	}
	if len(c.Source.Datasets) == 0 {
		return fmt.Errorf("config: source.datasets must not be empty")
	}
	if lo.ContainsBy(c.Source.Datasets, func(ds string) bool { return ds == "" }) {
		return fmt.Errorf("config: source.datasets must not contain empty entries")
	}
	if overlap, ok := lo.Find(c.Source.Datasets, func(ds string) bool {
		return ds == c.Target.Dataset ||
			strings.HasPrefix(ds, c.Target.Dataset+"/") ||
			strings.HasPrefix(c.Target.Dataset, ds+"/")
	}); ok {
		return fmt.Errorf("config: source dataset %q overlaps with target %q", overlap, c.Target.Dataset)
	}
	if len(c.Source.SnapshotFilter) == 0 {
		return fmt.Errorf("config: source.snapshot_filter must not be empty")
	}
	if !c.Source.SSH.IsLocal() && c.Source.SSH.Port < 1 {
		return fmt.Errorf("config: source.ssh.port must be >= 1 when source.ssh.host is set")
	}
	if !lo.Contains([]string{"local", "source"}, c.CheckZFS.Spool) {
		return fmt.Errorf("config: checkzfs.spool must be \"local\" or \"source\", got %q", c.CheckZFS.Spool)
	}
	return nil
}

// Load reads a YAML configuration file from path, applies defaults and
// validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	cfg.defaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}
