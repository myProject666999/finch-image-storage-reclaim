// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func size(n int64) *int64 { return &n }

func timeAt(day int) *time.Time {
	t := time.Date(2026, 11, day, 10, 0, 0, 0, time.UTC)
	return &t
}

// sharedLayerSnapshot is the base fixture: three images over four layers, with the
// first two layers shared by all of them.
//
//	layer-a: 100 (shared by every image)
//	layer-b: 200 (shared by every image)
//	layer-c: 300 (only web)
//	layer-d: 400 (only api and worker)
func sharedLayerSnapshot() *Snapshot {
	return &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
			{Digest: "sha256:c", Size: size(300)},
			{Digest: "sha256:d", Size: size(400)},
		},
		Images: []Image{
			{ID: "img-web", Names: []string{"example.com/web:1"}, Layers: []string{"sha256:a", "sha256:b", "sha256:c"}},
			{ID: "img-api", Names: []string{"example.com/api:1"}, Layers: []string{"sha256:a", "sha256:b", "sha256:d"}},
			{ID: "img-worker", Names: []string{"example.com/worker:1"}, Layers: []string{"sha256:a", "sha256:b", "sha256:d"}},
		},
	}
}

func TestComputePlan_SharedLayersAreDeduplicated(t *testing.T) {
	t.Parallel()

	plan, err := ComputePlan(sharedLayerSnapshot(), Options{All: true})
	require.NoError(t, err)

	// Naive: 600+700+700 = 2000. Correct: every layer is freed once = 100+200+300+400.
	assert.Equal(t, int64(1000), plan.Reclaimable)
	assert.Equal(t, int64(2000), plan.NaiveBytes)
	assert.NotEqual(t, plan.NaiveBytes, plan.Reclaimable)
	require.Len(t, plan.Remove, 3)

	var total int64
	byImage := make(map[string]int64)
	for _, removal := range plan.Remove {
		byImage[removal.Image.ID] = removal.ExclusiveBytes
		total += removal.ExclusiveBytes
	}
	assert.Equal(t, plan.Reclaimable, total, "per-image exclusive sizes must sum to the total")

	// Exactly one image is credited with each freed layer, so no layer is counted twice.
	assert.Equal(t, int64(100+200+400), byImage["img-api"], "api is first by name and takes the shared layers")
	assert.Equal(t, int64(300), byImage["img-web"], "only web has layer-c")
	assert.Zero(t, byImage["img-worker"], "worker shares every layer with api")
}

func TestComputePlan_SharedLayerWithProtectedImageIsNotReclaimable(t *testing.T) {
	t.Parallel()

	snap := sharedLayerSnapshot()
	// The worker image is in use, so its layers survive, including the shared layer-d.
	snap.Containers = []Container{{ID: "ctr-1", Name: "worker-1", State: StateRunning, Image: "example.com/worker:1"}}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	// Only layer-c is exclusive to a removed image. layer-d is shared with the
	// protected worker, and layer-a/b are shared with everything.
	assert.Equal(t, int64(300), plan.Reclaimable)
	require.Len(t, plan.Remove, 2, "img-api is listed even though it releases nothing on its own")
	byID := make(map[string]ImageRemoval)
	for _, removal := range plan.Remove {
		byID[removal.Image.ID] = removal
	}
	assert.Equal(t, []string{"sha256:c"}, byID["img-web"].Layers)
	assert.Equal(t, int64(300), byID["img-web"].ExclusiveBytes)
	assert.Zero(t, byID["img-api"].ExclusiveBytes)
	assert.Empty(t, byID["img-api"].Layers)

	require.Len(t, plan.Protected, 1)
	assert.Equal(t, "img-worker", plan.Protected[0].Image.ID)
}

func TestComputePlan_TagOnlyRemovalFreesNothing(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{
			{
				ID:     "img-shared",
				Names:  []string{"example.com/app:1", "example.com/app:latest"},
				Layers: []string{"sha256:a"},
			},
		},
	}

	plan, err := ComputePlan(snap, Options{
		Filters: Filters{Names: []string{"example.com/app:1"}},
	})
	require.NoError(t, err)

	assert.Zero(t, plan.Reclaimable)
	assert.Empty(t, plan.Remove, "the image keeps app:latest, so it must not be deleted")
	require.Len(t, plan.TagOnly, 1)
	assert.Equal(t, []string{"example.com/app:1"}, plan.TagOnly[0].Tags)

	// Removing both tags does delete the image and frees its layer.
	both, err := ComputePlan(snap, Options{
		Filters: Filters{Names: []string{"example.com/app:*"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(100), both.Reclaimable)
	assert.Empty(t, both.TagOnly)
	require.Len(t, both.Remove, 1)
}

func TestComputePlan_ContainerReferencesProtectImages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container Container
		option    Options
		protected bool
		reason    ReasonCode
	}{
		{
			name:      "running container by tag",
			container: Container{ID: "c1", Name: "by-tag", State: StateRunning, Image: "example.com/web:1"},
			reason:    ReasonRunningContainer,
			protected: true,
		},
		{
			name:      "running container by digest",
			container: Container{ID: "c2", Name: "by-digest", State: StateRunning, ImageDigests: []string{"sha256:web-digest"}},
			reason:    ReasonRunningContainer,
			protected: true,
		},
		{
			name:      "running container by image ID",
			container: Container{ID: "c3", Name: "by-id", State: StateRunning, ImageID: "img-web"},
			reason:    ReasonRunningContainer,
			protected: true,
		},
		{
			name:      "stopped container is treated as in use",
			container: Container{ID: "c4", Name: "stopped", State: StateExited, Image: "example.com/web:1"},
			reason:    ReasonStoppedContainer,
			protected: true,
		},
		{
			name:      "stopped container ignored when explicitly pruning stopped containers",
			container: Container{ID: "c4", Name: "stopped", State: StateExited, Image: "example.com/web:1"},
			option:    Options{All: true, PruneStoppedContainers: true},
			protected: false,
		},
		{
			name:      "unknown state is treated as running",
			container: Container{ID: "c5", Name: "mystery", State: ContainerState("zombie"), Image: "example.com/web:1"},
			reason:    ReasonRunningContainer,
			protected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snap := &Snapshot{
				Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
				Images: []Image{{
					ID:          "img-web",
					Names:       []string{"example.com/web:1"},
					Digest:      "sha256:web-digest",
					RepoDigests: []string{"example.com/web@sha256:web-digest"},
					Layers:      []string{"sha256:a"},
				}},
				Containers: []Container{tt.container},
			}

			opts := tt.option
			opts.All = true
			plan, err := ComputePlan(snap, opts)
			require.NoError(t, err)

			if !tt.protected {
				assert.Empty(t, plan.Protected)
				assert.Equal(t, int64(100), plan.Reclaimable)
				return
			}
			require.Len(t, plan.Protected, 1)
			assert.Zero(t, plan.Reclaimable)
			assert.Empty(t, plan.Remove)
			require.Len(t, plan.Protected[0].Reasons, 1)
			assert.Equal(t, tt.reason, plan.Protected[0].Reasons[0].Code)
		})
	}
}

func TestComputePlan_AllReasonsAreReported(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{{
			ID:     "img-web",
			Names:  []string{"example.com/web:1"},
			Labels: map[string]string{DefaultRetainLabel: "true"},
			Layers: []string{"sha256:a"},
		}},
		Containers: []Container{{ID: "c1", Name: "web-1", State: StateRunning, Image: "example.com/web:1"}},
	}

	plan, err := ComputePlan(snap, Options{All: true, RetainedNames: []string{"example.com/web:1"}})
	require.NoError(t, err)

	require.Len(t, plan.Protected, 1)
	codes := make([]ReasonCode, 0, len(plan.Protected[0].Reasons))
	for _, r := range plan.Protected[0].Reasons {
		codes = append(codes, r.Code)
	}
	assert.Equal(t, []ReasonCode{ReasonRunningContainer, ReasonRetainLabel, ReasonRetainConfig}, codes,
		"every matching reason is listed, in the fixed order")

	// Container protection and explicit retention are reported in separate categories.
	categories := make(map[string]bool)
	for _, r := range plan.Protected[0].Reasons {
		categories[r.Category()] = true
	}
	assert.True(t, categories["container-in-use"])
	assert.True(t, categories["explicit-retention"])
}

func TestComputePlan_RetainRulesProtectUnusedImages(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{
			{
				ID:     "img-label",
				Names:  []string{"example.com/labelled:1"},
				Labels: map[string]string{DefaultRetainLabel: "keep"},
				Layers: []string{"sha256:a"},
			},
			{ID: "img-config", Names: []string{"example.com/pinned:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-free", Names: []string{"example.com/free:1"}, Layers: []string{"sha256:a"}},
		},
	}

	plan, err := ComputePlan(snap, Options{All: true, RetainedNames: []string{"example.com/pinned:1"}})
	require.NoError(t, err)

	require.Len(t, plan.Remove, 1)
	assert.Equal(t, "img-free", plan.Remove[0].Image.ID)

	protected := make(map[string][]ReasonCode)
	for _, p := range plan.Protected {
		for _, r := range p.Reasons {
			protected[p.Image.ID] = append(protected[p.Image.ID], r.Code)
		}
	}
	assert.Equal(t, []ReasonCode{ReasonRetainLabel}, protected["img-label"])
	assert.Equal(t, []ReasonCode{ReasonRetainConfig}, protected["img-config"])
}

func TestComputePlan_InProgressOperationProtectsImage(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{{ID: "img-web", Names: []string{"example.com/web:1"}, Layers: []string{"sha256:a"}}},
		Operations: []Operation{
			{Kind: "build", Ref: "example.com/web:2", ImageID: "img-web"},
		},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	require.Len(t, plan.Protected, 1)
	assert.Equal(t, ReasonInProgressOperation, plan.Protected[0].Reasons[0].Code)
	assert.Empty(t, plan.Remove)
}

func TestComputePlan_DanglingImagesAreRemovedByDefault(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(50)},
		},
		Images: []Image{
			{ID: "img-tagged", Names: []string{"example.com/web:1"}, Layers: []string{"sha256:a"}},
			{ID: "img-dangling", Layers: []string{"sha256:b"}},
		},
	}

	plan, err := ComputePlan(snap, Options{})
	require.NoError(t, err)

	require.Len(t, plan.Remove, 1)
	assert.Equal(t, "img-dangling", plan.Remove[0].Image.ID)
	assert.Equal(t, int64(50), plan.Reclaimable)
	assert.Empty(t, plan.TagOnly)
}

func TestComputePlan_UnknownLayerSizeIsExcludedAndReported(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:known", Size: size(100)},
			{Digest: "sha256:unknown"}, // size metadata missing
		},
		Images: []Image{{
			ID:     "img-web",
			Names:  []string{"example.com/web:1"},
			Layers: []string{"sha256:known", "sha256:unknown"},
		}},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	assert.Equal(t, int64(100), plan.Reclaimable, "the unknown layer contributes nothing")
	assert.Equal(t, []string{"sha256:unknown"}, plan.UnknownLayers)
	require.Len(t, plan.Remove, 1)
	assert.Equal(t, []string{"sha256:unknown"}, plan.Remove[0].UnknownLayers)
	assert.Equal(t, int64(100), plan.Remove[0].ExclusiveBytes)
}

func TestComputePlan_IntermediateImagesAreProtectedByDefault(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
		},
		Images: []Image{
			{ID: "img-base", Layers: []string{"sha256:a"}},
			{ID: "img-child", Names: []string{"example.com/app:1"}, ParentID: "img-base", Layers: []string{"sha256:a", "sha256:b"}},
			{ID: "img-intermediate", Intermediate: true, Layers: []string{"sha256:a"}},
		},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	protectedIDs := make(map[string]bool)
	for _, p := range plan.Protected {
		protectedIDs[p.Image.ID] = true
	}
	assert.True(t, protectedIDs["img-base"], "an untagged parent is build state, not garbage")
	assert.True(t, protectedIDs["img-intermediate"])

	// Only the tagged leaf is removable; its only non-shared layer is layer-b.
	require.Len(t, plan.Remove, 1)
	assert.Equal(t, "img-child", plan.Remove[0].Image.ID)
	assert.Equal(t, int64(200), plan.Reclaimable)

	// With PruneIntermediate, the base becomes removable too.
	aggressive, err := ComputePlan(snap, Options{All: true, PruneIntermediate: true})
	require.NoError(t, err)
	var ids []string
	for _, removal := range aggressive.Remove {
		ids = append(ids, removal.Image.ID)
	}
	assert.ElementsMatch(t, []string{"img-base", "img-child", "img-intermediate"}, ids)
	assert.Equal(t, int64(300), aggressive.Reclaimable)
}

func TestComputePlan_FiltersApplyBeforeEstimation(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:b", Size: size(200)},
			{Digest: "sha256:c", Size: size(300)},
		},
		Images: []Image{
			{ID: "img-old", Names: []string{"example.com/old:1"}, CreatedAt: timeAt(1), Layers: []string{"sha256:a"}},
			{ID: "img-new", Names: []string{"example.com/new:1"}, CreatedAt: timeAt(20), Layers: []string{"sha256:a", "sha256:b"}},
			{ID: "img-other", Names: []string{"other.io/thing:1"}, CreatedAt: timeAt(2), Layers: []string{"sha256:c"}},
		},
	}

	cutoff := *timeAt(10)
	plan, err := ComputePlan(snap, Options{
		All:     true,
		Filters: Filters{CreatedBefore: &cutoff},
	})
	require.NoError(t, err)

	ids := make([]string, 0, len(plan.Remove))
	for _, removal := range plan.Remove {
		ids = append(ids, removal.Image.ID)
	}
	assert.ElementsMatch(t, []string{"img-old", "img-other"}, ids, "img-new is too recent")
	// img-old's only layer is shared with the surviving img-new, so nothing is freed by it.
	assert.Equal(t, int64(300), plan.Reclaimable)

	// Name filters select tags, and are applied before sizes are estimated.
	named, err := ComputePlan(snap, Options{All: true, Filters: Filters{Names: []string{"example.com/*"}}})
	require.NoError(t, err)
	ids = ids[:0]
	for _, removal := range named.Remove {
		ids = append(ids, removal.Image.ID)
	}
	assert.ElementsMatch(t, []string{"img-old", "img-new"}, ids)
	// img-other survives, so layer-c is not reclaimed. Both remaining images share
	// layer-a, which is freed once, plus layer-b.
	assert.Equal(t, int64(300), named.Reclaimable)
}

func TestComputePlan_OrphanLayersAreReportedButNotReclaimable(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:a", Size: size(100)},
			{Digest: "sha256:orphan", Size: size(999)},
			{Digest: "sha256:pulling", Size: size(888)},
		},
		Images: []Image{{ID: "img-web", Names: []string{"example.com/web:1"}, Layers: []string{"sha256:a"}}},
		Operations: []Operation{
			{Kind: "pull", Ref: "example.com/new:1", Layers: []string{"sha256:pulling"}},
		},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	assert.Equal(t, int64(100), plan.Reclaimable, "orphan bytes are never counted")
	require.Len(t, plan.OrphanLayers, 2)
	byDigest := make(map[string]OrphanLayer)
	for _, o := range plan.OrphanLayers {
		byDigest[o.Digest] = o
	}
	assert.False(t, byDigest["sha256:orphan"].InProgress)
	assert.True(t, byDigest["sha256:pulling"].InProgress)
	assert.Equal(t, "pull", byDigest["sha256:pulling"].OperationKind)
}

func TestComputePlan_RejectsInvalidSnapshots(t *testing.T) {
	t.Parallel()

	_, err := ComputePlan(nil, Options{})
	assert.ErrorIs(t, err, ErrNilSnapshot)

	_, err = ComputePlan(&Snapshot{Images: []Image{{Names: []string{"a:1"}}}}, Options{})
	assert.Error(t, err, "an image without an ID is ambiguous")

	_, err = ComputePlan(&Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(1)}, {Digest: "sha256:a", Size: size(2)}},
	}, Options{})
	assert.Error(t, err, "conflicting layer sizes must not be silently resolved")
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0 B", HumanBytes(0))
	assert.Equal(t, "1 B", HumanBytes(1))
	assert.Equal(t, "1.00 KiB", HumanBytes(1024))
	assert.Equal(t, "1.50 KiB", HumanBytes(1536))
	assert.Equal(t, "1.00 GiB", HumanBytes(1024*1024*1024))
	assert.Equal(t, "2.00 TiB", HumanBytes(2*1024*1024*1024*1024))
}

func TestWritePlanText_RendersProtectionsTagOnlyAndDangling(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{
			{
				ID:     "img-app",
				Names:  []string{"example.com/app:1", "example.com/app:latest"},
				Layers: []string{"sha256:a"},
			},
			{
				ID:     "img-used",
				Names:  []string{"example.com/used:1"},
				Labels: map[string]string{DefaultRetainLabel: "keep"},
				Layers: []string{"sha256:a"},
			},
			{ID: "img-dangling", Layers: []string{"sha256:a"}},
		},
		Containers: []Container{{ID: "c1", Name: "app-1", State: StateRunning, Image: "example.com/used:1"}},
	}

	plan, err := ComputePlan(snap, Options{
		All:           true,
		RetainedNames: []string{"example.com/used:1"},
		Filters:       Filters{Names: []string{"example.com/app:1", "example.com/used:1"}},
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, WritePlanText(&buf, plan))
	out := buf.String()

	assert.Contains(t, out, "Tags to remove without deleting their image")
	assert.Contains(t, out, "example.com/app:1 from image")
	assert.Contains(t, out, "Protected images (kept):")
	assert.Contains(t, out, "container-in-use:")
	assert.Contains(t, out, "explicit-retention:")
	assert.Contains(t, out, "retain-label")
	assert.Contains(t, out, "retain-config")
	assert.Zero(t, plan.Reclaimable, "the only candidate is a tag-only removal")
}

func TestComputePlan_DigestReferencesResolveInAnyForm(t *testing.T) {
	t.Parallel()

	// The image records itself with a repo digest; the container records only the raw
	// digest. Both name the same image, so the image must be protected.
	snap := &Snapshot{
		Layers: []Layer{{Digest: "sha256:a", Size: size(100)}},
		Images: []Image{{
			ID:          "img-web",
			Names:       []string{"example.com/web:1"},
			Digest:      "sha256:manifest",
			RepoDigests: []string{"example.com/web@sha256:manifest"},
			Layers:      []string{"sha256:a"},
		}},
		Containers: []Container{{
			ID: "c1", Name: "pinned", State: StateRunning, ImageDigests: []string{"sha256:manifest"},
		}},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)
	require.Len(t, plan.Protected, 1)
	assert.Equal(t, ReasonRunningContainer, plan.Protected[0].Reasons[0].Code)
}

func TestComputePlan_RejectsADuplicateTag(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Images: []Image{
			{ID: "img-1", Names: []string{"example.com/app:1"}},
			{ID: "img-2", Names: []string{"example.com/app:1"}},
		},
	}
	_, err := ComputePlan(snap, Options{All: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refers to two images",
		"an ambiguous tag must not silently resolve to whichever image was indexed last")
}

func TestComputePlan_OperationLayersProtectEveryImageUsingThem(t *testing.T) {
	t.Parallel()

	snap := &Snapshot{
		Layers: []Layer{
			{Digest: "sha256:shared", Size: size(100)},
			{Digest: "sha256:only-that", Size: size(50)},
		},
		Images: []Image{
			{ID: "img-1", Names: []string{"example.com/one:1"}, Layers: []string{"sha256:shared"}},
			{ID: "img-2", Names: []string{"example.com/two:1"}, Layers: []string{"sha256:shared", "sha256:only-that"}},
		},
		Operations: []Operation{{Kind: "pull", Layers: []string{"sha256:shared"}}},
	}

	plan, err := ComputePlan(snap, Options{All: true})
	require.NoError(t, err)

	assert.Empty(t, plan.Remove, "both images share the layer being pulled")
	assert.Len(t, plan.Protected, 2)
}
