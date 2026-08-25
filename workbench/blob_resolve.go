package workbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/content"
	"go.entitychurch.org/entity-core-go/ext/localfiles"

	"github.com/fxamacker/cbor/v2"
)

// BlobResolvePattern is the URI pattern under which the workbench's
// cross-peer blob-resolve handler registers. A subscription on a
// remote peer's local/files/{root}/* prefix (with include_payload:
// true) delivers tree-change notifications here; this handler does
// the full notification → blob-fetch → materialize pipeline in a
// single dispatch.
const BlobResolvePattern = "workbench/blob-resolve"

// BlobResolveHandler is the cross-peer materialization step in the
// Stage 3 cross-peer file-sync chain. Per `STAGE-3-DESIGN-RESPONSE.md
// §3` and arch's L10 algorithm-reference framing, this handler:
//
//  1. Unwraps the subscription delivery → tree-change notification.
//  2. Extracts the changed file entity from hctx.Included (delivered
//     because the subscription opted into `include_payload: true`).
//  3. Decodes the FileData → identifies the blob hash + source peer.
//  4. Drives content.EnsureClosure against content.AtPeer(hctx,
//     sourcePeerID) — cap-checked sequencer over system/content:get
//     that drains until the blob's full closure is locally present.
//     §7.4 sender batching + 503 partial-sync retry live inside the
//     SDK helper; this step is one call (was the §7.2 reimpl).
//  5. Dispatches local/files:write content-mode locally to atomically
//     write the file to disk (no bytes traverse the wire — the blob
//     is already in the local content store from step 4).
//
// **Why a single-handler shape (Q2 collapse, same reason as
// notification_ingest):** the notification URI carries the source
// peer ID + relative path; deriving the target tree path from the
// source URI requires string manipulation that continuation
// transforms can't express. Per the G1 idiom + the
// L11 deferral, substrate-resolution-as-a-handler-step is the
// correct shape.
//
// **State:** holds a map of source-prefix → target-prefix mappings
// populated via RegisterMount at sync-setup time. Lookup at
// receive-time matches the notification URI against registered
// source prefixes (longest-prefix match). For the typical case
// where both peers mount the same prefix (e.g.,
// `local/files/sync/`), source and target coincide; the map allows
// asymmetric mount paths if a deployment needs them.
//
// **Capability surface:** the handler operates under its
// internal_scope (declared in Manifest below). The cross-peer
// system/content:get dispatch reuses the standard cross-peer cap
// (the caller's connection cap). The local local/files:write
// dispatch uses the handler's grant on local/files.
type BlobResolveHandler struct {
	mu     sync.RWMutex
	mounts map[string]string
}

// NewBlobResolveHandler returns a new, mountless BlobResolveHandler.
// Use RegisterMount before subscribing to bind source/target prefixes.
func NewBlobResolveHandler() *BlobResolveHandler {
	return &BlobResolveHandler{
		mounts: make(map[string]string),
	}
}

// RegisterMount associates a source prefix on a remote peer with a
// target prefix on the local peer. Both prefixes are normalized to
// end with "/". For the symmetric case (peer A mounts
// local/files/sync/ → peer B mounts local/files/sync/), pass the
// same string for both.
func (h *BlobResolveHandler) RegisterMount(sourcePrefix, targetPrefix string) {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	if !strings.HasSuffix(targetPrefix, "/") {
		targetPrefix += "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mounts[sourcePrefix] = targetPrefix
}

// UnregisterMount removes a source-prefix mapping.
func (h *BlobResolveHandler) UnregisterMount(sourcePrefix string) {
	if !strings.HasSuffix(sourcePrefix, "/") {
		sourcePrefix += "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.mounts, sourcePrefix)
}

func (h *BlobResolveHandler) Name() string { return "workbench-blob-resolve" }

// Manifest declares the handler + its internal scope. The internal
// scope grants the handler authority to write through local/files
// (for materialization) and to fetch through system/content (for the
// cross-peer chunk pull, when targeting the local peer's content
// handler — cross-peer dispatches use the caller's connection cap).
func (h *BlobResolveHandler) Manifest() types.HandlerManifestData {
	return types.HandlerManifestData{
		Pattern: BlobResolvePattern,
		Name:    "workbench-blob-resolve",
		Operations: map[string]types.HandlerOperationSpec{
			"receive": {InputType: "primitive/any"},
		},
		InternalScope: []types.GrantEntry{
			{
				Handlers:   types.CapabilityScope{Include: []string{"local/files"}},
				Operations: types.CapabilityScope{Include: []string{"write", "delete"}},
				Resources:  types.CapabilityScope{Include: []string{"*"}},
			},
			{
				Handlers:   types.CapabilityScope{Include: []string{"system/content"}},
				Operations: types.CapabilityScope{Include: []string{"get"}},
				Resources:  types.CapabilityScope{Include: []string{"system/content"}},
			},
		},
	}
}

// Handle is the receive op: subscription delivery → blob fetch →
// local write. Returns 200 on successful materialization; 503
// blob_pending_sync when the source peer doesn't have the blob yet
// (caller's chain-retry path applies on next subscription event).
func (h *BlobResolveHandler) Handle(ctx context.Context, req *handler.Request) (*handler.Response, error) {
	if req.Operation != "receive" {
		return handler.NewErrorResponse(400, "unknown_operation",
			fmt.Sprintf("blob-resolve does not support operation %q", req.Operation))
	}
	hctx := req.Context
	if hctx == nil {
		return handler.NewErrorResponse(500, "internal_error",
			"blob-resolve requires handler context")
	}
	if hctx.Execute == nil {
		return handler.NewErrorResponse(500, "internal_error",
			"blob-resolve requires hctx.Execute (in-handler dispatch capability)")
	}

	// Unwrap inbox delivery → notification.
	notifEnt := req.Params
	if notifEnt.Type == types.TypeInboxDelivery {
		delivery, err := types.InboxDeliveryDataFromEntity(notifEnt)
		if err != nil {
			return handler.NewErrorResponse(400, "decode_delivery",
				"decode inbox delivery: "+err.Error())
		}
		notifEnt = entity.Entity{Type: types.TypeSubscriptionNotification, Data: delivery.Result}
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

	// Identify source peer + relative path from the notification URI.
	// URI shape: /{sourcePeerID}/{relativePath}
	sourcePeerID, relativeURI := splitPeerIDFromURI(notif.URI)
	if sourcePeerID == "" {
		return handler.NewErrorResponse(400, "invalid_uri",
			"notification URI lacks a source peer-id prefix: "+notif.URI)
	}

	// Find the matching source-prefix mount (longest-match).
	h.mu.RLock()
	var sourcePrefix, targetPrefix string
	for sp, tp := range h.mounts {
		if strings.HasPrefix(relativeURI, sp) && len(sp) > len(sourcePrefix) {
			sourcePrefix, targetPrefix = sp, tp
		}
	}
	h.mu.RUnlock()
	if sourcePrefix == "" {
		return handler.NewErrorResponse(404, "no_mount_for_uri",
			"no registered mount matches "+notif.URI)
	}

	// Deletion branch. The cross-peer source FileData was removed —
	// propagate by dispatching local/files:delete at the matching
	// LOCAL source path. handleDelete (DOMAIN-LOCAL-FILES §4.4) does
	// the fs unlink + tree:remove synchronously.
	//
	// Why explicit dispatch vs. just TreeRemove (which would let
	// DOMAIN-LOCAL-FILES §5.4 reverse_delete unlink the fs file):
	// the localfiles reverseTracker.isRecentlyWritten check
	// (core-go ext/localfiles/reverse.go:100) suppresses reverse-
	// events for paths within a 5s window after a handler write.
	// In normal collaborative editing, deletes shortly after writes
	// fall inside that window and reverse_delete is skipped, leaving
	// the fs file stranded. Explicit dispatch sidesteps the tracker
	// — handleDelete unlinks the file directly. Filed as a core-go
	// finding (reverseTracker check should be inside the write
	// branch only); workaround stays here until that lands.
	//
	// notif.Event values follow EXTENSION-SUBSCRIPTION ("created" /
	// "updated" / "deleted").
	if notif.Event == "deleted" {
		relPath := strings.TrimPrefix(relativeURI, sourcePrefix)
		localSourcePath := sourcePrefix + relPath
		// Only dispatch if a binding actually exists locally — a
		// concurrent watcher Remove may already have cleaned up.
		if hctx.LocationIndex != nil {
			if _, bound := hctx.LocationIndex.Get(localSourcePath); bound {
				_, err := hctx.Execute(ctx, "local/files", "delete", entity.Entity{},
					handler.WithResource(&types.ResourceTarget{Targets: []string{localSourcePath}}))
				if err != nil {
					return handler.NewErrorResponse(500, "delete_dispatch_failed",
						"local/files:delete dispatch: "+err.Error())
				}
			}
		}
		return ackEntity(200, map[string]interface{}{
			"deleted":     true,
			"source_path": localSourcePath,
		})
	}

	// Pull the changed file entity. Per EXTENSION-SUBSCRIPTION v3.14
	// include_payload, the engine attached the entity at notif.Hash
	// to the delivery; it arrives via hctx.Included on this side.
	// Fall back to a local store lookup if include_payload wasn't
	// set (we won't get a payload then, only a hash — and for
	// cross-peer, the entity won't be in the local store yet).
	var fileEnt entity.Entity
	if !notif.Hash.IsZero() {
		if ent, ok := hctx.Included[notif.Hash]; ok {
			fileEnt = ent
		} else if hctx.Store != nil {
			if ent, ok := hctx.Store.Get(notif.Hash); ok {
				fileEnt = ent
			}
		}
	}
	if fileEnt.Type == "" {
		return handler.NewErrorResponse(503, "file_entity_unresolved",
			fmt.Sprintf("file entity for hash %s not in Included or local store — "+
				"subscription needs include_payload: true", notif.Hash.String()))
	}
	if fileEnt.Type != localfiles.TypeFile {
		// Ignore non-file entities under the prefix (e.g., directory
		// listings or future entity types). The mount pattern is
		// fundamentally file-shaped; we don't materialize other types.
		return ackEntity(200, map[string]interface{}{
			"skipped":     true,
			"reason":      "not_a_file_entity",
			"entity_type": fileEnt.Type,
		})
	}
	file, err := localfiles.FileDataFromEntity(fileEnt)
	if err != nil {
		return handler.NewErrorResponse(400, "decode_file_data",
			"decode FileData: "+err.Error())
	}

	// Compute target path now so we can short-circuit on already-current
	// before any cross-peer work.
	relPath := strings.TrimPrefix(relativeURI, sourcePrefix)
	targetTreePath := targetPrefix + relPath

	// F9 idempotency short-circuit (Round 6 workbench-side fix):
	// the bidirectional symmetric topology of case 2 creates a
	// subscription loop where each TreeSet from local/files:write
	// fires a tree change event that the OTHER peer's subscription
	// observes, dispatching ANOTHER blob-resolve that targets the
	// same path with the same content hash. Without this check, the
	// loop runs unbounded (~150 iterations/sec; saturates CPU).
	//
	// Check: does my local tree at targetTreePath already have a
	// file entity bound with the same blob hash? If yes, the
	// materialization is a no-op — skip cross-peer fetch + write
	// dispatch entirely. Subsequent notifications for unchanged
	// content terminate the loop within one round-trip.
	//
	// The deeper architectural fix lives in core-go: hctx.TreeSet
	// (handler.go) should not fire a tree change event when the new
	// entity hash equals the existing entity hash at the same path.
	if hctx.Store != nil {
		if existingHash, ok := tryGetLocalFileBlobHash(hctx, targetTreePath); ok && existingHash == file.Content {
			return ackEntity(200, map[string]interface{}{
				"skipped":      true,
				"reason":       "already_current",
				"target_path":  targetTreePath,
				"blob_hash":    file.Content.String(),
			})
		}
	}

	// Fetch the blob closure cross-peer via system/content:get. Per
	// SDK-EXTENSION-OPERATIONS v0.8 §11 + PROPOSAL-CONTENT-MATERIALIZATION
	// v2 closure-think reframe: content.EnsureClosure is the cap-checked
	// sequencer; content.AtPeer aims dispatch at the source peer while
	// the cap-check still flows through the inner HandlerContext
	// dispatcher. 503 retry semantics + §7.4 sender batching are inside
	// EnsureClosure now (was previously workbench-side defense-in-depth).
	if !file.Content.IsZero() {
		disp := content.AtPeer(hctx, crypto.PeerID(sourcePeerID))
		if err := content.EnsureClosure(ctx, disp, file.Content, "system/content"); err != nil {
			// Map to chain-visible status. 503 stays 503 (caller's chain-
			// retry path on next subscription event); other failures
			// surface as 503 blob_pending_sync too — local-files chain
			// step treats any closure-fetch failure as retry-eligible.
			var se *content.StatusError
			status := uint(503)
			code := "blob_pending_sync"
			if errors.As(err, &se) {
				status = se.Status
				if se.Code != "" {
					code = se.Code
				}
			}
			return handler.NewErrorResponse(status, code,
				fmt.Sprintf("blob closure fetch from %s: %v", sourcePeerID, err))
		}
	}

	// Materialize via local local/files:write content-mode. Dispatcher
	// applies the handler's internal-scope grant on local/files:write.
	// No bytes traverse the wire — the blob is now in the local
	// content store from the fetch above. targetTreePath was computed
	// above for the F9 idempotency check.
	contentHash := file.Content
	writeReq := localfiles.WriteRequestData{
		Content:    &contentHash,
		CreateDirs: true,
	}
	writeReqRaw, err := ecf.Encode(writeReq)
	if err != nil {
		return handler.NewErrorResponse(500, "encode_write_request",
			"encode write request: "+err.Error())
	}
	writeReqEnt, err := entity.NewEntity(localfiles.TypeWriteRequest, cbor.RawMessage(writeReqRaw))
	if err != nil {
		return handler.NewErrorResponse(500, "build_write_request",
			"build write request entity: "+err.Error())
	}
	writeResp, err := hctx.Execute(ctx, "local/files", "write", writeReqEnt,
		handler.WithResource(&types.ResourceTarget{Targets: []string{targetTreePath}}))
	if err != nil {
		return handler.NewErrorResponse(500, "write_dispatch_failed",
			"dispatch local/files:write: "+err.Error())
	}
	if writeResp == nil || writeResp.Status >= 400 {
		status := uint(500)
		if writeResp != nil {
			status = writeResp.Status
		}
		return handler.NewErrorResponse(status, "write_failed",
			fmt.Sprintf("local/files:write returned status %d", status))
	}

	return ackEntity(200, map[string]interface{}{
		"target_path":  targetTreePath,
		"blob_hash":    file.Content.String(),
		"source_peer":  sourcePeerID,
		"size":         file.Size,
	})
}

// tryGetLocalFileBlobHash returns the blob hash of a file entity at
// the given local tree path, if one is bound. Used by the F9
// idempotency check to short-circuit redundant materialization on
// notification bounce-back. Returns (zero, false) on any miss or
// non-file entity; never errors out (idempotency check is best-effort).
func tryGetLocalFileBlobHash(hctx *handler.HandlerContext, treePath string) (hash.Hash, bool) {
	if hctx == nil || hctx.Store == nil {
		return hash.Hash{}, false
	}
	// LocationIndex lookup resolves the qualified path. blob-resolve
	// runs under the local peer's namespace; treePath is bare so we
	// qualify with the local peer-id via PeerContext if available.
	li := hctx.LocationIndex
	if li == nil {
		return hash.Hash{}, false
	}
	qualified := treePath
	if hctx.LocalPeerID != "" {
		qualified = "/" + string(hctx.LocalPeerID) + "/" + treePath
	}
	h, ok := li.Get(qualified)
	if !ok {
		return hash.Hash{}, false
	}
	ent, ok := hctx.Store.Get(h)
	if !ok {
		return hash.Hash{}, false
	}
	if ent.Type != localfiles.TypeFile {
		return hash.Hash{}, false
	}
	file, err := localfiles.FileDataFromEntity(ent)
	if err != nil {
		return hash.Hash{}, false
	}
	return file.Content, true
}

// splitPeerIDFromURI splits "/{peerID}/rest..." into (peerID, rest).
// Returns ("", uri) if the URI is not in qualified form.
func splitPeerIDFromURI(qualified string) (string, string) {
	if !strings.HasPrefix(qualified, "/") {
		return "", qualified
	}
	rest := qualified[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", qualified
	}
	return rest[:i], rest[i+1:]
}

// ackEntity returns a `workbench/blob-resolve/ack` result entity
// summarizing what happened. The subscription engine doesn't consume
// the result, but it shows up in dispatcher traces and is useful for
// tests asserting the chain ran.
func ackEntity(status uint, fields map[string]interface{}) (*handler.Response, error) {
	raw, err := ecf.Encode(fields)
	if err != nil {
		return handler.NewErrorResponse(500, "encode_ack", "encode ack: "+err.Error())
	}
	ent, err := entity.NewEntity("workbench/blob-resolve/ack", cbor.RawMessage(raw))
	if err != nil {
		return handler.NewErrorResponse(500, "build_ack", "build ack entity: "+err.Error())
	}
	return &handler.Response{Status: status, Result: ent}, nil
}
