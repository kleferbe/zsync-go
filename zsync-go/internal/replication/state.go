package replication

import (
	"strings"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
	"github.com/samber/lo"
)

// ---------------------------------------------------------------------------
// Source state – collected from the source host
// ---------------------------------------------------------------------------

// SourceDatasetInfo holds the collected state for a single dataset on the source.
type SourceDatasetInfo struct {
	Name      string
	Type      zfs.DatasetType
	TagValue  config.TagValue
	TagSource zfs.PropertySource
	// Snapshots sorted by creation time ascending (oldest first).
	Snapshots []zfs.Snapshot
}

// SourceState is the collected state from the source host.
type SourceState struct {
	Datasets []SourceDatasetInfo
}

// ---------------------------------------------------------------------------
// Target state – collected from the local target host
// ---------------------------------------------------------------------------

// TargetDatasetInfo holds the collected state for one dataset on the target.
// If Exists is false, Snapshots is nil.
type TargetDatasetInfo struct {
	Name   string // full target path, e.g. "backup/replicas/tank/data"
	Exists bool
	// Snapshots sorted by creation time ascending (oldest first).
	Snapshots []zfs.Snapshot
}

// TargetState is the collected state from the local target host.
type TargetState struct {
	// RootDataset is the configured target root (e.g. "backup/replicas").
	RootDataset string
	// RootExists indicates whether the target root dataset exists.
	RootExists bool
	// Datasets holds one entry per target dataset that was checked.
	// Keyed by the full target dataset name for quick lookup.
	Datasets map[string]TargetDatasetInfo
}

// ---------------------------------------------------------------------------
// Snapshot helpers
// ---------------------------------------------------------------------------

// FilterSnapshots returns only snapshots whose ShortName contains at least
// one of the filter patterns.
func FilterSnapshots(snaps []zfs.Snapshot, filter config.SnapshotFilter) []zfs.Snapshot {
	if len(filter) == 0 {
		return snaps
	}
	return lo.Filter(snaps, func(s zfs.Snapshot, _ int) bool {
		return lo.SomeBy(filter, func(e config.SnapshotFilterEntry) bool {
			return strings.Contains(s.ShortName, e.Filter)
		})
	})
}

// FilterSnapshotsByInterval returns only snapshots whose ShortName contains
// the given interval string.
func FilterSnapshotsByInterval(snaps []zfs.Snapshot, interval string) []zfs.Snapshot {
	return lo.Filter(snaps, func(s zfs.Snapshot, _ int) bool {
		return strings.Contains(s.ShortName, interval)
	})
}

// TargetDatasetName maps a source dataset name to its target path.
// Example: target="backup/replicas", source="tank/data/vm" → "backup/replicas/tank/data/vm".
func TargetDatasetName(targetRoot, sourceName string) string {
	return targetRoot + "/" + sourceName
}
