package zabbix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/replication"
	"github.com/kleferbe/zsync/internal/zfs"
)

func TestBuildReportCommonSnapshotAndCounts(t *testing.T) {
	filter := config.SnapshotFilter{{Filter: "daily", MinKeep: 1}}
	old := time.Unix(100, 0)
	mid := time.Unix(200, 0)
	newest := time.Unix(300, 0)
	source := &replication.SourceState{
		Datasets: []replication.SourceDatasetInfo{{
			Name: "tank/vm",
			Snapshots: []zfs.Snapshot{
				snap("tank/vm", "daily-old", "g1", old),
				snap("tank/vm", "daily-mid", "g2", mid),
				snap("tank/vm", "daily-new", "g3", newest),
				snap("tank/vm", "manual", "g9", newest.Add(time.Hour)),
			},
		}},
	}
	plan := &replication.Plan{
		Datasets: []replication.DatasetPlan{{
			SourceDataset: "tank/vm",
			TargetDataset: "backup/tank/vm",
		}},
	}
	targetSnaps := map[string][]zfs.Snapshot{
		"backup/tank/vm": {
			snap("backup/tank/vm", "daily-old", "g1", old),
			snap("backup/tank/vm", "daily-mid", "g2", mid),
		},
	}

	report := buildReport(time.Unix(1000, 0), filter, source, plan, targetSnaps, nil, &replication.ExecuteResult{}, nil)
	if !report.OK {
		t.Fatalf("ok = false, error %q", report.Error)
	}
	if report.WrittenAt != 1000 {
		t.Errorf("writtenAt = %d", report.WrittenAt)
	}
	ds := report.Datasets[0]
	if ds.CommonSnapshot != "daily-mid" || ds.CommonSnapshotCreatedAt != 200 {
		t.Errorf("common = %s @ %d", ds.CommonSnapshot, ds.CommonSnapshotCreatedAt)
	}
	if ds.LatestSourceSnapshot != "daily-new" || ds.LatestSourceSnapshotCreatedAt != 300 {
		t.Errorf("latest source = %s @ %d", ds.LatestSourceSnapshot, ds.LatestSourceSnapshotCreatedAt)
	}
	if ds.SourceSnapshotCount != 3 || ds.TargetSnapshotCount != 2 {
		t.Errorf("counts = %d/%d", ds.SourceSnapshotCount, ds.TargetSnapshotCount)
	}
}

func TestBuildReportExecuteError(t *testing.T) {
	report := buildReport(time.Unix(1, 0), nil, nil, &replication.Plan{
		Datasets: []replication.DatasetPlan{{SourceDataset: "tank"}},
	}, nil, nil, nil, errors.New("creating target root"))
	if report.OK || report.Error != "creating target root" || len(report.Datasets) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestBuildReportDatasetError(t *testing.T) {
	filter := config.SnapshotFilter{{Filter: "daily", MinKeep: 1}}
	created := time.Unix(50, 0)
	source := &replication.SourceState{
		Datasets: []replication.SourceDatasetInfo{{
			Name:      "tank/vm",
			Snapshots: []zfs.Snapshot{snap("tank/vm", "daily", "g1", created)},
		}},
	}
	plan := &replication.Plan{
		Datasets: []replication.DatasetPlan{{
			SourceDataset: "tank/vm",
			TargetDataset: "backup/tank/vm",
		}},
	}
	result := &replication.ExecuteResult{
		SyncErrors:    map[string]error{"tank/vm": errors.New("receive failed")},
		CleanupErrors: map[string]error{},
	}
	report := buildReport(time.Unix(1, 0), filter, source, plan, map[string][]zfs.Snapshot{
		"backup/tank/vm": {snap("backup/tank/vm", "daily", "g1", created)},
	}, nil, result, nil)
	if report.OK {
		t.Fatal("expected ok false")
	}
	if report.Datasets[0].Error != "receive failed" {
		t.Errorf("dataset error = %q", report.Datasets[0].Error)
	}
	if report.Error != "tank/vm: receive failed" {
		t.Errorf("report error = %q", report.Error)
	}
}

func TestPublishLocalReplacesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zfs-backup.json")
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Zabbix: config.ZabbixConfig{Place: config.PlaceLocal, Path: path},
	}
	report := Report{OK: true, WrittenAt: 1, Datasets: []DatasetStatus{}}
	if err := publish(context.Background(), cfg, nil, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ok": true`) || strings.Contains(string(data), "old") {
		t.Fatalf("file = %s", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir entries = %d", len(entries))
	}
}

func snap(dataset, name, guid string, created time.Time) zfs.Snapshot {
	return zfs.Snapshot{
		Name:      dataset + "@" + name,
		Dataset:   dataset,
		ShortName: name,
		GUID:      guid,
		Creation:  created,
	}
}
