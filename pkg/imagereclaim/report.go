// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Format selects how a report is rendered.
type Format string

// Supported output formats.
const (
	// FormatText is the human-readable form.
	FormatText Format = "text"
	// FormatJSON is a stable, script-consumable document. Its field names are part of
	// the interface and must not be renamed casually.
	FormatJSON Format = "json"
)

// WritePlanText renders an estimate. Every removal is listed with the bytes it alone
// releases, followed by totals and by sections for tag-only removals, protected
// images, size-unknown layers, and orphan layers.
func WritePlanText(w io.Writer, plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Reclaimable space: %s (%d bytes)\n", HumanBytes(plan.Reclaimable), plan.Reclaimable)
	fmt.Fprintf(&b, "Naive sum of image sizes (not a valid estimate): %s (%d bytes)\n",
		HumanBytes(plan.NaiveBytes), plan.NaiveBytes)
	if plan.NaiveBytes > plan.Reclaimable {
		fmt.Fprintf(&b, "Shared layers account for the %s difference.\n",
			HumanBytes(plan.NaiveBytes-plan.Reclaimable))
	}
	for _, layer := range plan.UnknownLayers {
		fmt.Fprintf(&b, "Size unknown for layer %s; excluded from the estimate.\n", layer)
	}
	if len(plan.UnknownLayers) > 0 {
		fmt.Fprintf(&b, "%d layer(s) have no size metadata; their bytes are not included above.\n", len(plan.UnknownLayers))
	}

	b.WriteString("\nImages to remove:\n")
	if len(plan.Remove) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, removal := range plan.Remove {
		fmt.Fprintf(&b, "  %-40s image %s  %s exclusive\n",
			strings.Join(removal.Tags, ", ")+danglingSuffix(removal.Tags),
			shortID(removal.Image.ID), HumanBytes(removal.ExclusiveBytes))
		if len(removal.UnknownLayers) > 0 {
			fmt.Fprintf(&b, "      %d layer(s) of unknown size not counted\n", len(removal.UnknownLayers))
		}
	}

	b.WriteString("\nEach freed layer is attributed to one image, so the per-image sizes add up to the total;\n")
	b.WriteString("an image that shares all of its layers with another removed or surviving image shows 0 B.\n")

	if len(plan.TagOnly) > 0 {
		b.WriteString("\nTags to remove without deleting their image (frees no space):\n")
		for _, t := range plan.TagOnly {
			fmt.Fprintf(&b, "  %s from image %s\n", strings.Join(t.Tags, ", "), shortID(t.Image.ID))
		}
	}

	if len(plan.Protected) > 0 {
		b.WriteString("\nProtected images (kept):\n")
		for _, p := range plan.Protected {
			fmt.Fprintf(&b, "  %s (%s)\n", p.Image.DisplayName(), shortID(p.Image.ID))
			byCategory := groupReasons(p.Reasons)
			for _, cat := range sortedKeys(byCategory) {
				fmt.Fprintf(&b, "      %s:\n", cat)
				for _, r := range byCategory[cat] {
					fmt.Fprintf(&b, "          %s (%s)\n", r.Code, r.Detail)
				}
			}
		}
	}

	if len(plan.OrphanLayers) > 0 {
		b.WriteString("\nUnreferenced layers (left in place; may be an operation in progress):\n")
		for _, o := range plan.OrphanLayers {
			size := "size unknown"
			if o.Size != nil {
				size = HumanBytes(*o.Size)
			}
			suffix := ""
			if o.InProgress {
				suffix = fmt.Sprintf(" (in progress: %s)", o.OperationKind)
			}
			fmt.Fprintf(&b, "  %s  %s%s\n", o.Digest, size, suffix)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// PlanDocument is the stable JSON shape of an estimate.
type PlanDocument struct {
	ReclaimableBytes int64            `json:"reclaimableBytes"`
	NaiveBytes       int64            `json:"naiveBytes"`
	Images           []ImageRemoval   `json:"images"`
	TagOnly          []TagRemoval     `json:"tagOnly"`
	Protected        []ProtectedImage `json:"protected"`
	UnknownLayers    []string         `json:"unknownLayers"`
	OrphanLayers     []OrphanLayer    `json:"orphanLayers"`
}

// Document converts a plan into its stable JSON form.
func (p *Plan) Document() PlanDocument {
	doc := PlanDocument{
		ReclaimableBytes: p.Reclaimable,
		NaiveBytes:       p.NaiveBytes,
		Images:           append([]ImageRemoval(nil), p.Remove...),
		TagOnly:          append([]TagRemoval(nil), p.TagOnly...),
		Protected:        append([]ProtectedImage(nil), p.Protected...),
		UnknownLayers:    append([]string(nil), p.UnknownLayers...),
		OrphanLayers:     append([]OrphanLayer(nil), p.OrphanLayers...),
	}
	if doc.Images == nil {
		doc.Images = []ImageRemoval{}
	}
	if doc.TagOnly == nil {
		doc.TagOnly = []TagRemoval{}
	}
	if doc.Protected == nil {
		doc.Protected = []ProtectedImage{}
	}
	if doc.UnknownLayers == nil {
		doc.UnknownLayers = []string{}
	}
	if doc.OrphanLayers == nil {
		doc.OrphanLayers = []OrphanLayer{}
	}
	return doc
}

// WritePlanJSON writes the estimate as stable JSON.
func WritePlanJSON(w io.Writer, plan *Plan) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(plan.Document())
}

// WriteReportText renders the execution outcome: what was removed, what a re-check
// protected, what failed, and what had already disappeared, then compares the
// estimate with the observed release.
func WriteReportText(w io.Writer, report *Report) error {
	if report == nil {
		return fmt.Errorf("nil report")
	}
	var b strings.Builder
	b.WriteString("Removal results:\n")
	for _, step := range report.Steps {
		name := strings.Join(step.Tags, ", ")
		if name == "" {
			name = step.Image.DisplayName()
		}
		switch step.Outcome {
		case OutcomeRemoved:
			fmt.Fprintf(&b, "  removed        %s (%s)  %s released\n", name, shortID(step.Image.ID), HumanBytes(deref(step.Released)))
		case OutcomeAlreadyGone:
			fmt.Fprintf(&b, "  already gone   %s (%s)  removed outside this run\n", name, shortID(step.Image.ID))
		case OutcomeSkipped:
			fmt.Fprintf(&b, "  skipped        %s (%s)  %s\n", name, shortID(step.Image.ID), reasonSummary(step.Reasons))
		case OutcomeFailed:
			fmt.Fprintf(&b, "  failed         %s (%s)  %s\n", name, shortID(step.Image.ID), step.Error)
		case OutcomeTagRemoved:
			fmt.Fprintf(&b, "  tag removed    %s (image kept)\n", name)
		}
	}

	fmt.Fprintf(&b, "\nTotals:\n")
	fmt.Fprintf(&b, "  planned      %s\n", HumanBytes(report.PlannedBytes))
	fmt.Fprintf(&b, "  removed      %s\n", HumanBytes(report.RemovedBytes))
	fmt.Fprintf(&b, "  already gone %s\n", HumanBytes(report.AlreadyGoneBytes))
	fmt.Fprintf(&b, "  skipped      %s\n", HumanBytes(report.SkippedBytes))
	fmt.Fprintf(&b, "  failed       %s\n", HumanBytes(report.FailedBytes))
	fmt.Fprintf(&b, "  released     %s (observed)\n", HumanBytes(report.ReleasedBytes))
	b.WriteString("  counts: ")
	counts := make([]string, 0, len(report.Counts))
	for _, outcome := range []Outcome{OutcomeRemoved, OutcomeSkipped, OutcomeFailed, OutcomeAlreadyGone, OutcomeTagRemoved} {
		if n := report.Counts[outcome]; n > 0 {
			counts = append(counts, fmt.Sprintf("%s=%d", outcome, n))
		}
	}
	b.WriteString(strings.Join(counts, " "))
	b.WriteString("\n")
	if report.DeltaWarning != "" {
		fmt.Fprintf(&b, "warning: %s\n", report.DeltaWarning)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteReportJSON writes the execution outcome as stable JSON.
func WriteReportJSON(w io.Writer, report *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// HumanBytes formats a byte count with binary units. It never rounds a nonzero value
// down to "0 B", so small releases stay visible.
func HumanBytes(n int64) string {
	if n == 0 {
		return "0 B"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	var out string
	switch {
	case unit == 0:
		out = fmt.Sprintf("%d B", n)
	case value < 10:
		out = fmt.Sprintf("%.2f %s", value, units[unit])
	default:
		out = fmt.Sprintf("%.1f %s", value, units[unit])
	}
	if negative {
		return "-" + out
	}
	return out
}

func groupReasons(reasons []Reason) map[string][]Reason {
	out := make(map[string][]Reason)
	for _, r := range reasons {
		out[r.Category()] = append(out[r.Category()], r)
	}
	return out
}

func sortedKeys(m map[string][]Reason) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func reasonSummary(reasons []Reason) string {
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if r.Detail != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", r.Code, r.Detail))
			continue
		}
		parts = append(parts, string(r.Code))
	}
	return strings.Join(parts, "; ")
}

func danglingSuffix(tags []string) string {
	if len(tags) == 0 {
		return "<dangling> "
	}
	return ""
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "<none>"
	}
	return id
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
