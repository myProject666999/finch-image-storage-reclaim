// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package imagereclaim

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// FakeRuntime is an in-memory Runtime used by tests. It owns a snapshot, applies
// removals to it, and can be told to fail specific removals. It is deliberately
// simple: it exists to prove the executor's behaviour, not to model containerd.
type FakeRuntime struct {
	mu       sync.Mutex
	snap     *Snapshot
	failures map[string]error
	// Hook runs before each removal, allowing a test to mutate the snapshot between
	// planning and execution.
	Hook func(id string)
	// removed records image IDs the runtime was asked to remove.
	removed []string
}

// NewFakeRuntime returns a FakeRuntime backed by a copy of snap.
func NewFakeRuntime(snap *Snapshot) *FakeRuntime {
	return &FakeRuntime{
		snap:     snap.Clone(),
		failures: make(map[string]error),
	}
}

// FailWith makes every attempt to remove the given image ID return err.
func (f *FakeRuntime) FailWith(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[id] = err
}

// Snapshot returns a copy of the current state, suitable for the executor's re-check.
func (f *FakeRuntime) Snapshot() (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap.Clone(), nil
}

// Removed returns the image IDs the runtime was asked to remove, in call order.
func (f *FakeRuntime) Removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// RemoveImage deletes the image and returns the bytes its exclusive layers occupied.
// Hook runs before the lock is taken, so a test may mutate the runtime's state from it.
func (f *FakeRuntime) RemoveImage(id string, _ []string) (int64, error) {
	f.mu.Lock()
	hook := f.Hook
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	if err, ok := f.failures[id]; ok {
		return 0, err
	}
	idx, err := newImageIndex(f.snap)
	if err != nil {
		return 0, err
	}
	target, ok := idx.byID[id]
	if !ok {
		return 0, fmt.Errorf("no such image %q", id)
	}

	keep := make(map[string]struct{})
	var freed int64
	for _, img := range f.snap.Images {
		if img.ID == id {
			continue
		}
		for _, l := range img.Layers {
			keep[l] = struct{}{}
		}
	}
	for _, l := range target.Layers {
		if _, shared := keep[l]; shared {
			continue
		}
		if size, ok := idx.sizes[l]; ok && size != nil {
			freed += *size
		}
	}

	filtered := f.snap.Images[:0]
	for _, img := range f.snap.Images {
		if img.ID != id {
			filtered = append(filtered, img)
		}
	}
	f.snap.Images = filtered
	f.snap.Containers = dropContainersFor(f.snap.Containers, target)
	return freed, nil
}

// RemoveTag removes a tag from whichever image carries it.
func (f *FakeRuntime) RemoveTag(tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := normalizeRef(tag)
	for i := range f.snap.Images {
		names := f.snap.Images[i].Names
		for j, name := range names {
			if normalizeRef(name) != want {
				continue
			}
			f.snap.Images[i].Names = append(append([]string(nil), names[:j]...), names[j+1:]...)
			sort.Strings(f.snap.Images[i].Names)
			return nil
		}
	}
	return fmt.Errorf("no such image tag %q", tag)
}

// dropContainersFor removes containers that referenced the deleted image. A real
// runtime refuses to delete an image that a container uses, so this only runs for
// images the re-check found unprotected, i.e. stopped containers being pruned.
func dropContainersFor(containers []Container, target Image) []Container {
	names := make(map[string]struct{}, len(target.Names))
	for _, n := range target.Names {
		names[normalizeRef(n)] = struct{}{}
	}
	digests := make(map[string]struct{}, len(target.RepoDigests)+1)
	if target.Digest != "" {
		digests[target.Digest] = struct{}{}
	}
	for _, d := range target.RepoDigests {
		digests[d] = struct{}{}
	}
	out := containers[:0]
	for _, c := range containers {
		referenced := c.ImageID == target.ID
		if _, ok := names[normalizeRef(c.Image)]; ok && c.Image != "" {
			referenced = true
		}
		for _, d := range c.ImageDigests {
			if _, ok := digests[d]; ok {
				referenced = true
			}
		}
		if !referenced {
			out = append(out, c)
		}
	}
	return out
}

// ErrFakeInjected is a convenient error for tests that inject a removal failure.
var ErrFakeInjected = errors.New("injected removal failure")
