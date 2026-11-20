// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrNoRuntimeForApply is returned when --apply is requested without a runtime.
var ErrNoRuntimeForApply = errors.New("image reclaim: --apply requires a removal runtime; no runtime was configured")

// CLI is the scriptable entry point. It reads a metadata snapshot, applies the same
// filters and protections as the API, and either prints the estimate (the default,
// which never removes anything) or executes it. The estimate path always runs first,
// so an apply is always preceded by exactly what it is about to delete.
type CLI struct {
	// Runtime performs removals for --apply.
	Runtime Runtime
	// SnapshotPath is the JSON snapshot file. Empty reads stdin.
	SnapshotPath string
	// Stdout and Stderr default to the process streams.
	Stdout io.Writer
	Stderr io.Writer
	// Now is injectable for tests.
	Now func() time.Time
	// stdinCache holds a snapshot read from stdin so that a later re-read, which
	// cannot consume stdin twice, still returns the same document.
	stdinCache []byte
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Run parses args and executes the requested mode. The arguments are deliberately
// long-form and stable so that scripts can rely on them:
//
//	--all                  consider every unused image, not only dangling ones
//	--filter name=GLOB     restrict by tag (repeatable)
//	--filter label=K=V     restrict by label (repeatable)
//	--filter before=RFC3339 restrict to images created before a time (repeatable)
//	--retain NAME          never remove images matching NAME (repeatable, glob allowed)
//	--prune-stopped        treat stopped containers as not using their image
//	--prune-intermediate   allow removal of build intermediates and untagged parents
//	--format text|json     output format (default text)
//	--apply                perform the removal instead of only estimating
func (c *CLI) Run(args []string) error {
	fs := flag.NewFlagSet("imagereclaim", flag.ContinueOnError)
	fs.SetOutput(c.stderr())

	var (
		all               bool
		pruneStopped      bool
		pruneIntermediate bool
		apply             bool
		format            string
		filters           stringList
		retain            stringList
		snapshotPath      string
	)
	fs.BoolVar(&all, "all", false, "consider every unused image, not only dangling ones")
	fs.BoolVar(&pruneStopped, "prune-stopped", false, "treat stopped containers as not using their image")
	fs.BoolVar(&pruneIntermediate, "prune-intermediate", false, "allow removal of build intermediates")
	fs.BoolVar(&apply, "apply", false, "perform removals instead of only estimating")
	fs.StringVar(&format, "format", string(FormatText), "output format: text or json")
	fs.Var(&filters, "filter", "filter in name=GLOB, label=K=V, or before=RFC3339 form (repeatable)")
	fs.Var(&retain, "retain", "image name to keep, glob allowed (repeatable)")
	fs.StringVar(&snapshotPath, "snapshot", c.SnapshotPath, "metadata snapshot file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("image reclaim: unexpected argument %q", fs.Arg(0))
	}

	f, err := parseFilters(filters)
	if err != nil {
		return err
	}
	opts := Options{
		All:                    all,
		PruneStoppedContainers: pruneStopped,
		PruneIntermediate:      pruneIntermediate,
		RetainedNames:          retain,
		Filters:                f,
		Now:                    c.now(),
	}

	snap, err := c.readSnapshot(snapshotPath)
	if err != nil {
		return err
	}
	plan, err := ComputePlan(snap, opts)
	if err != nil {
		return err
	}

	outFormat := Format(format)
	switch outFormat {
	case FormatText:
		if err := WritePlanText(c.stdout(), plan); err != nil {
			return err
		}
	case FormatJSON:
		if err := WritePlanJSON(c.stdout(), plan); err != nil {
			return err
		}
	default:
		return fmt.Errorf("image reclaim: unknown format %q", format)
	}

	if !apply {
		return nil
	}
	if c.Runtime == nil {
		return ErrNoRuntimeForApply
	}
	report, err := (&Executor{
		Runtime:  c.Runtime,
		Snapshot: c.snapshotProvider(snapshotPath),
		Now:      c.Now,
	}).Execute(plan)
	if err != nil {
		return err
	}
	switch outFormat {
	case FormatText:
		return WriteReportText(c.stdout(), report)
	default:
		return WriteReportJSON(c.stdout(), report)
	}
}

func (c *CLI) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CLI) stdout() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return os.Stdout
}

func (c *CLI) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

func (c *CLI) readSnapshot(path string) (*Snapshot, error) {
	switch path {
	case "", "-":
		if c.stdinCache == nil {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("read metadata snapshot from stdin: %w", err)
			}
			c.stdinCache = data
		}
		return DecodeSnapshot(bytes.NewReader(c.stdinCache))
	default:
		return c.readSnapshotFile(path)
	}
}

func (c *CLI) readSnapshotFile(path string) (*Snapshot, error) {
	f, err := os.Open(path) //nolint:gosec // the snapshot path is supplied by the invoking user
	if err != nil {
		return nil, fmt.Errorf("open metadata snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	return DecodeSnapshot(f)
}

// snapshotProvider supplies the pre-removal re-check. A runtime that can report its
// own live state is preferred, because a static snapshot file cannot show a container
// that started after the estimate; for a file-backed runtime, the file is re-read.
func (c *CLI) snapshotProvider(path string) func() (*Snapshot, error) {
	if source, ok := c.Runtime.(interface{ Snapshot() (*Snapshot, error) }); ok {
		return source.Snapshot
	}
	return func() (*Snapshot, error) { return c.readSnapshot(path) }
}

func parseFilters(values []string) (Filters, error) {
	var f Filters
	for _, v := range values {
		key, val, ok := strings.Cut(v, "=")
		if !ok {
			return f, fmt.Errorf("image reclaim: filter %q must be key=value", v)
		}
		switch key {
		case "name":
			f.Names = append(f.Names, val)
		case "label":
			k, labelValue, ok := strings.Cut(val, "=")
			if !ok {
				return f, fmt.Errorf("image reclaim: label filter %q must be label=key=value", v)
			}
			if f.Labels == nil {
				f.Labels = make(map[string]string)
			}
			f.Labels[k] = labelValue
		case "before":
			t, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return f, fmt.Errorf("image reclaim: before filter %q: %w", v, err)
			}
			// Several before filters keep the strictest cutoff.
			if f.CreatedBefore == nil || t.Before(*f.CreatedBefore) {
				f.CreatedBefore = &t
			}
		default:
			return f, fmt.Errorf("image reclaim: unknown filter %q", key)
		}
	}
	return f, nil
}
