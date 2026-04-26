package replication

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/kleferbe/zsync/internal/zfs"
	"github.com/samber/lo"
)

// WritePlanText writes a human-readable representation of the plan to w.
// This is separated from the plan structure so the display format does not
// influence the plan's data model.
func WritePlanText(w io.Writer, plan *Plan) {
	if plan.NeedTargetRoot {
		fmt.Fprintf(w, "Target root %q does not exist and will be created.\n\n", plan.TargetRootDataset)
	}

	if len(plan.Datasets) == 0 {
		fmt.Fprintln(w, "No datasets to replicate.")
		return
	}

	// Summary table
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tSOURCE\tTARGET\tSNAPS\tREASON")
	fmt.Fprintln(tw, "------\t------\t------\t-----\t------")
	for _, dp := range plan.Datasets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			dp.Action, dp.SourceDataset, dp.TargetDataset, len(dp.SendSnapshots), dp.Reason)
	}
	tw.Flush()

	// Details per dataset
	for _, dp := range plan.Datasets {
		if dp.Action == ActionSkip {
			continue
		}

		fmt.Fprintf(w, "\n--- %s ---\n", dp.SourceDataset)

		switch dp.Action {
		case ActionInitial:
			fmt.Fprintf(w, "  Type:    %s\n", dp.DatasetType)
			fmt.Fprintf(w, "  Action:  initial replication\n")
			fmt.Fprintf(w, "  Send %d snapshot(s) (first full, rest incremental):\n", len(dp.SendSnapshots))
			for _, s := range dp.SendSnapshots {
				fmt.Fprintf(w, "    → %s\n", s.ShortName)
			}

		case ActionIncremental:
			fmt.Fprintf(w, "  Type:    %s\n", dp.DatasetType)
			fmt.Fprintf(w, "  Action:  incremental replication\n")
			if dp.CommonSnapshot != nil {
				fmt.Fprintf(w, "  Common:  %s\n", dp.CommonSnapshot.Name)
			}
			fmt.Fprintf(w, "  Send %d snapshot(s):\n", len(dp.SendSnapshots))
			for _, s := range dp.SendSnapshots {
				fmt.Fprintf(w, "    → %s\n", s.ShortName)
			}

		case ActionReinitialize:
			fmt.Fprintf(w, "  Type:    %s\n", dp.DatasetType)
			fmt.Fprintf(w, "  Action:  reinitialize (no common snapshot)\n")
			fmt.Fprintf(w, "  Rename:  %s → %s\n", dp.TargetDataset, dp.RenameExistingTarget)
			fmt.Fprintf(w, "  Send %d snapshot(s) (first full, rest incremental):\n", len(dp.SendSnapshots))
			for _, s := range dp.SendSnapshots {
				fmt.Fprintf(w, "    → %s\n", s.ShortName)
			}

		case ActionError:
			fmt.Fprintf(w, "  ERROR: %s\n", dp.Reason)
		}

		if len(dp.Cleanup) > 0 {
			fmt.Fprintf(w, "  Cleanup:\n")
			for _, c := range dp.Cleanup {
				names := lo.Map(c.Delete, func(s zfs.Snapshot, _ int) string {
					return s.ShortName
				})
				fmt.Fprintf(w, "    [%s] delete %d, keep %d: %s\n",
					c.Interval, len(c.Delete), c.Keep, strings.Join(names, ", "))
			}
		}
	}
}
