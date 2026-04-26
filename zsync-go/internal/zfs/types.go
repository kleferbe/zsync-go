package zfs

import "time"

// DatasetType represents the ZFS dataset type.
type DatasetType string

const (
	Filesystem DatasetType = "filesystem"
	Volume     DatasetType = "volume"
)

// PropertySource indicates how a ZFS property was set.
type PropertySource string

const (
	SourceLocal     PropertySource = "local"
	SourceInherited PropertySource = "inherited"
	SourceReceived  PropertySource = "received"
	SourceDefault   PropertySource = "default"
	SourceNone      PropertySource = "-"
)

// Dataset represents a ZFS filesystem or volume.
type Dataset struct {
	Name string
	Type DatasetType
}

// Snapshot represents a single ZFS snapshot.
type Snapshot struct {
	// Name is the full snapshot name including dataset, e.g. "pool/data@daily-2025-01-15-1430".
	Name string
	// Dataset is the dataset part (before '@').
	Dataset string
	// ShortName is the snapshot part (after '@').
	ShortName string
	// GUID is the globally unique identifier of this snapshot.
	GUID string
	// Creation is the snapshot creation timestamp.
	Creation time.Time
}

// Property holds a ZFS property value together with its source.
type Property struct {
	Name   string
	Value  string
	Source PropertySource
}

// DatasetProperty holds a dataset name and a property.
type DatasetProperty struct {
	Dataset  string
	Property Property
}
