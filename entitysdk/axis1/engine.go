package axis1

import (
	"sync"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/ext/compute"
)

// Engine is the Axis-1 execution engine: a decoded-form cache plus the walker.
//
// It is safe for concurrent use. Create one per peer (or one per process — the
// cache is keyed by content hash, so entries are never peer-specific) and reuse
// it across every tick and every program instance.
type Engine struct {
	mu    sync.RWMutex
	cache map[hash.Hash]node

	// noCache re-decodes on every Evaluate instead of reusing the decoded form.
	//
	// This exists to SPLIT THE CREDIT for handoff §7 Q1 — it is a measurement
	// instrument, not a feature, and no production path should set it. Comparing
	// cached against uncached isolates what the §13.7 decode cache contributes
	// across ticks from what the resolved form + live frames contribute within
	// one tick.
	//
	// Note what it does NOT isolate: even uncached, Axis-1 decodes each node
	// once per EVAL, while Stage-1 re-decodes each node once per VISIT — and in
	// Life the map body is visited once per cell. So uncached is
	// "decode-once-per-eval + live frames", not "live frames alone". A clean
	// live-frames-only measurement would need a third engine that keeps the
	// CaptureScope/LoadScope round-trip, which is not worth building; the report
	// states the split with this caveat rather than implying a precision it
	// does not have.
	noCache bool

	// stats are engine-lifetime counters, for the cost sweep and for asserting
	// in tests that decode-once actually happened once.
	stats Stats
}

// Stats reports what the engine did.
//
// These are load-bearing, not decoration: they are how a test proves the rung
// was actually exercised rather than vacuously agreed with. Decodes flat across
// ticks proves the §13.7 cache works; Closures > 0 proves the live-frame path
// (the F-D2 fix) ran at all; Fallbacks == 0 on a benchmark proves the measured
// number is Axis-1's rather than Stage-1's wearing its name.
type Stats struct {
	// Decodes counts ROOT entities decoded — one per decodeCached miss, not
	// per node. Children are decoded inside their root's pass and are not
	// separately cached, so this is a count of cache misses, not of nodes.
	Decodes   int64
	CacheHits int64 // decode requests served from cache
	Evals     int64 // calls to Evaluate
	Fallbacks int64 // nodes routed to the Stage-1 seam
	Closures  int64 // live closures created (each one Stage-1 would have stored)
	Frames    int64 // frames allocated (lets + closure invocations)
}

func (s *Stats) merge(o Stats) {
	s.Decodes += o.Decodes
	s.CacheHits += o.CacheHits
	s.Evals += o.Evals
	s.Fallbacks += o.Fallbacks
	s.Closures += o.Closures
	s.Frames += o.Frames
}

// NewEngine builds an engine with an empty decode cache.
func NewEngine() *Engine {
	return &Engine{cache: make(map[hash.Hash]node)}
}

// NewEngineNoCache builds an engine that re-decodes on every Evaluate.
// Measurement instrument only — see Engine.noCache.
func NewEngineNoCache() *Engine {
	return &Engine{cache: make(map[hash.Hash]node), noCache: true}
}

// Stats returns a snapshot of the engine's counters.
func (e *Engine) Stats() Stats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stats
}

// ResetStats zeroes the counters, leaving the decode cache intact — so a
// benchmark can measure steady-state ticks without the first tick's decodes.
func (e *Engine) ResetStats() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stats = Stats{}
}

// decodeCached returns the resolved node for an entity, decoding it at most
// once per unique content hash (exploration §13.7).
//
// Keying on the IR content hash is what makes invalidation a non-problem: a
// different step is a different hash is a different entry. Nothing mutates, so
// nothing goes stale. The content store already dedups the IR; this cache is
// its in-memory shadow.
//
// Two goroutines racing on the same miss may both decode; the result is
// identical (decoding is pure) so the loser's work is simply discarded rather
// than guarded by a held lock across the decode.
func (e *Engine) decodeCached(ent entity.Entity, ctx *compute.EvalContext) (node, error) {
	h := ent.ContentHash
	if !h.IsZero() && !e.noCache {
		e.mu.RLock()
		n, ok := e.cache[h]
		e.mu.RUnlock()
		if ok {
			e.mu.Lock()
			e.stats.CacheHits++
			e.mu.Unlock()
			return n, nil
		}
	}

	d := &decoder{ctx: ctx}
	n, err := d.decodeRoot(ent)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if !h.IsZero() && !e.noCache {
		e.cache[h] = n
	}
	e.stats.Decodes++
	e.mu.Unlock()
	return n, nil
}

// Evaluate evaluates a compute expression entity on the Axis-1 engine.
//
// The signature deliberately mirrors ext/compute/eval.go::Evaluate — same
// entity, same *Budget, same *EvalContext, same (interface{}, error) result —
// except that the scope arrives as a plain map rather than a *compute.Scope,
// because compute.Scope cannot be enumerated (its bindings are unexported with
// no iteration API) and the engine must be able to flatten scopes at the
// boundary. Callers holding a map can drive either engine; the differential
// harness builds a *compute.Scope from the same map for Stage-1.
//
// The result is the INTERIOR form: a *constructedValue for a construct, a live
// *closure for a lambda. Cross the boundary with Materialize before writing to
// the tree or comparing hashes — that crossing is where the contract binds.
func (e *Engine) Evaluate(
	ent entity.Entity,
	root map[string]interface{},
	budget *compute.Budget,
	ctx *compute.EvalContext,
) (interface{}, error) {
	n, err := e.decodeCached(ent, ctx)
	if err != nil {
		return nil, err
	}

	// The evaluator counts into an unlocked local Stats and merges once at the
	// end. Taking the engine mutex per closure or per frame would put lock
	// traffic on the hot path and corrupt the very measurement these counters
	// exist to support.
	ev := &evaluator{eng: e, budget: budget, ctx: ctx}
	ev.local.Evals = 1
	v, err := ev.eval(n, nil, root)

	e.mu.Lock()
	e.stats.merge(ev.local)
	e.mu.Unlock()
	return v, err
}

// EvaluateMaterialized evaluates and crosses the boundary in one step,
// returning the materialized value — the form whose content hash is the thing
// the oracle compares.
func (e *Engine) EvaluateMaterialized(
	ent entity.Entity,
	root map[string]interface{},
	budget *compute.Budget,
	ctx *compute.EvalContext,
) (interface{}, error) {
	v, err := e.Evaluate(ent, root, budget, ctx)
	if err != nil {
		return nil, err
	}
	return materialize(v, ctx.ContentStore)
}
