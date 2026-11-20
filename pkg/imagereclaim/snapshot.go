// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package imagereclaim estimates how much disk space pruning images would free and
// then executes the pruning, re-checking protections immediately before each removal.
//
// The estimate is layer-based: several images usually share the same base layers, so
// adding up image sizes overstates the reclaimable space. Only layers that are no
// longer referenced by any surviving image once the deletion set is gone can be freed.
package imagereclaim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ContainerState is the state reported for a container in the metadata snapshot.
type ContainerState string

// Container states understood by the planner.
const (
	StateRunning    ContainerState = "running"
	StateCreated    ContainerState = "created"
	StatePaused     ContainerState = "paused"
	StateRestarting ContainerState = "restarting"
	StateExited     ContainerState = "exited"
	StateStopped    ContainerState = "stopped"
	StateDead       ContainerState = "dead"
)

// Snapshot is a local, point-in-time view of image, layer, container, and in-flight
// operation metadata. It is the only input to planning; it is not a live API client.
type Snapshot struct {
	Images     []Image     `json:"images"`
	Layers     []Layer     `json:"layers"`
	Containers []Container `json:"containers,omitempty"`
	Operations []Operation `json:"operations,omitempty"`
}

// Clone returns a deep copy that can be mutated independently of the original. Test
// fakes and the executor's re-check both use it to simulate the passage of time.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	out := &Snapshot{
		Images:     make([]Image, len(s.Images)),
		Layers:     make([]Layer, len(s.Layers)),
		Containers: make([]Container, len(s.Containers)),
		Operations: make([]Operation, len(s.Operations)),
	}
	for i, img := range s.Images {
		clone := img
		clone.Names = append([]string(nil), img.Names...)
		clone.RepoDigests = append([]string(nil), img.RepoDigests...)
		clone.Layers = append([]string(nil), img.Layers...)
		if img.Labels != nil {
			clone.Labels = make(map[string]string, len(img.Labels))
			for k, v := range img.Labels {
				clone.Labels[k] = v
			}
		}
		if img.CreatedAt != nil {
			t := *img.CreatedAt
			clone.CreatedAt = &t
		}
		out.Images[i] = clone
	}
	for i, layer := range s.Layers {
		clone := Layer{Digest: layer.Digest}
		if layer.Size != nil {
			size := *layer.Size
			clone.Size = &size
		}
		out.Layers[i] = clone
	}
	for i, c := range s.Containers {
		clone := c
		clone.ImageDigests = append([]string(nil), c.ImageDigests...)
		out.Containers[i] = clone
	}
	for i, op := range s.Operations {
		clone := op
		clone.Layers = append([]string(nil), op.Layers...)
		out.Operations[i] = clone
	}
	return out
}

// Image describes one image record. Names holds every tag that refers to the image;
// an image without names is dangling. Digest, RepoDigests, and ID are additional ways
// a container may refer to the image.
type Image struct {
	ID           string            `json:"id"`
	Names        []string          `json:"names,omitempty"`
	Digest       string            `json:"digest,omitempty"`
	RepoDigests  []string          `json:"repoDigests,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    *time.Time        `json:"createdAt,omitempty"`
	Intermediate bool              `json:"intermediate,omitempty"`
	// ParentID is set when the image was built on top of another image in the snapshot.
	ParentID string   `json:"parentID,omitempty"`
	Layers   []string `json:"layers"`
}

// Dangling reports whether the image has no tags left.
func (i Image) Dangling() bool {
	return len(i.Names) == 0
}

// DisplayName returns the lexicographically first tag of the image, or "<none>" when
// it is dangling. Picking the smallest tag rather than the first in the slice keeps
// reports stable when the snapshot lists tags in a different order.
func (i Image) DisplayName() string {
	if len(i.Names) == 0 {
		return "<none>"
	}
	first := i.Names[0]
	for _, name := range i.Names[1:] {
		if name < first {
			first = name
		}
	}
	return first
}

// Layer describes a layer in the content store. A nil Size means the size metadata is
// incomplete; such a layer is reported as size-unknown and never guessed.
type Layer struct {
	Digest string `json:"digest"`
	Size   *int64 `json:"size,omitempty"`
}

// Container describes a container's reference to an image. A reference may be a tag
// (Image), an image ID (ImageID), or one or more digests (ImageDigests).
type Container struct {
	ID           string         `json:"id"`
	Name         string         `json:"name,omitempty"`
	State        ContainerState `json:"state"`
	Image        string         `json:"image,omitempty"`
	ImageID      string         `json:"imageID,omitempty"`
	ImageDigests []string       `json:"imageDigests,omitempty"`
}

func (c Container) display() string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

// running reports whether the container currently uses its image. Unknown states are
// treated as running so that incomplete metadata never leads to an unsafe removal.
func (c Container) running() bool {
	switch c.State {
	case StateExited, StateStopped, StateDead:
		return false
	default:
		return true
	}
}

// Operation is an in-flight pull, build, or load. Images and layers touched by an
// operation must not be reclaimed, and layers already written by an unfinished
// operation must not be mistaken for orphaned content.
type Operation struct {
	Kind    string   `json:"kind"`
	Ref     string   `json:"ref,omitempty"`
	ImageID string   `json:"imageID,omitempty"`
	Layers  []string `json:"layers,omitempty"`
}

// DecodeSnapshot reads a snapshot from JSON.
func DecodeSnapshot(r io.Reader) (*Snapshot, error) {
	var snap Snapshot
	dec := json.NewDecoder(r)
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode image metadata snapshot: %w", err)
	}
	return &snap, nil
}

// validate rejects snapshots that cannot be planned against unambiguously.
func (s *Snapshot) validate() error {
	if s == nil {
		return errors.New("nil image metadata snapshot")
	}
	seen := make(map[string]struct{}, len(s.Images))
	for _, img := range s.Images {
		if img.ID == "" {
			return errors.New("image record with empty ID")
		}
		if _, ok := seen[img.ID]; ok {
			return fmt.Errorf("duplicate image ID %q in snapshot", img.ID)
		}
		seen[img.ID] = struct{}{}
	}
	tags := make(map[string]string, len(s.Images))
	for _, img := range s.Images {
		for _, name := range img.Names {
			key := normalizeRef(name)
			if prev, ok := tags[key]; ok && prev != img.ID {
				return fmt.Errorf("tag %q refers to two images (%s and %s)", name, prev, img.ID)
			}
			tags[key] = img.ID
		}
	}
	sizes := make(map[string]*int64, len(s.Layers))
	for _, layer := range s.Layers {
		if layer.Digest == "" {
			return errors.New("layer record with empty digest")
		}
		if prev, ok := sizes[layer.Digest]; ok {
			if (prev == nil) != (layer.Size == nil) || (prev != nil && *prev != *layer.Size) {
				return fmt.Errorf("conflicting sizes for layer %q in snapshot", layer.Digest)
			}
			continue
		}
		sizes[layer.Digest] = layer.Size
	}
	return nil
}

// normalizeRef gives a reference the default tag when it has none, without rewriting
// the registry part. Digest references are returned unchanged.
func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "@") {
		return ref
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon <= lastSlash {
		return ref + ":latest"
	}
	return ref
}

// digestKeys returns the ways a digest reference may be written. A container may
// record "sha256:abc" while the image knows itself as "example.com/web@sha256:abc";
// both must resolve to the same image.
func digestKeys(ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	keys := []string{ref}
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		if suffix := ref[at+1:]; suffix != "" {
			keys = append(keys, suffix)
		}
	}
	return keys
}

// refVariants returns the normalized reference plus its docker.io/library and
// docker.io short forms, so "alpine" matches "docker.io/library/alpine:latest".
func refVariants(ref string) []string {
	base := normalizeRef(ref)
	if base == "" {
		return nil
	}
	variants := []string{base}
	for _, prefix := range []string{"docker.io/library/", "index.docker.io/library/", "docker.io/", "index.docker.io/"} {
		if strings.HasPrefix(base, prefix) {
			variants = append(variants, strings.TrimPrefix(base, prefix))
		}
	}
	return variants
}
