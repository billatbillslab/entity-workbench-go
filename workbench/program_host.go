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
// **The base contract does not shard.** Arch ruled (§1) that telling a harness
// to derive k and shard an opaque IR was a compiler pass in a harness's clothes:
// a shard is a distinct baked expression (the range lowers to a literal index
// array), the instantiated size is baked into the IR as literals, and there is
// no `range` primitive to make N a runtime quantity. Sharding returns as the
// opt-in shard-range-port extension when `compute/range` lands. Phase 1 needs
// none of it — Snake 8×8, Life 16×16 and Asteroids ~24 slots are all under the
// 100k budget. Do not add sharding here speculatively.

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
		ap:     ap,
		desc:   desc,
		ports:  make(map[string]PortValue, len(desc.OutputPorts)),
		status: HostStopped,
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

// admit checks every declared shape against the shapes this host can drive.
// A host that lacks a shape refuses the whole program rather than half-render
// it — the openness of the vocabulary is only transfer-safe because of this.
func (h *Host) admit() error {
	for _, p := range append(append([]ProgramPort{}, h.desc.InputPorts...), h.desc.OutputPorts...) {
		if !DriverSupports(p.Shape) {
			return fmt.Errorf("unsupported shape %q on port %q (this host drives: %v)",
				p.Shape, p.Name, SupportedShapes())
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

// tickOnce runs one tick: eval the step, put the new state, refresh the
// projections. Plain eval → put, per the §1 ruling. Returns false to stop the
// loop (a fault).
func (h *Host) tickOnce() bool {
	typ, data, err := h.eval(h.desc.Step)
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
