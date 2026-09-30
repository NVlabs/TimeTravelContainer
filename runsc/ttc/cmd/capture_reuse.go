// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// A branch restored from a shared image, whether rehydrated or migrated,
// keeps that image's pages until it writes them, and the Sentry does not even
// load those it never reads. A capture of the branch in a cluster could share
// only its node's own sealed volatile origins, whose references the node's
// graph counts: it sent every page of the image again, which the Sentry then
// loaded from the shared store, and the node wrote twice and published once
// more. A shared image's objects are counted by the shared ledger instead,
// which knows nothing of the references the node's own objects hold.
//
// So a capture first adopts the image's memory tables and chunks into the
// node's graph, pinned, then claims the image at the shared ledger, and only
// then do its tables refer to them. The claim is the node's until its graph
// refers to nothing the image reaches: until the capture is published, whose
// references the ledger counts from then on, or collected.

// captureClaimPrefix begins the operation of every capture claim.
const captureClaimPrefix = "capture-"

// captureAdoptionTables is how many tables one graph transaction adopts, well
// within a journal record: 128 tables of 15 KiB.
const captureAdoptionTables = 128

// captureImage is a shared image whose pages a capture may share: its origin,
// and its memory roots as a branch records them.
type captureImage struct {
	snapshot uint64
	roots    []string
}

// captureImagesLimit bounds the images one capture adopts.
const captureImagesLimit = 4

// branchCaptureImages returns the images a capture of branchID in info may
// share pages with: the one it was restored from, then those of the family's
// other branches and the images the family's memory still loads from. A fork
// of a rehydrated branch, which has no image of its own, holds its source's
// pages.
func branchCaptureImages(info ttcbundle.ContainerInfo, branchID string) []captureImage {
	var images []captureImage
	add := func(snapshot uint64, roots []string) {
		if snapshot == 0 || len(roots) == 0 || len(images) == captureImagesLimit ||
			slices.ContainsFunc(images, func(image captureImage) bool { return image.snapshot == snapshot }) {
			return
		}
		images = append(images, captureImage{snapshot: snapshot, roots: slices.Clone(roots)})
	}
	for _, branch := range info.Branches {
		if branch.ID == branchID {
			add(branch.SnapshotID, branch.MemoryRootObjects)
		}
	}
	for _, branch := range info.Branches {
		add(branch.SnapshotID, branch.MemoryRootObjects)
	}
	for _, backing := range info.RuntimeMemoryBackings {
		add(backing.SnapshotID, backing.MemoryRootObjects)
	}
	return images
}

// captureReuse returns what a capture into prefix of the node's cluster graph
// may share: the pages of the node's own sealed volatile origins, and of each
// of images that it adopts and claims. release deletes the pins that keep what
// it adopted until the capture's tables refer to what they need of it. An
// image that cannot be claimed is not shared: its pages are captured anew.
func (b *runtimeBackend) captureReuse(prefix string, images []captureImage) (ttcsnapshot.MemoryReuse, func()) {
	reuse := ttcsnapshot.MemoryReuse{States: []ttcsnapshot.SnapshotState{ttcsnapshot.SnapshotSealedVolatile}}
	var pins []string
	for index, image := range images {
		origins, adopted, err := b.adoptCaptureImage(fmt.Sprintf("%s/adopted/%d/held", prefix, index), image)
		if err != nil {
			if b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: capturing all of the branch's memory, as the image it was restored from could not be shared: %v\n", err)
			}
			continue
		}
		reuse.Origins = append(reuse.Origins, origins...)
		pins = append(pins, adopted...)
	}
	return reuse, func() {
		if len(pins) != 0 {
			if err := ttcchunk.ReleaseHeldPagePins(b.snapshotGraph, pins); err != nil && b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttc: warning: releasing the shared image a capture adopted: %v\n", err)
			}
		}
	}
}

// adoptCaptureImage adopts the memory tables and chunks of image, a shared
// one, into the node's graph, each table pinned under pinPrefix, and claims
// the image at the shared ledger. It returns the origins the image reaches and
// the pins. An image still volatile is not adopted: this node's own is shared
// as it is, and another node's is not the ledger's to claim.
func (b *runtimeBackend) adoptCaptureImage(pinPrefix string, image captureImage) (_ []ttcsnapshot.SnapshotID, pins []string, retErr error) {
	if b.snapshotGraph == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.sharedReferences == nil {
		return nil, nil, errors.New("shared image claims are unavailable")
	}
	_, volatile, err := b.distributedSource.authority.SnapshotProvider(image.snapshot)
	if err != nil || volatile {
		return nil, nil, err
	}
	roots := make([]ttcsnapshot.ObjectID, len(image.roots))
	for index, text := range image.roots {
		if roots[index], err = ttcsnapshot.ParseObjectID(text); err != nil {
			return nil, nil, err
		}
	}
	catalog, err := b.immutableSnapshotCatalog(context.Background(), roots, "")
	if err != nil {
		return nil, nil, err
	}
	tables, chunks, origins, err := captureImageTables(catalog, roots)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if retErr != nil && len(pins) != 0 {
			_ = ttcchunk.ReleaseHeldPagePins(b.snapshotGraph, pins)
		}
	}()
	// The pins come first: a claim is kept while the graph refers to what
	// it claims, which the pins do until the capture's tables do.
	for begin := 0; begin < len(tables); begin += captureAdoptionTables {
		batch := tables[begin:min(begin+captureAdoptionTables, len(tables))]
		var needed []ttcsnapshot.Object
		for _, table := range batch {
			for _, edge := range table.Children {
				needed = append(needed, chunks[edge.Child])
			}
		}
		adopted, err := b.snapshotGraph.AdoptSharedMemory(pinPrefix, batch, needed)
		if errors.Is(err, ttcsnapshot.ErrObjectReclaiming) {
			// The graph let go of the image, as a publication of it
			// does, and has not collected it yet.
			for {
				if collected, err := b.snapshotGraph.Collect(4096); err != nil || collected == 0 {
					break
				}
			}
			adopted, err = b.snapshotGraph.AdoptSharedMemory(pinPrefix, batch, needed)
		}
		pins = append(pins, adopted...)
		if err != nil {
			return nil, pins, fmt.Errorf("adopting its memory tables: %w", err)
		}
	}
	if err := b.claimCaptureImage(image, origins); err != nil {
		return nil, pins, fmt.Errorf("claiming it at the shared ledger: %w", err)
	}
	return origins, pins, nil
}

// captureImageTables returns the memory tables of the trees at roots, read
// from catalog, the chunks they refer to, and the origins of both. Trees
// written before tables are not shared.
func captureImageTables(catalog *ttcsnapshot.ImmutableCatalog, roots []ttcsnapshot.ObjectID) ([]ttcsnapshot.Object, map[ttcsnapshot.ObjectID]ttcsnapshot.Object, []ttcsnapshot.SnapshotID, error) {
	var tables []ttcsnapshot.Object
	chunks := make(map[ttcsnapshot.ObjectID]ttcsnapshot.Object)
	origins := make(map[ttcsnapshot.SnapshotID]struct{})
	seen := make(map[ttcsnapshot.ObjectID]struct{})
	pending := slices.Clone(roots)
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		object, ok := catalog.Object(id)
		if !ok {
			return nil, nil, nil, fmt.Errorf("the image lacks object %s", id)
		}
		switch id.Kind {
		case ttcsnapshot.ObjectKindMemoryNode:
			for _, edge := range object.Children {
				pending = append(pending, edge.Child)
			}
		case ttcsnapshot.ObjectKindMemoryTable:
			tables = append(tables, object)
			origins[id.Snapshot] = struct{}{}
			for _, edge := range object.Children {
				chunk, ok := catalog.Object(edge.Child)
				if !ok {
					return nil, nil, nil, fmt.Errorf("the image lacks chunk %s", edge.Child)
				}
				chunks[edge.Child] = chunk
				origins[edge.Child.Snapshot] = struct{}{}
			}
		}
	}
	sorted := make([]ttcsnapshot.SnapshotID, 0, len(origins))
	for origin := range origins {
		sorted = append(sorted, origin)
	}
	slices.Sort(sorted)
	return tables, chunks, sorted, nil
}

// claimCaptureImage records and retains, at the shared ledger, the node's
// claim on image's roots, which reach origins. It tries once: the image is
// not shared if the coordinator is not there to count it.
func (b *runtimeBackend) claimCaptureImage(image captureImage, origins []ttcsnapshot.SnapshotID) error {
	groups, err := groupGraphRootBindings(image.roots)
	if err != nil {
		return err
	}
	if len(groups) != 1 {
		return fmt.Errorf("its roots are in %d origins", len(groups))
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	operation := fmt.Sprintf("%s%016x-%s", captureClaimPrefix, image.snapshot, id)
	path, err := graphReferenceIntentPath(b.activeDataRoot(), operation)
	if err != nil {
		return err
	}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: operation, Operation: operation,
		Purpose: graphReferencePurposeCapture, State: "retaining", CreatedAt: time.Now().UTC(),
	}
	for origin, roots := range groups {
		intent.Groups = append(intent.Groups, graphReferenceGroup{Origin: uint64(origin), Roots: roots})
	}
	for _, origin := range origins {
		intent.Origins = append(intent.Origins, uint64(origin))
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		return err
	}
	b.sharedReferences.claimMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = advanceGraphReferenceRetain(path, &intent, func(group graphReferenceGroup) error {
		return b.mutateGraphReferenceGroup(ctx, intent, group, true)
	})
	cancel()
	b.sharedReferences.claimMu.Unlock()
	if err != nil {
		// The next reconciliation releases it: the capture does not wait for
		// a coordinator it could not reach.
		return errors.Join(err, abandonCaptureClaim(path, intent))
	}
	return nil
}

// abandonCaptureClaim marks the capture claim at path for release. What was
// never retained is released too, as a retain may have reached the
// coordinator though its answer did not, and a release of what the ledger does
// not hold does nothing.
func abandonCaptureClaim(path string, intent graphReferenceIntent) error {
	intent.State = "releasing"
	for index := range intent.Groups {
		intent.Groups[index].Retained = true
	}
	return writeDurableJSON(path, &intent)
}

// reconcileCaptureClaim releases the capture claim at path once the node's
// memory graph refers to nothing of the origins its image reaches, checking
// one still needed ever less often, up to every two minutes.
func (b *runtimeBackend) reconcileCaptureClaim(path string, intent graphReferenceIntent) error {
	if intent.State == "releasing" {
		return b.releaseDistributedGraphReferences(intent.Operation)
	}
	now := time.Now()
	check := providerCheck{}
	if value, ok := b.distributedSource.providerChecks.Load(intent.Operation); ok {
		check = value.(providerCheck)
		if now.Before(check.next) {
			return nil
		}
	}
	if b.snapshotGraph != nil {
		for _, origin := range intent.Origins {
			if references, edges := b.snapshotGraph.OriginReferences(ttcsnapshot.SnapshotID(origin)); providerReferencesNeeded(references, edges) {
				if intent.State == "retaining" {
					// Its capture stopped with the claim half made.
					return b.resumeGraphReferenceRetain(path, &intent)
				}
				check.interval = min(max(2*check.interval, 5*time.Second), 2*time.Minute)
				check.next = now.Add(check.interval)
				b.distributedSource.providerChecks.Store(intent.Operation, check)
				return nil
			}
		}
	}
	b.distributedSource.providerChecks.Delete(intent.Operation)
	if err := abandonCaptureClaim(path, intent); err != nil {
		return err
	}
	return b.releaseDistributedGraphReferences(intent.Operation)
}
