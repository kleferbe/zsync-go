package replication

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
)

// DatasetResult is the measured outcome of one dataset in this run.
// Skipped datasets have no entry.
type DatasetResult struct {
	// BytesSent is the number of bytes written into the send/receive pipe,
	// summed over every snapshot and every attempt, including partial failures.
	BytesSent int64
	// Duration is the wall-clock time of sync and cleanup.
	Duration time.Duration
}

// ExecuteResult holds the outcome of executing a plan.
type ExecuteResult struct {
	// SyncErrors collects per-dataset errors from the send/receive phase.
	// Datasets with a sync error are skipped for cleanup.
	SyncErrors map[string]error
	// CleanupErrors collects per-dataset errors from the cleanup phase.
	CleanupErrors map[string]error
	// Duration is the wall-clock time of the whole execution.
	Duration time.Duration
	// Datasets holds bytes and runtime for each dataset that was synced.
	Datasets map[string]DatasetResult
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
// Brief status messages are written to status (typically os.Stdout).
func Execute(ctx context.Context, plan *Plan, cfg *config.Config, source, target *zfs.Client, status io.Writer) (*ExecuteResult, error) {
	result := &ExecuteResult{
		SyncErrors:    make(map[string]error),
		CleanupErrors: make(map[string]error),
		Datasets:      make(map[string]DatasetResult),
	}
	started := time.Now()
	defer func() { result.Duration = time.Since(started) }()

	// Step 1: Create target root if needed.
	if plan.NeedTargetRoot {
		slog.Info("creating target root dataset", "dataset", plan.TargetRootDataset)
		if err := target.CreateDataset(ctx, plan.TargetRootDataset, true, nil); err != nil {
			return result, fmt.Errorf("creating target root %s: %w", plan.TargetRootDataset, err)
		}
	}

	// Step 2: Per-dataset sync + cleanup.
	total := 0
	for _, dp := range plan.Datasets {
		if dp.Action != ActionSkip && dp.Action != ActionError {
			total++
		}
	}

	current := 0
	for _, dp := range plan.Datasets {
		if dp.Action == ActionSkip || dp.Action == ActionError {
			continue
		}

		current++
		fmt.Fprintf(status, "  [%d/%d] %s → %s (%s, %d snapshots)\n",
			current, total, dp.SourceDataset, dp.TargetDataset, dp.Action, len(dp.SendSnapshots))
		dsStarted := time.Now()

		var bytes int64
		var syncErr error
		switch dp.Action {
		case ActionInitial:
			bytes, syncErr = executeInitial(ctx, &dp, cfg, source, target)
		case ActionReinitialize:
			bytes, syncErr = executeReinitialize(ctx, &dp, cfg, source, target)
		case ActionIncremental:
			bytes, syncErr = executeIncremental(ctx, &dp, cfg, source, target)
		}

		if syncErr != nil {
			slog.Error("sync failed", "dataset", dp.SourceDataset, "action", dp.Action, "error", syncErr)
			result.SyncErrors[dp.SourceDataset] = syncErr
		} else if len(dp.Cleanup) > 0 {
			if err := executeCleanup(ctx, &dp, target); err != nil {
				slog.Error("cleanup failed", "dataset", dp.SourceDataset, "error", err)
				result.CleanupErrors[dp.SourceDataset] = err
			}
		}

		result.Datasets[dp.SourceDataset] = DatasetResult{
			BytesSent: bytes,
			Duration:  time.Since(dsStarted),
		}
	}

	return result, nil
}

// buildRecvOpts creates ReceiveOptions appropriate for the dataset type.
// Filesystems get -o canmount=noauto and -x mountpoint; volumes do not.
// The replication tag property is always excluded.
func buildRecvOpts(dsType zfs.DatasetType, tag string, force bool, resumable bool) zfs.ReceiveOptions {
	opts := zfs.ReceiveOptions{
		Force:             force,
		Resumable:         resumable,
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
func executeInitial(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) (int64, error) {
	slog.Info("starting initial replication", "source", dp.SourceDataset, "target", dp.TargetDataset, "snapshots", len(dp.SendSnapshots))

	var sent int64
	for i, snap := range dp.SendSnapshots {
		sendOpts := zfs.SendOptions{Raw: true}
		recvOpts := buildRecvOpts(dp.DatasetType, cfg.Source.Tag, i > 0, cfg.Retry.IsResumable())

		if i == 0 {
			// First snapshot: full send with properties.
			sendOpts.Props = true
		} else {
			// Subsequent snapshots: incremental from previous.
			sendOpts.IncrementalBase = dp.SendSnapshots[i-1].Name
		}

		slog.Info("sending snapshot", "snapshot", snap.Name, "full", i == 0)
		n, err := zfs.SendReceive(ctx, source, target, snap.Name, dp.TargetDataset, sendOpts, recvOpts, cfg.Retry.MaxRetries, time.Duration(cfg.Retry.DelaySeconds)*time.Second)
		sent += n
		if err != nil {
			return sent, fmt.Errorf("sending %s: %w", snap.Name, err)
		}
	}

	return sent, nil
}

// executeReinitialize renames the existing target, then performs a fresh initial sync.
//
// ZFS commands:
//
//	zfs rename <target> <target-old>
//	(then same as executeInitial)
func executeReinitialize(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) (int64, error) {
	slog.Info("reinitializing dataset",
		"target", dp.TargetDataset,
		"rename_to", dp.RenameExistingTarget,
	)

	if err := target.RenameDataset(ctx, dp.TargetDataset, dp.RenameExistingTarget); err != nil {
		return 0, fmt.Errorf("renaming %s to %s: %w", dp.TargetDataset, dp.RenameExistingTarget, err)
	}

	return executeInitial(ctx, dp, cfg, source, target)
}

// executeIncremental sends pending snapshots incrementally from the common snapshot.
func executeIncremental(ctx context.Context, dp *DatasetPlan, cfg *config.Config, source, target *zfs.Client) (int64, error) {
	slog.Info("starting incremental replication",
		"source", dp.SourceDataset,
		"target", dp.TargetDataset,
		"common", dp.CommonSnapshot.Name,
		"snapshots", len(dp.SendSnapshots),
	)

	base := dp.CommonSnapshot.Name
	var sent int64
	for _, snap := range dp.SendSnapshots {
		sendOpts := zfs.SendOptions{
			Raw:             true,
			IncrementalBase: base,
		}
		recvOpts := buildRecvOpts(dp.DatasetType, cfg.Source.Tag, true, cfg.Retry.IsResumable())

		slog.Info("sending snapshot", "snapshot", snap.Name, "base", base)
		n, err := zfs.SendReceive(ctx, source, target, snap.Name, dp.TargetDataset, sendOpts, recvOpts, cfg.Retry.MaxRetries, time.Duration(cfg.Retry.DelaySeconds)*time.Second)
		sent += n
		if err != nil {
			return sent, fmt.Errorf("sending %s (base %s): %w", snap.Name, base, err)
		}

		base = snap.Name
	}

	return sent, nil
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
