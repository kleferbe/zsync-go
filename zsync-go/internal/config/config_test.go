package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidConfig(t *testing.T) {
	yml := `
target: 
  dataset: backup/replicas
source:
  ssh:
    host: root@pve1
    port: 22
  datasets:
    - tank
  tag: bashclub:zsync
snapshot_filter:
  - filter: hourly
    min_keep: 3
  - filter: daily
    min_keep: 3
  - filter: weekly
    min_keep: 3
  - filter: monthly
    min_keep: 3
checkzfs:
  enabled: true
  prefix: zsync
  spool: local
`
	path := writeTemp(t, yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Target.Dataset != "backup/replicas" {
		t.Errorf("target.dataset = %q, want %q", cfg.Target.Dataset, "backup/replicas")
	}
	if cfg.Source.SSH.Host != "root@pve1" {
		t.Errorf("source.ssh.host = %q, want %q", cfg.Source.SSH.Host, "root@pve1")
	}
	if cfg.Source.SSH.Port != 22 {
		t.Errorf("source.ssh.port = %d, want %d", cfg.Source.SSH.Port, 22)
	}
	if cfg.Source.Tag != "bashclub:zsync" {
		t.Errorf("source.tag = %q, want %q", cfg.Source.Tag, "bashclub:zsync")
	}
	if len(cfg.SnapshotFilter) != 4 {
		t.Errorf("snapshot_filter len = %d, want 4", len(cfg.SnapshotFilter))
	}
	if cfg.SnapshotFilter.Regex() != "hourly|daily|weekly|monthly" {
		t.Errorf("snapshot_filter regex = %q", cfg.SnapshotFilter.Regex())
	}
	if cfg.SnapshotFilter[1].MinKeep != 3 {
		t.Errorf("snapshot_filter[daily].min_keep = %d, want 3", cfg.SnapshotFilter[1].MinKeep)
	}
	if !cfg.CheckZFS.Enabled {
		t.Error("checkzfs.enabled should be true")
	}
	if len(cfg.Source.Datasets) != 1 || cfg.Source.Datasets[0] != "tank" {
		t.Errorf("source.datasets = %v, want [tank]", cfg.Source.Datasets)
	}
}

func TestLoadDefaults(t *testing.T) {
	yml := `
target:
  dataset: backup/data
source:
  datasets:
    - tank
`
	path := writeTemp(t, yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source.Tag != "bashclub:zsync" {
		t.Errorf("default source.tag = %q, want %q", cfg.Source.Tag, "bashclub:zsync")
	}
	if cfg.Source.SSH.Port != 22 {
		t.Errorf("default source.ssh.port = %d, want 22", cfg.Source.SSH.Port)
	}
	if len(cfg.SnapshotFilter) != 4 {
		t.Errorf("default snapshot_filter len = %d, want 4", len(cfg.SnapshotFilter))
	}
	if cfg.SnapshotFilter[0].MinKeep != 3 {
		t.Errorf("default snapshot_filter[0].min_keep = %d, want 3", cfg.SnapshotFilter[0].MinKeep)
	}
	if cfg.CheckZFS.Spool != "local" {
		t.Errorf("default checkzfs.spool = %q, want %q", cfg.CheckZFS.Spool, "local")
	}
	if !cfg.Source.SSH.IsLocal() {
		t.Error("expected local mode when source.ssh.host is empty")
	}
}

func TestLoadMissingTarget(t *testing.T) {
	yml := `
source:
  ssh:
    host: root@pve1
  datasets:
    - tank
`
	path := writeTemp(t, yml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
}

func TestLoadMissingDatasets(t *testing.T) {
	yml := `
target:
  dataset: backup/data
`
	path := writeTemp(t, yml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing source.datasets")
	}
}

func TestLoadOverlappingDatasets(t *testing.T) {
	yml := `
target:
  dataset: tank/backup
source:
  datasets:
    - tank
`
	path := writeTemp(t, yml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for overlapping source and target datasets")
	}
}

func TestLoadInvalidSpool(t *testing.T) {
	yml := `
target:
  dataset: backup/data
source:
  datasets:
    - tank
checkzfs:
  spool: remote
`
	path := writeTemp(t, yml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid spool value")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := writeTemp(t, `{{{invalid`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "zsync.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}
