package workbench

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/content"
	"go.entitychurch.org/entity-core-go/ext/content/chunker"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"entity-workbench-go/entitysdk"
)

// SweepResult reports the outcome of a single mount sweep.
type SweepResult struct {
	RootName       string
	FilesystemRoot string

	// SourceRemoved lists tree paths under local/files/{root}/ that
	// were unbound because their filesystem counterpart no longer
	// exists. Each entry is the qualified tree path (peer-id stripped).
	SourceRemoved []string

	// TargetRemoved lists tree paths under the mount's target prefix
	// (e.g. archives/notes/) whose downstream doc/* entity was
	// removed in cascade. Empty when no target prefix is registered
	// in the notification-ingest mount map.
	TargetRemoved []string

	// SourceAdded lists tree paths that were bound because a file
	// exists on disk but no FileData was present in the tree (peer
	// was offline when the file appeared and the initial-scan
	// already ran for the live root). v1: counts only; actual
	// re-binding is left to a watcher rescan in a follow-on commit.
	SourceAdded []string

	// FilesystemFiles is the count of files seen on disk under the
	// mount root (excluding directories). For operator reporting.
	FilesystemFiles int

	// SourcePresent is the count of tree paths seen under the source
	// prefix before sweep. For operator reporting.
	SourcePresent int
}

// SweepMount reconciles a single mount's tree state against its
// filesystem state. Removes tree entries for files that no longer
// exist on disk; cascades through the notification-ingest mount
// registry to the target prefix if one is configured.
//
// Safe by construction (Phase E v2 §7.2):
//   - Scoped exclusively to local/files/{root}/ and the registered
//     target prefix. No content store mutation. No effect on other
//     extensions' tree namespaces.
//   - Uses the actual filesystem as authority — only removes tree
//     entries whose path is provably absent from disk. Never
//     removes entries for files that exist (even if they're
//     stale or modified — content reconciliation is the watcher's job).
//
// This is a cold-recovery tool. The live deletion path (watcher
// + notification-ingest "deleted" event + blob-resolve "deleted"
// event) handles deletes as they happen. Sweep catches drift that
// accumulated when the watcher was offline.
//
// The notification-ingest registry is consulted to derive the
// target prefix; if `ingest` is nil or the mount isn't registered,
// only the source prefix is swept.
func SweepMount(ap *entitysdk.AppPeer, ingest *NotificationIngestHandler, rootName string) (SweepResult, error) {
	res := SweepResult{RootName: rootName}

	lfHandler := ap.LocalFilesHandler()
	if lfHandler == nil {
		return res, errors.New("local/files handler not wired on this peer")
	}
	root, ok := lookupRoot(ap, rootName)
	if !ok {
		return res, fmt.Errorf("no mount with root name %q", rootName)
	}
	res.FilesystemRoot = root.FilesystemRoot
	sourcePrefix := root.Prefix
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}

	// Build the set of relative paths actually on disk.
	fsRel := map[string]struct{}{}
	walkErr := filepath.WalkDir(root.FilesystemRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root.FilesystemRoot, path)
		if err != nil {
			return err
		}
		fsRel[filepath.ToSlash(rel)] = struct{}{}
		res.FilesystemFiles++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return res, fmt.Errorf("walk fs root %s: %w", root.FilesystemRoot, walkErr)
	}

	// Walk the tree's local-peer slice under the source prefix.
	store := ap.Store()
	sourceEntries := store.List(sourcePrefix)
	res.SourcePresent = len(sourceEntries)

	// Lookup the target prefix from the notification-ingest registry
	// (if wired). Cascade removals follow the same source→target
	// mapping the live deletion path uses.
	targetPrefix := ""
	if ingest != nil {
		targetPrefix = ingest.LookupMount(sourcePrefix)
	}

	for _, e := range sourceEntries {
		// e.Path is qualified: /{peerID}/local/files/{root}/{rel}
		relInTree := stripPeerIDPath(e.Path)
		if !strings.HasPrefix(relInTree, sourcePrefix) {
			continue
		}
		rel := strings.TrimPrefix(relInTree, sourcePrefix)
		if rel == "" {
			continue
		}
		if _, onDisk := fsRel[rel]; onDisk {
			continue
		}
		// File is in tree but not on disk — remove the binding.
		// Use Level 0 Store.Remove since this is a sweep dispatched
		// by an operator; we're not going through cap-checked dispatch.
		// (The mount itself is the operator's deliberate authorization
		// to manage this namespace.)
		if store.Remove(relInTree) {
			res.SourceRemoved = append(res.SourceRemoved, rel)
			if targetPrefix != "" {
				targetPath := targetPrefix + rel
				if store.Remove(targetPath) {
					res.TargetRemoved = append(res.TargetRemoved, rel)
				}
			}
		}
	}

	sort.Strings(res.SourceRemoved)
	sort.Strings(res.TargetRemoved)
	return res, nil
}

// IngestMissingFiles is the second half of sweep: re-ingest files
// that exist on disk but have no source binding in the tree. Useful
// when a watcher missed a creation (e.g. peer was offline at the time)
// or when the operator wants to re-baseline.
//
// Reads each missing file's bytes, chunks via FastCDC + IngestBlob,
// writes a FileData entity at the source path. Does NOT trigger the
// notification-ingest cascade automatically — operator should rely
// on the subscription firing on the new FileData write to drive the
// downstream binding.
//
// This is intentionally additive: SweepMount handles removals,
// IngestMissingFiles handles additions. Operators typically call
// both via the shell verb's --add flag.
func IngestMissingFiles(ap *entitysdk.AppPeer, rootName string) (added int, errs []string, retErr error) {
	root, ok := lookupRoot(ap, rootName)
	if !ok {
		return 0, nil, fmt.Errorf("no mount with root name %q", rootName)
	}
	sourcePrefix := root.Prefix
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}

	store := ap.Store()
	cs := store.ContentStore()
	idHash := ap.IdentityHash()

	walkErr := filepath.WalkDir(root.FilesystemRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", path, walkErr))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root.FilesystemRoot, path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: rel: %s", path, err))
			return nil
		}
		sourcePath := sourcePrefix + filepath.ToSlash(rel)
		if store.Has(sourcePath) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: read: %s", path, err))
			return nil
		}
		ranges := chunker.ChunkFastCDC(body, types.DefaultChunkSize)
		blobHash, err := content.IngestBlob(body, ranges, types.ChunkingFastCDC, types.DefaultChunkSize, cs)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: ingest blob: %s", path, err))
			return nil
		}
		info, err := d.Info()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: stat: %s", path, err))
			return nil
		}
		modMillis := uint64(info.ModTime().UnixMilli())
		file := localfiles.FileData{
			Path:       filepath.ToSlash(rel),
			Size:       uint64(len(body)),
			ModifiedAt: &modMillis,
			Content:    blobHash,
		}
		_ = idHash // not needed for L0 put; future cap-checked dispatch would use it
		if _, err := store.Put(sourcePath, localfiles.TypeFile, file); err != nil {
			errs = append(errs, fmt.Sprintf("%s: store.put: %s", path, err))
			return nil
		}
		added++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return added, errs, fmt.Errorf("walk fs root %s: %w", root.FilesystemRoot, walkErr)
	}
	return added, errs, nil
}

// LookupMount returns the target prefix registered for the given
// source prefix, or "" if no mount matches. Exposed for sweep so it
// can cascade removals through the same mapping the live deletion
// path uses.
func (h *NotificationIngestHandler) LookupMount(sourcePrefix string) string {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.mounts[sourcePrefix].target
}

// lookupRoot reads the localfiles handler's root config for rootName.
func lookupRoot(ap *entitysdk.AppPeer, rootName string) (localfiles.RootConfigData, bool) {
	configPath := "system/config/local/files/" + rootName
	ent, ok := ap.Store().Get(configPath)
	if !ok {
		return localfiles.RootConfigData{}, false
	}
	cfg, err := localfiles.RootConfigDataFromEntity(ent)
	if err != nil {
		return localfiles.RootConfigData{}, false
	}
	return cfg, true
}

// stripPeerIDPath drops the leading `/{peer-id}/` segment from a
// fully-qualified tree path; returns the input unchanged if there's
// no leading peer-id.
func stripPeerIDPath(qualified string) string {
	if !strings.HasPrefix(qualified, "/") {
		return qualified
	}
	rest := qualified[1:]
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return rest
	}
	return rest[idx+1:]
}
