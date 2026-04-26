package replication

import (
	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
	"github.com/samber/lo"
)

// ---------------------------------------------------------------------------
// Action types
// ---------------------------------------------------------------------------

// ActionType describes what needs to happen for a dataset.
type ActionType int

const (
	// ActionSkip means no replication is needed (e.g. no matching snapshots).
	ActionSkip ActionType = iota
	// ActionInitial means a full initial send is required.
	ActionInitial
	// ActionIncremental means one or more incremental sends are required.
	ActionIncremental
	// ActionError means replication is not possible without manual intervention.
	ActionError
)

func (a ActionType) String() string {
	switch a {
	case ActionSkip:
		return "skip"
	case ActionInitial:
		return "initial"
	case ActionIncremental:
		return "incremental"
	case ActionError:
		return "error"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Plan structures
// ---------------------------------------------------------------------------

// Plan is the top-level replication plan built from collected state.
type Plan struct {
	// NeedTargetRoot is true when the target root dataset must be created.
	NeedTargetRoot bool
	// TargetRootDataset is the target root dataset name.
	TargetRootDataset string
	// Datasets contains one entry per dataset to process, in order.
	Datasets []DatasetPlan
}

// DatasetPlan describes the replication action for a single dataset.
type DatasetPlan struct {
	// SourceDataset is the dataset name on the source.
	SourceDataset string
	// TargetDataset is the full dataset path on the target.
	TargetDataset string
	// DatasetType is the ZFS type (filesystem or volume).
	DatasetType zfs.DatasetType
	// Action is the kind of replication to perform.
	Action ActionType
	// Reason is a human-readable explanation for the chosen action.
	Reason string

	// CommonSnapshot is the most recent snapshot present on both sides.
	// For initial replication this is nil – the first SendSnapshot must be
	// sent as a full stream. For incremental replication it is the base
	// snapshot; SendSnapshots are sent incrementally after it.
	CommonSnapshot *zfs.Snapshot
	// SendSnapshots is the ordered list of snapshots to send.
	// When CommonSnapshot is nil the first entry requires a full send,
	// all subsequent entries are sent incrementally based on the previous.
	SendSnapshots []zfs.Snapshot

	// --- Cleanup fields ---

	// Cleanup contains per-interval cleanup instructions for the target.
	Cleanup []IntervalCleanup
}

// IntervalCleanup describes which snapshots to remove on the target for one
// filter interval (e.g. "hourly").
type IntervalCleanup struct {
	Interval string
	// Delete lists snapshots to remove (oldest first).
	Delete []zfs.Snapshot
	// Keep is the number of snapshots that will remain after cleanup.
	Keep int
}

// ---------------------------------------------------------------------------
// Plan builder
// ---------------------------------------------------------------------------

// BuildPlan analyses source and target state, matches datasets, and produces
// a replication plan. This is a pure function – it does not execute any ZFS
// commands.
func BuildPlan(src *SourceState, tgt *TargetState, cfg *config.Config) *Plan {
	plan := &Plan{
		NeedTargetRoot:    !tgt.RootExists,
		TargetRootDataset: tgt.RootDataset,
	}

	filter := cfg.Source.SnapshotFilter

	plan.Datasets = lo.Map(src.Datasets, func(srcDS SourceDatasetInfo, _ int) DatasetPlan {
		tgtName := TargetDatasetName(cfg.Target.Dataset, srcDS.Name)
		tgtDS, found := tgt.Datasets[tgtName]
		if !found {
			tgtDS = TargetDatasetInfo{Name: tgtName, Exists: false}
		}
		return buildDatasetPlan(srcDS, tgtDS, filter, cfg.Target.MinKeep)
	})

	return plan
}

func buildDatasetPlan(srcDS SourceDatasetInfo, tgtDS TargetDatasetInfo, filter config.SnapshotFilter, minKeep int) DatasetPlan {
	dp := DatasetPlan{
		SourceDataset: srcDS.Name,
		TargetDataset: tgtDS.Name,
		DatasetType:   srcDS.Type,
	}

	// Filter source snapshots to only those matching snapshot_filter.
	srcFiltered := FilterSnapshots(srcDS.Snapshots, filter)

	if len(srcFiltered) == 0 {
		dp.Action = ActionSkip
		dp.Reason = "no matching snapshots on source"
		return dp
	}

	if !tgtDS.Exists {
		// Target does not exist → initial replication.
		// CommonSnapshot stays nil; all matching snapshots go into SendSnapshots.
		dp.Action = ActionInitial
		dp.SendSnapshots = srcFiltered
		dp.Reason = "target does not exist"
		return dp
	}

	// Target exists → find most recent common snapshot via GUID.
	tgtGUIDs := BuildGUIDIndex(tgtDS.Snapshots)

	// Walk source snapshots from newest to oldest to find the most recent
	// common snapshot.
	_, commonIdx, found := lo.FindLastIndexOf(srcFiltered, func(s zfs.Snapshot) bool {
		return lo.HasKey(tgtGUIDs, s.GUID)
	})

	if !found {
		dp.Action = ActionError
		dp.Reason = "no common snapshot found between source and target — manual intervention required"
		return dp
	}

	common := srcFiltered[commonIdx]
	dp.CommonSnapshot = &common

	// Everything after commonIdx needs to be sent.
	pending := srcFiltered[commonIdx+1:]

	if len(pending) == 0 {
		dp.Action = ActionSkip
		dp.Reason = "target is up to date"
		return dp
	}

	dp.Action = ActionIncremental
	dp.SendSnapshots = pending
	dp.Reason = "incremental replication"

	// Build cleanup plan for target.
	dp.Cleanup = buildCleanup(srcDS, tgtDS, filter, minKeep)

	return dp
}

// buildCleanup determines which target snapshots can be removed per interval.
func buildCleanup(srcDS SourceDatasetInfo, tgtDS TargetDatasetInfo, filter config.SnapshotFilter, minKeep int) []IntervalCleanup {
	if !tgtDS.Exists || len(tgtDS.Snapshots) == 0 {
		return nil
	}

	srcGUIDs := BuildGUIDIndex(srcDS.Snapshots)
	var cleanups []IntervalCleanup

	for _, interval := range filter {
		tgtInterval := FilterSnapshotsByInterval(tgtDS.Snapshots, interval)
		if len(tgtInterval) == 0 {
			continue
		}

		srcInterval := FilterSnapshotsByInterval(srcDS.Snapshots, interval)
		if len(srcInterval) == 0 {
			continue
		}
		oldestSrcGUID := srcInterval[0].GUID

		// On target, find the position of this GUID.
		_, cutIdx, cutFound := lo.FindIndexOf(tgtInterval, func(ts zfs.Snapshot) bool {
			return lo.HasKey(srcGUIDs, ts.GUID) && ts.GUID == oldestSrcGUID
		})

		if !cutFound {
			// The oldest source snapshot is not on the target — nothing to clean.
			continue
		}

		// Everything before cutIdx is older than the oldest source snapshot.
		candidates := tgtInterval[:cutIdx]
		total := len(tgtInterval)

		var toDelete []zfs.Snapshot
		for _, c := range candidates {
			if total-len(toDelete) > minKeep {
				toDelete = append(toDelete, c)
			}
		}

		if len(toDelete) > 0 {
			cleanups = append(cleanups, IntervalCleanup{
				Interval: interval,
				Delete:   toDelete,
				Keep:     total - len(toDelete),
			})
		}
	}

	return cleanups
}
