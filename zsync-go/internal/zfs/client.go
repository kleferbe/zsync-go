// Package zfs provides a typed client for ZFS operations.
// All commands are executed through an exec.Executor, which allows
// transparent local or remote (SSH) execution.
package zfs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kleferbe/zsync/internal/exec"
	"github.com/samber/lo"
)

// Client wraps ZFS operations. It delegates command execution to an
// Executor, so the same methods work for local and remote datasets.
type Client struct {
	exec exec.Executor
}

// NewClient creates a ZFS client that runs commands via the given executor.
func NewClient(e exec.Executor) *Client {
	return &Client{exec: e}
}

// Executor returns the underlying executor (useful for pipe operations).
func (c *Client) Executor() exec.Executor {
	return c.exec
}

// ---------------------------------------------------------------------------
// Dataset operations
// ---------------------------------------------------------------------------

// GetProperty reads a single ZFS property for a dataset.
//
//	zfs get -H -o value <property> <dataset>
func (c *Client) GetProperty(ctx context.Context, dataset, property string) (string, error) {
	out, err := c.exec.Run(ctx, "zfs", "get", "-H", "-o", "value", property, dataset)
	if err != nil {
		return "", err
	}
	return parsePropertyValue(out), nil
}

// GetDatasetProperties queries a property across all filesystems and volumes.
// Returns dataset name, property value and source for each.
//
//	zfs get -H -o name,property,value,source -t filesystem,volume <property>
func (c *Client) GetDatasetProperties(ctx context.Context, property string) ([]DatasetProperty, error) {
	out, err := c.exec.Run(ctx, "zfs", "get", "-H", "-o", "name,property,value,source", "-t", "filesystem,volume", property)
	if err != nil {
		return nil, err
	}
	return parseDatasetProperties(out)
}

// GetDatasetType returns the type (filesystem or volume) of a dataset.
//
//	zfs get -H -o value type <dataset>
func (c *Client) GetDatasetType(ctx context.Context, dataset string) (DatasetType, error) {
	out, err := c.exec.Run(ctx, "zfs", "get", "-H", "-o", "value", "type", dataset)
	if err != nil {
		return "", err
	}
	return parseDatasetType(out)
}

// DatasetExists checks whether a dataset exists.
//
//	zfs list -H <dataset>
func (c *Client) DatasetExists(ctx context.Context, dataset string) bool {
	_, err := c.exec.Run(ctx, "zfs", "list", "-H", dataset)
	return parseDatasetExists(err)
}

// ListSnapshots returns all snapshots for a dataset, sorted by creation
// time ascending.
//
//	zfs list -H -o name,guid,creation -p -t snapshot -s creation <dataset>
func (c *Client) ListSnapshots(ctx context.Context, dataset string) ([]Snapshot, error) {
	out, err := c.exec.Run(ctx, "zfs", "list", "-H", "-o", "name,guid,creation", "-p", "-t", "snapshot", "-s", "creation", dataset)
	if err != nil {
		// No snapshots is not necessarily an error – zfs list exits non-zero
		// when there are no results for some versions. Return empty.
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "no datasets available") {
			return nil, nil
		}
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return parseSnapshots(out)
}

// ListDatasets returns the names of all datasets (filesystems and volumes)
// at and below the given root, recursively.
//
//	zfs list -H -o name -r -t filesystem,volume <dataset>
func (c *Client) ListDatasets(ctx context.Context, root string) ([]string, error) {
	out, err := c.exec.Run(ctx, "zfs", "list", "-H", "-o", "name", "-r", "-t", "filesystem,volume", root)
	if err != nil {
		return nil, err
	}
	return lo.FilterMap(strings.Split(out, "\n"), func(line string, _ int) (string, bool) {
		line = strings.TrimRight(line, "\r")
		return line, line != ""
	}), nil
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// CreateDataset creates a dataset with canmount=noauto and optional extra properties.
//
//	zfs create [-p] -o canmount=noauto [-o key=val ...] <dataset>
func (c *Client) CreateDataset(ctx context.Context, dataset string, parents bool, props map[string]string) error {
	args := []string{"create"}
	if parents {
		args = append(args, "-p")
	}
	args = append(args, "-o", "canmount=noauto")
	for k, v := range props {
		args = append(args, "-o", k+"="+v)
	}
	args = append(args, dataset)

	slog.Info("creating dataset", "dataset", dataset, "executor", c.exec.String())
	_, err := c.exec.Run(ctx, "zfs", args...)
	return err
}

// SetProperty sets a property on a dataset.
//
//	zfs set <property>=<value> <dataset>
func (c *Client) SetProperty(ctx context.Context, dataset, property, value string) error {
	_, err := c.exec.Run(ctx, "zfs", "set", property+"="+value, dataset)
	return err
}

// DestroySnapshot destroys a single snapshot.
//
//	zfs destroy <snapshot>
func (c *Client) DestroySnapshot(ctx context.Context, snapshot string) error {
	slog.Info("destroying snapshot", "snapshot", snapshot, "executor", c.exec.String())
	_, err := c.exec.Run(ctx, "zfs", "destroy", snapshot)
	return err
}

// ---------------------------------------------------------------------------
// Send / Receive (pipeline)
// ---------------------------------------------------------------------------

// SendOptions controls zfs send behaviour.
type SendOptions struct {
	// Raw enables raw (encrypted) send (-w).
	Raw bool
	// Props includes properties in the stream (-p). Only for initial sends.
	Props bool
	// IncrementalBase is the name of the base snapshot for incremental sends (-i).
	IncrementalBase string
}

// ReceiveOptions controls zfs receive behaviour.
type ReceiveOptions struct {
	// Force enables forced receive with rollback (-F).
	Force bool
	// DiscardFirstName replaces the first element of the received dataset path (-d).
	DiscardFirstName bool
	// ExcludeProperties lists properties to exclude from receive (-x prop).
	ExcludeProperties []string
	// SetProperties lists properties to override (-o key=val).
	SetProperties map[string]string
}

// SendReceive pipes zfs send on the source to zfs receive on the target.
func SendReceive(ctx context.Context, source *Client, target *Client, snapshot string, targetDataset string, sendOpts SendOptions, recvOpts ReceiveOptions) error {
	sendArgs := buildSendArgs(snapshot, sendOpts)
	recvArgs := buildReceiveArgs(targetDataset, recvOpts)

	slog.Info("send/receive",
		"snapshot", snapshot,
		"target", targetDataset,
		"sender", source.exec.String(),
		"receiver", target.exec.String(),
	)

	return source.exec.RunPipe(ctx, target.exec, "zfs", sendArgs, "zfs", recvArgs)
}

func buildSendArgs(snapshot string, opts SendOptions) []string {
	args := []string{"send"}
	if opts.Raw {
		args = append(args, "-w")
	}
	if opts.Props {
		args = append(args, "-p")
	}
	if opts.IncrementalBase != "" {
		args = append(args, "-i", opts.IncrementalBase)
	}
	args = append(args, snapshot)
	return args
}

func buildReceiveArgs(target string, opts ReceiveOptions) []string {
	args := []string{"receive"}
	if opts.Force {
		args = append(args, "-F")
	}
	for _, p := range opts.ExcludeProperties {
		args = append(args, "-x", p)
	}
	for k, v := range opts.SetProperties {
		args = append(args, "-o", fmt.Sprintf("%s=%s", k, v))
	}
	if opts.DiscardFirstName {
		args = append(args, "-d")
	}
	args = append(args, target)
	return args
}
