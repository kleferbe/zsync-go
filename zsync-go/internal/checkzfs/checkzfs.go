package checkzfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kleferbe/zsync/internal/config"
	"github.com/kleferbe/zsync/internal/exec"
)

const (
	spoolDir           = "/var/lib/check_mk_agent/spool"
	defaultCheckzfsBin = "checkzfs"
	localHeader        = "<<<local>>>"
	spoolLocal         = "local"
	spoolSource        = "source"
)

// Run executes the checkzfs monitoring tool and places the output into the
// Checkmk agent spool directory. Depending on configuration, the spool file
// is placed locally or copied to the source host via scp.
func Run(ctx context.Context, cfg *config.Config, localExec, sourceExec exec.Executor) error {
	czCfg := cfg.CheckZFS

	bin := czCfg.Path
	if bin == "" {
		bin = defaultCheckzfsBin
	}

	// Verify checkzfs is available.
	if _, err := localExec.Run(ctx, "which", bin); err != nil {
		return fmt.Errorf("checkzfs binary %q not found: %w", bin, err)
	}

	spoolFileName := fmt.Sprintf("%d_%s", czCfg.SpoolMaxAge, czCfg.Prefix)
	tmpFile := filepath.Join(os.TempDir(), spoolFileName)

	// Write Checkmk section header.
	if err := os.WriteFile(tmpFile, []byte(localHeader+"\n"), 0644); err != nil {
		return fmt.Errorf("writing spool header: %w", err)
	}

	// Build checkzfs command arguments.
	args := buildArgs(cfg)

	slog.Info("running checkzfs", "bin", bin, "args", args)
	output, err := localExec.Run(ctx, bin, args...)
	if err != nil {
		// checkzfs may return non-zero for WARN/CRIT states.
		// The output is still valid and should be spooled.
		slog.Warn("checkzfs returned error (may indicate WARN/CRIT state)", "error", err)
	}

	// Append output to temp file.
	if output != "" {
		f, err := os.OpenFile(tmpFile, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("opening spool file for append: %w", err)
		}
		_, writeErr := fmt.Fprintln(f, output)
		closeErr := f.Close()
		if writeErr != nil {
			return fmt.Errorf("writing checkzfs output: %w", writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing spool file: %w", closeErr)
		}
	}

	// Move spool file to destination.
	if czCfg.Spool == spoolSource && !cfg.Source.SSH.IsLocal() {
		return spoolToSource(ctx, cfg, sourceExec, tmpFile, spoolFileName)
	}
	return spoolLocally(ctx, localExec, tmpFile, spoolFileName)
}

// buildArgs constructs the checkzfs CLI arguments from configuration.
func buildArgs(cfg *config.Config) []string {
	czCfg := cfg.CheckZFS

	var args []string

	// --source host:port (only when not local mode)
	if !cfg.Source.SSH.IsLocal() {
		args = append(args, "--source", fmt.Sprintf("%s:%d", cfg.Source.SSH.Host, cfg.Source.SSH.Port))
	}

	args = append(args,
		"--output", "checkmk",
		"--threshold", czCfg.MaxAge,
		"--maxsnapshots", czCfg.MaxSnapshotCount,
		"--prefix", czCfg.Prefix,
		"--replicafilter", fmt.Sprintf("^%s", cfg.Target.Dataset),
		"--filter", cfg.SnapshotFilter.Regex(),
	)

	return args
}

// spoolLocally moves the temp file into the local Checkmk spool directory.
func spoolLocally(ctx context.Context, localExec exec.Executor, tmpFile, spoolFileName string) error {
	slog.Info("spooling checkzfs output locally", "dest", spoolDir, "file", spoolFileName)

	if _, err := localExec.Run(ctx, "mkdir", "-p", spoolDir); err != nil {
		return fmt.Errorf("creating spool dir: %w", err)
	}

	dest := filepath.Join(spoolDir, spoolFileName)
	if _, err := localExec.Run(ctx, "mv", tmpFile, dest); err != nil {
		return fmt.Errorf("moving spool file to %s: %w", dest, err)
	}

	return nil
}

// spoolToSource copies the temp file to the source host's Checkmk spool
// directory via scp, then removes the local temp file.
func spoolToSource(ctx context.Context, cfg *config.Config, sourceExec exec.Executor, tmpFile, spoolFileName string) error {
	slog.Info("spooling checkzfs output to source", "host", cfg.Source.SSH.Host, "file", spoolFileName)

	// Ensure spool directory exists on source.
	if _, err := sourceExec.Run(ctx, "mkdir", "-p", spoolDir); err != nil {
		return fmt.Errorf("creating remote spool dir: %w", err)
	}

	// Use scp to copy the file. We go through the local executor since scp
	// runs locally and connects to the remote host.
	dest := fmt.Sprintf("%s:%s/%s", cfg.Source.SSH.Host, spoolDir, spoolFileName)
	scpArgs := []string{"-P", fmt.Sprintf("%d", cfg.Source.SSH.Port), tmpFile, dest}

	if _, err := (&exec.LocalExecutor{}).Run(ctx, "scp", scpArgs...); err != nil {
		return fmt.Errorf("scp to source: %w", err)
	}

	// Clean up local temp file.
	os.Remove(tmpFile)
	return nil
}
