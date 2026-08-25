package workbench

// THE PROGRAM DESCRIPTOR — `app/program/interface`, the manifest that makes a
// compute program addressable as a program rather than merely present in the
// tree.
//
// Per arch's PROPOSAL-APP-CONVENTION-COMPUTE-PROGRAM §2 (the field set) and
// HANDOFF-2026-07-17-generic-host-design-rulings (the rulings this build
// implements). The descriptor is an L5 convention: no EXTENSION-COMPUTE change,
// no wire change. It declares where input enters, where output leaves, and how
// to clock the tick — and nothing about what the program computes.
//
// The whole point: a host reads THIS and mounts the program. It never reads the
// program. `program_host.go` is the proof — it imports no program-specific
// symbol and cannot name an asteroid.
//
// ─── The one field that is ours, not arch's: Port.Source ──────────────────
//
// The proposed port model is {name, path, type_ref, kind, role, shape, scene?,
// initial?}. Building against it surfaced a gap arch's §7 Q4 asked us to look
// for ("anything the descriptor format itself couldn't express"):
//
//	*Nothing in a port says whether its value is ALREADY at `path`, or must be
//	 EVAL'd to materialize.*
//
// Both exist in the three shipped programs. Life's state port: the step writes
// the value to `state_path`; the host reads it. Asteroids' display port: the
// path holds an EXPRESSION (buildAsteroidsDisplayExpr → Build(displayPath),
// program_asteroids.go), which the host must eval to get a display list. The
// proposal calls output ports "paths / projections the HOST reads" (§2) and the
// mount loop says "materialize/refresh p # projections" (exploration §2) — but
// no field tells the host WHICH a port is, or WHAT to eval.
//
// A host could sniff the entity type at `path` and eval it if it looks like an
// expression. That is type-sniffing as control flow, and it would make the
// harness guess at program structure — the same overreach as the shard ruling
// (§1), one level down.
//
// So: `Source` is the expression to eval to materialize the port; absent means
// the value already lives at `path`. This makes `path` mean ONE thing for every
// port — where the port's value is — and keeps the host's rule uniform:
//
//	if p.Source != "" { put(p.Path, eval(p.Source)) } ; then read p.Path
//
// Reported to arch as a phase-1 descriptor finding. It is additive and does not
// disturb any other field. See docs/architecture/reviews/.
//
// ─── The second deviation: step/initial_state are PATHS here, not hashes ───
//
// The proposal types both `step` and `initial_state` as {type_ref:
// "system/hash"}. **A host cannot use a hash as given.** `system/compute:eval`
// resolves its target as a PATH and only a path:
//
//	exprPath := hctx.Resource.Targets[0]      // entity-core-go/ext/compute/handler.go:66
//	h.EvaluateAtPath(ctx, exprPath, req)      //                        handler.go:274
//
// There is no eval-by-hash. So a descriptor whose `step` is a hash gives a host
// nothing it can evaluate — it would first have to resolve the hash to an entity
// and write it to some path, and no field says which path.
//
// Phase 1 therefore carries evaluable PATHS, with the content hashes alongside
// as optional verification/transfer fields (StepHash, InitialStateHash). This is
// not a retreat from content-addressing: the expression graph is still fully
// content-addressed (Build returns its root hash), and the hashes are what
// phase 2 transfers. It is a statement that **the addressable unit for transfer
// (a hash) and the addressable unit for evaluation (a path) are different**, and
// the descriptor needs both. Program paths are program-relative (`app/life/...`,
// not peer-absolute), so they port to another peer's tree unchanged — which is
// why this stays transfer-safe.
//
// Reported to arch as the second phase-1 descriptor finding.

import (
	"fmt"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"

	"github.com/fxamacker/cbor/v2"
)

// Descriptor entity type names (kebab, per STYLE-NAMING-CONVENTIONS).
const (
	ProgramInterfaceType = "app/program/interface"
)

// Port roles — the coarse binding hint (proposal §2).
const (
	RoleState   = "state"
	RoleDisplay = "display"
	RoleInput   = "input"
)

// Port kinds (proposal §3). `snapshot` is a tree path (last-write-wins);
// `stream` is an inbox. All three shipped programs are snapshot-only.
const (
	KindSnapshot = "snapshot"
	KindStream   = "stream"
)

// Shapes — the driver ABI (exploration §4). `shape` selects the driver; `role`
// only hints. Grounded in the lineage of computer I/O, not in games, which is
// why `text` is here and is first-class.
//
// ShapeRawState is the declared ESCAPE HATCH, demoted by arch's §2 ruling: it
// stays in the vocabulary, but the falsification does not run through it. A
// "program-aware render" driver IS per-program code wearing a driver's coat.
const (
	ShapeText        = "text"         // char grid / stream — teletype → glass TTY → VT100
	ShapeDisplayList = "display-list" // polylines + kind tags — the vector display
	ShapeFramebuffer = "framebuffer"  // pixels — the raster display (no program uses it)
	ShapeKeySet      = "key-set"      // held-key bitmask snapshot — keyboard
	ShapeDirection   = "direction"    // latched enum snapshot — a d-pad / menu pick
	ShapeRawState    = "raw-state"    // the escape hatch; NOT a device lineage
)

// Tick modes (proposal §4).
const (
	TickClockDriven = "clock-driven"
	TickEventDriven = "event-driven"
)

// Shard forms (proposal §4/§4a). `static-k` is the Option-B floor that lands now
// (k frozen at authoring); `range` is the dynamic-k upgrade that waits on
// `compute/range`. This host drives static-k only.
const (
	ShardStaticK = "static-k"
	ShardRange   = "range"
)

// Orchestration models (proposal §4a). `host-managed` is the floor — the host
// loops the k evals and drives the stitch, synchronous/bounded failure, any
// peer. `continuation-managed` is the ceiling — fork/barrier/stitch as protocol
// (join + deliver_to), which travels but needs ext/continuation and the
// join-failure policy that is still open (§4b). This host drives host-managed
// only; continuation-managed is item 2.
const (
	OrchestrationHostManaged         = "host-managed"
	OrchestrationContinuationManaged = "continuation-managed"
)

// Stitch owners (proposal §4 Q4 — who authors the boundary bytes). This is the
// descriptive owner the descriptor names per model; it is provably identical
// across owners (parallel == serial == unsharded), but naming it is the ruling.
//   - `compute-gather` — a program-owned compute expression (the concat-free
//     gather), which returns the boundary to the program's own compute. This is
//     what the generic host drives: the stitch is just another eval, so the host
//     stays program-blind.
//   - `host` — Go host code concatenates (Axis-1's original form); a generic host
//     cannot do this without per-program knowledge, so it is not the host floor.
const (
	StitchOwnerComputeGather = "compute-gather"
	StitchOwnerHost          = "host"
)

// ProgramPort is `app/program/port`.
type ProgramPort struct {
	Name    string `cbor:"name"`
	Path    string `cbor:"path"`
	TypeRef string `cbor:"type_ref"`
	Kind    string `cbor:"kind"`
	Role    string `cbor:"role"`
	Shape   string `cbor:"shape"`

	// Source is the expression path to eval to materialize this port's value
	// into Path. Empty means the value is already at Path (the step wrote it).
	// See the header — this is the field the build surfaced as missing.
	Source string `cbor:"source,omitempty"`

	// Scene carries per-shape metadata a driver needs but cannot derive:
	// display-list {wrap, bounds}, text {mode, cols, rows}, key-set {keymap}.
	Scene map[string]interface{} `cbor:"scene,omitempty"`

	// Initial is the PATH of the seed value the host copies to Path before tick
	// 0 (F-E1: an unseeded input port read is a compute/error, so a program
	// with input ports cannot take its first tick until every one is seeded).
	// Input ports only. A path rather than a hash for the same reason as
	// Descriptor.InitialState — see the header.
	Initial string `cbor:"initial,omitempty"`
}

// ProgramTick is `app/program/tick`.
//
// OpCost is carried but UNUSED by the base host: arch ruled (§1) that the base
// mount contract does not shard — it is plain eval → put. Sharding is the opt-in
// extension that lands with `compute/range`. The field stays so a descriptor
// authored now is still valid then.
type ProgramTick struct {
	Mode     string `cbor:"mode"`
	RateHint uint64 `cbor:"rate_hint"`
	OpCost   uint64 `cbor:"op_cost,omitempty"`
}

// ProgramShard is `app/program/shard` — the sharding declaration (proposal
// §2/§4/§4a). Absent (nil) means the program is unsharded and the base mount
// contract applies (plain eval → put). Present means the host runs the sharded
// tick: k evals with a fresh budget each, then a stitch.
//
// ─── The build finding: arch's schema declares POLICY, not the ARTIFACTS ────
//
// Arch's `app/program/shard` (proposal §2) is {form, orchestration, k, n_field,
// stitch_owner}. Building the host against it surfaced the same gap the base
// descriptor already hit with step (the path-vs-hash finding): those five fields
// say *how* to shard and *who* owns the boundary, but not *what the host evals*.
// A host cannot eval a policy. The static-k floor is a family of k pre-authored
// shard expressions plus a stitch expression (proposal §4 "k hashes, k frozen at
// authoring"), and the host needs their evaluable PATHS — none of which the
// schema carries. So, exactly as with Step/InitialState, we add the addressable
// artifacts alongside the policy:
//
//	Shards       — the k shard expression paths (each a self-wrapping fragment)
//	FragmentBase — where the host writes fragment j before the stitch reads it
//	Stitch       — the stitch expression path (reads k fragments → state entity)
//
// Reported to arch as a descriptor finding (see docs/architecture/reviews/). It
// is additive; the policy fields are unchanged.
//
// ─── Why the shard expressions self-wrap into fragment entities ─────────────
//
// The host's whole falsifiable claim is that it names no program symbol. Its one
// rule is `put(path, eval(expr))` — it relocates whatever entity an eval
// produces, never decoding it. A sharded tick keeps that rule intact ONLY if the
// shard expression itself Constructs a field-accessible fragment entity (so the
// gather stitch can `lookup/tree` + `field` it): the host writes the fragment
// verbatim, exactly as it already writes the step result. If the host had to wrap
// a bare array into a fragment, it would need the field name — program knowledge.
// So self-wrapping is not an authoring nicety; it is what keeps the host blind.
type ProgramShard struct {
	// Form — "static-k" (the floor) or "range" (dynamic-k upgrade). Host drives
	// static-k only.
	Form string `cbor:"form"`

	// Orchestration — "host-managed" (the floor) or "continuation-managed" (the
	// ceiling, item 2). Host drives host-managed only.
	Orchestration string `cbor:"orchestration"`

	// K — the frozen shard count (static-k form). len(Shards) MUST equal K.
	K uint64 `cbor:"k"`

	// NField — the state field giving N, for the dynamic-k (range) form. Unused
	// by static-k (k is frozen, boundaries baked into the shard/stitch exprs).
	NField string `cbor:"n_field,omitempty"`

	// StitchOwner — who authors the boundary bytes (§4 Q4). "compute-gather" is
	// the program-owned form this host drives; "host" is the Go-concat form.
	StitchOwner string `cbor:"stitch_owner,omitempty"`

	// Shards — the k shard expression paths (the build finding). Each evals to a
	// self-wrapping fragment entity the host writes under FragmentBase.
	Shards []string `cbor:"shards"`

	// FragmentBase — the path prefix the host writes fragment j to
	// (`{FragmentBase}/frag{j}`), where the stitch reads them. The build finding.
	FragmentBase string `cbor:"fragment_base"`

	// Stitch — the stitch expression path: reads the k fragments and produces the
	// whole state entity, which the host writes to StatePath. The build finding.
	Stitch string `cbor:"stitch"`
}

// ProgramDescriptor is `app/program/interface` — the whole manifest.
//
// Step and InitialState are evaluable/readable PATHS, not hashes — see the
// header's second deviation. The *Hash fields carry the content hashes for
// verification and for phase-2 transfer.
type ProgramDescriptor struct {
	StatePath string `cbor:"state_path"`

	// InitialState is the path holding state₀. Restart re-copies it to
	// StatePath, which is why state₀ must persist somewhere other than the
	// live state path.
	InitialState string `cbor:"initial_state"`

	// Step is the path of the step expression: (state, input…) → state'.
	Step string `cbor:"step"`

	StepHash         string `cbor:"step_hash,omitempty"`
	InitialStateHash string `cbor:"initial_state_hash,omitempty"`

	InputPorts  []ProgramPort `cbor:"input_ports"`
	OutputPorts []ProgramPort `cbor:"output_ports"`
	Tick        ProgramTick   `cbor:"tick"`

	// Shard is the optional sharding declaration (proposal §2). Absent = the base
	// mount contract (plain eval → put). Present = the host runs the sharded tick.
	Shard *ProgramShard `cbor:"shard,omitempty"`
}

// Validate checks the descriptor is self-consistent before a host acts on it.
// A host that mounts a malformed descriptor half-way is worse than one that
// refuses: the admission rule (exploration §6) is that a host cleanly refuses
// what it cannot drive and says why — never a half-render.
func (d *ProgramDescriptor) Validate() error {
	if d.StatePath == "" {
		return fmt.Errorf("descriptor: state_path is required")
	}
	if d.Step == "" {
		return fmt.Errorf("descriptor: step is required")
	}
	if d.InitialState == "" {
		return fmt.Errorf("descriptor: initial_state is required")
	}
	if d.InitialState == d.StatePath {
		// state₀ must outlive the first tick, or Restart has nothing to restore.
		return fmt.Errorf("descriptor: initial_state must not be state_path")
	}
	switch d.Tick.Mode {
	case TickClockDriven:
		if d.Tick.RateHint == 0 {
			return fmt.Errorf("descriptor: clock-driven tick needs a rate_hint")
		}
	case TickEventDriven:
	default:
		return fmt.Errorf("descriptor: unknown tick mode %q", d.Tick.Mode)
	}
	for _, p := range d.InputPorts {
		if err := p.validate("input"); err != nil {
			return err
		}
		// F-E1 is per-port: an input port with no seed cannot take tick 0.
		if p.Initial == "" {
			return fmt.Errorf("descriptor: input port %q has no initial (F-E1)", p.Name)
		}
		if p.Source != "" {
			return fmt.Errorf("descriptor: input port %q has a source (only output ports project)", p.Name)
		}
	}
	for _, p := range d.OutputPorts {
		if err := p.validate("output"); err != nil {
			return err
		}
	}
	if d.Shard != nil {
		if err := d.Shard.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the shard block is self-consistent. It does NOT reject forms
// or orchestration models this host cannot drive — a descriptor may declare a
// range/continuation-managed shard and still be valid; whether a given host can
// mount it is the host's admission call (program_host.go), not the descriptor's.
// The descriptor's job is only structural integrity.
func (s *ProgramShard) validate() error {
	switch s.Form {
	case ShardStaticK:
		if s.K == 0 {
			return fmt.Errorf("descriptor: static-k shard needs k >= 1")
		}
		if uint64(len(s.Shards)) != s.K {
			return fmt.Errorf("descriptor: static-k shard declares k=%d but %d shard paths", s.K, len(s.Shards))
		}
		if s.Stitch == "" {
			return fmt.Errorf("descriptor: static-k shard needs a stitch path")
		}
		if s.FragmentBase == "" {
			return fmt.Errorf("descriptor: static-k shard needs a fragment_base")
		}
	case ShardRange:
		// The dynamic-k form is declarable but this host cannot drive it yet
		// (waits on compute/range). Structural check only: it names no static k.
	case "":
		return fmt.Errorf("descriptor: shard block has no form")
	default:
		return fmt.Errorf("descriptor: unknown shard form %q", s.Form)
	}
	switch s.Orchestration {
	case OrchestrationHostManaged, OrchestrationContinuationManaged:
	case "":
		return fmt.Errorf("descriptor: shard block has no orchestration")
	default:
		return fmt.Errorf("descriptor: unknown shard orchestration %q", s.Orchestration)
	}
	return nil
}

func (p *ProgramPort) validate(dir string) error {
	if p.Name == "" {
		return fmt.Errorf("descriptor: %s port has no name", dir)
	}
	if p.Path == "" {
		return fmt.Errorf("descriptor: %s port %q has no path", dir, p.Name)
	}
	if p.TypeRef == "" {
		return fmt.Errorf("descriptor: %s port %q has no type_ref", dir, p.Name)
	}
	switch p.Kind {
	case KindSnapshot, KindStream:
	default:
		return fmt.Errorf("descriptor: %s port %q has unknown kind %q", dir, p.Name, p.Kind)
	}
	if p.Shape == "" {
		return fmt.Errorf("descriptor: %s port %q has no shape", dir, p.Name)
	}
	return nil
}

// Entity encodes the descriptor as an `app/program/interface` entity.
func (d *ProgramDescriptor) Entity() (entity.Entity, error) {
	raw, err := ecf.Encode(d)
	if err != nil {
		return entity.Entity{}, fmt.Errorf("descriptor: encode: %w", err)
	}
	return entity.NewEntity(ProgramInterfaceType, cbor.RawMessage(raw))
}

// DecodeDescriptor reads a descriptor out of its entity.
func DecodeDescriptor(ent entity.Entity) (*ProgramDescriptor, error) {
	if ent.Type != ProgramInterfaceType {
		return nil, fmt.Errorf("descriptor: expected %s, got %s", ProgramInterfaceType, ent.Type)
	}
	var d ProgramDescriptor
	if err := ecf.Decode(ent.Data, &d); err != nil {
		return nil, fmt.Errorf("descriptor: decode: %w", err)
	}
	// `scene` is a free-form map, so it is the one place a CBOR-decode artifact
	// can reach a driver. Normalize it once, here, rather than in each renderer.
	for i := range d.InputPorts {
		d.InputPorts[i].Scene = normalizeScene(d.InputPorts[i].Scene)
	}
	for i := range d.OutputPorts {
		d.OutputPorts[i].Scene = normalizeScene(d.OutputPorts[i].Scene)
	}
	return &d, nil
}

// normalizeScene makes a CBOR-decoded scene safe for any consumer.
//
// The problem, concretely: CBOR decodes a NESTED map as
// map[interface{}]interface{}, because CBOR keys need not be strings. The
// top-level scene is typed map[string]interface{} so it decodes fine — but
// Asteroids' `scene.keymap` (bit → action) is nested, so it comes back
// interface-keyed, and encoding/json refuses it outright:
//
//	json: unsupported type: map[interface {}]interface {}
//
// Only Asteroids hit it: Life's and Snake's scenes are flat (mode/cols/rows).
// That is the pattern this whole rung keeps producing — the two grids agree with
// each other and hide the question, and the third program asks it.
//
// This is a decode artifact, not a JSON concern, so it is fixed where the
// descriptor is decoded: a scene handed to a driver is string-keyed all the way
// down, whatever the renderer does with it next.
func normalizeScene(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, vv := range t {
			out[fmt.Sprint(k)] = normalizeValue(vv)
		}
		return out
	case map[string]interface{}:
		return normalizeScene(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, vv := range t {
			out[i] = normalizeValue(vv)
		}
		return out
	default:
		return v
	}
}
