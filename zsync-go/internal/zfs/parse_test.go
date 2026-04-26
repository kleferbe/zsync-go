package zfs

import (
	"testing"
	"time"
)

func TestParseSnapshots(t *testing.T) {
	raw := "rpool/data@hourly-2025-01-15-1000\t12345678901234567\t1736935200\n" +
		"rpool/data@daily-2025-01-15-0000\t12345678901234568\t1736899200\n"

	snaps, err := parseSnapshots(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}

	s := snaps[0]
	if s.Name != "rpool/data@hourly-2025-01-15-1000" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Dataset != "rpool/data" {
		t.Errorf("dataset = %q", s.Dataset)
	}
	if s.ShortName != "hourly-2025-01-15-1000" {
		t.Errorf("short_name = %q", s.ShortName)
	}
	if s.GUID != "12345678901234567" {
		t.Errorf("guid = %q", s.GUID)
	}
	expected := time.Unix(1736935200, 0)
	if !s.Creation.Equal(expected) {
		t.Errorf("creation = %v, want %v", s.Creation, expected)
	}
}

func TestParseSnapshotsEmpty(t *testing.T) {
	snaps, err := parseSnapshots("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("got %d snapshots, want 0", len(snaps))
	}
}

func TestParseSnapshotsInvalidCreation(t *testing.T) {
	raw := "rpool/data@snap\t123\tnotanumber\n"
	_, err := parseSnapshots(raw)
	if err == nil {
		t.Fatal("expected error for invalid creation timestamp")
	}
}

func TestParseDatasetProperties(t *testing.T) {
	raw := "rpool/data\tbashclub:zsync\tall\tlocal\n" +
		"rpool/data/child\tbashclub:zsync\tall\tinherited from rpool/data\n" +
		"rpool/other\tbashclub:zsync\texclude\tlocal\n"

	props, err := parseDatasetProperties(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(props) != 3 {
		t.Fatalf("got %d properties, want 3", len(props))
	}

	tests := []struct {
		dataset string
		value   string
		source  PropertySource
	}{
		{"rpool/data", "all", SourceLocal},
		{"rpool/data/child", "all", SourceInherited},
		{"rpool/other", "exclude", SourceLocal},
	}
	for i, tc := range tests {
		p := props[i]
		if p.Dataset != tc.dataset {
			t.Errorf("[%d] dataset = %q, want %q", i, p.Dataset, tc.dataset)
		}
		if p.Property.Value != tc.value {
			t.Errorf("[%d] value = %q, want %q", i, p.Property.Value, tc.value)
		}
		if p.Property.Source != tc.source {
			t.Errorf("[%d] source = %q, want %q", i, p.Property.Source, tc.source)
		}
	}
}

func TestParseDatasetPropertiesEmpty(t *testing.T) {
	props, err := parseDatasetProperties("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(props) != 0 {
		t.Errorf("got %d properties, want 0", len(props))
	}
}

func TestParseDatasetType(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  DatasetType
	}{
		{"filesystem", Filesystem},
		{"filesystem\n", Filesystem},
		{"volume", Volume},
	} {
		got, err := parseDatasetType(tc.input)
		if err != nil {
			t.Errorf("parseDatasetType(%q): unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("parseDatasetType(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	_, err := parseDatasetType("bookmark")
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestParsePropertySource(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  PropertySource
	}{
		{"local", SourceLocal},
		{"received", SourceReceived},
		{"default", SourceDefault},
		{"-", SourceNone},
		{"inherited from rpool/data", SourceInherited},
	} {
		got := parsePropertySource(tc.input)
		if got != tc.want {
			t.Errorf("parsePropertySource(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSplitSnapshotName(t *testing.T) {
	ds, snap := splitSnapshotName("pool/data@hourly-2025-01-15")
	if ds != "pool/data" {
		t.Errorf("dataset = %q", ds)
	}
	if snap != "hourly-2025-01-15" {
		t.Errorf("snapshot = %q", snap)
	}

	ds2, snap2 := splitSnapshotName("pool/data")
	if ds2 != "pool/data" {
		t.Errorf("dataset = %q", ds2)
	}
	if snap2 != "" {
		t.Errorf("snapshot should be empty, got %q", snap2)
	}
}
