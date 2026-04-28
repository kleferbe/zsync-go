package replication

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
)

// ExecuteResult holds the outcome of executing a plan.
type ExecuteResult struct {
	// SyncErrors collects per-dataset errors from the send/receive phase.
	// Datasets with a sync error are skipped for cleanup.
	SyncErrors map[string]error
	// CleanupErrors collects per-dataset errors from the cleanup phase.
	CleanupErrors map[string]error
}

// HasErrors returns true if any sync or cleanup errors occurred.
func (r *ExecuteResult) HasErrors() bool {
	return len(r.SyncErrors) > 0 || len(r.CleanupErrors) > 0
}

// Execute runs the replication plan. For each dataset it:
//  1. Performs the sync action (initial/incremental/reinitialize).
//  2. Runs cleanup immediately after a successful sync.
//
// The target root is created first if needed. Execute never stops on the first
// error — it processes all datasets and collects errors.
func Execute(ctx context.Context, plan *Plan, cfg *config.Config, source, target *zfs.Client) (*ExecuteResult, error) {
	result := &ExecuteResult{
		SyncErrors:    make(map[string]error),
		CleanupErrors: make(map[string]error),
	}

	// Step 1: Create target root if needed.
	if plan.NeedTargetRoot {
		slog.Info("creating target root dataset", "dataset", plan.TargetRootDataset)
		if err := target.CreateDataset(ctx, plan.TargetRootDataset, true, nil); err != nil {
			return nil, fmt.Errorf("creating target root %s: %w", plan.TargetRootDataset, err)
		}
	}

	// Step 2: Per-dataset sync + cleanup.
	for _, dp := range plan.Datasets {
		var syncErr error
		switch dp.Action {
		case ActionSkip, ActionError:
			continue
		case ActionInitial:
			syncErr = executeInitial(ctx, &dp, cfg, source, target)
		case ActionReinitialize:
			syncErr = executeReinitialize(ctx, &dp, cfg, source, target)
		case ActionIncremental:
			syncErr = executeIncremental(ctx, &dp, cfg, source, target)
		}

		if syncErr != nil {
			slog.Error("sync failed", "dataset", dp.SourceDataset, "action", dp.Action, "error", syncErr)
			result.SyncErrors[dp.SourceDataset] = syncErr
			continue
		}

		// Cleanup immediately after successful sync.
		if len(dp.Cleanup) > 0 {
			if err := executeCleanup(ctx, &dp, target); err != nil {
				slog.Error("cleanup failed", "dataset", dp.SourceDataset, "error", err)
				result.CleanupErrors[dp.SourceDataset] = err
			}
		}
	}

	return result, nil
}

// buildRecvOpts creates ReceiveOptions appropriate for the dataset type.
// Filesystems get -o canmount=noauto and -x mountpoint; volumes do not.
// The replication tag property is always excluded.
func buildRecvOpts(dsType zfs.DatasetType, tag string, force bool) zfs.ReceiveOptions {
	opts := zfs.ReceiveOptions{
		Force:             force,
		ExcludeProperties: []string{tag},
	}
	if dsType == zfs.Filesystem {
		opts.SetProperties = map[string]string{"canmount": "noauto"}
		opts.ExcludeProperties = append(opts.ExcludeProperties, "mountpoint")
	}
	return opts
}

// executeInitial sends all snapshots for a new dataset.
// The first snapshot is a full send, subsequent ones are incremental.
func executeInitial(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) error {
	slog.Info("starting initial replication", "source", dp.SourceDataset, "target", dp.TargetDataset, "snapshots", len(dp.SendSnapshots))

	for i, snap := range dp.SendSnapshots {
		sendOpts := zfs.SendOptions{Raw: true}
		recvOpts := buildRecvOpts(dp.DatasetType, cfg.Source.Tag, i > 0)

		if i == 0 {
			// First snapshot: full send with properties.
			sendOpts.Props = true
		} else {
			// Subsequent snapshots: incremental from previous.
			sendOpts.IncrementalBase = dp.SendSnapshots[i-1].Name
		}

		slog.Info("sending snapshot", "snapshot", snap.Name, "full", i == 0)
		if err := zfs.SendReceive(ctx, source, target, snap.Name, dp.TargetDataset, sendOpts, recvOpts, cfg.Retry.MaxRetries, time.Duration(cfg.Retry.DelaySeconds)*time.Second); err != nil {
			return fmt.Errorf("sending %s: %w", snap.Name, err)
		}
	}

	return nil
}

// executeReinitialize renames the existing target, then performs a fresh initial sync.
//
// ZFS commands:
//
//	zfs rename <target> <target-old>
//	(then same as executeInitial)
func executeReinitialize(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) error {
	slog.Info("reinitializing dataset",
		"target", dp.TargetDataset,
		"rename_to", dp.RenameExistingTarget,
	)

	if err := target.RenameDataset(ctx, dp.TargetDataset, dp.RenameExistingTarget); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", dp.TargetDataset, dp.RenameExistingTarget, err)
	}

	return executeInitial(ctx, dp, cfg, source, target)
}

// executeIncremental sends pending snapshots incrementally from the common snapshot.
func executeIncremental(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) error {
	slog.Info("starting incremental replication",
		"source", dp.SourceDataset,
		"target", dp.TargetDataset,
		"common", dp.CommonSnapshot.Name,
		"snapshots", len(dp.SendSnapshots),
	)

	base := dp.CommonSnapshot.Name
	for _, snap := range dp.SendSnapshots {
		sendOpts := zfs.SendOptions{
			Raw:             true,
			IncrementalBase: base,
		}
		recvOpts := buildRecvOpts(dp.DatasetType, cfg.Source.Tag, true)

		slog.Info("sending snapshot", "snapshot", snap.Name, "base", base)
		if err := zfs.SendReceive(ctx, source, target, snap.Name, dp.TargetDataset, sendOpts, recvOpts, cfg.Retry.MaxRetries, time.Duration(cfg.Retry.DelaySeconds)*time.Second); err != nil {
			return fmt.Errorf("sending %s (base %s): %w", snap.Name, base, err)
		}

		base = snap.Name
	}

	return nil
}

// executeCleanup removes old snapshots on the target as planned.
//
// ZFS commands:
//
//	zfs destroy <target>@<snapshot>
func executeCleanup(ctx context.Context, dp *DatasetPlan, target *zfs.Client) error {
	for _, c := range dp.Cleanup {
		slog.Info("cleaning up interval", "dataset", dp.TargetDataset, "interval", c.Interval, "delete", len(c.Delete), "keep", c.Keep)
		for _, snap := range c.Delete {
			if err := target.DestroySnapshot(ctx, snap.Name); err != nil {
				return fmt.Errorf("destroying %s: %w", snap.Name, err)
			}
		}
	}
	return nil
}
