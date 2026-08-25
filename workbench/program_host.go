package workbench

// THE GENERIC HOST — mount(descriptor) → a running program, with zero
// per-program code.
//
// Arch's HANDOFF-2026-07-17 §5.2. The falsifiable claim this file exists to
// carry: **it contains no program-specific symbol.** It cannot name an asteroid,
// does not know what a cell is, and does not know how many ports any program
// has. It loops over the descriptor. If you ever need to add `if program ==
// "snake"` here, the descriptor is missing a field and THAT is the finding —
// report it, do not special-case it (§5.4).
//
// The mount contract (exploration §2, as corrected by the §1 ruling):
//
//	mount(descriptor_path):
//	  d ← read descriptor entity
//	  for p in d.input_ports:  seed p.initial at p.path          # F-E1
//	  for p in d.output_ports: bind driver by (role, shape, scene)
//	  for p in d.input_ports:  bind driver by (role, shape)
//	  loop at d.tick.rate_hint:
//	     eval(d.step) → put d.state_path                          # plain; NO shard
//	     refresh each output port                                 # projections
//
// **The base contract does not shard** — an unsharded descriptor is plain
// eval → put. Arch's original §1 ruling forbade the host deriving k and rewriting
// an opaque IR, and that still holds: the host never rewrites a `map`. But the
// Q5 reversal (HANDOFF-2026-07-18-…-rulings §2) cleared the *static-k Option-B
// floor* to land now, un-gated from `range`: a sharded descriptor carries a
// pre-authored family of k shard expressions plus a stitch expression (all baked
// at authoring, k frozen), and the host only LOOPS them — k evals + one stitch
// eval + one put. No IR rewriting, no derived k, no `range`. That is the sharded
// tick below (tickShardedOnce). Phase-1's three product programs (Snake 8×8,
// Life 16×16, Asteroids ~24 slots) are all under the 100k budget and mount
// unsharded; sharding is exercised by a deliberately-oversized program past the
// budget cliff (AuthorLifeSharded at 32×32).
//
// The host drives the FLOOR only: static-k form, host-managed orchestration, a
// program-owned (compute-gather) stitch. A descriptor that declares the `range`
// form or the continuation-managed model is refused at admission with the reason
// — those are the dynamic-k upgrade and item 2, declared but not yet driven here.

import (
	"fmt"
	"sync"
	"time"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/types"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk"
)

// Run status, reported by the host. Distinct from any program's in-program
// status (Snake's death is program state; these are the harness's).
const (
	HostStopped = iota
	HostRunning
	HostFaulted
)

// PortValue is a materialized output port: the shape a driver binds to, the
// scene metadata it can't infer, and the raw entity data. The host does not
// decode Data — decoding is the driver's job, per its shape. This is where
// "the host knows nothing about the program" is enforced structurally.
type PortValue struct {
	Name    string
	Shape   string
	TypeRef string
	Scene   map[string]interface{}
	Data    cbor.RawMessage
}

// HostFrame is one render of a mounted program: every output port, materialized.
type HostFrame struct {
	Ports   map[string]PortValue
	Status  int
	Ticks   uint64
	Err     string
	Running bool
}

// Host is one mounted program. It is created by Mount and owns run-state
// (start / stop / restart) — which is the host's surface, outside the program
// (proposal §5).
type Host struct {
	ap   *entitysdk.AppPeer
	desc *ProgramDescriptor

	mu        sync.Mutex
	ports     map[string]PortValue
	running   bool
	ticks     uint64
	status    int
	lastErr   string
	listeners []func()

	stopCh chan struct{}
	doneCh chan struct{}

	tickInterval time.Duration

	// parallelShards selects concurrent shard evaluation for a sharded program.
	// Defaults to true — parallelism is the whole point of the floor, and shard
	// evals are pure reads (Axis-1 §4.3: live frames make them side-effect-free),
	// so they fan out deterministically (each fragment lands in its own path;
	// the stitch reads them in declared order). The serial path exists so a test
	// can pin parallel == serial. Ignored for an unsharded program.
	parallelShards bool
}

// Mount reads the descriptor at descriptorPath and returns a stopped, seeded
// program. This is the entire host API for getting a program running; there is
// no per-program constructor.
func Mount(ap *entitysdk.AppPeer, descriptorPath string) (*Host, error) {
	if ap == nil {
		return nil, fmt.Errorf("Mount: nil AppPeer")
	}
	ent, ok, err := ap.Get(descriptorPath)
	if err != nil {
		return nil, fmt.Errorf("Mount: read descriptor at %s: %w", descriptorPath, err)
	}
	if !ok {
		return nil, fmt.Errorf("Mount: no descriptor at %s", descriptorPath)
	}
	desc, err := DecodeDescriptor(ent)
	if err != nil {
		return nil, fmt.Errorf("Mount: %w", err)
	}
	if err := desc.Validate(); err != nil {
		return nil, fmt.Errorf("Mount: %w", err)
	}

	h := &Host{
		ap:             ap,
		desc:           desc,
		ports:          make(map[string]PortValue, len(desc.OutputPorts)),
		status:         HostStopped,
		parallelShards: true,
	}
	if desc.Tick.Mode == TickClockDriven {
		h.tickInterval = time.Second / time.Duration(desc.Tick.RateHint)
	}

	// Admission (exploration §6): refuse a shape we cannot drive, and say why.
	// Never a half-mount.
	if err := h.admit(); err != nil {
		return nil, fmt.Errorf("Mount: %w", err)
	}
	if err := h.reseed(); err != nil {
		return nil, fmt.Errorf("Mount: %w", err)
	}
	return h, nil
}

// admit checks every declared shape against the shapes this host can drive, and
// — if the program is sharded — that the shard mode is one this host drives.
// A host that lacks a shape or a shard mode refuses the whole program rather than
// half-render it: the openness of the vocabulary is only transfer-safe because of
// this, and the same rule extends to sharding (never a half-mount).
func (h *Host) admit() error {
	for _, p := range append(append([]ProgramPort{}, h.desc.InputPorts...), h.desc.OutputPorts...) {
		if !DriverSupports(p.Shape) {
			return fmt.Errorf("unsupported shape %q on port %q (this host drives: %v)",
				p.Shape, p.Name, SupportedShapes())
		}
	}
	if s := h.desc.Shard; s != nil {
		// The floor this host drives: static-k, host-managed, program-owned
		// stitch. Anything else is declared-but-not-driven-here — refuse with the
		// reason, don't half-mount.
		if s.Form != ShardStaticK {
			return fmt.Errorf("shard form %q not driven by this host (drives: %q — %q is the dynamic-k upgrade, waits on compute/range)",
				s.Form, ShardStaticK, ShardRange)
		}
		if s.Orchestration != OrchestrationHostManaged {
			return fmt.Errorf("shard orchestration %q not driven by this host (drives: %q — %q is the transferable ceiling, waits on the join-failure policy)",
				s.Orchestration, OrchestrationHostManaged, OrchestrationContinuationManaged)
		}
		if s.StitchOwner != "" && s.StitchOwner != StitchOwnerComputeGather {
			return fmt.Errorf("shard stitch_owner %q not driven by this host (drives: %q — a generic host cannot own a %q stitch without per-program code)",
				s.StitchOwner, StitchOwnerComputeGather, StitchOwnerHost)
		}
	}
	return nil
}

// reseed writes state₀ and every input port's F-E1 seed, then materializes the
// output ports so a mounted-but-never-started program still renders.
func (h *Host) reseed() error {
	if err := h.copyEntity(h.desc.InitialState, h.desc.StatePath); err != nil {
		return fmt.Errorf("seed state: %w", err)
	}
	for _, p := range h.desc.InputPorts {
		if err := h.copyEntity(p.Initial, p.Path); err != nil {
			return fmt.Errorf("seed input port %q: %w", p.Name, err)
		}
	}
	h.mu.Lock()
	h.ticks = 0
	h.status = HostStopped
	h.lastErr = ""
	h.mu.Unlock()
	return h.refreshPorts()
}

// copyEntity moves the entity at src to dst, unchanged. The host never
// constructs program data — it only relocates entities the authoring step
// produced. This is what seeding is: state₀ and the F-E1 port seeds are
// authored values, not host values.
func (h *Host) copyEntity(src, dst string) error {
	ent, ok, err := h.ap.Get(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if !ok {
		return fmt.Errorf("nothing at %s", src)
	}
	if _, err := h.ap.PutEntity(dst, ent); err != nil {
		return fmt.Errorf("put %s: %w", dst, err)
	}
	return nil
}

// eval evaluates the expression at path and returns the result entity's type
// and data. This is the host's only compute call.
func (h *Host) eval(path string) (string, cbor.RawMessage, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return "", nil, err
	}
	resp, err := h.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
	if err != nil {
		return "", nil, fmt.Errorf("eval dispatch %s: %w", path, err)
	}
	if resp.Status != 200 {
		return "", nil, fmt.Errorf("eval %s: status %d (type=%s)", path, resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return "", nil, fmt.Errorf("eval %s: compute/error code=%s message=%q", path, ed.Code, ed.Message)
	}
	return resp.Type, resp.Data, nil
}

// tickOnce runs one tick and writes the new state, then refreshes the
// projections. Two shapes, selected by the descriptor:
//   - unsharded (Shard == nil): plain eval(step) → put(state), the base contract.
//   - sharded (Shard != nil): k shard evals + a stitch eval → put(state), the
//     host-managed floor (tickShardedOnce).
//
// Both bottom out on the same host rule — put an entity an eval produced, never
// decode it — so the sharded path adds no program knowledge to the host.
// Returns false to stop the loop (a fault).
func (h *Host) tickOnce() bool {
	var typ string
	var data cbor.RawMessage
	var err error
	if h.desc.Shard != nil {
		typ, data, err = h.tickShardedOnce()
	} else {
		typ, data, err = h.eval(h.desc.Step)
	}
	if err != nil {
		h.fault(err)
		return false
	}
	ent, err := entity.NewEntity(typ, data)
	if err != nil {
		h.fault(err)
		return false
	}
	if _, err := h.ap.PutEntity(h.desc.StatePath, ent); err != nil {
		h.fault(fmt.Errorf("put state: %w", err))
		return false
	}
	if err := h.refreshPorts(); err != nil {
		h.fault(err)
		return false
	}
	h.mu.Lock()
	h.ticks++
	h.mu.Unlock()
	h.notify()
	return true
}

// tickShardedOnce runs one host-managed static-k sharded generation and returns
// the stitched state entity's (type, data) — NOT yet written; tickOnce writes it,
// so the sharded and unsharded paths share the one put.
//
//	for j in 0..k:  put(fragment_base/frag{j}, eval(shards[j]))   # fresh budget each
//	stitch:         eval(stitch)                                   # program-owned gather
//
// Each shard eval gets a fresh 100k budget (the budget boundary is the handler
// invocation, not the expression tree — Axis-1 §4.1), which is the whole point:
// a step that busts the budget unsharded ticks when split into k that each fit.
// The k evals fan out in parallel when parallelShards is set — safe because a
// shard eval performs no writes (side-effect-free, Axis-1 §4.3), and each
// fragment is written to its OWN path, so completion order cannot reach the
// output. The stitch reads the k fragments in declared order, so the boundary is
// deterministic by construction (the parallel == serial == unsharded bar).
//
// The host writes each fragment verbatim (the shard expression self-wraps into a
// fragment entity — program_descriptor.go's ProgramShard header) and never
// decodes it. The stitch is just another eval, so the host stays program-blind.
func (h *Host) tickShardedOnce() (string, cbor.RawMessage, error) {
	s := h.desc.Shard
	if err := h.evalShardFragments(s); err != nil {
		return "", nil, err
	}
	// The program-owned stitch reads the k fragments and produces the whole state
	// entity. Its result IS the new state — one more eval, no host stitching.
	typ, data, err := h.eval(s.Stitch)
	if err != nil {
		return "", nil, fmt.Errorf("stitch: %w", err)
	}
	return typ, data, nil
}

// evalShardFragments evaluates the k shards and writes each fragment to its path.
// Serial or parallel per parallelShards; both write the identical fragments, so
// the stitch that follows is deterministic either way.
func (h *Host) evalShardFragments(s *ProgramShard) error {
	if !h.parallelShards {
		for j, path := range s.Shards {
			if err := h.evalShardFragment(s, j, path); err != nil {
				return err
			}
		}
		return nil
	}

	// Parallel: eval the k shards concurrently (pure reads), collect the fragment
	// entities, then write them in index order. Writing is serialized after the
	// fan-out so the store sees the same put sequence as the serial path — the
	// eval is what parallelizes, not the put.
	type frag struct {
		typ  string
		data cbor.RawMessage
		err  error
	}
	frags := make([]frag, len(s.Shards))
	var wg sync.WaitGroup
	for j, path := range s.Shards {
		wg.Add(1)
		go func(j int, path string) {
			defer wg.Done()
			frags[j].typ, frags[j].data, frags[j].err = h.eval(path)
		}(j, path)
	}
	wg.Wait()
	for j := range frags {
		if frags[j].err != nil {
			return fmt.Errorf("shard %d: %w", j, frags[j].err)
		}
	}
	for j := range frags {
		if err := h.putFragment(s, j, frags[j].typ, frags[j].data); err != nil {
			return err
		}
	}
	return nil
}

// evalShardFragment evals one shard and writes its fragment (the serial path).
func (h *Host) evalShardFragment(s *ProgramShard, j int, path string) error {
	typ, data, err := h.eval(path)
	if err != nil {
		return fmt.Errorf("shard %d: %w", j, err)
	}
	return h.putFragment(s, j, typ, data)
}

// putFragment writes fragment j verbatim to {fragment_base}/frag{j}. The host
// does not construct or decode the fragment — the shard expression produced a
// self-wrapping entity; the host only relocates it, exactly as it relocates the
// step result in the unsharded path.
func (h *Host) putFragment(s *ProgramShard, j int, typ string, data cbor.RawMessage) error {
	ent, err := entity.NewEntity(typ, data)
	if err != nil {
		return fmt.Errorf("shard %d: %w", j, err)
	}
	fragPath := fmt.Sprintf("%s/frag%d", s.FragmentBase, j)
	if _, err := h.ap.PutEntity(fragPath, ent); err != nil {
		return fmt.Errorf("shard %d: put fragment %s: %w", j, fragPath, err)
	}
	return nil
}

// refreshPorts materializes every output port. A port with a Source is a
// projection: eval it and put the value at Path. A port without one already has
// its value at Path (the step wrote it). This is the uniform rule the Source
// field buys — see program_descriptor.go's header.
func (h *Host) refreshPorts() error {
	vals := make(map[string]PortValue, len(h.desc.OutputPorts))
	for _, p := range h.desc.OutputPorts {
		var typ string
		var data cbor.RawMessage

		if p.Source != "" {
			t, d, err := h.eval(p.Source)
			if err != nil {
				return fmt.Errorf("port %q: %w", p.Name, err)
			}
			ent, err := entity.NewEntity(t, d)
			if err != nil {
				return fmt.Errorf("port %q: %w", p.Name, err)
			}
			if _, err := h.ap.PutEntity(p.Path, ent); err != nil {
				return fmt.Errorf("port %q: put %s: %w", p.Name, p.Path, err)
			}
			typ, data = t, d
		} else {
			ent, ok, err := h.ap.Get(p.Path)
			if err != nil {
				return fmt.Errorf("port %q: read %s: %w", p.Name, p.Path, err)
			}
			if !ok {
				return fmt.Errorf("port %q: nothing at %s", p.Name, p.Path)
			}
			typ, data = ent.Type, ent.Data
		}

		if typ != p.TypeRef {
			return fmt.Errorf("port %q: declared type_ref %s but got %s", p.Name, p.TypeRef, typ)
		}
		vals[p.Name] = PortValue{
			Name: p.Name, Shape: p.Shape, TypeRef: p.TypeRef,
			Scene: p.Scene, Data: data,
		}
	}
	h.mu.Lock()
	h.ports = vals
	h.mu.Unlock()
	return nil
}

// Input writes a value to a named input port. The host does not interpret it —
// the driver encoded it per the port's shape.
func (h *Host) Input(portName string, typeRef string, data cbor.RawMessage) error {
	for _, p := range h.desc.InputPorts {
		if p.Name != portName {
			continue
		}
		if typeRef != p.TypeRef {
			return fmt.Errorf("input port %q: declared type_ref %s but got %s", p.Name, p.TypeRef, typeRef)
		}
		ent, err := entity.NewEntity(typeRef, data)
		if err != nil {
			return err
		}
		if _, err := h.ap.PutEntity(p.Path, ent); err != nil {
			return fmt.Errorf("input port %q: put %s: %w", p.Name, p.Path, err)
		}
		return nil
	}
	return fmt.Errorf("no input port named %q", portName)
}

// Start clocks the tick. No-op if already running.
func (h *Host) Start() {
	h.mu.Lock()
	if h.running || h.stopCh != nil {
		h.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	h.stopCh, h.doneCh = stop, done
	h.running = true
	h.status = HostRunning
	interval := h.tickInterval
	h.mu.Unlock()
	h.notify()

	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !h.tickOnce() {
					return
				}
			}
		}
	}()
}

// Stop halts the tick loop and joins the goroutine.
func (h *Host) Stop() {
	h.mu.Lock()
	stop, done := h.stopCh, h.doneCh
	h.stopCh, h.doneCh = nil, nil
	if h.running {
		h.running = false
		if h.status == HostRunning {
			h.status = HostStopped
		}
	}
	h.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	h.notify()
}

// Restart stops, reseeds from the descriptor's initial_state, and leaves the
// program stopped. Run-state is the host's surface, not the program's.
func (h *Host) Restart() error {
	h.Stop()
	if err := h.reseed(); err != nil {
		return err
	}
	h.notify()
	return nil
}

// Render returns the current frame — every materialized output port. A pure
// read of host state.
func (h *Host) Render() HostFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	ports := make(map[string]PortValue, len(h.ports))
	for k, v := range h.ports {
		ports[k] = v
	}
	return HostFrame{
		Ports: ports, Status: h.status, Ticks: h.ticks,
		Err: h.lastErr, Running: h.running,
	}
}

// Descriptor exposes the mounted descriptor (read-only use: drivers need the
// port declarations to bind).
func (h *Host) Descriptor() *ProgramDescriptor { return h.desc }

// OnChange registers a listener, returning an unsubscribe func.
func (h *Host) OnChange(fn func()) func() {
	h.mu.Lock()
	idx := len(h.listeners)
	h.listeners = append(h.listeners, fn)
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		if idx < len(h.listeners) {
			h.listeners[idx] = nil
		}
		h.mu.Unlock()
	}
}

func (h *Host) notify() {
	h.mu.Lock()
	ls := append([]func(){}, h.listeners...)
	h.mu.Unlock()
	for _, fn := range ls {
		if fn != nil {
			fn()
		}
	}
}

func (h *Host) fault(err error) {
	h.mu.Lock()
	h.lastErr = err.Error()
	h.status = HostFaulted
	h.running = false
	h.mu.Unlock()
	h.notify()
}

// Close stops the program.
func (h *Host) Close() { h.Stop() }
