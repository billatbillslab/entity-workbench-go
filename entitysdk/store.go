package entitysdk

import (
	"fmt"
	"sort"
	"sync"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
)

// ListEntriesSorted returns entries from a LocationIndex matching the
// prefix, sorted by path. An empty prefix returns all entries.
func ListEntriesSorted(li store.LocationIndex, prefix string) []store.LocationEntry {
	entries := li.List(prefix)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries
}

// Store is the Level 0 direct-store surface for a local peer.
//
// Level 0 operations read and write the content store and location index
// directly, without dispatching through system/tree. They run with the
// peer's standing grant — the peer's own authority — and are intended for
// the code that built and configured the peer.
//
// The emit pathway still fires on every mutation (notifying location
// index), so history, subscription, and query-index consumers see writes
// that go through Store. What Level 0 skips is the dispatch chain:
// signature verification, capability grant check, handler resolution.
//
// If the peer was constructed with a constrained grant, Level 0 writes
// that exceed those constraints are a policy bypass, not a system
// inconsistency — the tree is correct and consumers fire, but the grant
// constraints simply aren't enforced because dispatch was not invoked.
//
// For capability-enforced operations, use the Level 1 dispatched forms on
// AppPeer (coming in a later change). See SDK-OPERATIONS §2.7 for the
// level model.
type Store struct {
	content       store.ContentStore
	locationIndex store.LocationIndex
	log           *EventLog
	peerID        string    // needed for Watch pattern canonicalization
	watchHub      *watchHub // L0 raw-sink watch fanout; nil until wired
}

// NewStore constructs a Level 0 store accessor from a peer's content
// store and location index.
func NewStore(cs store.ContentStore, li store.LocationIndex) *Store {
	return &Store{content: cs, locationIndex: li}
}

// SetLog attaches an event log for operation tracing.
func (s *Store) SetLog(log *EventLog) { s.log = log }

// Get retrieves the entity bound at path. Returns (zero, false) if no
// binding exists.
func (s *Store) Get(path string) (entity.Entity, bool) {
	h, ok := s.locationIndex.Get(path)
	if !ok {
		if s.log != nil {
			s.log.Debugf("store.get %s → not found", path)
		}
		return entity.Entity{}, false
	}
	ent, ok := s.content.Get(h)
	if ok && s.log != nil {
		s.log.Verbosef("store.get %s → %s (%d bytes)", path, ent.Type, len(ent.Data))
	}
	return ent, ok
}

// Put constructs an entity from typeName + data, stores it, and binds
// path to its content hash. Returns the stored entity's content hash.
func (s *Store) Put(path, typeName string, data interface{}) (hash.Hash, error) {
	raw, err := encodeData(data)
	if err != nil {
		return hash.Hash{}, WrapError(400, "encode_failed", "store.put encode", err)
	}
	ent, err := entity.NewEntity(typeName, raw)
	if err != nil {
		return hash.Hash{}, WrapError(400, "invalid_entity", "store.put entity", err)
	}
	h, err := s.content.Put(ent)
	if err != nil {
		return hash.Hash{}, WrapError(500, "store_failed", "store.put content", err)
	}
	s.locationIndex.Set(path, h)

	if s.log != nil {
		s.log.Verbosef("store.put %s → %s (%d bytes)", path, typeName, len(raw))
	}
	return h, nil
}

// PutCAS is a compare-and-swap put (SDK-OPERATIONS §3.2). The write
// applies only if the binding at path currently matches expected.
//
// Pass a zero hash.Hash to mean "path must not exist" (create-only).
//
// Returns a 409 conflict Error if the current binding does not match
// expected.
//
// Note: at Level 0 this is implemented as get-then-put; it is atomic
// against other goroutines using the same Store but NOT against
// writes issued through handler dispatch concurrently. For
// capability-enforced atomic CAS, dispatch system/tree put with
// expected_hash directly.
func (s *Store) PutCAS(path, typeName string, data interface{}, expected hash.Hash) (hash.Hash, error) {
	current, exists := s.locationIndex.Get(path)
	if expected.IsZero() {
		if exists {
			return hash.Hash{}, NewError(409, "conflict",
				"path already exists: "+path)
		}
	} else {
		if !exists {
			return hash.Hash{}, NewError(409, "conflict",
				"no binding at path: "+path)
		}
		if current != expected {
			return hash.Hash{}, NewError(409, "conflict",
				"current binding at "+path+" does not match expected hash")
		}
	}
	return s.Put(path, typeName, data)
}

// List returns all entries under prefix, sorted by path. An empty
// prefix returns all entries.
func (s *Store) List(prefix string) []store.LocationEntry {
	entries := ListEntriesSorted(s.locationIndex, prefix)
	if s.log != nil {
		if prefix == "" {
			s.log.Verbosef("store.list (all) → %d entries", len(entries))
		} else {
			s.log.Verbosef("store.list %q → %d entries", prefix, len(entries))
		}
	}
	return entries
}

// Has reports whether a binding exists at path.
func (s *Store) Has(path string) bool {
	return s.locationIndex.Has(path)
}

// Remove unbinds path. The underlying entity remains in the content
// store. Returns true if a binding existed and was removed.
func (s *Store) Remove(path string) bool {
	_, existed := s.locationIndex.Remove(path)
	if s.log != nil {
		if existed {
			s.log.Verbosef("store.remove %s", path)
		} else {
			s.log.Debugf("store.remove %s → not found", path)
		}
	}
	return existed
}

// EntityCount returns the number of entities in the content store.
func (s *Store) EntityCount() int {
	return s.content.Len()
}

// GetByHash returns the entity stored under content-hash h, or
// (zero entity, false) if absent. Useful for direct hash → entity
// lookups that bypass the location index — e.g. inspecting a
// revision version entity, walking a trie node, or chasing a
// content-addressed pointer surfaced in a result envelope.
func (s *Store) GetByHash(h hash.Hash) (entity.Entity, bool) {
	return s.content.Get(h)
}

// ContentStore returns the underlying ContentStore. Workbench code
// that needs to write blob + chunk entities (CONTENT v3.6 substrate)
// uses this to call content.IngestBlob directly. Prefer Put / dispatched
// ops for tree-bound writes; direct ContentStore access is the
// substrate-trusted-code path per L10.
func (s *Store) ContentStore() store.ContentStore { return s.content }

// PathCount returns the number of paths in the location index
// (SDK-OPERATIONS §8.4 path_count). Distinct from EntityCount: paths
// can share the same content hash, so PathCount ≥ EntityCount in
// general. Implemented via LocationIndex.LenPrefix("") which routes
// to a SQL COUNT(*) under the peer's namespace — O(log N) via the
// path index, not O(N).
func (s *Store) PathCount() int {
	return s.locationIndex.LenPrefix("")
}

// Watch registers for L0 raw-sink change notifications matching pattern.
// Pattern forms per SDK-OPERATIONS §6.1:
//
//   - exact:  "workspace/settings/theme"
//   - prefix: "workspace/settings/*"
//
// Peer-relative patterns are canonicalized to "/{localPeerID}/…";
// absolute patterns ("/{peerID}/…") are used as-is.
//
// WARNING: Store.Watch observes mutations directly off the local peer's
// tree-event sink. It does NOT go through handler dispatch — no
// capability check, no author, no delivery token. Use it only when the
// calling code is the peer's own code (same trust domain that holds
// the peer's standing grant). For dispatched notifications — including
// cross-peer delivery, token-gated access, or rate-limited streams —
// use AppPeer.Subscribe (see pass 2 of SDK-ALIGNMENT §7). The name
// "Store.Watch" matches the L0 naming convention on Store.Get /
// Store.Put / Store.Remove: the security boundary is visible at the
// call site.
//
// Returns an error if the watch hub is not wired (a misconfigured
// store) or the pattern is invalid.
func (s *Store) Watch(pattern string) (*StoreWatch, error) {
	if s.watchHub == nil {
		return nil, NewError(500, "watch_unavailable",
			"store has no watch hub wired; construct via CreatePeer")
	}
	match, perr := parsePattern(pattern, s.peerID)
	if perr != nil {
		return nil, perr
	}
	w := &StoreWatch{
		pattern: pattern,
		match:   match,
		events:  make(chan ChangeEvent, 64),
		done:    make(chan struct{}),
		hub:     s.watchHub,
	}
	if err := s.watchHub.register(w); err != nil {
		return nil, err
	}
	return w, nil
}

// Unwatch cancels w. Equivalent to w.Close(). Safe to call with a
// StoreWatch that's already closed.
func (s *Store) Unwatch(w *StoreWatch) {
	if w == nil {
		return
	}
	w.Close()
}

// OnPrefixChange subscribes to all tree mutations under a prefix and
// delivers them to handler. This is the foundational pattern for panel
// models and other consumers that display data from a region of the
// tree: each consumer owns its prefix subscription + local view-state,
// and updates incrementally instead of re-scanning the whole tree.
//
// The handler fires for every relevant event with a ChangeEvent
// containing the qualified path, the new hash (zero on remove), and
// the event type. Handlers fetch entity bodies via Store.Get as needed
// — the SDK does not decode here because the consumer knows its own
// types better than this layer.
//
// **Seeding.** At attach time the SDK enumerates existing paths under
// prefix and delivers a synthetic ChangePut for each one before
// streaming live events. This means handlers always see "current state
// of path = X" semantics: every path the handler should care about
// arrives as a ChangePut (with the current hash) at some point — once
// from the seed, again from any subsequent mutation. Handlers must
// therefore be idempotent ("set the model's view of path P to the
// state at hash H"), not delta-oriented.
//
// **Atomicity.** The watch is registered BEFORE the seed enumerates,
// so any mutation between attach and seed-completion is captured in
// the watch buffer and delivered after the seed. The price is possible
// duplicate ChangePut for the same path (seed value + actual event
// value) — harmless for idempotent handlers.
//
// **Threading.** The handler runs on an SDK-owned goroutine. Callers
// that touch render state must marshal back to their render thread
// themselves. Slow handlers backpressure the watch chan (64-deep) and
// eventually the hub — keep handlers fast; spawn a goroutine inside
// if blocking work is required.
//
// **Cancel.** Idempotent. Closes the underlying watch and waits for
// the SDK goroutine to exit. Safe to call multiple times; safe to call
// after peer close.
//
// **Pattern.** prefix should end in `/` (e.g. `"docs/"`). SDK appends
// `*` to form the underlying Store.Watch pattern (`"docs/*"`). A
// special-case empty prefix subscribes to ALL paths under the local
// peer — used by panels like the tree-browser that legitimately
// display the whole tree.
//
// If the store has no watch hub wired (raw Store constructed without
// CreatePeer — typical in unit-test scaffolding), the seed phase
// still runs but no live updates are delivered. Cancel is a no-op
// in that case.
func (s *Store) OnPrefixChange(prefix string, handler func(ChangeEvent)) (cancel func()) {
	// Stores without a watch hub (test scaffolding) cannot deliver
	// live events. Seed-only mode: deliver one ChangePut per existing
	// entry under the prefix (or all entries when prefix is empty),
	// SYNCHRONOUSLY on the caller's goroutine so unit tests can
	// construct + assert without polling. Cancel is a no-op.
	if s.watchHub == nil {
		for _, e := range s.List(prefix) {
			handler(ChangeEvent{
				EventType: ChangePut,
				Path:      e.Path,
				NewHash:   e.Hash,
			})
		}
		return func() {}
	}

	var pattern string
	if prefix == "" {
		// "Everything under this peer." Canonicalize directly to the
		// peer-qualified wildcard; parsePattern would reject a bare "*".
		if s.peerID == "" {
			panic("entitysdk: Store.OnPrefixChange(\"\") requires the store to know its peer-id")
		}
		pattern = "/" + s.peerID + "/*"
	} else {
		pattern = prefix
		if pattern[len(pattern)-1] == '/' {
			pattern = pattern + "*"
		} else {
			pattern = pattern + "/*"
		}
	}

	w, err := s.Watch(pattern)
	if err != nil {
		panic(fmt.Sprintf("entitysdk: Store.OnPrefixChange watch failed: %v", err))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		// Seed: deliver synthetic ChangePut for each existing entry
		// under prefix. Done on the SDK goroutine so the caller doesn't
		// block waiting for the seed (matters when N is large).
		for _, e := range s.List(prefix) {
			handler(ChangeEvent{
				EventType: ChangePut,
				Path:      e.Path,
				NewHash:   e.Hash,
			})
		}

		// Live event drain. The watch was registered before the seed,
		// so any event that fired during seeding is in the buffer and
		// will be delivered now. Some paths may double-fire (seed +
		// real event); handlers must be idempotent.
		for ev := range w.Events() {
			handler(ev)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			w.Close()
			<-done
		})
	}
}
