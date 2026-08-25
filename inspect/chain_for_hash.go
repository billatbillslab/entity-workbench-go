package inspect

// Reverse causality — "given an entity hash, what chain produced it?"
//
// Substrate-honest scope:
//
//  1. Chain-error markers — path is system/runtime/chain-errors/
//     {kind}/{chain_id}/{step}/{reason}/{marker_hash}. The terminal
//     {marker_hash} segment IS the marker entity's content hash; the
//     {chain_id} segment is the chain. Direct path-derived attribution.
//
//  2. Suspended continuations — system/continuation/suspended type
//     entities carry chain_id in their body per CONTINUATION §2.4.
//     Direct body-derived attribution.
//
// Other entity types are NOT covered without a substrate-side change
// in core-go: forward/join continuations don't carry chain_id in their
// bodies (it flows through dispatch context), and entities bound as
// chain outputs at application paths have no chain_id metadata
// substrate-side. The right fix is for DispatchEvent + ContentStoreEvent
// to carry ChainID + ParentChainID — routed upstream to core-go.

import (
	"strings"

	"go.entitychurch.org/entity-core-go/core/hash"
	coretypes "go.entitychurch.org/entity-core-go/core/types"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

// ChainForHash returns the chain_id that produced the entity with the
// given content hash, when discoverable from substrate-honest sources
// (chain-error markers and suspended continuations).
//
// Returns ("", false) when the hash is not found in either source.
//
// O(bindings) per call — walks the location index once. For batched
// lookups over many hashes, prefer BuildChainIndex once and look up
// against the resulting map.
func ChainForHash(peer *entitysdk.AppPeer, h hash.Hash) (string, bool) {
	target := h.String()
	for _, e := range peer.Store().List("") {
		// Chain-error marker — terminal segment of the path is the
		// marker hash, the chain_id is the second segment after the
		// /chain-errors/ marker.
		if chainID, ok := chainIDFromMarkerPath(e.Path, target); ok {
			return chainID, true
		}

		// Suspended continuation — body carries chain_id.
		if e.Hash.String() != target {
			continue
		}
		if !strings.Contains(e.Path, "system/continuation/") {
			continue
		}
		ent, ok := peer.Store().GetByHash(e.Hash)
		if !ok || ent.Type != coretypes.TypeContinuationSuspended {
			continue
		}
		var body coretypes.ContinuationSuspendedData
		if err := cbor.Unmarshal(ent.Data, &body); err == nil && body.ChainID != "" {
			return body.ChainID, true
		}
	}
	return "", false
}

// BuildChainIndex walks the store once and returns a map from
// substrate-attributable entity hashes to their producing chain_id.
// Covers the same two source paths as ChainForHash; use this for batch
// lookups when rendering chain traces over many entities.
func BuildChainIndex(peer *entitysdk.AppPeer) map[string]string {
	out := map[string]string{}
	for _, e := range peer.Store().List("") {
		if chainID, ok := chainIDFromMarkerPath(e.Path, e.Hash.String()); ok {
			out[e.Hash.String()] = chainID
			continue
		}
		if !strings.Contains(e.Path, "system/continuation/") {
			continue
		}
		ent, ok := peer.Store().GetByHash(e.Hash)
		if !ok || ent.Type != coretypes.TypeContinuationSuspended {
			continue
		}
		var body coretypes.ContinuationSuspendedData
		if err := cbor.Unmarshal(ent.Data, &body); err == nil && body.ChainID != "" {
			out[e.Hash.String()] = body.ChainID
		}
	}
	return out
}

// chainIDFromMarkerPath returns the chain_id when path is a chain-error
// marker path AND its terminal segment matches targetHash. Empty match
// when path isn't a marker or terminal doesn't match.
func chainIDFromMarkerPath(path, targetHash string) (string, bool) {
	idx := strings.Index(path, "system/runtime/chain-errors/")
	if idx < 0 {
		return "", false
	}
	tail := path[idx+len("system/runtime/chain-errors/"):]
	parts := strings.Split(tail, "/")
	// Expected: {kind}/{chain_id}/{step}/{reason}/{marker_hash}
	if len(parts) < 5 {
		return "", false
	}
	if parts[len(parts)-1] != targetHash {
		return "", false
	}
	return parts[1], true
}
