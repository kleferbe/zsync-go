package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/kleferbe/zsync/internal/config"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("c", "/etc/zsync/zsync.yaml", "path to configuration file")
		debug      = flag.Bool("d", false, "enable debug logging")
		dryRun     = flag.Bool("dry-run", false, "build and display replication plan without executing")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("zsync", version)
		os.Exit(0)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("configuration loaded",
		"target", cfg.Target.Dataset,
		"ssh.host", cfg.Source.SSH.Host,
		"ssh.port", cfg.Source.SSH.Port,
		"tag", cfg.Source.Tag,
		"snapshot_filter", cfg.Source.SnapshotFilter.Regex(),
		"min_keep", cfg.Target.MinKeep,
		"local_mode", cfg.Source.SSH.IsLocal(),
	)

	if *dryRun {
		slog.Info("dry-run mode: plan would be displayed here")
		os.Exit(0)
	}

	// TODO: phases will be added in subsequent steps:
	// 1. Discover datasets on source (zfs get $tag)
	// 2. Collect snapshot state on source and target
	// 3. Build replication plan
	// 4. Execute plan (or print in dry-run mode)
	// 5. Cleanup old snapshots
	// 6. Run checkzfs monitoring

	slog.Info("zsync completed successfully")
}

func usage() {
	fmt.Fprintf(os.Stderr, `zsync %s - ZFS replication tool

Usage: zsync [flags]

Flags:
`, version)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
zsync replicates ZFS datasets from a source host to a local target dataset.
Datasets are selected via a ZFS user property on the source (default: bashclub:zsync).
Replication is incremental when a common snapshot exists on both sides.
`)
}
