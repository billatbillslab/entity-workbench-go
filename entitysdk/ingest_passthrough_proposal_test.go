package entitysdk_test

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"github.com/fxamacker/cbor/v2"
)

// TestProposalD_IngestResultPassThrough_NavigationPath verifies the
// chain composition path proposed in
// `entity-core-architecture/.../EXPLORATION-CHAIN-ENVELOPE-COMPOSABILITY.md` §13.3:
//
//	When system/content/ingest-result gains an optional `root` field
//	that carries envelope.root through unchanged, the merge step's
//	transform `extract: "data.root.data.head"` can navigate to the
//	wrapped result's semantic field (the version hash).
//
// This test builds the proposed ingest-result shape manually
// (without modifying the kernel), then walks the navigation path
// using the exact same navigate() logic the continuation handler
// uses (replicated here — see ext/continuation/handler.go:899-920).
//
// Goal: confirm the §13.3 analysis works in the CBOR + transform
// stack we actually have, not just on paper. Specifically that
// crossing two entity boundaries via `data.X.data.Y` does NOT trip
// on cbor.RawMessage round-tripping through interface{} as bytes
// instead of as a nested map.
func TestProposalD_IngestResultPassThrough_NavigationPath(t *testing.T) {
	// --- Step 1: build the FetchResultData entity (the wrapper that
	//             carries the version hash as semantic content). ---
	versionHash, _ := hash.Compute("test/version-hash-marker", cbor.RawMessage{0xa0})
	if versionHash.IsZero() {
		t.Fatal("test fixture: versionHash unexpectedly zero")
	}

	fetchResult := types.RevisionFetchResultData{
		Head: versionHash,
	}
	fetchResultEnt, err := fetchResult.ToEntity()
	if err != nil {
		t.Fatalf("encode FetchResultData entity: %v", err)
	}
	t.Logf("FetchResultData entity:")
	t.Logf("  type:         %s", fetchResultEnt.Type)
	t.Logf("  content_hash: %s", fetchResultEnt.ContentHash)

	// --- Step 2: build the proposed (d) ingest-result shape.
	//             Type: system/content/ingest-result
	//             Data: { root: <fetchResultEnt>, root_hash: <hash>, ingested_count: N }
	//
	// Note: the production type is `ContentIngestResultData` with
	// only root_hash + ingested_count. The proposal would add `root`
	// as an optional system/entity-typed field. We build the
	// proposed shape by encoding a map directly so we don't need a
	// kernel change to exercise the transform path. ---
	proposedShape := map[string]interface{}{
		"root":           fetchResultEnt,             // the new field
		"root_hash":      fetchResultEnt.ContentHash, // existing field
		"ingested_count": uint64(5),                  // existing field
	}
	proposedDataBytes, err := ecf.Encode(proposedShape)
	if err != nil {
		t.Fatalf("encode proposed ingest-result data: %v", err)
	}
	proposedIngestResult, err := entity.NewEntity(
		"system/content/ingest-result",
		cbor.RawMessage(proposedDataBytes),
	)
	if err != nil {
		t.Fatalf("build proposed ingest-result entity: %v", err)
	}
	t.Logf("Proposed (d) ingest-result entity:")
	t.Logf("  type:         %s", proposedIngestResult.Type)
	t.Logf("  content_hash: %s", proposedIngestResult.ContentHash)
	t.Logf("  data length:  %d bytes", len(proposedIngestResult.Data))

	// --- Step 3: encode the full ingest-result entity (this is what
	//             would be in delivery.Result when the chain advances
	//             from ingest to merge — bytes of the response entity). ---
	deliveryResultBytes, err := ecf.Encode(proposedIngestResult)
	if err != nil {
		t.Fatalf("encode ingest-result entity: %v", err)
	}

	// --- Step 4: simulate the continuation transform — decode the
	//             delivery result bytes via cbor.Unmarshal into
	//             interface{} (matches applyTransform at handler.go:874-878). ---
	var transformInput interface{}
	if err := cbor.Unmarshal(deliveryResultBytes, &transformInput); err != nil {
		t.Fatalf("transform decode (matches applyTransform): %v", err)
	}

	// --- Step 5: walk `data.root.data.head` using the same navigate
	//             logic the kernel uses (handler.go:899-920). ---
	const pathToHead = "data.root.data.head"
	got := navigateLocal(transformInput, pathToHead)
	if got == nil {
		t.Fatalf("navigate(%q) returned nil — path failed somewhere along entity boundaries", pathToHead)
	}
	t.Logf("navigate(%q) returned: %T = %v", pathToHead, got, got)

	// --- Step 6: confirm the result is the version hash. ---
	gotHash, ok := hashFromNavigatedValue(got)
	if !ok {
		t.Fatalf("navigate(%q) result is not a hash-shaped value: %T = %v",
			pathToHead, got, got)
	}
	if gotHash != versionHash {
		t.Errorf("navigated hash mismatch:\n  got:  %s\n  want: %s", gotHash, versionHash)
	} else {
		t.Logf("✓ navigated hash matches version hash: %s", gotHash)
	}

	// --- Step 7: also verify the simpler `data.root_hash` path still
	//             works (the current chain pattern). Under (d), both
	//             must work — `root_hash` for snapshot-IS-root case,
	//             `data.root.data.X` for wrapper case. ---
	rootHashValue := navigateLocal(transformInput, "data.root_hash")
	if rootHashValue == nil {
		t.Fatal("navigate(data.root_hash) returned nil — existing path broke")
	}
	rootHash, ok := hashFromNavigatedValue(rootHashValue)
	if !ok {
		t.Fatalf("data.root_hash not hash-shaped: %T = %v", rootHashValue, rootHashValue)
	}
	if rootHash != fetchResultEnt.ContentHash {
		t.Errorf("data.root_hash mismatch:\n  got:  %s\n  want: %s",
			rootHash, fetchResultEnt.ContentHash)
	} else {
		t.Logf("✓ data.root_hash matches fetch-result wrapper hash: %s", rootHash)
	}
}

// TestProposalD_DistinguishWrapperCaseFromCleanCase: in the clean
// "snapshot-IS-root" case (tree extract → merge), the next op wants
// hash(envelope.root). Under (d), `data.root_hash` continues to
// produce that value. Confirms (d) doesn't break the clean case
// that today's primitives already handle.
func TestProposalD_DistinguishWrapperCaseFromCleanCase(t *testing.T) {
	// Build a fake snapshot entity (in the real case this would be a
	// system/tree/snapshot or similar — content is the "value" the
	// next op wants directly, no metadata wrapper).
	snapshotData, _ := cbor.Marshal(map[string]string{"snapshot": "marker"})
	snapshotEnt, err := entity.NewEntity("test/snapshot", cbor.RawMessage(snapshotData))
	if err != nil {
		t.Fatal(err)
	}

	// Proposed ingest-result shape (same as above, but root IS the value).
	proposedShape := map[string]interface{}{
		"root":           snapshotEnt,
		"root_hash":      snapshotEnt.ContentHash,
		"ingested_count": uint64(3),
	}
	proposedDataBytes, _ := ecf.Encode(proposedShape)
	ingestResult, _ := entity.NewEntity(
		"system/content/ingest-result",
		cbor.RawMessage(proposedDataBytes),
	)
	deliveryBytes, _ := ecf.Encode(ingestResult)

	var transformInput interface{}
	cbor.Unmarshal(deliveryBytes, &transformInput)

	// Clean case: extract data.root_hash — that's hash(snapshot) which
	// is what merge wants as `source`.
	rootHashValue := navigateLocal(transformInput, "data.root_hash")
	if rootHashValue == nil {
		t.Fatal("data.root_hash navigation failed")
	}
	got, ok := hashFromNavigatedValue(rootHashValue)
	if !ok {
		t.Fatalf("data.root_hash not hash-shaped: %T", rootHashValue)
	}
	if got != snapshotEnt.ContentHash {
		t.Errorf("clean-case root_hash mismatch:\n  got:  %s\n  want: %s",
			got, snapshotEnt.ContentHash)
	} else {
		t.Logf("✓ clean case: data.root_hash = hash(snapshot) — chain composes correctly")
	}
}

// --- Helpers ---

// navigateLocal replicates the continuation handler's navigate()
// function (ext/continuation/handler.go:899-920) so this test
// exercises the exact same path-walking logic without requiring a
// running continuation handler.
func navigateLocal(value interface{}, dottedPath string) interface{} {
	if dottedPath == "" {
		return value
	}
	segments := strings.Split(dottedPath, ".")
	current := value
	for _, seg := range segments {
		if current == nil {
			return nil
		}
		switch m := current.(type) {
		case map[interface{}]interface{}:
			current = m[seg]
		case map[string]interface{}:
			current = m[seg]
		default:
			return nil
		}
	}
	return current
}

// hashFromNavigatedValue converts a navigated CBOR value to a Hash.
// CBOR-encoded hashes round-trip through interface{} as []byte
// (containing the algorithm tag + digest). This helper does the
// minimum decoding to recover the Hash struct.
func hashFromNavigatedValue(v interface{}) (hash.Hash, bool) {
	// Re-encode and decode as Hash — robust to whatever interface{}
	// shape the CBOR library produced.
	raw, err := cbor.Marshal(v)
	if err != nil {
		return hash.Hash{}, false
	}
	var h hash.Hash
	if err := cbor.Unmarshal(raw, &h); err != nil {
		return hash.Hash{}, false
	}
	return h, true
}
