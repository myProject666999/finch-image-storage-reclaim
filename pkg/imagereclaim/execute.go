// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Runtime is the minimal interface the executor needs. The real runtime removes an
// image and reports how many bytes the content store released; a fake runtime in
// tests implements the same interface.
type Runtime interface {
	// RemoveImage removes every tag of the given image ID. It returns the number of
	// bytes the backing store actually released, which may differ from the estimate
	// when layers are still pinned by content the snapshot did not list.
	RemoveImage(id string, tags []string) (freedBytes int64, err error)
	// RemoveTag removes a single tag without touching the image.
	RemoveTag(tag string) error
}

// Outcome classifies what happened to one planned image.
type Outcome string

// Outcomes reported per image.
const (
	// OutcomeRemoved means the image no longer exists after execution.
	OutcomeRemoved Outcome = "removed"
	// OutcomeSkipped means the re-check found a protection (or a changed reference
	// set) that was absent at planning time.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeFailed means the runtime refused the removal. Other images are still
	// processed.
	OutcomeFailed Outcome = "failed"
	// OutcomeTagRemoved means only a tag was removed; the image and its layers stay.
	OutcomeTagRemoved Outcome = "tag-removed"
	// OutcomeAlreadyGone means the image disappeared between planning and execution,
	// typically because the user removed it in another terminal. It counts as done,
	// not as a failure.
	OutcomeAlreadyGone Outcome = "already-gone"
)

// StepResult is the per-image result of execution.
type StepResult struct {
	Image      Image     `json:"image"`
	Tags       []string  `json:"tags,omitempty"`
	Outcome    Outcome   `json:"outcome"`
	Reasons    []Reason  `json:"reasons,omitempty"`
	Planned    int64     `json:"plannedBytes"`
	Released   *int64    `json:"releasedBytes,omitempty"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
}

// Report is the execution half of the workflow.
//
// The estimate is split into four buckets that always add up to PlannedBytes, so a
// reader can see exactly where the estimate went:
//
//	PlannedBytes = RemovedBytes + AlreadyGoneBytes + SkippedBytes + FailedBytes
//
// ReleasedBytes is the observed side and is compared only with RemovedBytes: images
// that were already gone released their space outside this run, and failed removals
// released nothing observable here.
type Report struct {
	Steps []StepResult `json:"steps"`
	// PlannedBytes is the plan's reclaimable estimate.
	PlannedBytes int64 `json:"plannedBytes"`
	// RemovedBytes is the estimate for images this run removed.
	RemovedBytes int64 `json:"removedBytes"`
	// AlreadyGoneBytes is the estimate for images deleted outside this run.
	AlreadyGoneBytes int64 `json:"alreadyGoneBytes"`
	// SkippedBytes is the estimate for images the re-check protected.
	SkippedBytes int64 `json:"skippedBytes"`
	// FailedBytes is the estimate for images the runtime refused to remove.
	FailedBytes int64 `json:"failedBytes"`
	// ReleasedBytes is the sum of the bytes the runtime reported as freed.
	ReleasedBytes int64           `json:"releasedBytes"`
	Counts        map[Outcome]int `json:"counts"`
	// DeltaWarning is set when ReleasedBytes differs materially from RemovedBytes.
	DeltaWarning string    `json:"deltaWarning,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
}

// EstimatedTotal returns the sum of the four estimate buckets. It equals PlannedBytes
// unless a bug dropped a step.
func (r *Report) EstimatedTotal() int64 {
	return r.RemovedBytes + r.AlreadyGoneBytes + r.SkippedBytes + r.FailedBytes
}

// Executor performs a plan against a live (or fake) runtime.
type Executor struct {
	// Snapshot returns a fresh metadata snapshot for the pre-removal re-check.
	Snapshot func() (*Snapshot, error)
	// Runtime performs removals.
	Runtime Runtime
	// Now is injectable for tests; it defaults to time.Now.
	Now func() time.Time
}

// deltaThreshold is the relative gap between estimate and observation above which a
// warning is emitted.
const deltaThreshold = 0.10

// ErrNilRuntime is returned when an executor has no runtime.
var ErrNilRuntime = errors.New("nil image reclaim runtime")

// ErrNilSnapshotFunc is returned when an executor cannot refresh metadata.
var ErrNilSnapshotFunc = errors.New("nil image metadata snapshot provider")

// Execute removes the planned images. Before each removal it takes a fresh snapshot
// and re-evaluates every protection rule, so an image a container started using after
// the user confirmed the estimate is skipped with the reason shown. A fresh snapshot is
// taken per image rather than once per batch: the gap that matters is the one between
// the last check and the removal, not the one between the estimate and the batch. A
// failure or a new protection on one image never stops the others.
func (e *Executor) Execute(plan *Plan) (*Report, error) {
	if e == nil || e.Runtime == nil {
		return nil, ErrNilRuntime
	}
	if e.Snapshot == nil {
		return nil, ErrNilSnapshotFunc
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}

	report := &Report{PlannedBytes: plan.Reclaimable, Counts: make(map[Outcome]int), StartedAt: now()}

	for _, removal := range plan.Remove {
		step := StepResult{Image: removal.Image, Tags: removal.Tags, Planned: removal.ExclusiveBytes}
		step.Outcome, step.Reasons, step.Released, step.Error = e.removeOne(plan.Options, removal)
		step.FinishedAt = now()
		switch step.Outcome {
		case OutcomeRemoved:
			report.ReleasedBytes += deref(step.Released)
			report.RemovedBytes += removal.ExclusiveBytes
		case OutcomeSkipped:
			report.SkippedBytes += removal.ExclusiveBytes
		case OutcomeAlreadyGone:
			report.AlreadyGoneBytes += removal.ExclusiveBytes
		case OutcomeFailed:
			report.FailedBytes += removal.ExclusiveBytes
		}
		report.Counts[step.Outcome]++
		report.Steps = append(report.Steps, step)
	}

	for _, tagRemoval := range plan.TagOnly {
		tags := append([]string(nil), tagRemoval.Tags...)
		sort.Strings(tags)
		for _, tag := range tags {
			step := StepResult{Image: tagRemoval.Image, Tags: []string{tag}}
			_, exists, lookupErr := e.lookupImage(tagRemoval.Image.ID)
			switch {
			case lookupErr != nil:
				// Removing a tag is cheap to retry, but a metadata read failure means the
				// current state is unknown, so report it instead of acting blindly.
				step.Outcome = OutcomeFailed
				step.Error = lookupErr.Error()
				report.Counts[OutcomeFailed]++
			case !exists:
				step.Outcome = OutcomeAlreadyGone
				report.Counts[OutcomeAlreadyGone]++
			default:
				if err := e.Runtime.RemoveTag(tag); err != nil {
					step.Outcome = OutcomeFailed
					step.Error = err.Error()
					report.Counts[OutcomeFailed]++
				} else {
					step.Outcome = OutcomeTagRemoved
					report.Counts[OutcomeTagRemoved]++
				}
			}
			step.FinishedAt = now()
			report.Steps = append(report.Steps, step)
		}
	}

	report.FinishedAt = now()
	if gap := report.PlannedBytes - report.EstimatedTotal(); gap != 0 {
		// The buckets are supposed to account for every planned byte. A gap would mean
		// the report is hiding something, so say so rather than print a total that does
		// not reconcile.
		report.DeltaWarning = strings.TrimSpace(report.DeltaWarning + " " +
			fmt.Sprintf("internal accounting gap of %d bytes between the plan and the per-outcome totals", gap))
	}
	if warning := deltaWarning(report.RemovedBytes, report.ReleasedBytes); warning != "" {
		report.DeltaWarning = strings.TrimSpace(report.DeltaWarning + " " + warning)
	}
	return report, nil
}

// removeOne performs the pre-removal re-check for a single image and removes it when
// it is still safe. It returns the outcome and, on success, the bytes the runtime
// released.
func (e *Executor) removeOne(opts Options, removal ImageRemoval) (Outcome, []Reason, *int64, string) {
	fresh, err := e.Snapshot()
	if err != nil {
		// A snapshot we cannot read means we cannot prove the image is unused, so the
		// safe answer is to skip it rather than delete it.
		return OutcomeSkipped, []Reason{{
			Code:   ReasonSnapshotUnavailable,
			Detail: err.Error(),
		}}, nil, ""
	}
	img, exists := e.lookupImageIn(fresh, removal.Image.ID)
	if !exists {
		return OutcomeAlreadyGone, nil, nil, ""
	}
	protected, err := EvaluateProtections(fresh, opts)
	if err != nil {
		return OutcomeSkipped, []Reason{{
			Code:   ReasonSnapshotUnavailable,
			Detail: err.Error(),
		}}, nil, ""
	}
	if reasons, blocked := protected[removal.Image.ID]; blocked {
		return OutcomeSkipped, reasons, nil, ""
	}
	if added := unexpectedTags(img, removal.Tags); len(added) > 0 {
		return OutcomeSkipped, []Reason{{
			Code:   ReasonTagChanged,
			Detail: fmt.Sprintf("image gained tag(s) since planning: %v", added),
		}}, nil, ""
	}
	release, err := e.Runtime.RemoveImage(removal.Image.ID, removal.Tags)
	if err != nil {
		return OutcomeFailed, nil, nil, err.Error()
	}
	return OutcomeRemoved, nil, &release, ""
}

// lookupImage re-reads the snapshot to check whether an image still exists.
func (e *Executor) lookupImage(id string) (Image, bool, error) {
	fresh, err := e.Snapshot()
	if err != nil {
		return Image{}, false, fmt.Errorf("refresh image metadata before removing tag: %w", err)
	}
	img, ok := e.lookupImageIn(fresh, id)
	return img, ok, nil
}

func (e *Executor) lookupImageIn(snap *Snapshot, id string) (Image, bool) {
	for _, img := range snap.Images {
		if img.ID == id {
			return img, true
		}
	}
	return Image{}, false
}

// unexpectedTags lists tags the image carries now that the plan did not account for.
// A tag added after planning means the image is in use by something new.
func unexpectedTags(img Image, planned []string) []string {
	known := make(map[string]struct{}, len(planned))
	for _, t := range planned {
		known[normalizeRef(t)] = struct{}{}
	}
	var added []string
	for _, t := range img.Names {
		if _, ok := known[normalizeRef(t)]; !ok {
			added = append(added, t)
		}
	}
	sort.Strings(added)
	return added
}

func deltaWarning(estimate, released int64) string {
	if estimate == released {
		return ""
	}
	base := estimate
	if base == 0 {
		base = released
	}
	if base == 0 {
		return ""
	}
	gap := estimate - released
	if gap < 0 {
		gap = -gap
	}
	if float64(gap)/float64(base) <= deltaThreshold {
		return ""
	}
	return fmt.Sprintf(
		"estimated %d bytes reclaimable for the removed images but the runtime reported %d bytes released",
		estimate, released)
}
