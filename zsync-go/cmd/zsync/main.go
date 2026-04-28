package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kleferbe/zsync/internal/checkzfs"
	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/exec"
	"github.com/kleferbe/zsync/internal/replication"
	"github.com/kleferbe/zsync/internal/zfs"
)

var version = "dev"

func main() {
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	defaultConfig := filepath.Join(filepath.Dir(exePath), "zsync.yaml")

	var (
		configPath = flag.String("c", defaultConfig, "path to configuration file")
		debug      = flag.Bool("d", false, "enable debug logging")
		dryRun     = flag.Bool("dry-run", false, "build and display replication plan without executing")
		logFile    = flag.String("l", "", "write log output to `file` instead of stderr")
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

	logWriter := io.Writer(os.Stderr)
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot open log file %s: %v\n", *logFile, err)
			os.Exit(1)
		}
		defer f.Close()
		logWriter = f
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	slog.Info("configuration loaded",
		"target", cfg.Target.Dataset,
		"ssh.host", cfg.Source.SSH.Host,
		"ssh.port", cfg.Source.SSH.Port,
		"tag", cfg.Source.Tag,
		"snapshot_filter", cfg.SnapshotFilter.Regex(),
		"local_mode", cfg.Source.SSH.IsLocal(),
	)

	ctx := context.Background()

	// Build executors.
	localExec := exec.NewLocal()
	var sourceExec exec.Executor
	if cfg.Source.SSH.IsLocal() {
		sourceExec = localExec
	} else {
		sshExec := exec.NewSSH(cfg.Source.SSH.Host, cfg.Source.SSH.Port)

		// Negotiate optimal SSH cipher based on AES-NI hardware support.
		cipher := exec.NegotiateCipher(ctx, localExec, sshExec)
		sshExec.SetCipher(cipher)

		sourceExec = sshExec
	}

	sourceClient := zfs.NewClient(sourceExec)
	targetClient := zfs.NewClient(localExec)

	// Phase 1: Discover source datasets.
	fmt.Println("Discovering source datasets...")
	slog.Info("discovering source datasets")
	srcState, err := replication.DiscoverSource(ctx, cfg, sourceClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: source discovery failed: %v\n", err)
		os.Exit(1)
	}

	// Phase 2: Discover target datasets.
	fmt.Println("Discovering target datasets...")
	slog.Info("discovering target datasets")
	tgtState, err := replication.DiscoverTarget(ctx, cfg, targetClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: target discovery failed: %v\n", err)
		os.Exit(1)
	}

	// Phase 3: Build replication plan.
	pb := &replication.PlanBuilder{
		Source: srcState,
		Target: tgtState,
		Config: cfg,
	}
	plan := pb.Build()

	// Display plan.
	replication.WritePlanText(os.Stdout, plan, *dryRun)

	if *dryRun {
		os.Exit(0)
	}

	// Phase 4: Execute plan.
	fmt.Println("\nExecuting replication plan...")
	slog.Info("executing replication plan")
	result, err := replication.Execute(ctx, plan, cfg, sourceClient, targetClient, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: execution failed: %v\n", err)
		os.Exit(1)
	}

	if result.HasErrors() {
		for ds, e := range result.SyncErrors {
			fmt.Fprintf(os.Stderr, "error: sync %s: %v\n", ds, e)
			slog.Error("sync error", "dataset", ds, "error", e)
		}
		for ds, e := range result.CleanupErrors {
			fmt.Fprintf(os.Stderr, "error: cleanup %s: %v\n", ds, e)
			slog.Error("cleanup error", "dataset", ds, "error", e)
		}
		os.Exit(1)
	}

	// Phase 5: checkzfs monitoring.
	if cfg.CheckZFS.Enabled {
		slog.Info("running checkzfs monitoring")
		if err := checkzfs.Run(ctx, cfg, localExec, sourceExec); err != nil {
			fmt.Fprintf(os.Stderr, "error: checkzfs failed: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("Replication completed successfully.")
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
