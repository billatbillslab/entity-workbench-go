package entitysdk

import (
	"strings"
	"sync"

	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
)

// ChangeEventType classifies a watch event.
type ChangeEventType string

const (
	ChangePut    ChangeEventType = "put"    // create or modify
	ChangeRemove ChangeEventType = "remove" // binding unbound
)

// ChangeEvent is delivered to a watch subscriber when a matched path is
// mutated. Shape mirrors SDK-OPERATIONS §6.1: event_type + path +
// new_hash (zero on remove).
//
// The same event shape is used by both the L0 Store.Watch raw-sink
// form and (future) the L1 dispatched form so callers can switch
// without touching consumer code.
type ChangeEvent struct {
	EventType ChangeEventType
	Path      string    // qualified path from the tree change event
	NewHash   hash.Hash // zero on remove
}

// StoreWatch is a handle to an active L0 raw-sink watch. It observes
// tree mutations on the local peer WITHOUT going through dispatch —
// no capability check, no author, no delivery token. See Store.Watch
// for the layering rationale (same L0-bypass warning that applies to
// Store.Get/Put/Remove).
//
// Events are delivered on the channel returned by Events(). Call
// Close (or Store.Unwatch) to stop receiving.
type StoreWatch struct {
	pattern string
	match   func(path string) bool

	events chan ChangeEvent

	// done is closed FIRST on cancellation, before events is closed.
	// It is what makes a blocking send abortable: the hub parks on
	// `select { case events <- ce: case <-done: }` while holding mu,
	// so a cancel unblocks the send instead of deadlocking against
	// the consumer, and events is only ever closed by a party holding
	// mu — which no sender can be past at that point.
	//
	// Without this split the hub sampled `closed` under mu, released
	// mu, and then sent; a cancel landing in that window closed the
	// channel under the sender and the next send panicked with "send
	// on closed channel". Surfaced by the foreign-namespace
	// subscription gate (`foreign_namespace_subscription_test.go`),
	// which registers two watches and cancels both while events are
	// in flight.
	done     chan struct{}
	doneOnce sync.Once

	hub    *watchHub
	closed bool
	mu     sync.Mutex
}

// signalDone closes done exactly once. Callers must not close done
// directly — cancel and hub-shutdown both reach it.
func (w *StoreWatch) signalDone() {
	w.doneOnce.Do(func() { close(w.done) })
}

// Pattern returns the pattern this StoreWatch was created with.
func (w *StoreWatch) Pattern() string { return w.pattern }

// Events returns the read-only event channel. It is closed when the
// watch is cancelled (via Close / Store.Unwatch) or when the peer is
// closed.
func (w *StoreWatch) Events() <-chan ChangeEvent { return w.events }

// Close cancels the watch and closes the event channel. Safe to call
// more than once.
func (w *StoreWatch) Close() {
	w.hub.unregister(w)
}

// watchHub is the per-peer fanout hub: it drains the core tree-event
// sink and distributes matching events to each registered StoreWatch.
// Owned by Store since this is L0 machinery.
type watchHub struct {
	sink <-chan store.TreeChangeEvent

	mu      sync.Mutex
	watches map[*StoreWatch]struct{}
	closed  bool
}

func newWatchHub(sink <-chan store.TreeChangeEvent) *watchHub {
	h := &watchHub{
		sink:    sink,
		watches: make(map[*StoreWatch]struct{}),
	}
	go h.run()
	return h
}

func (h *watchHub) run() {
	for evt := range h.sink {
		h.mu.Lock()
		subs := make([]*StoreWatch, 0, len(h.watches))
		for w := range h.watches {
			subs = append(subs, w)
		}
		h.mu.Unlock()

		ce := ChangeEvent{Path: evt.Path, NewHash: evt.Hash}
		switch evt.ChangeType {
		case store.ChangeDeleted:
			ce.EventType = ChangeRemove
			ce.NewHash = hash.Hash{}
		default:
			ce.EventType = ChangePut
		}

		for _, w := range subs {
			if !w.match(evt.Path) {
				continue
			}
			// Blocking send — spec §6.1 forbids silent drops. A slow
			// consumer will slow the hub; use a generous buffer at
			// registration to absorb bursts.
			//
			// mu is held ACROSS the send, which is what keeps the
			// channel from closing underneath it (see StoreWatch.done).
			// The send is abortable via done, so holding mu here
			// cannot deadlock a canceller: unregister closes done
			// before it reaches for mu.
			w.mu.Lock()
			if w.closed {
				w.mu.Unlock()
				continue
			}
			select {
			case w.events <- ce:
			case <-w.done:
			}
			w.mu.Unlock()
		}
	}

	// Sink closed — close all pending watches. Same ordering rule as
	// unregister: done first, then events under mu.
	h.mu.Lock()
	h.closed = true
	for w := range h.watches {
		w.signalDone()
		w.mu.Lock()
		if !w.closed {
			w.closed = true
			close(w.events)
		}
		w.mu.Unlock()
	}
	h.watches = nil
	h.mu.Unlock()
}

func (h *watchHub) register(w *StoreWatch) *Error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return NewError(500, "peer_closed", "peer is closed")
	}
	h.watches[w] = struct{}{}
	return nil
}

func (h *watchHub) unregister(w *StoreWatch) {
	// Order matters: closing done first releases any hub send parked
	// on this watch, so the mu acquisition below cannot block behind
	// a consumer that has stopped reading.
	w.signalDone()

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.events)
	w.mu.Unlock()

	h.mu.Lock()
	delete(h.watches, w)
	h.mu.Unlock()
}

// parsePattern validates a pattern and returns a matcher function. The
// matcher takes a qualified event path (as delivered by the core) and
// reports whether the pattern covers it.
//
// Per SDK-OPERATIONS §6.1 only two forms are specified:
//   - exact path:  "knowledge/articles/intro"
//   - prefix/*:    "knowledge/articles/*"
//
// Peer-relative patterns are canonicalized to "/{localPeerID}/{pattern}".
// Patterns containing "*" anywhere other than as a trailing "/*" are
// rejected (reserved for future specification).
func parsePattern(pattern, localPeerID string) (func(string) bool, *Error) {
	if pattern == "" {
		return nil, NewError(400, "invalid_pattern", "pattern is empty")
	}

	// Disallow mid-pattern wildcards and bare "*".
	if idx := strings.Index(pattern, "*"); idx >= 0 {
		if !strings.HasSuffix(pattern, "/*") {
			return nil, NewError(400, "invalid_pattern",
				"only exact and prefix/* patterns are specified (pattern="+pattern+")")
		}
	}

	abs := pattern
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + localPeerID + "/" + abs
	}

	if strings.HasSuffix(abs, "/*") {
		prefix := strings.TrimSuffix(abs, "*") // keep trailing slash
		return func(p string) bool { return strings.HasPrefix(p, prefix) }, nil
	}

	exact := abs
	return func(p string) bool { return p == exact }, nil
}
