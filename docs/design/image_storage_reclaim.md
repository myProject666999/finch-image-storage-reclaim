# Image storage reclaim

This document explains how `pkg/imagereclaim` estimates and performs image cleanup,
and why the estimate is not the sum of the image sizes.

## The estimate is layer-based

Images share layers. Deleting one image releases only the layers that no surviving
image references any more, so summing image sizes overstates the reclaimable space.

The estimate therefore works in three steps:

1. **Select** the candidate images or tags, after applying filters
   (`--filter name=`, `--filter label=`, `--filter before=`) and the scope
   (`--all`, otherwise only dangling images).
2. **Protect**: remove from the candidate set every image that a rule protects.
3. **Deduplicate**: compute the set of layers referenced by the removed images but not
   by any surviving image. Sum those layer sizes.

`naiveBytes` is reported alongside the estimate only as a diagnostic. It is the
un-deduplicated sum and is never presented as reclaimable space.

Each freed layer is attributed to exactly one image, so the per-image `exclusiveBytes`
values add up to the total. An image that shares all of its layers with a surviving or
co-removed image legitimately shows `0 B`.

### Tags

An image may have several tags. Removing one tag does not delete the image and frees
no space; only removing the last tag does. The plan reports these as `tagOnly` entries,
separate from `images`, and the estimate for them is zero.

`--all` with no name filter removes every unused image completely. A name filter
selects tags, so `--filter name='app:1'` on an image that also has `app:latest` is a
tag-only removal.

### Dangling and intermediate images

- Dangling images (no tags) are candidates in both modes.
- Intermediate build images, and untagged parents of other images, are protected by
  default (`build-intermediate`). `--prune-intermediate` makes them candidates.
- A parent is also protected whenever a child image is protected, because removing it
  would invalidate the child's build-cache chain.

### Unknown sizes

A layer whose size is missing from the snapshot metadata is reported as size-unknown
and contributes nothing to the estimate. The plan lists those digests in
`unknownLayers`, and the total states how many layers were excluded. Padding the
number with zero or a guessed value would make the estimate misleading in the other
direction, so neither is done.

## Protections

Every matching reason is reported, not just the first one, because the remedies differ.
Reasons carry a category so the two kinds of protection stay visibly separate:

| Code | Category | Meaning |
| --- | --- | --- |
| `running-container` | container-in-use | A running, paused, created, or restarting container uses the image. Unknown states are treated as running. |
| `stopped-container` | container-in-use | Only exited/stopped containers use it. Still treated as in use unless `--prune-stopped` is given. |
| `in-progress-operation` | in-progress | A pull, build, or load in flight references the image or its layers. |
| `retain-label` | explicit-retention | The image carries `io.finch.image.retain`. |
| `retain-config` | explicit-retention | The image name is listed with `--retain`. |
| `build-intermediate` | build-cache | Intermediate image or untagged parent of another image. |
| `tag-changed` | changed-since-plan | Only at execution: the image gained a tag after the estimate. |

Container references are resolved through tags, image IDs, and digests, so a container
recorded by digest still protects its image.

## The estimate and the execution are separate

The estimate never mutates anything. Execution is opt-in (`--apply`) and always:

1. runs the estimate first and prints it;
2. re-reads a fresh snapshot and re-evaluates **all** protections — an image that a
   container started using in the meantime is skipped with the reason shown;
3. removes what is still safe, one image at a time. A failure on one image is recorded
   and does not stop the others;
4. reports what was removed, what was skipped and why, what failed, and what had
   already disappeared (deleted in another terminal). An already-absent image counts
   as done, not as a failure.

Removal results are bucketed so the arithmetic is checkable:

```
plannedBytes = removedBytes + alreadyGoneBytes + skippedBytes + failedBytes
```

`releasedBytes` is the observed side and is compared with `removedBytes`. When the two
differ by more than 10%, `deltaWarning` explains the gap.

## Orphan layers

Layers in the content store that no image references are reported but never counted as
reclaimable and never removed. A pull writing its layers before the image record exists
looks exactly like an orphan, so an unreferenced layer is more likely an operation in
progress than garbage. When the layer belongs to a known in-flight operation the report
says so.

## Output for scripts

`--format json` emits a stable document:

```json
{
  "reclaimableBytes": 1000,
  "naiveBytes": 2000,
  "images": [{"image": {"id": "..."}, "tags": ["..."], "exclusiveBytes": 600}],
  "tagOnly": [],
  "protected": [{"image": {"id": "..."}, "reasons": [{"code": "running-container", "detail": "web-1"}]}],
  "unknownLayers": [],
  "orphanLayers": []
}
```

Field names are part of the interface. `reclaimableBytes` is the number to trust;
`naiveBytes` exists to show why a naive sum would have been wrong.
