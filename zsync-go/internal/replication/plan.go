package replication

import (
	"fmt"

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
	// ActionReinitialize means the target exists but has no common snapshot.
	// The existing target dataset will be renamed and a fresh initial send performed.
	ActionReinitialize
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
	case ActionReinitialize:
		return "reinitialize"
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

	// RenameExistingTarget is set for ActionReinitialize: the existing target
	// dataset will be renamed to this path before sending.
	RenameExistingTarget string

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

// PlanBuilder holds the collected state needed to produce a replication plan.
type PlanBuilder struct {
	Source *SourceState
	Target *TargetState
	Config *config.Config
}

// Build analyses source and target state, matches datasets, and produces
// a replication plan. This is a pure method – it does not execute any ZFS
// commands.
func (pb *PlanBuilder) Build() *Plan {
	plan := &Plan{
		NeedTargetRoot:    !pb.Target.RootExists,
		TargetRootDataset: pb.Target.RootDataset,
	}

	plan.Datasets = lo.Map(pb.Source.Datasets, func(srcDS SourceDatasetInfo, _ int) DatasetPlan {
		tgtName := TargetDatasetName(pb.Config.Target.Dataset, srcDS.Name)
		tgtDS, found := pb.Target.Datasets[tgtName]
		if !found {
			tgtDS = TargetDatasetInfo{Name: tgtName, Exists: false}
		}
		return pb.buildDatasetPlan(srcDS, tgtDS)
	})

	return plan
}

// BuildPlan is a convenience wrapper around PlanBuilder.Build.
func BuildPlan(src *SourceState, tgt *TargetState, cfg *config.Config) *Plan {
	return (&PlanBuilder{Source: src, Target: tgt, Config: cfg}).Build()
}

func (pb *PlanBuilder) buildDatasetPlan(srcDS SourceDatasetInfo, tgtDS TargetDatasetInfo) DatasetPlan {
	dp := DatasetPlan{
		SourceDataset: srcDS.Name,
		TargetDataset: tgtDS.Name,
		DatasetType:   srcDS.Type,
	}

	filter := pb.Config.SnapshotFilter

	// Filter source snapshots to only those matching snapshot_filter.
	srcFiltered := FilterSnapshots(srcDS.Snapshots, filter)

	if len(srcFiltered) == 0 {
		dp.Action = ActionSkip
		dp.Reason = "no matching snapshots on source"
		return dp
	}

	if !tgtDS.Exists {
		// Target does not exist → initial replication.
		// Only send the newest min_keep snapshots per interval to avoid
		// transferring snapshots that would be cleaned up immediately.
		dp.Action = ActionInitial
		dp.SendSnapshots = pb.trimForInitial(srcFiltered)
		dp.Reason = "target does not exist"
		return dp
	}

	// Target exists → find most recent common snapshot via GUID.
	tgtGUIDs := lo.SliceToMap(tgtDS.Snapshots, func(s zfs.Snapshot) (string, struct{}) {
		return s.GUID, struct{}{}
	})

	// Walk source snapshots from newest to oldest to find the most recent
	// common snapshot.
	_, commonIdx, found := lo.FindLastIndexOf(srcFiltered, func(s zfs.Snapshot) bool {
		return lo.HasKey(tgtGUIDs, s.GUID)
	})

	if !found {
		// No common snapshot → reinitialize: rename existing target, send fresh.
		dp.Action = ActionReinitialize
		dp.RenameExistingTarget = uniqueName(tgtDS.Name, "old", lo.Keys(pb.Target.Datasets))
		dp.SendSnapshots = pb.trimForInitial(srcFiltered)
		dp.Reason = "no common snapshot — target will be renamed and reinitialized"
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
	dp.Cleanup = pb.buildCleanup(tgtDS, dp.SendSnapshots)

	return dp
}

// buildCleanup determines which target snapshots can be removed per interval.
// It accounts for the snapshots that will be added by the sync (sendSnapshots)
// so that exactly entry.MinKeep snapshots remain per interval after sync + cleanup.
// Only existing target snapshots are candidates for deletion (oldest first).
func (pb *PlanBuilder) buildCleanup(tgtDS TargetDatasetInfo, sendSnapshots []zfs.Snapshot) []IntervalCleanup {
	if !tgtDS.Exists || len(tgtDS.Snapshots) == 0 {
		return nil
	}

	var cleanups []IntervalCleanup

	for _, entry := range pb.Config.SnapshotFilter {
		tgtInterval := FilterSnapshotsByInterval(tgtDS.Snapshots, entry.Filter)
		incomingCount := len(FilterSnapshotsByInterval(sendSnapshots, entry.Filter))
		totalAfterSync := len(tgtInterval) + incomingCount
		excessCount := totalAfterSync - entry.MinKeep
		if excessCount <= 0 {
			continue
		}

		// Delete oldest existing target snapshots, but at most all of them.
		deleteCount := min(excessCount, len(tgtInterval))
		toDelete := tgtInterval[:deleteCount]

		cleanups = append(cleanups, IntervalCleanup{
			Interval: entry.Filter,
			Delete:   toDelete,
			Keep:     totalAfterSync - deleteCount,
		})
	}

	return cleanups
}

// trimForInitial reduces a set of filtered snapshots to at most min_keep per
// interval. For each filter entry, only the newest entry.MinKeep snapshots
// are retained. The result is the union of all kept snapshots, preserving
// the original creation-time order.
func (pb *PlanBuilder) trimForInitial(snaps []zfs.Snapshot) []zfs.Snapshot {
	// Collect GUIDs of snapshots to keep.
	keepGUIDs := make(map[string]struct{})
	for _, entry := range pb.Config.SnapshotFilter {
		matched := FilterSnapshotsByInterval(snaps, entry.Filter)
		// Keep only the newest min_keep (= tail of the slice, since oldest first).
		if len(matched) > entry.MinKeep {
			matched = matched[len(matched)-entry.MinKeep:]
		}
		for _, s := range matched {
			keepGUIDs[s.GUID] = struct{}{}
		}
	}

	return lo.Filter(snaps, func(s zfs.Snapshot, _ int) bool {
		return lo.HasKey(keepGUIDs, s.GUID)
	})
}

// uniqueName returns a name derived from base that does not collide with any
// entry in existing. It tries "<base>-<postfix>", then "<base>-<postfix>-2",
// "<base>-<postfix>-3", etc.
func uniqueName(base, postfix string, existing []string) string {
	names := lo.SliceToMap(existing, func(s string) (string, struct{}) {
		return s, struct{}{}
	})
	candidate := base + "-" + postfix
	if !lo.HasKey(names, candidate) {
		return candidate
	}
	for i := 2; ; i++ {
		candidate = fmt.Sprintf("%s-%s-%d", base, postfix, i)
		if !lo.HasKey(names, candidate) {
			return candidate
		}
	}
}
