package workbench

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"github.com/fxamacker/cbor/v2"
)

const NotificationIngestPattern = "workbench/ingest-from-notification"

// NotificationIngestPatternDoc is the URI prefix where the workbench's
// notification-ingest handler registers. Phase E mount verbs
// subscribe their tree-event notifications to this URI; the handler
// does the full notification → FileData read → transform → tree
// bind pipeline in one step.
//
// **Why this exists.** The Phase E ingest chain as originally
// designed is a 3-step continuation chain:
//
//	subscription → local/files:read → workbench/ingest-transform → system/tree:put
//
// **Reckoning.** The original docstring claimed this
// handler existed because `resource_extract` "doesn't exist in
// today's continuation schema." That claim was wrong:
// EXTENSION-CONTINUATION v1.7+ ships `resource_extract` /
// `target_extract` / `operation_extract`, and entity-core-go has
// them fully wired (`core/types/continuation.go:30-32`,
// `ext/continuation/handler.go:377-379`).
//
// **What survives the reckoning.** Even with `resource_extract`
// available, decomposing this into the 3-step chain is harder than
// expected for two specific reasons:
//
//  1. **URI normalization.** The subscription notification's URI
//     is qualified (`/{peerID}/local/files/{root}/{relpath}`).
//     `system/tree:get` wants an unqualified tree path.
//     `resource_extract` is dotted-path navigation, not string
//     manipulation — it can't strip the `/{peerID}/` prefix from
//     a qualified URI.
//  2. **Target path computation.** Step 3's `system/tree:put`
//     needs target = `{target_prefix} + {relpath_within_source}`.
//     The target_prefix is per-mount (static at install); the
//     relpath has to be derived from the source URI. Computing
//     that requires either a chain step that handles string
//     transforms (which continuations don't), or threading both
//     prefixes through `Params` and into the transform handler's
//     output.
//
// So the single-handler shape IS load-bearing here — not because
// `resource_extract` is missing, but because chain primitives can't
// express URI normalization without an extra "string-transform"
// step or a workbench-side helper.
//
// **Feedback for T1.4** (continuation programming guide): the
// guide should call out URI-normalization-in-chains as a known
// limitation, with the recommendation "collapse to a single
// handler when the source/target shapes need string-level
// reshaping, not just field extraction."
//
// **Capability surface.** The handler uses its internal scope grant
// for system/tree:get + system/tree:put on the configured prefixes.
// The user-facing capability for invoking this handler still passes
// through the chain dispatch_capability that the mount verb mints —
// nothing about Q2 weakens the capability guarantees, which is the
// load-bearing security property. (The user's point: "the
// Capability Grant should be the limiting factor, not transforms."
// Confirmed — transforms aren't a security boundary, and we
// shouldn't be designing them as if they were.)
//
// **State.** The handler holds a map of source-prefix →
// target-prefix mappings, populated via `RegisterMount` at mount
// time. Lookup at receive time matches the notification's URI
// against registered source prefixes (longest-prefix wins). For a
// single-process peer with a handful of mounts this is fine. A
// production-scale variant would persist the mapping in the tree
// and rebuild on startup — the existing `system/config/local/files/*`
// namespace already has the data; we'd just need to load it.
type NotificationIngestHandler struct {
	mu       sync.RWMutex
	mounts   map[string]mountState // source prefix → mount config (target + dynamic filter)
	lfHelper *localfiles.Handler
}

// mountState bundles a registered mount's target prefix with any
// runtime-adjusted Include/Exclude glob filters (Phase E v2 §7.3
// workbench-application layer; localfiles' own watcher filter is
// orthogonal and persists until the next peer restart).
type mountState struct {
	target  string
	include []string
	exclude []string
}

// NewNotificationIngestHandler returns a new handler. The
// localfiles handler ref is captured so the ingest handler can
// share its root-mapping convention (path normalization,
// case-folding, etc.) — today we don't actually use it for
// resolution since the FileData is already in the tree, but the
// reference is held in case future iterations need fs path
// validation.
func NewNotificationIngestHandler(lf *localfiles.Handler) *NotificationIngestHandler {
	return &NotificationIngestHandler{
		mounts:   make(map[string]mountState),
		lfHelper: lf,
	}
}

// RegisterMount associates a source prefix in the local-files
// namespace with a target prefix in the workbench's revision-
// tracked namespace. Called by the mount verb after AddRoot. Both
// prefixes are normalized to end with "/". Initial filters are
// empty (all files pass); use SetMountFilters to adjust at runtime.
func (h *NotificationIngestHandler) RegisterMount(sourcePrefix, targetPrefix string) {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	if !strings.HasSuffix(targetPrefix, "/") {
		targetPrefix += "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Preserve any existing filters on the mount; RegisterMount is
	// called from both fresh-mount and restart-restore paths.
	prev := h.mounts[sourcePrefix]
	h.mounts[sourcePrefix] = mountState{
		target:  targetPrefix,
		include: prev.include,
		exclude: prev.exclude,
	}
}

// SetMountFilters replaces the workbench-application filter for a
// registered mount. Patterns use filepath.Match glob syntax (matched
// against the basename, mirroring localfiles' own filter semantics).
// `include` empty = pass everything; non-empty = at least one match
// required. `exclude` always wins. Returns false if no mount is
// registered for the source prefix.
//
// Note (Phase E v2 §7.3): localfiles' own watcher filter is NOT
// reconfigured by this call — the watcher will still emit FileData
// entities for non-matching files. The workbench filter suppresses
// only the workbench-owned doc/markdown-file cascade at the target
// prefix. Operators tightening filters should run `mount sweep` to
// clean up source-prefix residue. Full runtime reconfiguration
// requires the core-go UpdateRootFilters API.
func (h *NotificationIngestHandler) SetMountFilters(sourcePrefix string, include, exclude []string) bool {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ms, ok := h.mounts[sourcePrefix]
	if !ok {
		return false
	}
	ms.include = append([]string(nil), include...)
	ms.exclude = append([]string(nil), exclude...)
	h.mounts[sourcePrefix] = ms
	return true
}

// MountFilters returns the current filter for a mount, or empty
// slices if none. For operator inspection / shell display.
func (h *NotificationIngestHandler) MountFilters(sourcePrefix string) (include, exclude []string, ok bool) {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	ms, ok := h.mounts[sourcePrefix]
	if !ok {
		return nil, nil, false
	}
	return append([]string(nil), ms.include...), append([]string(nil), ms.exclude...), true
}

// UnregisterMount removes a source-prefix mapping. Used by unmount.
func (h *NotificationIngestHandler) UnregisterMount(sourcePrefix string) {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.mounts, sourcePrefix)
}

func (h *NotificationIngestHandler) Name() string { return "workbench-notification-ingest" }

// Manifest declares the handler + its internal scope. The internal
// scope grants the handler authority to read entities under the
// local-files namespace and bind doc/markdown-file entities at the
// per-mount target prefixes. The grant scope is broad
// (`local/files/*` for reads, `*` for puts) because the mount set
// is dynamic — narrowing happens via the workbench's mount-verb
// cap minting, not via the handler's internal scope.
func (h *NotificationIngestHandler) Manifest() types.HandlerManifestData {
	return types.HandlerManifestData{
		Pattern: NotificationIngestPattern,
		Name:    "workbench-notification-ingest",
		Operations: map[string]types.HandlerOperationSpec{
			"receive": {InputType: "primitive/any"},
		},
		InternalScope: []types.GrantEntry{
			{
				Handlers:   types.CapabilityScope{Include: []string{"system/tree"}},
				Operations: types.CapabilityScope{Include: []string{"get", "put"}},
				Resources:  types.CapabilityScope{Include: []string{"*"}},
			},
		},
	}
}

// Handle accepts subscription-delivered notifications and runs the
// ingest pipeline. Operation MUST be "receive"; other ops 400.
func (h *NotificationIngestHandler) Handle(_ context.Context, req *handler.Request) (*handler.Response, error) {
	if req.Operation != "receive" {
		return handler.NewErrorResponse(400, "unknown_operation",
			fmt.Sprintf("notification-ingest does not support operation %q", req.Operation))
	}
	hctx := req.Context
	if hctx == nil || hctx.Store == nil || hctx.LocationIndex == nil {
		return handler.NewErrorResponse(500, "internal_error",
			"notification-ingest requires store + location index")
	}

	// The subscription engine delivers a `system/subscription/notification`
	// entity wrapped in a `system/inbox/delivery`. Unwrap to
	// get the notification.
	notifEnt := req.Params
	if notifEnt.Type == types.TypeInboxDelivery {
		delivery, err := types.InboxDeliveryDataFromEntity(notifEnt)
		if err != nil {
			return handler.NewErrorResponse(400, "decode_delivery",
				"decode inbox delivery: "+err.Error())
		}
		// The delivery's Result is the original notification entity's data;
		// the wrapping entity type isn't preserved across the delivery wrap,
		// so reconstruct from the raw data.
		inner := entity.Entity{Type: types.TypeSubscriptionNotification, Data: delivery.Result}
		notifEnt = inner
	}
	if notifEnt.Type != types.TypeSubscriptionNotification {
		return handler.NewErrorResponse(400, "wrong_input_type",
			"expected "+types.TypeSubscriptionNotification+", got "+notifEnt.Type)
	}
	notif, err := types.SubscriptionNotificationDataFromEntity(notifEnt)
	if err != nil {
		return handler.NewErrorResponse(400, "decode_notification",
			"decode notification: "+err.Error())
	}

	// notif.URI is the qualified path that changed:
	//   /{peerID}/local/files/{root}/{relpath}
	// Strip the peer-id prefix so we can match against our mounts'
	// source prefix (which is relative — `local/files/{root}/`).
	relativeURI := stripPeerIDPrefix(notif.URI)

	// Find the matching mount (longest source-prefix match).
	h.mu.RLock()
	var sourcePrefix, targetPrefix string
	var include, exclude []string
	for sp, ms := range h.mounts {
		if strings.HasPrefix(relativeURI, sp) && len(sp) > len(sourcePrefix) {
			sourcePrefix, targetPrefix = sp, ms.target
			include, exclude = ms.include, ms.exclude
		}
	}
	h.mu.RUnlock()
	if sourcePrefix == "" {
		return handler.NewErrorResponse(404, "no_mount_for_uri",
			"no registered mount matches "+notif.URI)
	}
	relPath := strings.TrimPrefix(relativeURI, sourcePrefix)
	targetPath := targetPrefix + relPath

	// Phase E v2 §7.3 — workbench-application dynamic filter. Skip
	// non-matching files (relpath basename matched against the
	// mount's runtime include/exclude). Note this only suppresses
	// the doc/markdown-file cascade; the localfiles watcher still
	// produces FileData entities at the source prefix (full runtime
	// reconfiguration needs the core-go UpdateRootFilters API).
	if !passesMountFilter(relPath, include, exclude) {
		resultRaw, _ := ecf.Encode(map[string]interface{}{
			"skipped": true,
			"reason":  "filtered_out",
			"path":    relPath,
		})
		resultEnt, _ := entity.NewEntity("workbench/ingest-from-notification/result", cbor.RawMessage(resultRaw))
		// On a filter change, files already bound at the target
		// stay bound until the operator runs `mount sweep <root>`
		// (sweep cross-references against the live fs filter set).
		return &handler.Response{Status: 200, Result: resultEnt}, nil
	}

	// Deletion branch. Workbench-application cascade: when the source
	// FileData is removed (DOMAIN-LOCAL-FILES §5.2/§5.4 reverse_delete
	// or watcher fs-unlink TreeRemove), remove the workbench-owned
	// doc/markdown-file at archives/notes/. The target namespace is
	// workbench-internal — the cascade is an application convention,
	// not a spec'd behavior. notif.Event values follow EXTENSION-
	// SUBSCRIPTION ("created" / "updated" / "deleted").
	if notif.Event == "deleted" {
		hctx.TreeRemove(targetPath, "receive")
		resultRaw, _ := ecf.Encode(map[string]interface{}{
			"deleted":     true,
			"target_path": targetPath,
		})
		resultEnt, _ := entity.NewEntity("workbench/ingest-from-notification/result", cbor.RawMessage(resultRaw))
		return &handler.Response{Status: 200, Result: resultEnt}, nil
	}

	// Look up the FileData entity at the source path. The localfiles
	// watcher already wrote it there — we read directly from the
	// tree, no need to dispatch local/files:read.
	hashBound, ok := hctx.LocationIndex.Get(relativeURI)
	if !ok {
		return handler.NewErrorResponse(404, "source_not_bound",
			"FileData not bound at "+relativeURI+" (watcher may have removed it)")
	}
	fileEnt, ok := hctx.Store.Get(hashBound)
	if !ok {
		return handler.NewErrorResponse(500, "store_get_failed",
			"FileData entity not in store for hash "+hashBound.String())
	}
	if fileEnt.Type != localfiles.TypeFile {
		return handler.NewErrorResponse(409, "wrong_source_type",
			"source entity at "+relativeURI+" is "+fileEnt.Type+", expected "+localfiles.TypeFile)
	}
	file, err := localfiles.FileDataFromEntity(fileEnt)
	if err != nil {
		return handler.NewErrorResponse(500, "decode_filedata",
			"decode FileData: "+err.Error())
	}

	// Type discrimination by extension. Only markdown files become
	// doc/markdown-file entities today; everything else is admitted
	// into the FileData layer (the watcher already wrote it) but no
	// typed doc/* entity is produced and no tree:put happens. The
	// long-term direction is per-extension type registry
	// (markdown → doc/markdown-file, code → doc/code-file, text →
	// doc/text-file, ...) backed by a type-aware viewer; until then
	// we declare the markdown POC explicitly here.
	if !isMarkdownPath(file.Path) {
		resultRaw, _ := ecf.Encode(map[string]interface{}{
			"skipped":     true,
			"reason":      "type_not_handled",
			"source_path": file.Path,
			"source_uri":  relativeURI,
		})
		resultEnt, _ := entity.NewEntity("workbench/ingest-from-notification/result", cbor.RawMessage(resultRaw))
		return &handler.Response{Status: 200, Result: resultEnt}, nil
	}

	// Build the doc/markdown-file entity. Pass file.Content through as
	// a hash ref into system/content/blob (DOMAIN-LOCAL-FILES v1.3 §2.1
	// shape); we only peek the first chunk for title extraction, so
	// arbitrarily large files round-trip without in-memory materialization.
	// Missing blob is a partial-sync condition; surface 503 so the
	// subscription chain can retry on the next sync event.
	firstChunk, blobPresent, err := LoadMarkdownFirstChunk(hctx.Store, file.Content)
	if err != nil {
		return handler.NewErrorResponse(500, "first_chunk_failed",
			"load first chunk for "+file.Content.String()+": "+err.Error())
	}
	if !blobPresent {
		// L12 canonical name (Amendment 2 of v1.3). See parallel comment
		// in workbench/ingest_transform.go.
		return handler.NewErrorResponse(503, "blob_pending_sync",
			"blob "+file.Content.String()+" not yet in local content store")
	}

	title := extractFirstHeading(string(firstChunk))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))
	}
	md := MarkdownFileData{
		Path:    file.Path,
		Title:   title,
		Content: file.Content,
		Size:    int64(file.Size),
	}
	mdEnt, err := md.ToEntity()
	if err != nil {
		return handler.NewErrorResponse(500, "build_entity",
			"build doc/markdown-file: "+err.Error())
	}

	// Compute the target tree path and bind. targetPath was already
	// computed at the top of Handle (shared with the deletion branch).
	mdHash, err := hctx.Store.Put(mdEnt)
	if err != nil {
		return handler.NewErrorResponse(500, "store_put",
			"persist doc/markdown-file: "+err.Error())
	}
	hctx.TreeSet(targetPath, mdHash, "receive")

	// Result entity is just an acknowledgement — the subscription
	// engine doesn't consume it, but it shows up in traces.
	resultRaw, _ := ecf.Encode(map[string]interface{}{
		"target_path":  targetPath,
		"content_hash": mdHash.Bytes(),
	})
	resultEnt, _ := entity.NewEntity("workbench/ingest-from-notification/result", cbor.RawMessage(resultRaw))
	return &handler.Response{Status: 200, Result: resultEnt}, nil
}

// isMarkdownPath returns true when the path's extension marks it as
// markdown. Kept narrow and explicit so the v1 POC has zero ambiguity
// about what gets a typed entity; future generic-ingest work
// generalizes this into a type-registry lookup.
func isMarkdownPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// passesMountFilter applies the runtime include/exclude filter (§7.3)
// to a relpath. Patterns are matched against the basename via
// filepath.Match (same semantics as localfiles' own filter at
// config.go:135-148). Empty include = no positive filter; non-empty
// include requires at least one match. Empty exclude = no negative
// filter; any exclude match rejects.
func passesMountFilter(relPath string, include, exclude []string) bool {
	name := filepath.Base(relPath)
	for _, p := range exclude {
		if matched, _ := filepath.Match(p, name); matched {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, p := range include {
		if matched, _ := filepath.Match(p, name); matched {
			return true
		}
	}
	return false
}

// stripPeerIDPrefix removes the leading `/{peer-id}/` from a
// qualified path. Returns the input unchanged if there's no leading
// slash or no second slash. Mirror of what the namespace machinery
// does internally — duplicated here because we don't have a public
// helper at this layer.
func stripPeerIDPrefix(qualified string) string {
	if !strings.HasPrefix(qualified, "/") {
		return qualified
	}
	rest := qualified[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return qualified
	}
	return rest[i+1:]
}
