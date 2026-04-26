package zfs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/samber/lo"
)

// parseTabLines splits raw zfs output (tab-separated, one record per line)
// into a slice of string slices. Empty lines are skipped.
func parseTabLines(raw string) [][]string {
	return lo.FilterMap(strings.Split(raw, "\n"), func(line string, _ int) ([]string, bool) {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			return nil, false
		}
		return strings.Split(line, "\t"), true
	})
}

// parseSnapshots parses output of:
//
//	zfs list -H -o name,guid,creation -t snapshot -s creation <dataset>
//
// creation is expected as a unix timestamp (-p flag).
func parseSnapshots(raw string) ([]Snapshot, error) {
	lines := parseTabLines(raw)
	result := make([]Snapshot, 0, len(lines))
	for _, fields := range lines {
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		guid := fields[1]
		creationUnix, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing creation time %q for %s: %w", fields[2], name, err)
		}

		dataset, shortName := splitSnapshotName(name)
		result = append(result, Snapshot{
			Name:      name,
			Dataset:   dataset,
			ShortName: shortName,
			GUID:      guid,
			Creation:  time.Unix(creationUnix, 0),
		})
	}
	return result, nil
}

// parseDatasetProperties parses output of:
//
//	zfs get -H -o name,property,value,source -t filesystem,volume <property>
func parseDatasetProperties(raw string) ([]DatasetProperty, error) {
	lines := parseTabLines(raw)
	result := make([]DatasetProperty, 0, len(lines))
	for _, fields := range lines {
		if len(fields) < 4 {
			continue
		}
		src := parsePropertySource(fields[3])
		result = append(result, DatasetProperty{
			Dataset: fields[0],
			Property: Property{
				Name:   fields[1],
				Value:  fields[2],
				Source: src,
			},
		})
	}
	return result, nil
}

// parseDatasetType parses the value from:
//
//	zfs get -H -o value type <dataset>
func parseDatasetType(raw string) (DatasetType, error) {
	v := strings.TrimSpace(raw)
	switch DatasetType(v) {
	case Filesystem:
		return Filesystem, nil
	case Volume:
		return Volume, nil
	default:
		return "", fmt.Errorf("unknown dataset type %q", v)
	}
}

// parsePropertySource normalises the source column from zfs get output.
// Values like "inherited from pool/parent" are mapped to SourceInherited.
func parsePropertySource(raw string) PropertySource {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "local":
		return SourceLocal
	case raw == "received":
		return SourceReceived
	case raw == "default":
		return SourceDefault
	case raw == "-":
		return SourceNone
	case strings.HasPrefix(raw, "inherited"):
		return SourceInherited
	default:
		return PropertySource(raw)
	}
}

// splitSnapshotName splits "pool/data@snap" into ("pool/data", "snap").
func splitSnapshotName(name string) (dataset, shortName string) {
	if i := strings.Index(name, "@"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

// parseDatasetExists checks whether a `zfs list -H <dataset>` call
// succeeded. The caller should pass err from the Run call.
// Returns true if the dataset exists (err == nil).
func parseDatasetExists(err error) bool {
	return err == nil
}

// parsePropertyValue parses output of:
//
//	zfs get -H -o value <property> <dataset>
func parsePropertyValue(raw string) string {
	return strings.TrimSpace(raw)
}
