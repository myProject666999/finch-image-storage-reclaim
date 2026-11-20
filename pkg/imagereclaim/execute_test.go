// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threeImageSnapshot has three independent images, one layer each, so that estimates
// and observed releases are easy to reason about.
func threeImageSnapshot() *Snapshot {
	return &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
			{Digest: "sha256:c", Size: size(300)},
		},
		Images: []Image{
			{ID: "img-a", Names: []string{"example.com/a:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-b", Names: []string{"example.com/b:1"}, Layers: []string{"sha256:b"}},
			{ID: "img-c", Names: []string{"example.com/c:1"}, Layers: []string{"sha256:c"}},
		},
	}
}

func TestExecute_RemovesEverythingThePlanListed(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)
	require.Equal(t, int64(600), plan.Reclaimable)

	runtime := NewFakeRuntime(snap)
	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 3, report.Counts[OutcomeRemoved])
	assert.Equal(t, int64(600), report.PlannedBytes)
	assert.Equal(t, int64(600), report.RemovedBytes)
	assert.Equal(t, int64(600), report.ReleasedBytes)
	assert.Equal(t, report.PlannedBytes, report.EstimatedTotal())
	assert.Empty(t, report.DeltaWarning)
	assert.ElementsMatch(t, []string{"img-a", "img-b", "img-c"}, runtime.Removed())
}

func TestExecute_RechecksProtectionAfterPlanning(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)
	require.Equal(t, int64(600), plan.Reclaimable)

	// Between planning and execution, a container starts using img-b.
	runtime := NewFakeRuntime(snap)
	live := snap.Clone()
	live.Containers = append(live.Containers, Container{
		ID: "ctr-new", Name: "started-later", State: StateRunning, Image: "example.com/b:1",
	})
	runtime.snap = live

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 2, report.Counts[OutcomeRemoved])
	assert.Equal(t, 1, report.Counts[OutcomeSkipped])
	assert.Equal(t, int64(200), report.SkippedBytes)
	assert.Equal(t, int64(400), report.RemovedBytes, "the skipped image is excluded from the comparison")
	assert.Equal(t, int64(400), report.ReleasedBytes)
	assert.Equal(t, report.PlannedBytes, report.EstimatedTotal())
	assert.NotContains(t, runtime.Removed(), "img-b")

	var skipped *StepResult
	for i := range report.Steps {
		if report.Steps[i].Outcome == OutcomeSkipped {
			skipped = &report.Steps[i]
		}
	}
	require.NotNil(t, skipped)
	assert.Equal(t, "img-b", skipped.Image.ID)
	require.Len(t, skipped.Reasons, 1)
	assert.Equal(t, ReasonRunningContainer, skipped.Reasons[0].Code)
	assert.Equal(t, "started-later", skipped.Reasons[0].Detail)
}

func TestExecute_RechecksDigestReferenceAddedAfterPlanning(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	snap.Images[1].Digest = "sha256:digest-b"
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	runtime := NewFakeRuntime(snap)
	live := snap.Clone()
	live.Containers = append(live.Containers, Container{
		ID: "ctr-digest", Name: "by-digest", State: StatePaused, ImageDigests: []string{"sha256:digest-b"},
	})
	runtime.snap = live

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Counts[OutcomeSkipped])
	assert.NotContains(t, runtime.Removed(), "img-b")
}

func TestExecute_FailureOnOneImageDoesNotStopOthers(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	runtime := NewFakeRuntime(snap)
	runtime.FailWith("img-a", ErrFakeInjected)

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 2, report.Counts[OutcomeRemoved])
	assert.Equal(t, 1, report.Counts[OutcomeFailed])
	assert.ElementsMatch(t, []string{"img-a", "img-b", "img-c"}, runtime.Removed(),
		"the failure is recorded but the other images are still attempted")

	var failed StepResult
	for _, step := range report.Steps {
		if step.Outcome == OutcomeFailed {
			failed = step
		}
	}
	assert.Equal(t, "img-a", failed.Image.ID)
	assert.Contains(t, failed.Error, "injected removal failure")

	// The totals stay internally consistent: only images that were actually removed
	// enter the estimate-versus-observation comparison, so a failure does not produce
	// a spurious delta warning.
	assert.Equal(t, int64(500), report.ReleasedBytes)
	assert.Equal(t, int64(500), report.RemovedBytes)
	assert.Equal(t, int64(100), report.FailedBytes)
	assert.Equal(t, report.PlannedBytes, report.EstimatedTotal(),
		"removed + already-gone + skipped + failed adds up to the plan")
	assert.Empty(t, report.DeltaWarning)
}

func TestExecute_ImageAlreadyGoneCountsAsDone(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	runtime := NewFakeRuntime(snap)
	// The user removes img-c in another terminal after planning.
	live := snap.Clone()
	live.Images = live.Images[:2]
	runtime.snap = live

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Counts[OutcomeAlreadyGone])
	assert.Equal(t, 2, report.Counts[OutcomeRemoved])
	// It was not a failure, so it is not reported as one.
	assert.Zero(t, report.Counts[OutcomeFailed])

	var gone StepResult
	for _, step := range report.Steps {
		if step.Outcome == OutcomeAlreadyGone {
			gone = step
		}
	}
	assert.Equal(t, "img-c", gone.Image.ID)
	assert.Nil(t, gone.Released, "nothing was released by this run")
}

func TestExecute_DeltaWarningWhenObservationDiffersFromEstimate(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	// A runtime that under-reports: layers it cannot attribute are not counted.
	runtime := &underreportingRuntime{FakeRuntime: NewFakeRuntime(snap)}
	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, int64(600), report.RemovedBytes)
	assert.Equal(t, int64(0), report.ReleasedBytes)
	assert.NotEmpty(t, report.DeltaWarning)
	assert.Contains(t, report.DeltaWarning, "600")
}

func TestExecute_RequiresRuntimeAndSnapshot(t *testing.T) {
	t.Parallel()

	_, err := (&Executor{}).Execute(&Plan{})
	assert.ErrorIs(t, err, ErrNilRuntime)

	_, err = (&Executor{Runtime: NewFakeRuntime(threeImageSnapshot())}).Execute(&Plan{})
	assert.ErrorIs(t, err, ErrNilSnapshotFunc)
}

func TestExecute_TagOnlyRemovalKeepsImage(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{{
			ID:     "img-app",
			Names:  []string{"example.com/app:1", "example.com/app:latest"},
			Layers: []string{"sha256:a"},
		}},
	}
	plan, err := ComputePlan(snap, Options{All: true, Filters: Filters{Names: []string{"example.com/app:1"}}})
	require.NoError(t, err)
	require.Len(t, plan.TagOnly, 1)

	runtime := NewFakeRuntime(snap)
	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Counts[OutcomeTagRemoved])
	assert.Zero(t, report.ReleasedBytes)
	assert.Empty(t, runtime.Removed(), "no image deletion was attempted")

	current, err := runtime.Snapshot()
	require.NoError(t, err)
	require.Len(t, current.Images, 1)
	assert.Equal(t, []string{"example.com/app:latest"}, current.Images[0].Names)
}

// underreportingRuntime reports zero released bytes while still deleting images.
type underreportingRuntime struct {
	*FakeRuntime
}

func (u *underreportingRuntime) RemoveImage(id string, tags []string) (int64, error) {
	if _, err := u.FakeRuntime.RemoveImage(id, tags); err != nil {
		return 0, err
	}
	return 0, nil
}

func TestReportJSONIsStable(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)
	runtime := NewFakeRuntime(snap)
	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, WriteReportJSON(&buf, report))

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	// Field names are part of the script-facing contract.
	for _, key := range []string{
		"steps", "plannedBytes", "removedBytes", "alreadyGoneBytes",
		"skippedBytes", "failedBytes", "releasedBytes", "counts",
	} {
		assert.Contains(t, decoded, key)
	}
}

func TestWritePlanText_ReportsSharedLayerDifferenceAndUnknowns(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(100)},
			{Digest: "sha256:missing"},
			{Digest: "sha256:orphan", Size: size(7)},
		},
		Images: []Image{
			{ID: "img-1", Names: []string{"example.com/one:1"}, Layers: []string{"sha256:a", "sha256:b", "sha256:missing"}},
			{ID: "img-2", Names: []string{"example.com/two:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-d", Layers: []string{"sha256:b"}},
		},
	}
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, WritePlanText(&buf, plan))
	out := buf.String()

	assert.Contains(t, out, "Reclaimable space: 200 B (200 bytes)")
	assert.Contains(t, out, "Naive sum of image sizes")
	assert.Contains(t, out, "Size unknown for layer sha256:missing")
	assert.Contains(t, out, "Unreferenced layers")
	assert.Contains(t, out, "<dangling>", "the dangling image is listed explicitly")
}

func TestWritePlanJSON_ListsDanglingImageSeparately(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
		},
		Images: []Image{
			{ID: "img-tagged", Names: []string{"example.com/app:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-dangling", Layers: []string{"sha256:b"}},
		},
	}
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, WritePlanJSON(&buf, plan))

	var doc PlanDocument
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	assert.Equal(t, int64(300), doc.ReclaimableBytes)
	require.Len(t, doc.Images, 2)

	var dangling *ImageRemoval
	for i := range doc.Images {
		if doc.Images[i].Image.ID == "img-dangling" {
			dangling = &doc.Images[i]
		}
	}
	require.NotNil(t, dangling)
	assert.Empty(t, dangling.Tags)
	assert.Empty(t, dangling.Image.Names)
	assert.Equal(t, int64(200), dangling.ExclusiveBytes)
}

func TestWriteReportText_SeparatesProtectionCategories(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
			{Digest: "sha256:c", Size: size(300)},
			{Digest: "sha256:d", Size: size(400)},
		},
		Images: []Image{
			{ID: "img-used", Names: []string{"example.com/used:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-pinned", Names: []string{"example.com/pinned:1"}, Layers: []string{"sha256:b"}},
			{ID: "img-removable", Names: []string{"example.com/removable:1"}, Layers: []string{"sha256:c"}},
			{ID: "img-gone", Names: []string{"example.com/gone:1"}, Layers: []string{"sha256:d"}},
		},
		Containers: []Container{{ID: "c1", Name: "web-1", State: StateRunning, Image: "example.com/used:1"}},
	}
	opts := Options{All: true, RetainedNames: []string{"example.com/pinned:1"}}
	plan, err := ComputePlan(snap, opts)
	require.NoError(t, err)
	require.Len(t, plan.Protected, 2)
	require.Equal(t, int64(700), plan.Reclaimable)

	// Container-based protection and explicit retention are distinct categories.
	categories := make(map[string]string)
	for _, p := range plan.Protected {
		for _, r := range p.Reasons {
			categories[p.Image.ID] = r.Category()
		}
	}
	assert.Equal(t, "container-in-use", categories["img-used"])
	assert.Equal(t, "explicit-retention", categories["img-pinned"])

	runtime := NewFakeRuntime(snap)
	live := snap.Clone()
	// img-gone is deleted in another terminal between planning and execution.
	filtered := live.Images[:0]
	for _, img := range live.Images {
		if img.ID != "img-gone" {
			filtered = append(filtered, img)
		}
	}
	live.Images = filtered
	runtime.snap = live

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Counts[OutcomeRemoved])
	assert.Equal(t, 1, report.Counts[OutcomeAlreadyGone])
	assert.Equal(t, int64(300), report.ReleasedBytes)
	assert.Equal(t, int64(700), report.PlannedBytes)
	assert.Equal(t, report.PlannedBytes, report.EstimatedTotal())

	var buf bytes.Buffer
	require.NoError(t, WriteReportText(&buf, report))
	out := buf.String()

	assert.Contains(t, out, "removed        example.com/removable:1")
	assert.Contains(t, out, "already gone   example.com/gone:1")
	assert.Contains(t, out, "planned      700 B")
	assert.Contains(t, out, "released     300 B (observed)")
}

func TestExecute_RechecksBeforeEachRemovalNotOnlyOnceUpFront(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	runtime := NewFakeRuntime(snap)
	// The hook fires inside the runtime on the first removal attempt. A container
	// appears at that moment, i.e. after any single up-front check would already have
	// run, so only a per-image re-check can catch it.
	runtime.Hook = func(id string) {
		if id != "img-a" {
			return
		}
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		runtime.snap.Containers = append(runtime.snap.Containers, Container{
			ID: "ctr-race", Name: "race", State: StateRunning, Image: "example.com/c:1",
		})
	}

	report, err := (&Executor{Snapshot: runtime.Snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 2, report.Counts[OutcomeRemoved])
	assert.Equal(t, 1, report.Counts[OutcomeSkipped])
	assert.NotContains(t, runtime.Removed(), "img-c", "the image acquired by a container mid-run is skipped")
	assert.Equal(t, report.PlannedBytes, report.EstimatedTotal())
}

func TestExecute_UnreadableSnapshotSkipsRatherThanDeletes(t *testing.T) {
	t.Parallel()

	snap := threeImageSnapshot()
	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	runtime := NewFakeRuntime(snap)
	calls := 0
	snapshot := func() (*Snapshot, error) {
		calls++
		if calls == 2 {
			return nil, assert.AnError
		}
		return runtime.Snapshot()
	}

	report, err := (&Executor{Snapshot: snapshot, Runtime: runtime}).Execute(plan)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Counts[OutcomeSkipped])
	assert.Equal(t, 2, report.Counts[OutcomeRemoved])
	for i := range report.Steps {
		if report.Steps[i].Outcome == OutcomeSkipped {
			require.Len(t, report.Steps[i].Reasons, 1)
			assert.Equal(t, ReasonSnapshotUnavailable, report.Steps[i].Reasons[0].Code)
		}
	}
}
