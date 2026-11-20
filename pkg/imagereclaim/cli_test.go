// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSnapshot writes a snapshot to a temp file and returns its path.
func writeSnapshot(t *testing.T, snap *Snapshot) string {
	t.Helper()
	data, err := json.Marshal(snap)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "snapshot.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestCLI_DefaultModeOnlyEstimates(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	path := writeSnapshot(t, snap)

	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout}
	require.NoError(t, cli.Run([]string{"--all"}))

	out := stdout.String()
	assert.Contains(t, out, "Reclaimable space: 600 B (600 bytes)")
	assert.NotContains(t, out, "Removal results", "the estimate mode must not execute")
}

func TestCLI_ApplyRequiresRuntime(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, threeImageSnapshot())
	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout}

	err := cli.Run([]string{"--all", "--apply"})
	assert.ErrorIs(t, err, ErrNoRuntimeForApply)
	// The estimate is still printed, so the user sees what would have happened.
	assert.Contains(t, stdout.String(), "Reclaimable space")
}

func TestCLI_JSONIsScriptConsumable(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, sharedLayerSnapshot())
	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout}
	require.NoError(t, cli.Run([]string{"--all", "--format", "json"}))

	var doc PlanDocument
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	assert.Equal(t, int64(1000), doc.ReclaimableBytes)
	assert.Equal(t, int64(2000), doc.NaiveBytes)
	require.Len(t, doc.Images, 3)

	// The JSON fields scripts depend on must keep their names.
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &raw))
	for _, key := range []string{"reclaimableBytes", "naiveBytes", "images", "tagOnly", "protected", "unknownLayers", "orphanLayers"} {
		assert.Contains(t, raw, key)
	}
}

func TestCLI_FiltersChangeTheDeletionSetBeforeEstimation(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
		},
		Images: []Image{
			{ID: "img-old", Names: []string{"example.com/old:1"}, CreatedAt: timeAt(1), Layers: []string{"sha256:a"}},
			{ID: "img-new", Names: []string{"example.com/new:1"}, CreatedAt: timeAt(20), Layers: []string{"sha256:b"}},
		},
	}
	path := writeSnapshot(t, snap)

	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout}
	require.NoError(t, cli.Run([]string{"--all", "--format", "json", "--filter", "before=2026-11-10T00:00:00Z"}))

	var doc PlanDocument
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	require.Len(t, doc.Images, 1)
	assert.Equal(t, "img-old", doc.Images[0].Image.ID)
	assert.Equal(t, int64(100), doc.ReclaimableBytes)
}

func TestCLI_RetainFlagKeepsImages(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, threeImageSnapshot())
	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout}
	require.NoError(t, cli.Run([]string{"--all", "--format", "json", "--retain", "example.com/b:1"}))

	var doc PlanDocument
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	assert.Equal(t, int64(400), doc.ReclaimableBytes)
	require.Len(t, doc.Protected, 1)
	assert.Equal(t, "img-b", doc.Protected[0].Image.ID)
	assert.Equal(t, ReasonRetainConfig, doc.Protected[0].Reasons[0].Code)
}

func TestCLI_ApplyRunsTheEstimateThenTheRemoval(t *testing.T) {
	t.Parallel()

	// The snapshot file is only used for the estimate; the fake runtime owns live state
	// and supplies the pre-removal re-check.
	snap := threeImageSnapshot()
	path := writeSnapshot(t, snap)

	runtime := NewFakeRuntime(snap)
	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout, Runtime: runtime}
	require.NoError(t, cli.Run([]string{"--all", "--apply"}))

	out := stdout.String()
	assert.Contains(t, out, "Reclaimable space: 600 B (600 bytes)")
	assert.Contains(t, out, "Removal results:")
	assert.Contains(t, out, "released     600 B (observed)")
	assert.ElementsMatch(t, []string{"img-a", "img-b", "img-c"}, runtime.Removed())
}

func TestCLI_ApplySkipsAnImageUsedByAContainerStartedAfterTheEstimate(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	path := writeSnapshot(t, snap)
	runtime := NewFakeRuntime(snap)

	// Simulate the gap between estimate and apply: a container starts on img-b.
	live := snap.Clone()
	live.Containers = append(live.Containers, Container{
		ID: "ctr-late", Name: "late", State: StateRunning, Image: "example.com/b:1",
	})
	runtime.snap = live

	var stdout bytes.Buffer
	cli := &CLI{SnapshotPath: path, Stdout: &stdout, Runtime: runtime}
	require.NoError(t, cli.Run([]string{"--all", "--apply"}))

	out := stdout.String()
	assert.Contains(t, out, "skipped        example.com/b:1")
	assert.Contains(t, out, "running-container")
	assert.NotContains(t, runtime.Removed(), "img-b")
	assert.Contains(t, out, "released     400 B (observed)")
}

func TestCLI_RejectsBadArguments(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, threeImageSnapshot())
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown filter key", []string{"--filter", "size=10"}, "unknown filter"},
		{"filter without value", []string{"--filter", "name"}, "must be key=value"},
		{"bad label filter", []string{"--filter", "label=novalue"}, "label=key=value"},
		{"bad time", []string{"--filter", "before=yesterday"}, "before filter"},
		{"unknown format", []string{"--format", "yaml"}, "unknown format"},
		{"stray argument", []string{"extra"}, "unexpected argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := &CLI{SnapshotPath: path, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
			err := cli.Run(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestDecodeSnapshot_RoundTrips(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(threeImageSnapshot())
	require.NoError(t, err)

	snap, err := DecodeSnapshot(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, snap.Images, 3)

	_, err = DecodeSnapshot(strings.NewReader("{"))
	assert.Error(t, err, "malformed JSON must be reported, not partially accepted")
}

func TestCLI_ReadsSnapshotFromStdinAndRereadsTheSameDocument(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(threeImageSnapshot())
	require.NoError(t, err)

	// Replace os.Stdin for the duration of this test: the CLI reads it only once and
	// caches the bytes, so the apply path's re-reads still see the same snapshot.
	old := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	var stdout bytes.Buffer
	cli := &CLI{Stdout: &stdout}
	require.NoError(t, cli.Run([]string{"--all", "--format", "json"}))

	var doc PlanDocument
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	assert.Equal(t, int64(600), doc.ReclaimableBytes)
}

func TestCLI_SnapshotProviderFallsBackToTheFile(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, threeImageSnapshot())
	cli := &CLI{SnapshotPath: path, Runtime: &fileOnlyRuntime{}}
	provider := cli.snapshotProvider(path)
	snap, err := provider()
	require.NoError(t, err)
	assert.Len(t, snap.Images, 3)
}

// fileOnlyRuntime implements Runtime but not the optional Snapshot method, so the CLI
// must fall back to re-reading the snapshot file.
type fileOnlyRuntime struct{}

func (fileOnlyRuntime) RemoveImage(string, []string) (int64, error) { return 0, nil }
func (fileOnlyRuntime) RemoveTag(string) error                      { return nil }
