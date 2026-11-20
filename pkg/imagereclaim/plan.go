// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// DefaultRetainLabel is the image label that marks an image as explicitly retained.
// Images carrying it are never part of the deletion set.
const DefaultRetainLabel = "io.finch.image.retain"

// ReasonCode identifies why an image was protected from removal.
type ReasonCode string

// Reason codes reported for protected images. Every matching reason is reported, not
// only the first one, because each requires a different remedy from the user.
const (
	// ReasonRunningContainer marks images used by a running, paused, created, or
	// restarting container.
	ReasonRunningContainer ReasonCode = "running-container"
	// ReasonStoppedContainer marks images used only by exited or stopped containers.
	// They are treated as in use unless stopped containers are pruned too.
	ReasonStoppedContainer ReasonCode = "stopped-container"
	// ReasonInProgressOperation marks images referenced by a pull, build, or load in flight.
	ReasonInProgressOperation ReasonCode = "in-progress-operation"
	// ReasonRetainLabel marks images carrying the retain label.
	ReasonRetainLabel ReasonCode = "retain-label"
	// ReasonRetainConfig marks images named in the configured retention list.
	ReasonRetainConfig ReasonCode = "retain-config"
	// ReasonBuildIntermediate marks intermediate images and parents of retained images.
	ReasonBuildIntermediate ReasonCode = "build-intermediate"
	// ReasonTagChanged is produced only during execution: the image gained a tag after
	// planning, which means something new may be using it.
	ReasonTagChanged ReasonCode = "tag-changed"
	// ReasonSnapshotUnavailable is produced only during execution: the pre-removal
	// re-check could not read metadata, so the image cannot be proven unused.
	ReasonSnapshotUnavailable ReasonCode = "snapshot-unavailable"
)

// reasonOrder fixes the order in which reasons are listed for an image.
var reasonOrder = []ReasonCode{
	ReasonRunningContainer,
	ReasonStoppedContainer,
	ReasonInProgressOperation,
	ReasonRetainLabel,
	ReasonRetainConfig,
	ReasonBuildIntermediate,
	ReasonTagChanged,
	ReasonSnapshotUnavailable,
}

func reasonRank(code ReasonCode) int {
	for i, c := range reasonOrder {
		if c == code {
			return i
		}
	}
	return len(reasonOrder)
}

// Category groups reasons by the remedy they require. Container-based protection and
// explicit retention are reported separately because they are resolved differently.
func (r Reason) Category() string {
	switch r.Code {
	case ReasonRunningContainer, ReasonStoppedContainer:
		return "container-in-use"
	case ReasonRetainLabel, ReasonRetainConfig:
		return "explicit-retention"
	case ReasonInProgressOperation:
		return "in-progress"
	case ReasonTagChanged, ReasonSnapshotUnavailable:
		return "changed-since-plan"
	default:
		return "build-cache"
	}
}

// Reason explains one protection. Detail names the container or operation involved.
type Reason struct {
	Code   ReasonCode `json:"code"`
	Detail string     `json:"detail,omitempty"`
}

// Filters narrows the candidate set before any size is estimated. Filters select which
// tags may be removed; they never override a protection.
type Filters struct {
	// Names matches image tags, using path.Match style wildcards ("*", "?").
	Names []string `json:"names,omitempty"`
	// Labels selects images carrying all of the given labels.
	Labels map[string]string `json:"labels,omitempty"`
	// CreatedBefore selects images created strictly before this time.
	CreatedBefore *time.Time `json:"createdBefore,omitempty"`
}

// Options controls which images are eligible and which protections apply.
type Options struct {
	// All selects every unused image; when false only dangling (untagged) images are
	// candidates. Removing tags never frees space unless the image loses its last tag.
	All bool
	// PruneStoppedContainers treats images referenced by exited/stopped containers as
	// removable. Off by default: stopped containers still count as using their image.
	PruneStoppedContainers bool
	// PruneIntermediate allows removal of intermediate build images and of untagged
	// parents of retained images. Off by default.
	PruneIntermediate bool
	// RetainLabel is the label that marks an image as retained. Empty uses
	// DefaultRetainLabel.
	RetainLabel string
	// RetainedNames lists image names that must never be removed, even when unused.
	RetainedNames []string
	// Filters further restrict the deletion set. Filtering happens before estimation.
	Filters Filters
	// Now is the reference time for CreatedBefore. Zero uses time.Now.
	Now time.Time
}

func (o Options) retainLabel() string {
	if o.RetainLabel == "" {
		return DefaultRetainLabel
	}
	return o.RetainLabel
}

// ImageRemoval is one image planned for complete removal, together with the layers
// whose bytes are actually attributable to it.
type ImageRemoval struct {
	Image Image    `json:"image"`
	Tags  []string `json:"tags"`
	// Layers lists the layers that would be released solely because of this image.
	Layers []string `json:"layers,omitempty"`
	// ExclusiveBytes is the sum of Layers' sizes. It is 0 when all layers are shared.
	ExclusiveBytes int64 `json:"exclusiveBytes"`
	// UnknownLayers lists released layers whose size is missing from the snapshot.
	UnknownLayers []string `json:"unknownLayers,omitempty"`
}

// TagRemoval is a tag that would be removed without removing the image itself. It
// frees no space because the image keeps at least one other tag.
type TagRemoval struct {
	Image Image    `json:"image"`
	Tags  []string `json:"tags"`
}

// ProtectedImage is an image that a protection rule keeps out of the deletion set.
type ProtectedImage struct {
	Image   Image    `json:"image"`
	Reasons []Reason `json:"reasons"`
}

// OrphanLayer is a layer in the content store that no image references. Orphans are
// left alone: they are often the partially written output of an operation still in
// flight. They are reported but never added to the reclaimable total.
type OrphanLayer struct {
	Digest        string `json:"digest"`
	Size          *int64 `json:"size,omitempty"`
	InProgress    bool   `json:"inProgress"`
	OperationKind string `json:"operationKind,omitempty"`
}

// Plan is the estimate half of the workflow. It contains everything the executor
// needs to re-check and everything the report needs to show.
type Plan struct {
	Remove        []ImageRemoval   `json:"remove"`
	TagOnly       []TagRemoval     `json:"tagOnly,omitempty"`
	Protected     []ProtectedImage `json:"protected,omitempty"`
	OrphanLayers  []OrphanLayer    `json:"orphanLayers,omitempty"`
	Reclaimable   int64            `json:"reclaimableBytes"`
	UnknownLayers []string         `json:"unknownLayers,omitempty"`
	// NaiveBytes is the diagnostic sum of the candidate images' own layer sizes without
	// de-duplication. It is always greater than or equal to Reclaimable and is never
	// reported as an estimate.
	NaiveBytes int64     `json:"naiveBytes"`
	PlannedAt  time.Time `json:"plannedAt"`
	Options    Options   `json:"options"`
}

// Errors returned while planning.
var (
	// ErrNilSnapshot is returned when planning against a nil snapshot.
	ErrNilSnapshot = errors.New("nil image metadata snapshot")
)

type imageIndex struct {
	byID    map[string]Image
	byName  map[string]string   // normalized tag -> image ID
	byDigst map[string][]string // digest -> image IDs
	sizes   map[string]*int64
	layerOf map[string][]string
}

// newImageIndex validates the snapshot and builds lookup tables.
func newImageIndex(snap *Snapshot) (*imageIndex, error) {
	if err := snap.validate(); err != nil {
		if snap == nil {
			return nil, ErrNilSnapshot
		}
		return nil, err
	}
	idx := &imageIndex{
		byID:    make(map[string]Image, len(snap.Images)),
		byName:  make(map[string]string),
		byDigst: make(map[string][]string),
		sizes:   make(map[string]*int64, len(snap.Layers)),
		layerOf: make(map[string][]string, len(snap.Images)),
	}
	for _, img := range snap.Images {
		idx.byID[img.ID] = img
		for _, name := range img.Names {
			idx.byName[normalizeRef(name)] = img.ID
		}
		for _, d := range append([]string{img.Digest}, img.RepoDigests...) {
			for _, key := range digestKeys(d) {
				if !slices.Contains(idx.byDigst[key], img.ID) {
					idx.byDigst[key] = append(idx.byDigst[key], img.ID)
				}
			}
		}
		idx.layerOf[img.ID] = img.Layers
	}
	// Later entries win when two images report different sizes for the same layer;
	// validate already rejected genuinely conflicting metadata.
	for _, layer := range snap.Layers {
		idx.sizes[layer.Digest] = layer.Size
	}
	return idx, nil
}

// nameMatches reports whether ref matches any of the given patterns.
func nameMatches(ref string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, candidate := range refVariants(ref) {
		for _, pattern := range patterns {
			for _, p := range patternVariants(pattern) {
				if p == candidate || wildcardMatch(p, candidate) {
					return true
				}
			}
		}
	}
	return false
}

// patternVariants is refVariants for a glob pattern. A pattern containing wildcards is
// never given a default tag, because appending ":latest" to "app:*" would change what
// the pattern means.
func patternVariants(pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	if strings.ContainsAny(pattern, "*?") || strings.Contains(pattern, "@") {
		return []string{pattern}
	}
	return refVariants(pattern)
}

// wildcardMatch matches pattern against name with "*" and "?" wildcards.
func wildcardMatch(pattern, name string) bool {
	// Iterative glob match to avoid pathological recursion on long names.
	px, nx := 0, 0
	star, mark := -1, 0
	for nx < len(name) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == name[nx]):
			px++
			nx++
		case px < len(pattern) && pattern[px] == '*':
			star = px
			px++
			mark = nx
		case star >= 0:
			px = star + 1
			mark++
			nx = mark
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

func (f Filters) matches(img Image) bool {
	if f.CreatedBefore != nil {
		if img.CreatedAt == nil || !img.CreatedAt.Before(*f.CreatedBefore) {
			return false
		}
	}
	for k, v := range f.Labels {
		if img.Labels[k] != v {
			return false
		}
	}
	if len(f.Names) > 0 {
		if img.Dangling() {
			return false
		}
		matched := false
		for _, name := range img.Names {
			if nameMatches(name, f.Names) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// references maps an image ID to the containers that point at it through a tag, an
// image ID, or a digest.
func (s *Snapshot) references(idx *imageIndex) map[string][]Container {
	refs := make(map[string][]Container, len(s.Images))
	seen := make(map[string]map[string]struct{}, len(s.Images))
	add := func(id string, c Container) {
		if seen[id] == nil {
			seen[id] = make(map[string]struct{})
		}
		key := c.ID + "|" + c.Name
		if _, ok := seen[id][key]; ok {
			return
		}
		seen[id][key] = struct{}{}
		refs[id] = append(refs[id], c)
	}
	for _, c := range s.Containers {
		ids := make(map[string]struct{})
		if c.Image != "" {
			if id, ok := idx.byName[normalizeRef(c.Image)]; ok {
				ids[id] = struct{}{}
			}
			// A container may be recorded with its tag even if the tag appeared after
			// the image was indexed; try the raw form as well.
			if id, ok := idx.byName[c.Image]; ok {
				ids[id] = struct{}{}
			}
		}
		if c.ImageID != "" {
			if _, ok := idx.byID[c.ImageID]; ok {
				ids[c.ImageID] = struct{}{}
			}
		}
		for _, d := range c.ImageDigests {
			for _, key := range digestKeys(d) {
				for _, id := range idx.byDigst[key] {
					ids[id] = struct{}{}
				}
			}
		}
		for id := range ids {
			add(id, c)
		}
	}
	for id := range refs {
		sort.Slice(refs[id], func(i, j int) bool { return refs[id][i].display() < refs[id][j].display() })
	}
	return refs
}

// operationsByImage maps image IDs to the operations in flight that touch them. A
// layer is resolved through a reverse index so the cost does not grow with the
// product of operation layers and image layers.
func operationsByImage(s *Snapshot, idx *imageIndex) map[string][]Operation {
	byLayer := make(map[string][]string, len(idx.layerOf))
	for id, layers := range idx.layerOf {
		for _, l := range layers {
			byLayer[l] = append(byLayer[l], id)
		}
	}
	ops := make(map[string][]Operation)
	for _, op := range s.Operations {
		ids := make(map[string]struct{})
		if op.ImageID != "" {
			ids[op.ImageID] = struct{}{}
		}
		if op.Ref != "" {
			if id, ok := idx.byName[normalizeRef(op.Ref)]; ok {
				ids[id] = struct{}{}
			}
			if id, ok := idx.byName[op.Ref]; ok {
				ids[id] = struct{}{}
			}
		}
		for _, layer := range op.Layers {
			for _, id := range byLayer[layer] {
				ids[id] = struct{}{}
			}
		}
		for id := range ids {
			if _, known := idx.byID[id]; !known {
				continue
			}
			ops[id] = append(ops[id], op)
		}
	}
	return ops
}

func (s *Snapshot) children(idx *imageIndex) map[string][]string {
	children := make(map[string][]string)
	for _, img := range s.Images {
		if img.ParentID != "" {
			if _, ok := idx.byID[img.ParentID]; ok {
				children[img.ParentID] = append(children[img.ParentID], img.ID)
			}
		}
	}
	return children
}

// EvaluateProtections evaluates every protection rule against every image in the
// snapshot and returns the matching reasons per image ID. It is exported so that the
// executor's pre-removal re-check uses exactly the same rules as the estimate.
func EvaluateProtections(s *Snapshot, opts Options) (map[string][]Reason, error) {
	idx, err := newImageIndex(s)
	if err != nil {
		return nil, err
	}
	return protections(s, idx, opts), nil
}

// protections evaluates every protection rule for every image. All matching reasons
// are returned in the fixed canonical order.
func protections(s *Snapshot, idx *imageIndex, opts Options) map[string][]Reason {
	res := make(map[string][]Reason)
	add := func(id string, reason Reason) {
		res[id] = append(res[id], reason)
	}

	refs := s.references(idx)
	for id, containers := range refs {
		for _, c := range containers {
			if c.running() {
				add(id, Reason{Code: ReasonRunningContainer, Detail: c.display()})
				continue
			}
			if !opts.PruneStoppedContainers {
				add(id, Reason{Code: ReasonStoppedContainer, Detail: c.display()})
			}
		}
	}

	for id, ops := range operationsByImage(s, idx) {
		kinds := make([]string, 0, len(ops))
		for _, op := range ops {
			kind := op.Kind
			if kind == "" {
				kind = "operation"
			}
			if op.Ref != "" {
				kind = fmt.Sprintf("%s %s", kind, op.Ref)
			}
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		add(id, Reason{Code: ReasonInProgressOperation, Detail: strings.Join(kinds, ", ")})
	}

	retainLabel := opts.retainLabel()
	retained := make(map[string]struct{})
	for _, img := range s.Images {
		if _, ok := img.Labels[retainLabel]; ok && img.Labels[retainLabel] != "" && img.Labels[retainLabel] != "false" {
			add(img.ID, Reason{Code: ReasonRetainLabel, Detail: fmt.Sprintf("%s=%s", retainLabel, img.Labels[retainLabel])})
		}
		for _, name := range img.Names {
			if len(opts.RetainedNames) > 0 && nameMatches(name, opts.RetainedNames) {
				retained[img.ID] = struct{}{}
			}
		}
	}
	for id := range retained {
		add(id, Reason{Code: ReasonRetainConfig, Detail: "listed in retained image configuration"})
	}

	if !opts.PruneIntermediate {
		children := s.children(idx)
		for _, img := range s.Images {
			if img.Intermediate {
				add(img.ID, Reason{Code: ReasonBuildIntermediate, Detail: "intermediate build image"})
			}
			if img.Dangling() && len(children[img.ID]) > 0 {
				add(img.ID, Reason{Code: ReasonBuildIntermediate, Detail: "parent of another image"})
			}
		}
		// A parent must survive whenever a child is itself protected, because removing
		// the parent would invalidate the child's build-cache chain.
		for parent, kids := range children {
			if len(res[parent]) > 0 {
				continue
			}
			for _, kid := range kids {
				if len(res[kid]) > 0 {
					add(parent, Reason{
						Code:   ReasonBuildIntermediate,
						Detail: fmt.Sprintf("parent of protected image %s", idx.byID[kid].DisplayName()),
					})
					break
				}
			}
		}
	}

	for id := range res {
		reasons := res[id]
		sort.SliceStable(reasons, func(i, j int) bool { return reasonRank(reasons[i].Code) < reasonRank(reasons[j].Code) })
		deduped := reasons[:0]
		for i, r := range reasons {
			if i > 0 && reasons[i-1] == r {
				continue
			}
			deduped = append(deduped, r)
		}
		res[id] = deduped
	}
	return res
}

// candidates decides which tags (or which dangling images) are in the deletion set
// before any protection or size is considered. A name filter selects individual tags,
// which is how removing one tag of a multi-tag image is expressed.
func candidates(s *Snapshot, opts Options) map[string][]string {
	sel := make(map[string][]string)
	for _, img := range s.Images {
		if img.Dangling() {
			// Unnamed images cannot match a name filter. They are candidates in both
			// modes: in All mode they are unused images, otherwise they are the
			// dangling set.
			if len(opts.Filters.Names) == 0 && opts.Filters.matches(img) {
				sel[img.ID] = nil
			}
			continue
		}
		tags := img.Names
		if len(opts.Filters.Names) > 0 {
			tags = nil
			for _, name := range img.Names {
				if nameMatches(name, opts.Filters.Names) {
					tags = append(tags, name)
				}
			}
		}
		if len(tags) == 0 || !opts.Filters.matches(img) {
			continue
		}
		if !opts.All && len(opts.Filters.Names) == 0 {
			continue
		}
		sel[img.ID] = append([]string(nil), tags...)
	}
	return sel
}

// ComputePlan computes the estimate. It never mutates the snapshot or talks to a runtime.
func ComputePlan(snap *Snapshot, opts Options) (*Plan, error) {
	idx, err := newImageIndex(snap)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	protected := protections(snap, idx, opts)
	plan := &Plan{PlannedAt: now, Options: opts}
	sel := candidates(snap, opts)

	removedAll := make(map[string]struct{})
	tagOnly := make(map[string][]string)
	for id, tags := range sel {
		if _, isProtected := protected[id]; isProtected {
			continue
		}
		img := idx.byID[id]
		if img.Dangling() || len(tags) >= len(img.Names) {
			removedAll[id] = struct{}{}
			continue
		}
		tagOnly[id] = tags
	}

	for _, img := range snap.Images {
		reasons, ok := protected[img.ID]
		if !ok {
			continue
		}
		if _, selected := sel[img.ID]; selected {
			plan.Protected = append(plan.Protected, ProtectedImage{Image: img, Reasons: reasons})
		}
	}

	// Survivors keep their layers. Tag-only images also survive.
	survivingLayers := make(map[string]struct{})
	for _, img := range snap.Images {
		if _, removed := removedAll[img.ID]; !removed {
			for _, l := range img.Layers {
				survivingLayers[l] = struct{}{}
			}
		}
	}

	// Layers touched by in-flight operations are never released, even when every
	// image that references them is going away.
	operationLayers := make(map[string]string)
	for _, op := range snap.Operations {
		kind := op.Kind
		if kind == "" {
			kind = "operation"
		}
		for _, l := range op.Layers {
			operationLayers[l] = kind
		}
	}

	// Freed layer set: referenced by a removed image, referenced by no survivor, and
	// not touched by an operation in flight.
	freed := make(map[string]struct{})
	for id := range removedAll {
		for _, l := range idx.layerOf[id] {
			if _, shared := survivingLayers[l]; shared {
				continue
			}
			if _, inFlight := operationLayers[l]; inFlight {
				continue
			}
			freed[l] = struct{}{}
		}
	}

	// Attribute each freed layer to exactly one removed image so that per-image sizes
	// add up to the total. Images are ordered deterministically.
	removedIDs := make([]string, 0, len(removedAll))
	for id := range removedAll {
		removedIDs = append(removedIDs, id)
	}
	sort.Slice(removedIDs, func(i, j int) bool {
		a, b := idx.byID[removedIDs[i]], idx.byID[removedIDs[j]]
		if a.DisplayName() != b.DisplayName() {
			return a.DisplayName() < b.DisplayName()
		}
		return a.ID < b.ID
	})

	seenLayers := make(map[string]struct{}, len(freed))
	unknown := make(map[string]struct{})
	for _, id := range removedIDs {
		img := idx.byID[id]
		removal := ImageRemoval{Image: img, Tags: append([]string{}, img.Names...)}
		sort.Strings(removal.Tags)
		for _, l := range img.Layers {
			if _, ok := freed[l]; !ok {
				continue
			}
			if _, taken := seenLayers[l]; taken {
				continue
			}
			seenLayers[l] = struct{}{}
			removal.Layers = append(removal.Layers, l)
			size, known := idx.sizes[l]
			switch {
			case !known || size == nil:
				removal.UnknownLayers = append(removal.UnknownLayers, l)
				unknown[l] = struct{}{}
			default:
				removal.ExclusiveBytes += *size
			}
		}
		sort.Strings(removal.Layers)
		sort.Strings(removal.UnknownLayers)
		plan.Remove = append(plan.Remove, removal)
		plan.Reclaimable += removal.ExclusiveBytes
	}

	for id, tags := range tagOnly {
		img := idx.byID[id]
		sorted := append([]string(nil), tags...)
		sort.Strings(sorted)
		plan.TagOnly = append(plan.TagOnly, TagRemoval{Image: img, Tags: sorted})
	}
	sort.Slice(plan.TagOnly, func(i, j int) bool {
		return plan.TagOnly[i].Image.DisplayName() < plan.TagOnly[j].Image.DisplayName()
	})

	// Diagnostic: the sum of the images' own sizes, layer by layer, without the
	// de-duplication that makes the real estimate correct.
	for _, id := range removedIDs {
		for _, l := range idx.layerOf[id] {
			if size, ok := idx.sizes[l]; ok && size != nil {
				plan.NaiveBytes += *size
			}
		}
	}

	for l := range unknown {
		plan.UnknownLayers = append(plan.UnknownLayers, l)
	}
	sort.Strings(plan.UnknownLayers)

	// Orphan layers: present in the content store, referenced by no image at all.
	referenced := make(map[string]struct{})
	for _, img := range snap.Images {
		for _, l := range img.Layers {
			referenced[l] = struct{}{}
		}
	}
	for _, layer := range snap.Layers {
		if _, ok := referenced[layer.Digest]; ok {
			continue
		}
		orphan := OrphanLayer{Digest: layer.Digest, Size: layer.Size}
		if kind, ok := operationLayers[layer.Digest]; ok {
			orphan.InProgress = true
			orphan.OperationKind = kind
		}
		plan.OrphanLayers = append(plan.OrphanLayers, orphan)
	}
	sort.Slice(plan.OrphanLayers, func(i, j int) bool { return plan.OrphanLayers[i].Digest < plan.OrphanLayers[j].Digest })

	sort.Slice(plan.Protected, func(i, j int) bool {
		a, b := plan.Protected[i].Image, plan.Protected[j].Image
		if a.DisplayName() != b.DisplayName() {
			return a.DisplayName() < b.DisplayName()
		}
		return a.ID < b.ID
	})
	return plan, nil
}
