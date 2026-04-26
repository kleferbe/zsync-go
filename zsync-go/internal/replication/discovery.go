package replication

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/zfs"
	"github.com/samber/lo"
)

// DiscoverSource collects the current ZFS state from the source host.
// It discovers tagged datasets and their snapshots. No mutations.
func DiscoverSource(ctx context.Context, cfg *config.Config, source *zfs.Client) (*SourceState, error) {
	state := &SourceState{}

	props, err := source.GetDatasetProperties(ctx, cfg.Source.Tag)
	if err != nil {
		return nil, fmt.Errorf("querying tag %q: %w", cfg.Source.Tag, err)
	}

	for _, root := range cfg.Source.Datasets {
		// Filter to datasets under this root.
		rootProps := lo.Filter(props, func(dp zfs.DatasetProperty, _ int) bool {
			return dp.Dataset == root || strings.HasPrefix(dp.Dataset, root+"/")
		})

		for _, dp := range rootProps {
			tagVal := config.TagValue(dp.Property.Value)
			tagSrc := dp.Property.Source

			if tagSrc == zfs.SourceReceived {
				slog.Debug("skipping received tag", "dataset", dp.Dataset)
				continue
			}

			if !shouldInclude(tagVal, tagSrc) {
				slog.Debug("excluding dataset", "dataset", dp.Dataset, "tag", tagVal, "source", tagSrc)
				continue
			}

			dsType, err := source.GetDatasetType(ctx, dp.Dataset)
			if err != nil {
				return nil, fmt.Errorf("getting type for %s: %w", dp.Dataset, err)
			}

			snaps, err := source.ListSnapshots(ctx, dp.Dataset)
			if err != nil {
				return nil, fmt.Errorf("listing snapshots for %s: %w", dp.Dataset, err)
			}

			state.Datasets = append(state.Datasets, SourceDatasetInfo{
				Name:      dp.Dataset,
				Type:      dsType,
				TagValue:  tagVal,
				TagSource: tagSrc,
				Snapshots: snaps,
			})

			slog.Debug("source dataset", "name", dp.Dataset, "type", dsType, "snapshots", len(snaps))
		}
	}

	slog.Info("discovered source datasets", "count", len(state.Datasets))
	return state, nil
}

// DiscoverTarget collects the current ZFS state from the local target host.
// It discovers all datasets under the configured target root and their
// snapshots. Matching to source datasets happens later in BuildPlan.
func DiscoverTarget(ctx context.Context, cfg *config.Config, target *zfs.Client) (*TargetState, error) {
	state := &TargetState{
		RootDataset: cfg.Target.Dataset,
		RootExists:  target.DatasetExists(ctx, cfg.Target.Dataset),
		Datasets:    make(map[string]TargetDatasetInfo),
	}

	slog.Debug("target root", "dataset", cfg.Target.Dataset, "exists", state.RootExists)

	if !state.RootExists {
		slog.Info("target root does not exist, nothing to discover")
		return state, nil
	}

	allNames, err := target.ListDatasets(ctx, cfg.Target.Dataset)
	if err != nil {
		return nil, fmt.Errorf("listing datasets under %s: %w", cfg.Target.Dataset, err)
	}

	// Skip the root dataset itself – it's tracked separately.
	names := lo.Without(allNames, cfg.Target.Dataset)

	for _, name := range names {

		snaps, err := target.ListSnapshots(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("listing snapshots for target %s: %w", name, err)
		}

		info := TargetDatasetInfo{
			Name:      name,
			Exists:    true,
			Snapshots: snaps,
		}

		slog.Debug("target dataset", "name", name, "snapshots", len(snaps))
		state.Datasets[name] = info
	}

	slog.Info("discovered target datasets", "count", len(state.Datasets))
	return state, nil
}

// shouldInclude determines whether a dataset with the given tag value and
// source should be included in replication.
func shouldInclude(tagVal config.TagValue, tagSrc zfs.PropertySource) bool {
	switch tagVal {
	case config.TagAll:
		return true
	case config.TagSubvols:
		// "subvols" on a locally-set dataset means: replicate the children,
		// not the dataset itself. Inherited → include.
		return tagSrc != zfs.SourceLocal
	case config.TagExclude:
		return false
	default:
		return false
	}
}
