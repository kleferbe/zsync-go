package replication

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var t0 = time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)

func snap(dataset, shortName, guid string, hoursAfterT0 int) zfs.Snapshot {
	full := dataset + "@" + shortName
	return zfs.Snapshot{
		Name:      full,
		Dataset:   dataset,
		ShortName: shortName,
		GUID:      guid,
		Creation:  t0.Add(time.Duration(hoursAfterT0) * time.Hour),
	}
}

func defaultFilter() config.SnapshotFilter {
	return config.SnapshotFilter{
		{Filter: "hourly", MinKeep: 3},
		{Filter: "daily", MinKeep: 3},
		{Filter: "weekly", MinKeep: 3},
		{Filter: "monthly", MinKeep: 3},
	}
}

func defaultCfg() *config.Config {
	return &config.Config{
		Target:         config.TargetConfig{Dataset: "backup/replicas"},
		SnapshotFilter: defaultFilter(),
	}
}

// ---------------------------------------------------------------------------
// Plan builder tests
// ---------------------------------------------------------------------------

func TestBuildPlan_InitialReplication(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data",
			Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-13", "aaa", 0),
				snap("tank/data", "daily-2025-01-14", "bbb", 24),
				snap("tank/data", "daily-2025-01-15", "ccc", 48),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {Name: "backup/replicas/tank/data", Exists: false},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())

	if len(plan.Datasets) != 1 {
		t.Fatalf("got %d dataset plans, want 1", len(plan.Datasets))
	}
	dp := plan.Datasets[0]

	if dp.Action != ActionInitial {
		t.Errorf("action = %v, want initial", dp.Action)
	}
	if dp.CommonSnapshot != nil {
		t.Errorf("common snapshot should be nil for initial, got %+v", dp.CommonSnapshot)
	}
	if len(dp.SendSnapshots) != 3 {
		t.Errorf("send snapshots len = %d, want 3", len(dp.SendSnapshots))
	}
	if dp.SendSnapshots[0].GUID != "aaa" {
		t.Errorf("first send snapshot should be oldest (aaa), got %s", dp.SendSnapshots[0].GUID)
	}
}

func TestBuildPlan_IncrementalReplication(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data",
			Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-11", "aaa", 0),
				snap("tank/data", "daily-2025-01-12", "bbb", 24),
				snap("tank/data", "daily-2025-01-13", "ccc", 48),
				snap("tank/data", "daily-2025-01-14", "ddd", 72),
				snap("tank/data", "daily-2025-01-15", "eee", 96),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {
				Name: "backup/replicas/tank/data", Exists: true,
				Snapshots: []zfs.Snapshot{
					snap("backup/replicas/tank/data", "daily-2025-01-11", "aaa", 0),
					snap("backup/replicas/tank/data", "daily-2025-01-12", "bbb", 24),
					snap("backup/replicas/tank/data", "daily-2025-01-13", "ccc", 48),
				},
			},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())
	dp := plan.Datasets[0]

	if dp.Action != ActionIncremental {
		t.Errorf("action = %v, want incremental", dp.Action)
	}
	if dp.CommonSnapshot == nil || dp.CommonSnapshot.GUID != "ccc" {
		t.Errorf("common snapshot should be ccc, got %+v", dp.CommonSnapshot)
	}
	if len(dp.SendSnapshots) != 2 {
		t.Fatalf("send snapshots len = %d, want 2", len(dp.SendSnapshots))
	}
	if dp.SendSnapshots[0].GUID != "ddd" {
		t.Errorf("first send snapshot should be ddd, got %s", dp.SendSnapshots[0].GUID)
	}
	if dp.SendSnapshots[1].GUID != "eee" {
		t.Errorf("second send snapshot should be eee, got %s", dp.SendSnapshots[1].GUID)
	}
}

func TestBuildPlan_UpToDate(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data", Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-15", "aaa", 0),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {
				Name: "backup/replicas/tank/data", Exists: true,
				Snapshots: []zfs.Snapshot{
					snap("backup/replicas/tank/data", "daily-2025-01-15", "aaa", 0),
				},
			},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())
	dp := plan.Datasets[0]

	if dp.Action != ActionSkip {
		t.Errorf("action = %v, want skip (up to date)", dp.Action)
	}
}

func TestBuildPlan_NoCommonSnapshot(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data", Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-15", "aaa", 0),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {
				Name: "backup/replicas/tank/data", Exists: true,
				Snapshots: []zfs.Snapshot{
					snap("backup/replicas/tank/data", "daily-2025-01-10", "zzz", 0),
				},
			},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())
	dp := plan.Datasets[0]

	if dp.Action != ActionError {
		t.Errorf("action = %v, want error", dp.Action)
	}
}

func TestBuildPlan_NoMatchingSnapshots(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data", Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "manual-backup", "aaa", 0),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {Name: "backup/replicas/tank/data", Exists: false},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())
	dp := plan.Datasets[0]

	if dp.Action != ActionSkip {
		t.Errorf("action = %v, want skip", dp.Action)
	}
}

func TestBuildPlan_TargetRootMissing(t *testing.T) {
	srcState := &SourceState{}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  false,
		Datasets:    map[string]TargetDatasetInfo{},
	}
	plan := BuildPlan(srcState, tgtState, defaultCfg())
	if !plan.NeedTargetRoot {
		t.Error("NeedTargetRoot should be true")
	}
}

func TestBuildPlan_Cleanup(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data", Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-13", "d13", 0),
				snap("tank/data", "daily-2025-01-14", "d14", 24),
				snap("tank/data", "daily-2025-01-15", "d15", 48),
				snap("tank/data", "daily-2025-01-16", "d16", 72),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {
				Name:   "backup/replicas/tank/data",
				Exists: true,
				Snapshots: []zfs.Snapshot{
					snap("backup/replicas/tank/data", "daily-2025-01-10", "d10", -72),
					snap("backup/replicas/tank/data", "daily-2025-01-11", "d11", -48),
					snap("backup/replicas/tank/data", "daily-2025-01-12", "d12", -24),
					snap("backup/replicas/tank/data", "daily-2025-01-13", "d13", 0),
					snap("backup/replicas/tank/data", "daily-2025-01-14", "d14", 24),
					snap("backup/replicas/tank/data", "daily-2025-01-15", "d15", 48),
				},
			},
		},
	}

	// Source has d13-d16, target has d10-d15.
	// Common snapshot = d15 (most recent on both sides).
	// SendSnapshots = [d16] (1 incoming daily).
	// After sync: target has 7 daily snapshots (d10..d16).
	// minKeep = 3 → delete 4 oldest (d10, d11, d12, d13), keep 3 (d14, d15, d16).
	plan := BuildPlan(srcState, tgtState, defaultCfg())
	dp := plan.Datasets[0]

	if dp.Action != ActionIncremental {
		t.Fatalf("action = %v, want incremental", dp.Action)
	}
	if len(dp.Cleanup) != 1 {
		t.Fatalf("cleanup intervals = %d, want 1", len(dp.Cleanup))
	}
	c := dp.Cleanup[0]
	if c.Interval != "daily" {
		t.Errorf("interval = %q, want daily", c.Interval)
	}
	if len(c.Delete) != 4 {
		t.Errorf("delete count = %d, want 4", len(c.Delete))
	}
	if c.Keep != 3 {
		t.Errorf("keep = %d, want 3", c.Keep)
	}
}

func TestBuildPlan_MultipleDatasets(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{
			{
				Name: "tank/vms", Type: zfs.Volume,
				Snapshots: []zfs.Snapshot{
					snap("tank/vms", "hourly-01", "h1", 0),
				},
			},
			{
				Name: "tank/data", Type: zfs.Filesystem,
				Snapshots: []zfs.Snapshot{
					snap("tank/data", "daily-01", "d1", 0),
				},
			},
		},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  true,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/vms": {Name: "backup/replicas/tank/vms", Exists: false},
			"backup/replicas/tank/data": {
				Name: "backup/replicas/tank/data", Exists: true,
				Snapshots: []zfs.Snapshot{
					snap("backup/replicas/tank/data", "daily-01", "d1", 0),
				},
			},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())
	if len(plan.Datasets) != 2 {
		t.Fatalf("datasets = %d, want 2", len(plan.Datasets))
	}
	if plan.Datasets[0].Action != ActionInitial {
		t.Errorf("[0] action = %v, want initial", plan.Datasets[0].Action)
	}
	if plan.Datasets[1].Action != ActionSkip {
		t.Errorf("[1] action = %v, want skip (up to date)", plan.Datasets[1].Action)
	}
}

// ---------------------------------------------------------------------------
// Display test
// ---------------------------------------------------------------------------

func TestWritePlanText(t *testing.T) {
	srcState := &SourceState{
		Datasets: []SourceDatasetInfo{{
			Name: "tank/data", Type: zfs.Filesystem,
			Snapshots: []zfs.Snapshot{
				snap("tank/data", "daily-2025-01-14", "aaa", 0),
				snap("tank/data", "daily-2025-01-15", "bbb", 24),
			},
		}},
	}
	tgtState := &TargetState{
		RootDataset: "backup/replicas",
		RootExists:  false,
		Datasets: map[string]TargetDatasetInfo{
			"backup/replicas/tank/data": {Name: "backup/replicas/tank/data", Exists: false},
		},
	}

	plan := BuildPlan(srcState, tgtState, defaultCfg())

	var buf bytes.Buffer
	WritePlanText(&buf, plan)
	out := buf.String()

	if !strings.Contains(out, "will be created") {
		t.Error("expected target root creation notice")
	}
	if !strings.Contains(out, "initial") {
		t.Error("expected 'initial' action in output")
	}
	if !strings.Contains(out, "tank/data") {
		t.Error("expected dataset name in output")
	}
}

// ---------------------------------------------------------------------------
// State helper tests
// ---------------------------------------------------------------------------

func TestFilterSnapshots(t *testing.T) {
	snaps := []zfs.Snapshot{
		snap("ds", "hourly-01", "a", 0),
		snap("ds", "daily-01", "b", 1),
		snap("ds", "manual-backup", "c", 2),
		snap("ds", "weekly-01", "d", 3),
	}
	filtered := FilterSnapshots(snaps, config.SnapshotFilter{
		{Filter: "hourly", MinKeep: 3},
		{Filter: "daily", MinKeep: 3},
		{Filter: "weekly", MinKeep: 3},
	})
	if len(filtered) != 3 {
		t.Errorf("filtered len = %d, want 3", len(filtered))
	}
}

func TestFilterSnapshotsByInterval(t *testing.T) {
	snaps := []zfs.Snapshot{
		snap("ds", "hourly-01", "a", 0),
		snap("ds", "daily-01", "b", 1),
		snap("ds", "hourly-02", "c", 2),
	}
	hourly := FilterSnapshotsByInterval(snaps, "hourly")
	if len(hourly) != 2 {
		t.Errorf("hourly len = %d, want 2", len(hourly))
	}
}
