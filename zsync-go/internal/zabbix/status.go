package zabbix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/exec"
	"github.com/kleferbe/zsync/internal/replication"
	"github.com/kleferbe/zsync/internal/zfs"
	"github.com/samber/lo"
)

// Report is the status file read by the Zabbix agent.
type Report struct {
	OK          bool            `json:"ok"`
	WrittenAt   int64           `json:"writtenAt"`
	Error       string          `json:"error"`
	DurationSec int64           `json:"durationSec"`
	Datasets    []DatasetStatus `json:"datasets"`
}

// DatasetStatus is the replication state of one dataset.
type DatasetStatus struct {
	SourceDataset                 string `json:"sourceDataset"`
	TargetDataset                 string `json:"targetDataset"`
	CommonSnapshot                string `json:"commonSnapshot"`
	CommonSnapshotCreatedAt       int64  `json:"commonSnapshotCreatedAt"`
	LatestSourceSnapshot          string `json:"latestSourceSnapshot"`
	LatestSourceSnapshotCreatedAt int64  `json:"latestSourceSnapshotCreatedAt"`
	SourceSnapshotCount           int    `json:"sourceSnapshotCount"`
	TargetSnapshotCount           int    `json:"targetSnapshotCount"`
	BytesSent                     int64  `json:"bytesSent"`
	DurationSec                   int64  `json:"durationSec"`
	Error                         string `json:"error"`
}

// Run builds the status from the pre-sync source inventory, the plan and a
// fresh target snapshot list, then writes it atomically.
// execErr is the error returned by Execute itself. result may be nil in that case.
func Run(
	ctx context.Context,
	cfg *config.Config,
	source *replication.SourceState,
	plan *replication.Plan,
	result *replication.ExecuteResult,
	execErr error,
	target *zfs.Client,
	sourceExec exec.Executor,
) error {
	var targetSnaps map[string][]zfs.Snapshot
	var listErrs map[string]error
	if execErr == nil && plan != nil {
		targetSnaps, listErrs = listTargetSnapshots(ctx, target, plan)
	}
	report := buildReport(time.Now(), cfg.SnapshotFilter, source, plan, targetSnaps, listErrs, result, execErr)
	return publish(ctx, cfg, sourceExec, report)
}

func listTargetSnapshots(ctx context.Context, target *zfs.Client, plan *replication.Plan) (map[string][]zfs.Snapshot, map[string]error) {
	snaps := map[string][]zfs.Snapshot{}
	errs := map[string]error{}
	for _, dp := range plan.Datasets {
		listed, err := target.ListSnapshots(ctx, dp.TargetDataset)
		if err != nil {
			errs[dp.TargetDataset] = err
			continue
		}
		snaps[dp.TargetDataset] = listed
	}
	return snaps, errs
}

func buildReport(
	now time.Time,
	filter config.SnapshotFilter,
	source *replication.SourceState,
	plan *replication.Plan,
	targetSnaps map[string][]zfs.Snapshot,
	listErrs map[string]error,
	result *replication.ExecuteResult,
	execErr error,
) Report {
	report := Report{
		WrittenAt: now.Unix(),
		Datasets:  []DatasetStatus{},
	}
	report.DurationSec = durationSec(result)
	if execErr != nil {
		report.Error = execErr.Error()
		return report
	}
	if plan == nil {
		return report
	}

	bySource := lo.SliceToMap(sourceDatasets(source), func(ds replication.SourceDatasetInfo) (string, replication.SourceDatasetInfo) {
		return ds.Name, ds
	})
	report.Datasets = lo.Map(plan.Datasets, func(dp replication.DatasetPlan, _ int) DatasetStatus {
		srcSnaps := replication.FilterSnapshots(bySource[dp.SourceDataset].Snapshots, filter)
		tgtSnaps := replication.FilterSnapshots(targetSnaps[dp.TargetDataset], filter)
		tgtGUIDs := lo.SliceToMap(tgtSnaps, func(s zfs.Snapshot) (string, struct{}) {
			return s.GUID, struct{}{}
		})
		return datasetStatus(dp, srcSnaps, tgtSnaps, tgtGUIDs, datasetProblems(dp, result, listErrs), datasetResult(result, dp.SourceDataset))
	})

	problems := lo.FilterMap(report.Datasets, func(ds DatasetStatus, _ int) (string, bool) {
		return ds.SourceDataset + ": " + ds.Error, ds.Error != ""
	})
	report.Error = strings.Join(problems, "; ")
	report.OK = !lo.SomeBy(report.Datasets, func(ds DatasetStatus) bool {
		return ds.Error != "" || ds.CommonSnapshotCreatedAt == 0
	})
	return report
}

func sourceDatasets(source *replication.SourceState) []replication.SourceDatasetInfo {
	if source == nil {
		return nil
	}
	return source.Datasets
}

func datasetProblems(dp replication.DatasetPlan, result *replication.ExecuteResult, listErrs map[string]error) string {
	var parts []string
	if result != nil {
		if err := result.SyncErrors[dp.SourceDataset]; err != nil {
			parts = append(parts, err.Error())
		}
		if err := result.CleanupErrors[dp.SourceDataset]; err != nil {
			parts = append(parts, err.Error())
		}
	}
	if err := listErrs[dp.TargetDataset]; err != nil {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func durationSec(result *replication.ExecuteResult) int64 {
	if result == nil {
		return 0
	}
	return int64(result.Duration.Round(time.Second) / time.Second)
}

func datasetResult(result *replication.ExecuteResult, source string) replication.DatasetResult {
	if result == nil {
		return replication.DatasetResult{}
	}
	return result.Datasets[source]
}

func datasetStatus(dp replication.DatasetPlan, srcSnaps, tgtSnaps []zfs.Snapshot, tgtGUIDs map[string]struct{}, problem string, measured replication.DatasetResult) DatasetStatus {
	status := DatasetStatus{
		SourceDataset:       dp.SourceDataset,
		TargetDataset:       dp.TargetDataset,
		SourceSnapshotCount: len(srcSnaps),
		TargetSnapshotCount: len(tgtSnaps),
		BytesSent:           measured.BytesSent,
		DurationSec:         int64(measured.Duration.Round(time.Second) / time.Second),
		Error:               problem,
	}
	if len(srcSnaps) > 0 {
		latest := lo.MaxBy(srcSnaps, newerThan)
		status.LatestSourceSnapshot = latest.ShortName
		status.LatestSourceSnapshotCreatedAt = latest.Creation.Unix()
	}
	commonSnaps := lo.Filter(srcSnaps, func(s zfs.Snapshot, _ int) bool {
		return lo.HasKey(tgtGUIDs, s.GUID)
	})
	if len(commonSnaps) > 0 {
		common := lo.MaxBy(commonSnaps, newerThan)
		status.CommonSnapshot = common.ShortName
		status.CommonSnapshotCreatedAt = common.Creation.Unix()
	}
	return status
}

func newerThan(item, max zfs.Snapshot) bool {
	return item.Creation.After(max.Creation)
}

func publish(ctx context.Context, cfg *config.Config, sourceExec exec.Executor, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding zabbix status: %w", err)
	}
	data = append(data, '\n')

	tmpPath, err := writeTemp(data)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	if cfg.Zabbix.Place == config.PlaceSource && !cfg.Source.SSH.IsLocal() {
		return moveToSource(ctx, cfg, sourceExec, tmpPath)
	}
	slog.Info("writing zabbix status", "path", cfg.Zabbix.Path)
	return moveLocal(tmpPath, cfg.Zabbix.Path)
}

func writeTemp(data []byte) (string, error) {
	tmp, err := os.CreateTemp("", "zsync-status-*")
	if err != nil {
		return "", fmt.Errorf("creating status file: %w", err)
	}
	name := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(name)
		if writeErr != nil {
			return "", fmt.Errorf("writing status file: %w", writeErr)
		}
		return "", fmt.Errorf("closing status file: %w", closeErr)
	}
	if err := os.Chmod(name, 0644); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("setting status file mode: %w", err)
	}
	return name, nil
}

func moveLocal(tmpPath, dest string) error {
	staged := filepath.Join(filepath.Dir(dest), ".zsync-status-"+filepath.Base(tmpPath))
	if err := os.Rename(tmpPath, staged); err != nil {
		if err := copyFile(tmpPath, staged); err != nil {
			return fmt.Errorf("moving status file: %w", err)
		}
	}
	if err := os.Rename(staged, dest); err != nil {
		if !os.IsExist(err) || os.Remove(dest) != nil || os.Rename(staged, dest) != nil {
			os.Remove(staged)
			return fmt.Errorf("replacing status file: %w", err)
		}
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(dest)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return nil
}

func moveToSource(ctx context.Context, cfg *config.Config, sourceExec exec.Executor, tmpPath string) error {
	slog.Info("copying zabbix status to source", "host", cfg.Source.SSH.Host, "path", cfg.Zabbix.Path)

	remoteTmp := cfg.Zabbix.Path + ".tmp"
	dest := fmt.Sprintf("%s:%s", cfg.Source.SSH.Host, remoteTmp)
	scpArgs := []string{"-P", fmt.Sprintf("%d", cfg.Source.SSH.Port), tmpPath, dest}
	if _, err := (&exec.LocalExecutor{}).Run(ctx, "scp", scpArgs...); err != nil {
		return fmt.Errorf("scp status file: %w", err)
	}
	if _, err := sourceExec.Run(ctx, "mv", "-f", remoteTmp, cfg.Zabbix.Path); err != nil {
		return fmt.Errorf("replacing remote status file: %w", err)
	}
	return nil
}
