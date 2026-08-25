package entitysdk_test

// EXPERIMENT-ASTEROIDS (Exp-F) — the first HETEROGENEOUS, VARIABLE-SET actor
// simulation as a compute program. Life/Snake/Tetris are all uniform (a `map`
// over a fixed grid); this is the first probe whose actor set changes length
// every tick and whose actors read *each other*.
//
// Arch context (entity-system-architecture):
//   docs/status/HANDOFF-2026-07-16-compute-heterogeneous-actor-probe.md — the ask
//   docs/research/explorations/EXPLORATION-COMPUTE-PROGRAM-RUNTIME-CONTRACT.md
//     §7.1  what Doom introduces: variable actor set + cross-actor spatial query
//     §7.2  blockmap/BSP — "the spatial machinery is expressible" (tested here
//           under real load, all-pairs first)
//     §8    the port taxonomy (snapshot vs stream; display/framebuffer)
//     §10.3 the non-determinism boundary — RNG enters as INPUT
//   docs/proposals/PROPOSAL-COMPUTE-COLLECTION-PRIMITIVES.md — array-concat;
//     this probe is its SECOND data point (§3.1 of the handoff).
//
// The three frontier shapes, and how each is lowered here:
//
//   1. VARIABLE ACTOR SET — asteroids split, bullets spawn/expire, ship dies.
//      Lowered as a FIXED-CAPACITY actor array with kind=0 meaning "free slot"
//      (Doom's own fixed mobj cap — canonical, not a workaround). The state
//      shape stays fixed, so array-concat is sidestepped entirely. Whether
//      that is *clean* is the headline question this probe reports on.
//
//   2. CROSS-ACTOR SPATIAL QUERY — collision. Naive all-pairs: each actor
//      filters the whole (frozen, previous) actor set for colliders. Every
//      read is of state_{n-1}, so the step stays pure and independent per
//      actor over read-only data — the exact property that let Life shard.
//
//   3. HETEROGENEOUS THINK — if/else on a `kind` field inside the actor map's
//      closure selects per-type logic. (Arch's expectation; confirmed, see the
//      findings block below.)
//
// STATE ENCODING — struct-of-arrays (SoA), all fields flat arrays indexed by
// slot. This is deliberate and is itself a finding (F-F1 below): the natural
// array-of-structs encoding materializes each actor to its OWN content-
// addressed entity and puts 33-byte hash refs in the state array, which would
// cost the host CAP content-store lookups to read one frame. So the step
// builds actors array-of-structs IN-FLIGHT (compute/construct values, on which
// compute/field works directly — ext/compute/eval_construct.go evalField case
// *constructedValue) and PROJECTS them to flat arrays at the boundary. The
// expensive per-actor logic runs once; the field-extraction maps are cheap.
//
// FIXED-POINT — no floats, no division. Positions/velocities are int64 in
// 1/256 units. Rotation is 16 steps with sin/cos LOOKUP TABLES emitted as
// literal arrays, PRE-SCALED by the acceleration/speed constant so no division
// is ever needed (Doom's finesine table, same trick). This sidesteps the
// F-E "div is true division" trap completely rather than lowering around it.
//
// ⚠️ LCG: reads the HIGH bits — (s>>16)%N, lowered as floor-div by 65536 then
// mod. F-D3 burned this track once: a power-of-two-modulus LCG has period 2^k
// in its low k bits, every row came out identical, and it sat at a plausible
// density so every rule-level test waved it through. The durable lesson is the
// ASSERTION, not the seed: TestAsteroidsAntiVacuity below asserts structurally
// (distinct actors, spawns actually occurred, splits actually occurred), never
// statistically.

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"

	"github.com/fxamacker/cbor/v2"
)

// --- shape constants -------------------------------------------------------

const (
	astStateType = "app/asteroids/state"
	astInputType = "app/asteroids/input"

	// actor kinds. 0 = free slot (the "live flag", inverted).
	astFree     = uint64(0)
	astShip     = uint64(1)
	astAsteroid = uint64(2)
	astBullet   = uint64(3)

	// fixed-point: 1 world unit = astFP sub-units.
	astFP    = int64(256)
	astWorld = int64(256) * astFP // 256x256 world, wraps

	astRotSteps = uint64(16)

	// pre-scaled table magnitudes (see astCosTable): no division at eval.
	astThrustAcc  = int64(24)  // per-tick dv while thrusting (FP units)
	astBulletSpd  = int64(768) // bullet velocity (FP units/tick) = 3 units
	astBulletTTL  = uint64(18)
	astShipRadius = int64(3) * astFP
	astAstRadius  = int64(6) * astFP // scaled by size below

	// input bits (the held-key snapshot port — a SET, not one value).
	astKeyLeft   = 0
	astKeyRight  = 1
	astKeyThrust = 2
	astKeyFire   = 3

	astLCGMul = uint64(1103515245)
	astLCGAdd = uint64(12345)
	astLCGMod = uint64(2147483648)
)

// astCapacity is the fixed mobj cap. Slot 0 is the ship by convention.
const astCapacity = 24

// --- state -----------------------------------------------------------------

// astState is struct-of-arrays: every field is a length-astCapacity array
// indexed by slot. See the header note on why this is not array-of-structs.
type astState struct {
	Kinds  []uint64 `cbor:"kinds"`
	Xs     []int64  `cbor:"xs"`
	Ys     []int64  `cbor:"ys"`
	Vxs    []int64  `cbor:"vxs"`
	Vys    []int64  `cbor:"vys"`
	Rots   []uint64 `cbor:"rots"`
	Szs    []uint64 `cbor:"szs"`  // asteroid size 1..3; 0 otherwise
	Ttls   []uint64 `cbor:"ttls"` // bullet lifetime; 0 otherwise
	RNG    uint64   `cbor:"rng"`
	Score  uint64   `cbor:"score"`
	Status uint64   `cbor:"status"` // 0 = playing, 1 = ship destroyed
}

func astStateEntity(s astState) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{
		"kinds": s.Kinds, "xs": s.Xs, "ys": s.Ys,
		"vxs": s.Vxs, "vys": s.Vys, "rots": s.Rots,
		"szs": s.Szs, "ttls": s.Ttls,
		"rng": s.RNG, "score": s.Score, "status": s.Status,
	})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(astStateType, cbor.RawMessage(raw))
}

// astSeed: ship at world centre facing up, plus `nAst` size-3 asteroids placed
// deterministically around it with distinct drift velocities. Deterministic by
// construction — the seed is state_0, so a game stays a pure function of
// (state_0, input-stream) per §10.3.
func astSeed(nAst int, rng uint64) astState {
	s := astState{
		Kinds: make([]uint64, astCapacity),
		Xs:    make([]int64, astCapacity),
		Ys:    make([]int64, astCapacity),
		Vxs:   make([]int64, astCapacity),
		Vys:   make([]int64, astCapacity),
		Rots:  make([]uint64, astCapacity),
		Szs:   make([]uint64, astCapacity),
		Ttls:  make([]uint64, astCapacity),
		RNG:   rng % astLCGMod,
	}
	s.Kinds[0] = astShip
	s.Xs[0] = astWorld / 2
	s.Ys[0] = astWorld / 2
	s.Rots[0] = 0

	// Asteroids on a ring, each with a different heading — no RNG needed at
	// seed time, so the placement is inspectable and the test is structural.
	for i := 0; i < nAst && i+1 < astCapacity; i++ {
		slot := i + 1
		ang := 2 * math.Pi * float64(i) / float64(nAst)
		s.Kinds[slot] = astAsteroid
		s.Xs[slot] = astWorld/2 + int64(math.Round(math.Cos(ang)*float64(80*astFP)))
		s.Ys[slot] = astWorld/2 + int64(math.Round(math.Sin(ang)*float64(80*astFP)))
		s.Vxs[slot] = int64(math.Round(math.Cos(ang+1.0) * 96))
		s.Vys[slot] = int64(math.Round(math.Sin(ang+1.0) * 96))
		s.Szs[slot] = 3
	}
	return s
}

// astSeedDuel: ship at centre facing up (rot 0) with ONE size-3 asteroid 60
// units directly above it, drifting slowly sideways.
//
// This seed exists to make the interesting branches REACHABLE and therefore
// the differential test non-vacuous: a bullet fired at tick 1 travels up at 3
// units/tick and reaches the asteroid's 18-unit hit radius around tick 14
// (well inside the 18-tick TTL), forcing a SPLIT; the halves deflect apart, so
// later shots force further splits and eventually a size-1 kill (score).
// The asteroid is far enough (60 units vs. the 21-unit ship+asteroid radius)
// that the ship survives long enough to shoot.
func astSeedDuel(rng uint64) astState {
	s := astState{
		Kinds: make([]uint64, astCapacity),
		Xs:    make([]int64, astCapacity),
		Ys:    make([]int64, astCapacity),
		Vxs:   make([]int64, astCapacity),
		Vys:   make([]int64, astCapacity),
		Rots:  make([]uint64, astCapacity),
		Szs:   make([]uint64, astCapacity),
		Ttls:  make([]uint64, astCapacity),
		RNG:   rng % astLCGMod,
	}
	s.Kinds[0] = astShip
	s.Xs[0] = astWorld / 2
	s.Ys[0] = astWorld / 2

	s.Kinds[1] = astAsteroid
	s.Xs[1] = astWorld / 2
	s.Ys[1] = astWorld/2 - 60*astFP
	s.Vxs[1] = 32 // slow sideways drift so split halves separate
	s.Szs[1] = 3
	return s
}

// --- trig tables (pre-scaled; emitted as compute literals) ------------------

// astTable builds a length-astRotSteps table of round(mag*cos(theta+phase)).
// Pre-scaling by `mag` is what removes division from the whole program.
func astTable(mag int64, phase float64) []int64 {
	t := make([]int64, astRotSteps)
	for i := range t {
		ang := 2*math.Pi*float64(i)/float64(astRotSteps) + phase
		t[i] = int64(math.Round(float64(mag) * math.Cos(ang)))
	}
	return t
}

// Heading rot=i is angle θ=2πi/16 measured clockwise from "up", so the unit
// direction is (sin θ, -cos θ) and rot 0 = up (-y), rot 4 = right (+x).
// Both components are expressed as a phase-shifted cos so one table builder
// serves both: sin θ = cos(θ-π/2), and -cos θ = cos(θ+π).
func astCosTable(mag int64) []int64 { return astTable(mag, -math.Pi/2) } // → dx
func astSinTable(mag int64) []int64 { return astTable(mag, math.Pi) }    // → dy

// --- input port ------------------------------------------------------------

// astInputEntity writes the held-key SET as a bitmask (one integer), plus the
// tick's RNG draw is NOT here — RNG lives in state and advances per tick.
//
// This is the held-key finding (F-F3): Snake's port is a single last-write-wins
// direction, which cannot express "thrust + rotate + fire at once". A bitmask
// keeps the port a SNAPSHOT (sample the current key set at the tick boundary)
// and needs NO new port kind — the §8 taxonomy's "Input — keys/buttons | stream"
// row is about discrete EVENTS, not held state.
func astInputEntity(keys uint64) (entity.Entity, error) {
	raw, err := ecf.Encode(map[string]interface{}{"keys": keys})
	if err != nil {
		return entity.Entity{}, err
	}
	return entity.NewEntity(astInputType, cbor.RawMessage(raw))
}

func astKeys(bits ...int) uint64 {
	var k uint64
	for _, b := range bits {
		k |= 1 << uint(b)
	}
	return k
}

// --- Go-side oracle --------------------------------------------------------
//
// astNext mirrors the compute step EXACTLY — same branch structure, same
// tables, same LCG, same slot-allocation ranks. Any divergence is a step bug.
// This is the differential half of the Axis-1 oracle discipline: the compute
// result is checked field-by-field against this AND against the hand-built
// entity hash at the materialized boundary.

func astWrapGo(v, d int64) int64 { return (v + d + astWorld) % astWorld }

func astDistSqGo(s astState, i, j int) int64 {
	dx := s.Xs[i] - s.Xs[j]
	dy := s.Ys[i] - s.Ys[j]
	return dx*dx + dy*dy
}

// astBulletHitsGo: is asteroid j hit by any live bullet? (mirrors bulletHits)
func astBulletHitsGo(s astState, j int) bool {
	r := astAstRadius * int64(s.Szs[j])
	for b := 0; b < astCapacity; b++ {
		if s.Kinds[b] == astBullet && astDistSqGo(s, j, b) < r*r {
			return true
		}
	}
	return false
}

// astBulletSpentGo: did bullet b hit any asteroid? (mirrors bulletSpent)
func astBulletSpentGo(s astState, b int) bool {
	for a := 0; a < astCapacity; a++ {
		if s.Kinds[a] != astAsteroid {
			continue
		}
		r := astAstRadius * int64(s.Szs[a])
		if astDistSqGo(s, a, b) < r*r {
			return true
		}
	}
	return false
}

func astShipHitGo(s astState) bool {
	for a := 0; a < astCapacity; a++ {
		if s.Kinds[a] != astAsteroid {
			continue
		}
		r := astShipRadius + astAstRadius*int64(s.Szs[a])
		if astDistSqGo(s, a, 0) < r*r {
			return true
		}
	}
	return false
}

func astBitGo(keys uint64, k int) bool { return (keys>>uint(k))&1 == 1 }

func astNext(s astState, keys uint64) astState {
	if s.Status == 1 {
		return s
	}
	firing := astBitGo(keys, astKeyFire)
	shipHit := astShipHitGo(s)

	splits := []int{}
	for j := 0; j < astCapacity; j++ {
		if s.Kinds[j] == astAsteroid && s.Szs[j] > 1 && astBulletHitsGo(s, j) {
			splits = append(splits, j)
		}
	}

	out := astState{
		Kinds: make([]uint64, astCapacity),
		Xs:    make([]int64, astCapacity),
		Ys:    make([]int64, astCapacity),
		Vxs:   make([]int64, astCapacity),
		Vys:   make([]int64, astCapacity),
		Rots:  make([]uint64, astCapacity),
		Szs:   make([]uint64, astCapacity),
		Ttls:  make([]uint64, astCapacity),
	}
	cosThrust, sinThrust := astCosTable(astThrustAcc), astSinTable(astThrustAcc)
	cosBullet, sinBullet := astCosTable(astBulletSpd), astSinTable(astBulletSpd)

	for i := 0; i < astCapacity; i++ {
		switch s.Kinds[i] {
		case astShip:
			d := uint64(0)
			if astBitGo(keys, astKeyRight) {
				d = 1
			} else if astBitGo(keys, astKeyLeft) {
				d = astRotSteps - 1
			}
			nrot := (s.Rots[i] + d) % astRotSteps
			nvx, nvy := s.Vxs[i], s.Vys[i]
			if astBitGo(keys, astKeyThrust) {
				nvx += cosThrust[nrot]
				nvy += sinThrust[nrot]
			}
			out.Kinds[i] = astShip
			if shipHit {
				out.Kinds[i] = astFree
			}
			out.Xs[i] = astWrapGo(s.Xs[i], nvx)
			out.Ys[i] = astWrapGo(s.Ys[i], nvy)
			out.Vxs[i], out.Vys[i] = nvx, nvy
			out.Rots[i] = nrot

		case astAsteroid:
			if astBulletHitsGo(s, i) {
				if s.Szs[i] > 1 {
					out.Kinds[i] = astAsteroid
					out.Xs[i], out.Ys[i] = s.Xs[i], s.Ys[i]
					out.Vxs[i], out.Vys[i] = -s.Vys[i], s.Vxs[i]
					out.Rots[i] = s.Rots[i]
					out.Szs[i] = s.Szs[i] - 1
				} // else: stays free (zero value)
			} else {
				out.Kinds[i] = astAsteroid
				out.Xs[i] = astWrapGo(s.Xs[i], s.Vxs[i])
				out.Ys[i] = astWrapGo(s.Ys[i], s.Vys[i])
				out.Vxs[i], out.Vys[i] = s.Vxs[i], s.Vys[i]
				out.Rots[i], out.Szs[i] = s.Rots[i], s.Szs[i]
			}

		case astBullet:
			if s.Ttls[i] <= 1 || astBulletSpentGo(s, i) {
				break // free
			}
			out.Kinds[i] = astBullet
			out.Xs[i] = astWrapGo(s.Xs[i], s.Vxs[i])
			out.Ys[i] = astWrapGo(s.Ys[i], s.Vys[i])
			out.Vxs[i], out.Vys[i] = s.Vxs[i], s.Vys[i]
			out.Rots[i] = s.Rots[i]
			out.Ttls[i] = s.Ttls[i] - 1

		default: // free slot — claim a spawn by rank
			r := 0
			for j := 0; j < i; j++ {
				if s.Kinds[j] == astFree {
					r++
				}
			}
			if firing && r == 0 {
				sr := s.Rots[0]
				out.Kinds[i] = astBullet
				out.Xs[i] = astWrapGo(s.Xs[0], cosBullet[sr])
				out.Ys[i] = astWrapGo(s.Ys[0], sinBullet[sr])
				out.Vxs[i], out.Vys[i] = cosBullet[sr], sinBullet[sr]
				out.Rots[i] = sr
				out.Ttls[i] = astBulletTTL
				break
			}
			j := r
			if firing {
				j = r - 1
			}
			if j < len(splits) {
				src := splits[j]
				out.Kinds[i] = astAsteroid
				out.Xs[i], out.Ys[i] = s.Xs[src], s.Ys[src]
				out.Vxs[i], out.Vys[i] = s.Vys[src], -s.Vxs[src]
				out.Rots[i] = s.Rots[src]
				out.Szs[i] = s.Szs[src] - 1
			}
		}
	}

	scored := 0
	for j := 0; j < astCapacity; j++ {
		if s.Kinds[j] == astAsteroid && s.Szs[j] == 1 && astBulletHitsGo(s, j) {
			scored++
		}
	}
	out.RNG = (s.RNG*astLCGMul + astLCGAdd) % astLCGMod
	out.Score = s.Score + uint64(scored)
	if shipHit {
		out.Status = 1
	}
	return out
}

// --- the step expression (the PROGRAM) -------------------------------------

// buildAsteroidsStepExpr lowers one tick: (state, input) -> state'.
//
// Lowering rules honored (POC findings review doc + Exp-D/E):
//
//	F-D1  integer floor-div = div(sub(a, mod(a,b)), b)  — used ONLY for the
//	      key-bitmask bit extraction; the sim itself is division-free.
//	F-E2a partial ops guarded behind lazy `if` (index only when in-bounds) —
//	      critical for the spawn-source lookup: index(splits, j) is only
//	      reachable once j < length(splits) is established.
//	F-E2b rare-path work inside the branch, not a let (let is EAGER).
//	let*  binding order is SORTED-name order — names are chosen to sort right.
func buildAsteroidsStepExpr(ap *entitysdk.AppPeer, statePath, inputPath string) *entitysdk.Builder {
	return buildAsteroidsStepRange(ap, statePath, inputPath, 0, astCapacity)
}

// buildAsteroidsStepRange lowers the step for slots [i0,i1) only. i0=0,
// i1=astCapacity is the whole tick; any narrower range is a SHARD.
//
// The shard split is by SLOT INDEX, not geometry — the same move as the Life
// shard rig (axis1_shard_test.go), and it ports here almost unchanged.
//
// The property that makes it sound is the one arch predicted (§3.2): every
// actor reads the FROZEN PREVIOUS whole state, so the per-actor step is
// independent over read-only data. Note what that means concretely — the
// collision/rank/split filters below keep iterating the FULL index set
// (idxLit) even in a shard; only the actor map's collection narrows to the
// shard's slots (slotLit). A shard reads everything and computes a strip.
//
// A shard returns the same field shape as a full tick, but its arrays cover
// only its slots — a FRAGMENT, which the host stitches. Per the Life rig's §5
// finding, fragments have no meaningful content hash; only the stitched whole
// does.
func buildAsteroidsStepRange(ap *entitysdk.AppPeer, statePath, inputPath string, i0, i1 int) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope
	full := i0 == 0 && i1 == astCapacity

	// idxLit — the FULL actor set. Every cross-actor query reads all of it,
	// in a shard exactly as in a full tick.
	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	idxLit := c.Literal(indices)

	// slotLit — the slots THIS eval is responsible for producing.
	slots := make([]uint64, 0, i1-i0)
	for i := i0; i < i1; i++ {
		slots = append(slots, uint64(i))
	}
	slotLit := c.Literal(slots)

	// floorDiv: F-D1. Only sound for non-negative a (the bitmask case).
	floorDiv := func(a, b *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("div", c.Arithmetic("sub", a, c.Arithmetic("mod", a, b)), b)
	}
	// bit k of the held-key snapshot.
	bit := func(k int) *entitysdk.Builder {
		return c.Compare("eq",
			c.Arithmetic("mod", floorDiv(sc("keys"), c.Literal(uint64(1)<<uint(k))), c.Literal(uint64(2))),
			c.Literal(uint64(1)))
	}

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}

	// wrap: (v + delta + WORLD) mod WORLD. Keeps the operand non-negative so
	// `mod` never sees a negative (|delta| < WORLD always holds here).
	wrap := func(v, d *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("mod",
			c.Arithmetic("add", c.Arithmetic("add", v, d), c.Literal(astWorld)),
			c.Literal(astWorld))
	}

	// distSq(i, j) over the FROZEN previous state. No wrap-around distance —
	// collisions across the world seam are missed; noted as a probe
	// simplification, not a lowering constraint (see the findings block).
	distSq := func(i, j *entitysdk.Builder) *entitysdk.Builder {
		dx := c.Arithmetic("sub", at("xs", i), at("xs", j))
		dy := c.Arithmetic("sub", at("ys", i), at("ys", j))
		return c.Arithmetic("add",
			c.Arithmetic("mul", dx, dx),
			c.Arithmetic("mul", dy, dy))
	}

	// astHitRadiusSq(j): squared collision radius of asteroid j, scaled by size.
	astHitRadiusSq := func(j *entitysdk.Builder) *entitysdk.Builder {
		r := c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", j))
		return c.Arithmetic("mul", r, r)
	}

	// --- cross-actor spatial query (the O(N^2) all-pairs core) --------------

	// bulletHits(j): is asteroid j hit by any live bullet? filter+length over
	// the whole frozen actor set. This is the capturing-closure shape F-D2
	// punishes (the enclosing let's arrays are captured per element) — which is
	// exactly why this probe is worth measuring on Axis-1.
	bulletHitsRaw := func(j *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt",
			c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": idxLit,
				"fn": c.Lambda([]string{"b"},
					c.Logic("and",
						c.Compare("eq", at("kinds", sc("b")), c.Literal(astBullet)),
						c.Compare("lt", distSq(j, sc("b")), astHitRadiusSq(j)))),
			})),
			c.Literal(uint64(0)))
	}

	// bulletHits indexes the PRECOMPUTED per-actor hit array ("hitb", bound once
	// below) instead of re-scanning every bullet.
	//
	// Whether asteroid j was hit is a function of the frozen previous state
	// alone, but it is needed in THREE places (the split scan, the asteroid's
	// own think, and the score tally) — so the naive lowering ran the same
	// O(CAP) scan three times per asteroid. Hoisting is a pure refactor, and
	// F2's differential against the Go oracle is what proves it: if this changed
	// a single field on a single tick, F2 fails.
	bulletHits := func(j *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc("hitb"), j)
	}

	// bulletSpent(b): did bullet b hit any asteroid this tick?
	bulletSpent := func(b *entitysdk.Builder) *entitysdk.Builder {
		return c.Compare("gt",
			c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": idxLit,
				"fn": c.Lambda([]string{"a"},
					c.Logic("and",
						c.Compare("eq", at("kinds", sc("a")), c.Literal(astAsteroid)),
						c.Compare("lt", distSq(sc("a"), b), astHitRadiusSq(sc("a"))))),
			})),
			c.Literal(uint64(0)))
	}

	// shipHit: does any asteroid overlap the ship (slot 0)?
	shipHit := c.Compare("gt",
		c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn": c.Lambda([]string{"a"},
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("a")), c.Literal(astAsteroid)),
					c.Compare("lt",
						distSq(sc("a"), c.Literal(uint64(0))),
						c.Arithmetic("mul",
							c.Arithmetic("add", c.Literal(astShipRadius),
								c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", sc("a")))),
							c.Arithmetic("add", c.Literal(astShipRadius),
								c.Arithmetic("mul", c.Literal(astAstRadius), at("szs", sc("a")))))))),
		})),
		c.Literal(uint64(0)))

	// --- the fixed-cap slot allocator (the concat sidestep) -----------------
	//
	// `splits` = the asteroids that were hit AND are big enough to split; each
	// contributes exactly ONE new asteroid (the other half reuses the source's
	// own slot). A free slot claims a spawn by its RANK among free slots:
	// rank 0 goes to the new bullet (if firing), the rest to splits in order.
	//
	// This is the fixed-cap tax, and it is O(CAP) per free slot (the rank
	// filter) on top of O(CAP) per actor (collision) — see F-F2.
	splits := c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": idxLit,
		"fn": c.Lambda([]string{"j"},
			c.Logic("and",
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("j")), c.Literal(astAsteroid)),
					c.Compare("gt", at("szs", sc("j")), c.Literal(uint64(1)))),
				bulletHits(sc("j")))),
	})

	// rank(i): how many free slots precede slot i.
	rank := func(i *entitysdk.Builder) *entitysdk.Builder {
		return c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn": c.Lambda([]string{"j"},
				c.Logic("and",
					c.Compare("lt", sc("j"), i),
					c.Compare("eq", at("kinds", sc("j")), c.Literal(astFree)))),
		}))
	}

	free := c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
		"kind": c.Literal(astFree),
		"x":    c.Literal(int64(0)), "y": c.Literal(int64(0)),
		"vx": c.Literal(int64(0)), "vy": c.Literal(int64(0)),
		"rot": c.Literal(uint64(0)), "sz": c.Literal(uint64(0)),
		"ttl": c.Literal(uint64(0)),
	})

	// --- per-kind think -----------------------------------------------------

	// ship: rotate (held left/right), thrust along heading, wrap, die on hit.
	shipThink := c.Let(map[string]*entitysdk.Builder{
		"nrot": c.Arithmetic("mod",
			c.Arithmetic("add", at("rots", sc("i")),
				c.If(bit(astKeyRight), c.Literal(uint64(1)),
					c.If(bit(astKeyLeft), c.Arithmetic("sub", c.Literal(astRotSteps), c.Literal(uint64(1))),
						c.Literal(uint64(0))))),
			c.Literal(astRotSteps)),
	}, c.Let(map[string]*entitysdk.Builder{
		"nvx": c.If(bit(astKeyThrust),
			c.Arithmetic("add", at("vxs", sc("i")),
				c.Index(c.Literal(astCosTable(astThrustAcc)), sc("nrot"))),
			at("vxs", sc("i"))),
		"nvy": c.If(bit(astKeyThrust),
			c.Arithmetic("add", at("vys", sc("i")),
				c.Index(c.Literal(astSinTable(astThrustAcc)), sc("nrot"))),
			at("vys", sc("i"))),
	}, c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
		"kind": c.If(shipHit, c.Literal(astFree), c.Literal(astShip)),
		"x":    wrap(at("xs", sc("i")), sc("nvx")),
		"y":    wrap(at("ys", sc("i")), sc("nvy")),
		"vx":   sc("nvx"), "vy": sc("nvy"),
		"rot": sc("nrot"), "sz": c.Literal(uint64(0)),
		"ttl": c.Literal(uint64(0)),
	})))

	// asteroid: drift + wrap; on hit either shrink in place (sz>1) or die.
	asteroidThink := c.If(bulletHits(sc("i")),
		c.If(c.Compare("gt", at("szs", sc("i")), c.Literal(uint64(1))),
			// shrink in place; velocity deflects one rotation step.
			c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
				"kind": c.Literal(astAsteroid),
				"x":    at("xs", sc("i")), "y": at("ys", sc("i")),
				"vx":  c.Arithmetic("sub", c.Literal(int64(0)), at("vys", sc("i"))),
				"vy":  at("vxs", sc("i")),
				"rot": at("rots", sc("i")),
				"sz":  c.Arithmetic("sub", at("szs", sc("i")), c.Literal(uint64(1))),
				"ttl": c.Literal(uint64(0)),
			}),
			free),
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(astAsteroid),
			"x":    wrap(at("xs", sc("i")), at("vxs", sc("i"))),
			"y":    wrap(at("ys", sc("i")), at("vys", sc("i"))),
			"vx":   at("vxs", sc("i")), "vy": at("vys", sc("i")),
			"rot": at("rots", sc("i")), "sz": at("szs", sc("i")),
			"ttl": c.Literal(uint64(0)),
		}))

	// bullet: move + wrap, age out, die on impact.
	bulletThink := c.If(
		c.Logic("or",
			c.Compare("lte", at("ttls", sc("i")), c.Literal(uint64(1))),
			bulletSpent(sc("i"))),
		free,
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(astBullet),
			"x":    wrap(at("xs", sc("i")), at("vxs", sc("i"))),
			"y":    wrap(at("ys", sc("i")), at("vys", sc("i"))),
			"vx":   at("vxs", sc("i")), "vy": at("vys", sc("i")),
			"rot": at("rots", sc("i")), "sz": c.Literal(uint64(0)),
			"ttl": c.Arithmetic("sub", at("ttls", sc("i")), c.Literal(uint64(1))),
		}))

	// free slot: claim a spawn by rank, else stay free. All the expensive work
	// (rank, splits) sits INSIDE branches per F-E2b where it is affordable to,
	// but `rank` is needed by both arms so it binds once here.
	freeThink := c.Let(map[string]*entitysdk.Builder{
		"r": rank(sc("i")),
	}, c.If(c.Logic("and", sc("firing"), c.Compare("eq", sc("r"), c.Literal(uint64(0)))),
		// the new bullet: spawns at the ship's nose, along the ship's heading.
		c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
			"kind": c.Literal(astBullet),
			"x": wrap(at("xs", c.Literal(uint64(0))),
				c.Index(c.Literal(astCosTable(astBulletSpd)), at("rots", c.Literal(uint64(0))))),
			"y": wrap(at("ys", c.Literal(uint64(0))),
				c.Index(c.Literal(astSinTable(astBulletSpd)), at("rots", c.Literal(uint64(0))))),
			"vx":  c.Index(c.Literal(astCosTable(astBulletSpd)), at("rots", c.Literal(uint64(0)))),
			"vy":  c.Index(c.Literal(astSinTable(astBulletSpd)), at("rots", c.Literal(uint64(0)))),
			"rot": at("rots", c.Literal(uint64(0))),
			"sz":  c.Literal(uint64(0)),
			"ttl": c.Literal(astBulletTTL),
		}),
		// otherwise: the j-th split claims this slot, if there is a j-th split.
		// F-E2a — index(splits, j) is ONLY evaluated inside the lt guard.
		c.Let(map[string]*entitysdk.Builder{
			"j": c.Arithmetic("sub", sc("r"),
				c.If(sc("firing"), c.Literal(uint64(1)), c.Literal(uint64(0)))),
		}, c.If(c.Compare("lt", sc("j"), c.Length(sc("splits"))),
			c.Let(map[string]*entitysdk.Builder{
				"src": c.Index(sc("splits"), sc("j")),
			}, c.Construct(astStateType+"/actor", map[string]*entitysdk.Builder{
				"kind": c.Literal(astAsteroid),
				"x":    at("xs", sc("src")), "y": at("ys", sc("src")),
				// the mirror half: deflects opposite the in-place half.
				"vx":  at("vys", sc("src")),
				"vy":  c.Arithmetic("sub", c.Literal(int64(0)), at("vxs", sc("src"))),
				"rot": at("rots", sc("src")),
				"sz":  c.Arithmetic("sub", at("szs", sc("src")), c.Literal(uint64(1))),
				"ttl": c.Literal(uint64(0)),
			})),
			free))))

	// --- the actor map (array-of-structs, IN-FLIGHT) ------------------------
	//
	// Heterogeneous dispatch = a chain of `if` on kind. Arch's expectation
	// (§3.3: "is `if` enough?") — confirmed, see F-F4.
	actorFn := c.Lambda([]string{"i"},
		c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(astShip)), shipThink,
			c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(astAsteroid)), asteroidThink,
				c.If(c.Compare("eq", at("kinds", sc("i")), c.Literal(astBullet)), bulletThink,
					freeThink))))

	// project(field): pull one field out of the in-flight actor structs into a
	// flat array. compute/field works directly on *constructedValue, so the
	// per-actor logic above runs ONCE and these are cheap.
	project := func(field string) *entitysdk.Builder {
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": sc("acts"),
			"fn":         c.Lambda([]string{"a"}, c.Field(sc("a"), field)),
		})
	}

	// scored: how many asteroids died this tick (hit and size 1).
	scored := c.Length(c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
		"collection": idxLit,
		"fn": c.Lambda([]string{"j"},
			c.Logic("and",
				c.Logic("and",
					c.Compare("eq", at("kinds", sc("j")), c.Literal(astAsteroid)),
					c.Compare("eq", at("szs", sc("j")), c.Literal(uint64(1)))),
				bulletHits(sc("j")))),
	}))

	alive := c.Let(map[string]*entitysdk.Builder{
		// the ONLY place the shard range appears: this eval produces its slots.
		"acts": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": slotLit,
			"fn":         actorFn,
		}),
		// ⚠️ HIGH bits — (s>>16) % astRotSteps. F-D3: the low bits of a
		// power-of-two-modulus LCG have period 2^k and look plausible.
		"nrng": c.Arithmetic("mod",
			c.Arithmetic("add",
				c.Arithmetic("mul", sc("rng"), c.Literal(astLCGMul)),
				c.Literal(astLCGAdd)),
			c.Literal(astLCGMod)),
	}, c.Construct(astStateType, map[string]*entitysdk.Builder{
		"kinds": project("kind"),
		"xs":    project("x"), "ys": project("y"),
		"vxs": project("vx"), "vys": project("vy"),
		"rots": project("rot"), "szs": project("sz"), "ttls": project("ttl"),
		"rng":   sc("nrng"),
		"score": c.Arithmetic("add", sc("score"), scored),
		"status": c.If(shipHit, c.Literal(uint64(1)),
			c.Literal(uint64(0))),
	}))

	// F-E2b: `firing`/`splits` bind INSIDE the live branch. let is eager, so
	// binding them outside would run the whole O(N^2) split scan on every tick
	// of a finished game.
	live := c.Let(map[string]*entitysdk.Builder{
		"firing": bit(astKeyFire),
		// let* evaluates in SORTED name order, so "hitb" lands before "splits"
		// and splits can see it. The name is chosen to sort that way.
		"hitb": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": idxLit,
			"fn":         c.Lambda([]string{"j"}, bulletHitsRaw(sc("j"))),
		}),
		"splits": splits,
	}, alive)

	// The frozen-when-dead tick returns `s` verbatim — same entity, same hash
	// (cf. Exp-E E4). A SHARD must not: `s` is the whole state, and returning it
	// as a fragment would hand the host a full state to stitch as a strip. Only
	// the full step owns the dead check; the shard rig asserts a live game.
	body := live
	if full {
		body = c.If(c.Compare("eq", sc("status"), c.Literal(uint64(1))), sc("s"), live)
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"keys":   c.Field(c.LookupTreeLocal(inputPath), "keys"),
		"kinds":  c.Field(sc("s"), "kinds"),
		"rng":    c.Field(sc("s"), "rng"),
		"rots":   c.Field(sc("s"), "rots"),
		"score":  c.Field(sc("s"), "score"),
		"status": c.Field(sc("s"), "status"),
		"szs":    c.Field(sc("s"), "szs"),
		"ttls":   c.Field(sc("s"), "ttls"),
		"vxs":    c.Field(sc("s"), "vxs"),
		"vys":    c.Field(sc("s"), "vys"),
		"xs":     c.Field(sc("s"), "xs"),
		"ys":     c.Field(sc("s"), "ys"),
	}, body))
}

// --- the OUTPUT PORT variants (the taxonomy probe) --------------------------
//
// Life and Snake never had to answer "what is an output port?", because for a
// grid game the STATE *is* the display — arch's §6 sketch points the output
// port straight at the state path (`output_ports: [{name: "grid", path:
// "app/life/state"}]`). Two grid probes in a row made that look like a law.
// It was a coincidence of both being grids.
//
// Asteroids is the first program where state and display come apart: the state
// is an actor array; the display is not. Something must turn one into the
// other, and WHERE that something sits is the whole port question. There are
// exactly three positions for the boundary, and the same three apply to Doom:
//
//   1. STATE PORT      — the port is the state path (what Life/Snake do). The
//                        renderer receives actors and must know what an
//                        asteroid looks like: per-program custom logic, in the
//                        renderer, in C#.
//   2. DISPLAY LIST    — the program emits GEOMETRY (world-space polygon
//                        vertices + a kind tag). The renderer draws polylines
//                        and knows nothing about asteroids. Generic renderer,
//                        fixed vocabulary.
//   3. FRAMEBUFFER     — the program RASTERIZES; the port is pixels. The
//                        renderer blits. Maximally generic — and it is exactly
//                        the row arch already has in the §8 taxonomy
//                        ("Display / framebuffer | out | snapshot").
//
// These two builders let the SAME sim be measured through all three, so the
// choice can turn on an op-cost number rather than on taste. That measurement
// is TestExpAsteroidsF5_OutputPortCost.

const (
	astDisplayType = "app/asteroids/display"
	astFrameType   = "app/asteroids/frame"
)

// astRadiusUnits: the per-kind collision/draw radius expressed in WHOLE WORLD
// UNITS (not FP sub-units), so it can multiply a per-unit trig table without
// any division. Mirrors astShipRadius/astAstRadius/1-unit bullets.
func astRadiusUnitsExpr(c *entitysdk.ComputeBuilder, kind, sz *entitysdk.Builder) *entitysdk.Builder {
	return c.If(c.Compare("eq", kind, c.Literal(astAsteroid)),
		c.Arithmetic("mul", c.Literal(int64(6)), sz),
		c.If(c.Compare("eq", kind, c.Literal(astShip)),
			c.Literal(int64(3)),
			c.Literal(int64(1)))) // bullet (and free slots, unused)
}

// buildAsteroidsDisplayList lowers the DISPLAY-LIST output port: for each slot
// it emits a kind tag plus a world-space quad (4 vertices), rotated by the
// actor's heading and scaled by its radius.
//
// The renderer's contract becomes "draw these polylines, colour by kind" — a
// fixed vocabulary that is identical for Asteroids, Snake, Life, or Doom's
// automap. No asteroid-specific knowledge crosses the boundary.
//
// Note the trig table is scaled by astFP (one world unit), so a vertex offset
// is table[idx] * radiusUnits — still no division anywhere.
func buildAsteroidsDisplayList(ap *entitysdk.AppPeer, statePath string) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	unitCos := c.Literal(astCosTable(astFP))
	unitSin := c.Literal(astSinTable(astFP))

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}

	// The SHIP is drawn as an arrowhead so its heading is visible; everything
	// else is a symmetric diamond.
	//
	// This is worth noticing as a port result, not just a cosmetic fix: a
	// diamond has its vertices 90 degrees apart, so a rotated diamond is the
	// SAME diamond and the ship's heading was invisible on screen. The fix is
	// here, in the PROGRAM, because the outline of a ship is the program's
	// business — the renderer only draws a polyline through whatever vertices
	// arrive. Making the ship directional changed zero lines of C#.
	//
	// Offsets are in 1/16 turns from the heading; radii are in whole world
	// units (the trig table is pre-scaled by one unit, so offset = table[idx] *
	// radiusUnits and no division appears).
	//   nose (0) --- 6 --- tail notch (8) --- 10 --- back to nose
	//
	// The drawn outline is deliberately LARGER than the ship's 3-unit collision
	// radius (astShipRadius): the nose reaches 9 units. That is the classic
	// forgiving hitbox — you die when the ship's BODY is hit, not when the nose
	// tip clips an asteroid — and it is a display decision, so it lives here in
	// the display list and does not touch the step.
	shipVertOff := []uint64{0, 6, 8, 10}
	shipVertRad := []int64{9, 4, 3, 4}

	// vertex k: heading + this kind's k-th offset, at this kind's k-th radius.
	vert := func(tbl *entitysdk.Builder, axis string, k int) *entitysdk.Builder {
		isShip := c.Compare("eq", at("kinds", sc("i")), c.Literal(astShip))
		off := c.If(isShip,
			c.Literal(shipVertOff[k]),
			c.Literal(uint64(k)*(astRotSteps/4)))
		radu := c.If(isShip, c.Literal(shipVertRad[k]), sc("radu"))
		idx := c.Arithmetic("mod",
			c.Arithmetic("add", at("rots", sc("i")), off),
			c.Literal(astRotSteps))
		return c.Arithmetic("add", at(axis, sc("i")),
			c.Arithmetic("mul", c.Index(tbl, idx), radu))
	}

	quadFields := map[string]*entitysdk.Builder{
		"kind": at("kinds", sc("i")),
	}
	for k := 0; k < 4; k++ {
		quadFields[fmt.Sprintf("x%d", k)] = vert(unitCos, "xs", k)
		quadFields[fmt.Sprintf("y%d", k)] = vert(unitSin, "ys", k)
	}

	quadFn := c.Lambda([]string{"i"},
		c.Let(map[string]*entitysdk.Builder{
			"radu": astRadiusUnitsExpr(c, at("kinds", sc("i")), at("szs", sc("i"))),
		}, c.Construct(astDisplayType+"/quad", quadFields)))

	project := func(field string) *entitysdk.Builder {
		return c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": sc("quads"),
			"fn":         c.Lambda([]string{"q"}, c.Field(sc("q"), field)),
		})
	}

	outFields := map[string]*entitysdk.Builder{"kinds": project("kind")}
	for k := 0; k < 4; k++ {
		outFields[fmt.Sprintf("x%d", k)] = project(fmt.Sprintf("x%d", k))
		outFields[fmt.Sprintf("y%d", k)] = project(fmt.Sprintf("y%d", k))
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"kinds": c.Field(sc("s"), "kinds"),
		"rots":  c.Field(sc("s"), "rots"),
		"szs":   c.Field(sc("s"), "szs"),
		"xs":    c.Field(sc("s"), "xs"),
		"ys":    c.Field(sc("s"), "ys"),
	}, c.Let(map[string]*entitysdk.Builder{
		"quads": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(indices),
			"fn":         quadFn,
		}),
	}, c.Construct(astDisplayType, outFields))))
}

// buildAsteroidsFramebuffer lowers the FRAMEBUFFER output port: a fbW x fbH
// paletted bitmap, one `kind` per pixel (0 = background). The renderer blits
// and knows nothing whatsoever about the program.
//
// This is the shape Doom's port would be, and the cost shape is the point: a
// pure (state) -> frame function cannot SCATTER into a mutable buffer, so it
// must GATHER — every output pixel asks "which actors cover me?" — making the
// port O(pixels x LIVE actors) rather than O(actors).
//
// Note "LIVE", not "slots": the first version of this measurement gathered over
// all CAP slots and recomputed per-actor constants inside the pixel loop, which
// inflated the price 13x and produced a materially wrong conclusion (F6 is the
// decomposition that caught it). The default here is the honest lowering.
func buildAsteroidsFramebuffer(ap *entitysdk.AppPeer, statePath string, fbW, fbH int) *entitysdk.Builder {
	return buildAsteroidsFramebufferV(ap, statePath, fbW, fbH, true, true)
}

// buildAsteroidsFramebufferV builds the framebuffer port in one of two
// lowerings, so the cost of the PORT can be separated from the cost of the
// AUTHOR'S MISTAKES (F6 measures both):
//
//   - hoisted=false — the naive gather. Each pixel/actor test recomputes the
//     actor's squared radius from scratch, and because it is written mul(r, r)
//     with r inline, it evaluates r TWICE. ~56 ops per pixel-actor pair.
//   - hoisted=true  — the per-actor radius array is computed ONCE (a map over
//     actors, outside the pixel loop) and each pixel/actor test indexes it.
//   - liveOnly=true — the pixel loop gathers over the LIVE actor list (built
//     once, outside) instead of all CAP slots. This is the one that changes the
//     shape of the answer: the gather is O(pixels x LIVE), not O(pixels x CAP),
//     and a fixed-cap array is mostly empty most of the time.
//
// The difference between them is the honest measure of how much of the
// framebuffer's price is inherent to gather and how much was self-inflicted.
func buildAsteroidsFramebufferV(ap *entitysdk.AppPeer, statePath string, fbW, fbH int, hoisted, liveOnly bool) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	pix := make([]uint64, fbW*fbH)
	for i := range pix {
		pix[i] = uint64(i)
	}
	// world units per pixel — a literal, so no division at eval.
	scaleX := astWorld / int64(fbW)
	scaleY := astWorld / int64(fbH)

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}

	// squared draw radius of actor a, in FP units — the expensive sub-expression.
	// Inline (naive) it is recomputed per pixel per actor AND evaluates r twice.
	radSqInline := func(a *entitysdk.Builder) *entitysdk.Builder {
		r := c.Arithmetic("mul", c.Literal(astFP),
			astRadiusUnitsExpr(c, at("kinds", a), at("szs", a)))
		return c.Arithmetic("mul", r, r)
	}
	// Hoisted: index the precomputed per-actor array (bound as "rad2s" below).
	radSq := radSqInline
	if hoisted {
		radSq = func(a *entitysdk.Builder) *entitysdk.Builder {
			return c.Index(sc("rad2s"), a)
		}
	}

	// does actor a cover the pixel's world point? When gathering over the live
	// list the kind!=free test is redundant — the list is already live-only.
	inRange := c.Compare("lt",
		c.Let(map[string]*entitysdk.Builder{
			"dx": c.Arithmetic("sub", sc("wx"), at("xs", sc("a"))),
			"dy": c.Arithmetic("sub", sc("wy"), at("ys", sc("a"))),
		}, c.Arithmetic("add",
			c.Arithmetic("mul", sc("dx"), sc("dx")),
			c.Arithmetic("mul", sc("dy"), sc("dy")))),
		radSq(sc("a")))
	coversBody := inRange
	if !liveOnly {
		coversBody = c.Logic("and",
			c.Compare("neq", at("kinds", sc("a")), c.Literal(astFree)),
			inRange)
	}
	covers := c.Lambda([]string{"a"}, coversBody)

	// the collection each pixel gathers over: every slot, or just the live ones.
	gatherOver := c.Literal(indices)
	if liveOnly {
		gatherOver = sc("live")
	}

	pixFn := c.Lambda([]string{"p"},
		c.Let(map[string]*entitysdk.Builder{
			"px": c.Arithmetic("mod", sc("p"), c.Literal(uint64(fbW))),
		}, c.Let(map[string]*entitysdk.Builder{
			// F-D1 floor-div: py = (p - px) / fbW.
			"py": c.Arithmetic("div",
				c.Arithmetic("sub", sc("p"), sc("px")), c.Literal(uint64(fbW))),
		}, c.Let(map[string]*entitysdk.Builder{
			"wx": c.Arithmetic("mul", sc("px"), c.Literal(scaleX)),
			"wy": c.Arithmetic("mul", sc("py"), c.Literal(scaleY)),
		}, c.Let(map[string]*entitysdk.Builder{
			"hits": c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": gatherOver,
				"fn":         covers,
			}),
			// F-E2a: index(hits,0) is only reachable inside the length guard.
		}, c.If(c.Compare("gt", c.Length(sc("hits")), c.Literal(uint64(0))),
			at("kinds", c.Index(sc("hits"), c.Literal(uint64(0)))),
			c.Literal(uint64(0))))))))

	frame := c.Construct(astFrameType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(fbW)),
		"height": c.Literal(uint64(fbH)),
		"pixels": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(pix),
			"fn":         pixFn,
		}),
	})

	// The live-list hoist: a fixed-cap array is mostly free slots, and every
	// pixel was asking each of them "do you cover me?" only to be told kind==0.
	// Build the live list ONCE (O(CAP)) and gather over it (O(pixels x LIVE)).
	if liveOnly {
		frame = c.Let(map[string]*entitysdk.Builder{
			"live": c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
				"collection": c.Literal(indices),
				"fn": c.Lambda([]string{"a"},
					c.Compare("neq", at("kinds", sc("a")), c.Literal(astFree))),
			}),
		}, frame)
	}

	// The hoist: the per-actor squared radius is a function of the actor alone,
	// so it belongs OUTSIDE the pixel loop — one map over CAP actors, indexed
	// P times, instead of P*CAP recomputations.
	if hoisted {
		frame = c.Let(map[string]*entitysdk.Builder{
			"rad2s": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
				"collection": c.Literal(indices),
				"fn": c.Lambda([]string{"a"},
					c.Let(map[string]*entitysdk.Builder{
						"r": c.Arithmetic("mul", c.Literal(astFP),
							astRadiusUnitsExpr(c, at("kinds", sc("a")), at("szs", sc("a")))),
					}, c.Arithmetic("mul", sc("r"), sc("r")))), // r bound once, not twice
			}),
		}, frame)
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"kinds": c.Field(sc("s"), "kinds"),
		"szs":   c.Field(sc("s"), "szs"),
		"xs":    c.Field(sc("s"), "xs"),
		"ys":    c.Field(sc("s"), "ys"),
	}, frame))
}

// --- the BLOCKMAP (arch §3.2 / §7.2) ---------------------------------------

const astBlockmapType = "app/asteroids/blockmap"

// buildAsteroidsBlockmap lowers a blockmap: a fixed binsPerAxis x binsPerAxis
// grid over the world, each cell holding the list of live actor indices inside
// it. This is Doom's own spatial index and the standard answer to O(n^2)
// collision — arch asked (§3.2) whether it is expressible in compute, and §7.2
// claims it is because it is "Life-shaped (a fixed grid) feeding an actor map".
//
// It IS expressible — that much of §7.2 is right, and this builds it. The
// question F7 measures is whether it is AFFORDABLE, which is a different
// question and is the one that decides whether it helps.
func buildAsteroidsBlockmap(ap *entitysdk.AppPeer, statePath string, binsPerAxis int) *entitysdk.Builder {
	c := ap.Compute()
	sc := c.LookupScope

	indices := make([]uint64, astCapacity)
	for i := range indices {
		indices[i] = uint64(i)
	}
	nBins := binsPerAxis * binsPerAxis
	binIdx := make([]uint64, nBins)
	for i := range binIdx {
		binIdx[i] = uint64(i)
	}
	cellSize := astWorld / int64(binsPerAxis)

	at := func(arr string, i *entitysdk.Builder) *entitysdk.Builder {
		return c.Index(sc(arr), i)
	}
	// F-D1 floor-div; positions are non-negative so this is sound.
	floorDiv := func(a, b *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("div", c.Arithmetic("sub", a, c.Arithmetic("mod", a, b)), b)
	}
	binOf := func(a *entitysdk.Builder) *entitysdk.Builder {
		return c.Arithmetic("add",
			c.Arithmetic("mul",
				floorDiv(at("ys", a), c.Literal(cellSize)),
				c.Literal(int64(binsPerAxis))),
			floorDiv(at("xs", a), c.Literal(cellSize)))
	}

	return c.Let(map[string]*entitysdk.Builder{
		"s": c.LookupTreeLocal(statePath),
	}, c.Let(map[string]*entitysdk.Builder{
		"kinds": c.Field(sc("s"), "kinds"),
		"xs":    c.Field(sc("s"), "xs"),
		"ys":    c.Field(sc("s"), "ys"),
	}, c.Construct(astBlockmapType, map[string]*entitysdk.Builder{
		"bins": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(binIdx),
			// THE SHAPE THAT DECIDES EVERYTHING: each bin asks EVERY actor "are
			// you in me?". That is a GATHER, and it is not an implementation
			// choice — a pure function must compute each bin's contents from its
			// inputs, and it cannot instead have each actor append itself to a
			// bin (that would be a scatter into mutable state).
			"fn": c.Lambda([]string{"b"},
				c.BuiltinsCall("filter", map[string]*entitysdk.Builder{
					"collection": c.Literal(indices),
					"fn": c.Lambda([]string{"a"},
						c.Logic("and",
							c.Compare("neq", at("kinds", sc("a")), c.Literal(astFree)),
							c.Compare("eq", binOf(sc("a")), sc("b")))),
				})),
		}),
	})))
}

type astBlockmapWire struct {
	Bins [][]uint64 `cbor:"bins"`
}

// TestExpAsteroidsF7_BlockmapIsAScatterStructure answers arch's §3.2/§7.2 —
// and the answer is not the expected one.
//
// The blockmap is the standard fix for O(n^2) collision, and §7.2 says the
// spatial machinery is expressible in compute. Both true. But "expressible" is
// not "cheaper", and the reason is the SAME one that prices the framebuffer
// (§4 of the report):
//
//	In an imperative engine a blockmap is built by SCATTER — walk the actors
//	once, each appends itself to its cell: O(N). Pure compute cannot scatter,
//	so each bin must GATHER: ask all N actors "are you in me?" → O(B x N).
//
// So the build is O(B x N). To make bins sparse enough to actually cut the
// collision term you need B to grow with N — at which point O(B x N) is O(N^2)
// and the blockmap has cost what it was meant to save.
//
// This test measures the build cost against B with N fixed. The claim is
// falsifiable and stated as such: if cost does NOT scale ~linearly in B, the
// gather reasoning is wrong and the report must be corrected.
func TestExpAsteroidsF7_BlockmapIsAScatterStructure(t *testing.T) {
	if testing.Short() {
		t.Skip("blockmap sweep bisects the budget; slow")
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeedDuel(7)
	seed.Kinds[2] = astBullet
	seed.Xs[2] = astWorld / 2
	seed.Ys[2] = astWorld/2 - 20*astFP
	seed.Ttls[2] = astBulletTTL
	p := astSetup(t, ap, "app/asteroids/f7", seed)
	live := astCapacity - astCount(seed, astFree)

	type row struct {
		axis, bins, ops int
		binned          int
	}
	var rows []row
	for _, axis := range []int{2, 4, 8} {
		path := fmt.Sprintf("app/asteroids/f7/bm%d", axis)
		if _, err := buildAsteroidsBlockmap(ap, p.statePath, axis).
			Build(context.Background(), path); err != nil {
			t.Fatalf("build blockmap %d: %v", axis, err)
		}
		ops, ok := astEvalOps(t, ap, path)
		if !ok {
			t.Fatalf("blockmap %dx%d is over the op cap", axis, axis)
		}
		// CORRECTNESS before cost: every live actor must land in exactly one
		// bin. A blockmap that bins nothing is cheap and useless.
		var bm astBlockmapWire
		astEvalPort(t, ap, path, &bm)
		if len(bm.Bins) != axis*axis {
			t.Fatalf("blockmap %dx%d returned %d bins", axis, axis, len(bm.Bins))
		}
		binned := 0
		for _, b := range bm.Bins {
			binned += len(b)
		}
		if binned != live {
			t.Fatalf("blockmap %dx%d binned %d actors, want %d live — the index is "+
				"wrong, so its cost is meaningless", axis, axis, binned, live)
		}
		rows = append(rows, row{axis, axis * axis, ops, binned})
	}

	// The naive all-pairs collision this is supposed to replace, for scale.
	stepOps, ok := astEvalOps(t, ap, p.stepPath)
	if !ok {
		t.Fatal("step over the cap")
	}

	t.Logf("blockmap BUILD cost, %d slots (%d live), world %dx%d:",
		astCapacity, live, astWorld, astWorld)
	t.Logf("  %-14s %6s %9s %14s", "grid", "bins", "ops", "ops/bin")
	for _, r := range rows {
		t.Logf("  %-14s %6d %9d %14.1f",
			fmt.Sprintf("%dx%d", r.axis, r.axis), r.bins, r.ops,
			float64(r.ops)/float64(r.bins))
	}
	t.Logf("  (the whole step, incl. naive O(N^2) collision, is %d ops)", stepOps)

	// THE CLAIM, stated so it can fail: cost is ~linear in the BIN COUNT,
	// because every bin scans every actor. ops/bin should stay ~flat.
	first, last := rows[0], rows[len(rows)-1]
	binRatio := float64(last.bins) / float64(first.bins)
	opsRatio := float64(last.ops) / float64(first.ops)
	t.Logf("  bins x%.0f -> ops x%.1f  (a GATHER predicts these track; a scatter "+
		"would predict ops stay flat as bins grow)", binRatio, opsRatio)
	if opsRatio < binRatio*0.5 {
		t.Fatalf("blockmap build cost did NOT track the bin count (bins x%.0f, ops "+
			"x%.1f) — the O(B x N) gather reasoning is WRONG and the report must "+
			"be corrected", binRatio, opsRatio)
	}
	t.Logf("READ: the blockmap BUILD is O(bins x actors) — each bin gathers over " +
		"every actor. An imperative engine builds the same index in O(actors) by " +
		"scatter (each actor appends itself to its cell). To make bins sparse " +
		"enough to cut the O(N^2) collision term, B must grow with N — and then " +
		"O(B x N) IS O(N^2). The blockmap does not rescue pure-compute collision; " +
		"it relocates the same product. What WOULD: a sort primitive (bin actors " +
		"by cell key in O(N log N), bins become contiguous ranges).")
}

// --- the rig ---------------------------------------------------------------

type astProgram struct {
	ap                             *entitysdk.AppPeer
	statePath, inputPath, stepPath string
}

func astSetup(t testing.TB, ap *entitysdk.AppPeer, root string, seed astState) *astProgram {
	t.Helper()
	p := &astProgram{
		ap:        ap,
		statePath: root + "/state",
		inputPath: root + "/input",
		stepPath:  root + "/step",
	}
	ent, err := astStateEntity(seed)
	if err != nil {
		t.Fatalf("seed state entity: %v", err)
	}
	if _, err := ap.PutEntity(p.statePath, ent); err != nil {
		t.Fatalf("put seed state: %v", err)
	}
	// F-E1: port initialization is the RUNTIME's job — an unseeded input port
	// read is a compute/error. Seed the held-key snapshot with "no keys down".
	p.writeInput(t, 0)
	if _, err := buildAsteroidsStepExpr(ap, p.statePath, p.inputPath).
		Build(context.Background(), p.stepPath); err != nil {
		t.Fatalf("build step: %v", err)
	}
	return p
}

// writeInput is the (simulated) input driver: one PutEntity at the declared
// port path — last-write-wins snapshot semantics, held-key SET as a bitmask.
func (p *astProgram) writeInput(t testing.TB, keys uint64) {
	t.Helper()
	ent, err := astInputEntity(keys)
	if err != nil {
		t.Fatalf("input entity: %v", err)
	}
	if _, err := p.ap.PutEntity(p.inputPath, ent); err != nil {
		t.Fatalf("put input: %v", err)
	}
}

func (p *astProgram) tick() (*astState, hash.Hash, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, hash.Hash{}, err
	}
	resp, err := p.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{p.stepPath}})
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("eval dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, hash.Hash{}, fmt.Errorf("eval status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, hash.Hash{}, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	if resp.Type != astStateType {
		return nil, hash.Hash{}, fmt.Errorf("expected %s result, got type=%s", astStateType, resp.Type)
	}
	var s astState
	if err := ecf.Decode(resp.Data, &s); err != nil {
		return nil, hash.Hash{}, fmt.Errorf("decode state: %w", err)
	}
	ent, err := entity.NewEntity(astStateType, cbor.RawMessage(resp.Data))
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := p.ap.PutEntity(p.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, fmt.Errorf("put state: %w", err)
	}
	return &s, h, nil
}

// astCount tallies live actors by kind — the structural observable every
// anti-vacuity assertion in this file is built on.
func astCount(s astState, kind uint64) int {
	n := 0
	for _, k := range s.Kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// --- F1: the step evaluates at all, and actors move -------------------------

func TestExpAsteroidsF1_DriftsAndWraps(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeed(4, 12345)
	p := astSetup(t, ap, "app/asteroids/f1", seed)

	got, _, err := p.tick()
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	// ANTI-VACUITY: the tick must have actually simulated something. A dead
	// grid comparing equal to a dead grid is how F-D3 passed while proving
	// nothing — so assert the actors are live AND that they moved.
	if n := astCount(*got, astAsteroid); n != 4 {
		t.Fatalf("expected 4 asteroids to survive tick 1, got %d", n)
	}
	if got.Kinds[0] != astShip {
		t.Fatalf("ship died on tick 1 (kind=%d) — seed places asteroids clear of it", got.Kinds[0])
	}
	moved := 0
	for i := 1; i <= 4; i++ {
		if got.Xs[i] != seed.Xs[i] || got.Ys[i] != seed.Ys[i] {
			moved++
		}
	}
	if moved != 4 {
		t.Fatalf("only %d/4 asteroids drifted — the step is not simulating motion", moved)
	}
	t.Logf("PASS F1: step evaluates; 4 asteroids drift, ship survives, %d actors live",
		astCapacity-astCount(*got, astFree))
}

// astAssertState compares a compute tick against the oracle field-by-field,
// plus the materialized-boundary hash check (same shape as assertSnakeState).
func astAssertState(t *testing.T, tick int, got *astState, gotHash hash.Hash, want astState) {
	t.Helper()
	bad := func(f string) {
		t.Fatalf("tick %d: compute diverged from oracle on %s\ngot:  %+v\nwant: %+v",
			tick, f, *got, want)
	}
	for i := 0; i < astCapacity; i++ {
		if got.Kinds[i] != want.Kinds[i] {
			bad(fmt.Sprintf("kinds[%d]", i))
		}
		if got.Xs[i] != want.Xs[i] || got.Ys[i] != want.Ys[i] {
			bad(fmt.Sprintf("pos[%d]", i))
		}
		if got.Vxs[i] != want.Vxs[i] || got.Vys[i] != want.Vys[i] {
			bad(fmt.Sprintf("vel[%d]", i))
		}
		if got.Rots[i] != want.Rots[i] || got.Szs[i] != want.Szs[i] || got.Ttls[i] != want.Ttls[i] {
			bad(fmt.Sprintf("attrs[%d]", i))
		}
	}
	if got.RNG != want.RNG || got.Score != want.Score || got.Status != want.Status {
		bad("scalars")
	}
	wantEnt, err := astStateEntity(want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != 1 && gotHash != wantEnt.ContentHash {
		t.Fatalf("tick %d: constructed state hash != hand-built oracle hash", tick)
	}
}

// --- F2: the differential run — spawns, splits, and the oracle --------------
//
// The load-bearing correctness test. A scripted game runs against the compute
// step and the Go oracle in lockstep; every tick must agree field-by-field AND
// at the materialized hash.
//
// ANTI-VACUITY (the discipline that has caught three bugs on this track): a
// differential test that never fires a bullet would agree with the oracle on
// an empty branch set and prove nothing about spawn/split — the shapes this
// probe exists to test. So the run ASSERTS, structurally, that bullets
// actually spawned, that a split actually occurred, and that the actor set
// actually changed length.
func TestExpAsteroidsF2_DifferentialSpawnAndSplit(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeedDuel(7)
	p := astSetup(t, ap, "app/asteroids/f2", seed)

	// fire on these ticks; coast otherwise.
	fireAt := map[int]bool{1: true, 20: true, 30: true, 40: true, 50: true}
	const ticks = 60

	cur := seed
	maxAsteroids := astCount(seed, astAsteroid)
	sawBullet, sawSplit := false, false
	prevAsteroids := maxAsteroids

	for tk := 1; tk <= ticks; tk++ {
		var keys uint64
		if fireAt[tk] {
			keys = astKeys(astKeyFire)
		}
		p.writeInput(t, keys)

		want := astNext(cur, keys)
		got, gh, err := p.tick()
		if err != nil {
			t.Fatalf("tick %d: %v", tk, err)
		}
		astAssertState(t, tk, got, gh, want)

		na := astCount(*got, astAsteroid)
		if astCount(*got, astBullet) > 0 {
			sawBullet = true
		}
		if na > prevAsteroids {
			sawSplit = true
		}
		if na > maxAsteroids {
			maxAsteroids = na
		}
		prevAsteroids = na
		cur = want
	}

	if !sawBullet {
		t.Fatal("VACUOUS: no bullet ever spawned — the spawn branch never ran, " +
			"so this run proves nothing about the variable actor set")
	}
	if !sawSplit {
		t.Fatal("VACUOUS: no asteroid ever split — the split/slot-allocation branch " +
			"never ran, so this run proves nothing about set GROWTH")
	}
	if maxAsteroids <= astCount(seed, astAsteroid) {
		t.Fatalf("VACUOUS: actor set never grew (max asteroids %d <= seed %d)",
			maxAsteroids, astCount(seed, astAsteroid))
	}
	t.Logf("PASS F2: %d ticks, compute == oracle field-by-field AND at the "+
		"materialized hash every tick; set grew %d -> %d asteroids; final score=%d",
		ticks, astCount(seed, astAsteroid), maxAsteroids, cur.Score)
}

// --- F3: replay determinism under a VARIABLE actor set ----------------------
//
// The §10.3 non-determinism boundary, stressed against a set that changes size
// — "exactly the case a naive engine gets wrong" (arch §4). The bar is Exp-E's
// replay test widened from Snake's fixed grid to spawns and deaths.
//
// The claim under test: a game is a pure function of (state_0, input-stream).
// So two independent program instances — separate tree roots, separate step
// expressions, separate content — fed the SAME state_0 and the SAME input
// stream must produce a bit-identical hash chain, tick for tick.
//
// ⚠️ The LCG here reads HIGH bits ((s>>16)%N). F-D3 burned this track with the
// low-bit trap: a power-of-two-modulus LCG has period 2^k in its low k bits,
// so every row came out identical at a plausible density and every rule-level
// test waved it through. The durable lesson is the ASSERTION, not the seed —
// hence the structural anti-vacuity checks below rather than a statistical one.
func TestExpAsteroidsF3_ReplayDeterminism(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeedDuel(7)
	// The input stream IS the complete record of non-determinism (§10.3).
	// The firing script is F2's (proven to reach score 2 — i.e. to actually
	// complete spawn -> split -> death). The held-key SET is exercised AFTER
	// the kills: thrusting/rotating earlier steers the ship off-target, the
	// shots miss, and the run goes vacuous — which is precisely what the
	// assertions below caught on the first attempt at this test.
	stream := make([]uint64, 0, 60)
	for tk := 1; tk <= 60; tk++ {
		var keys uint64
		switch {
		case tk == 1 || tk == 20 || tk == 30 || tk == 40 || tk == 50:
			keys = astKeys(astKeyFire)
		case tk >= 52 && tk%2 == 0:
			keys = astKeys(astKeyRight, astKeyThrust) // held-key SET: two at once
		}
		stream = append(stream, keys)
	}

	run := func(root string) ([]hash.Hash, astState) {
		p := astSetup(t, ap, root, seed)
		chain := make([]hash.Hash, 0, len(stream))
		var last astState
		for tk, keys := range stream {
			p.writeInput(t, keys)
			got, h, err := p.tick()
			if err != nil {
				t.Fatalf("%s tick %d: %v", root, tk+1, err)
			}
			chain = append(chain, h)
			last = *got
		}
		return chain, last
	}

	chainA, endA := run("app/asteroids/f3a")
	chainB, endB := run("app/asteroids/f3b")

	for i := range chainA {
		if chainA[i] != chainB[i] {
			t.Fatalf("replay diverged at tick %d: %x != %x", i+1, chainA[i], chainB[i])
		}
	}

	// ANTI-VACUITY 1 — two frozen games also produce identical chains. Prove
	// the sim actually MOVED: the chain must not be a constant.
	distinct := map[hash.Hash]bool{}
	for _, h := range chainA {
		distinct[h] = true
	}
	if distinct[chainA[0]] && len(distinct) < len(chainA)/2 {
		t.Fatalf("VACUOUS: only %d distinct states across %d ticks — the sim is "+
			"barely advancing, so hash equality proves little", len(distinct), len(chainA))
	}
	// ANTI-VACUITY 2 — the whole point is a set that CHANGES SIZE. If the run
	// never spawned or split, this is just Snake with extra steps.
	if endA.Score == 0 {
		t.Fatal("VACUOUS: score 0 — no asteroid was ever destroyed, so the " +
			"variable-set path (spawn -> split -> death) never completed")
	}
	if endA.Score != endB.Score {
		t.Fatalf("replay produced different scores: %d != %d", endA.Score, endB.Score)
	}

	t.Logf("PASS F3: %d-tick replay is bit-identical across two independent "+
		"program instances under a VARIABLE actor set (%d distinct state hashes, "+
		"final score=%d). Non-determinism enters only via the input stream (§10.3); "+
		"spawn/death are pure functions of (state, input).",
		len(stream), len(distinct), endA.Score)
}

// --- F5: what does each output port COST? (the taxonomy deliverable) -------

// the wire shapes of the two derived ports.
type astDisplayList struct {
	Kinds []uint64 `cbor:"kinds"`
	X0    []int64  `cbor:"x0"`
	Y0    []int64  `cbor:"y0"`
	X1    []int64  `cbor:"x1"`
	Y1    []int64  `cbor:"y1"`
	X2    []int64  `cbor:"x2"`
	Y2    []int64  `cbor:"y2"`
	X3    []int64  `cbor:"x3"`
	Y3    []int64  `cbor:"y3"`
}

type astFrame struct {
	Width  uint64   `cbor:"width"`
	Height uint64   `cbor:"height"`
	Pixels []uint64 `cbor:"pixels"`
}

// astEvalPort evaluates an expression at path and decodes the result into out.
func astEvalPort(t *testing.T, ap *entitysdk.AppPeer, path string, out interface{}) {
	t.Helper()
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
	if err != nil {
		t.Fatalf("%s: eval dispatch: %v", path, err)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		t.Fatalf("%s: compute/error code=%s message=%q", path, ed.Code, ed.Message)
	}
	if resp.Status != 200 {
		t.Fatalf("%s: eval status %d", path, resp.Status)
	}
	if err := ecf.Decode(resp.Data, out); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
}

// astEvalOps measures an expression's op cost by bisecting the budget cap —
// the same trick as evalOps (axis1_budget_scope_test.go), but it first checks
// the expression evaluates AT ALL under the default cap. Without that guard the
// bisect converges to DefaultMaxOps for anything over the cliff and silently
// reports the cap as if it were a measurement.
//
// Approximate by construction (a search, not an instrument). Op cost is
// observable only by running out of it until the peers ship a dry_run surface —
// the capability gap already routed to the cohort.
func astEvalOps(t *testing.T, ap *entitysdk.AppPeer, path string) (int, bool) {
	t.Helper()
	ent, ok := ap.TestContentStore().Get(mustIndex(t, ap, path))
	if !ok {
		t.Fatalf("expr not in store: %s", path)
	}
	ctx := ap.TestEvalContext()
	if _, err := computeEvaluate(ent, compute_DefaultMaxOps, ctx); err != nil {
		return 0, false // over the default cap — the cliff
	}
	lo, hi := 1, compute_DefaultMaxOps
	for lo < hi {
		mid := (lo + hi) / 2
		if _, err := computeEvaluate(ent, mid, ctx); err != nil {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, true
}

// TestExpAsteroidsF5_OutputPortCost prices the three positions of the
// rasterization boundary against ONE sim. This is the number the port-taxonomy
// decision should turn on, and it answers the same question for Doom.
//
// Each port is checked for CORRECTNESS before it is priced — measuring the
// op cost of an expression that produces garbage would be the purest form of
// the vacuous pass this track keeps hitting.
func TestExpAsteroidsF5_OutputPortCost(t *testing.T) {
	if testing.Short() {
		t.Skip("port-cost sweep bisects the budget; slow")
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	// A state with all three actor kinds live, so every port renders a
	// heterogeneous scene rather than an empty one.
	seed := astSeedDuel(7)
	seed.Kinds[2] = astBullet
	seed.Xs[2] = astWorld / 2
	seed.Ys[2] = astWorld/2 - 20*astFP
	seed.Ttls[2] = astBulletTTL

	p := astSetup(t, ap, "app/asteroids/f5", seed)

	// --- port 1: the STATE port. The step itself is the whole cost; the
	// renderer then does the interpreting, in the renderer's language.
	stepOps, ok := astEvalOps(t, ap, p.stepPath)
	if !ok {
		t.Fatal("the step itself is over the default op cap — re-check the seed")
	}

	// --- port 2: the DISPLAY LIST.
	dlPath := "app/asteroids/f5/display"
	if _, err := buildAsteroidsDisplayList(ap, p.statePath).
		Build(context.Background(), dlPath); err != nil {
		t.Fatalf("build display list: %v", err)
	}
	var dl astDisplayList
	astEvalPort(t, ap, dlPath, &dl)

	// CORRECTNESS before cost: the ship's quad must be 4 vertices at ~3 units
	// from the ship's centre, and the kinds must survive the projection.
	if len(dl.Kinds) != astCapacity || len(dl.X0) != astCapacity {
		t.Fatalf("display list wrong length: kinds=%d x0=%d", len(dl.Kinds), len(dl.X0))
	}
	if dl.Kinds[0] != astShip || dl.Kinds[1] != astAsteroid || dl.Kinds[2] != astBullet {
		t.Fatalf("display list lost the kind tags: %v", dl.Kinds[:3])
	}
	// The ship is an ARROWHEAD, not a symmetric diamond: its four vertices sit
	// at 9/4/3/4 world units from the centre (nose, flank, tail notch, flank) —
	// larger than the 3-unit collision radius on purpose (forgiving hitbox).
	// Asserting the radii individually is what makes the shape DIRECTIONAL —
	// a diamond's vertices are 90 degrees apart, so a rotated diamond is the
	// same diamond and the heading is invisible on screen.
	shipWantUnits := []int64{9, 4, 3, 4}
	shipVerts := [][2]int64{{dl.X0[0], dl.Y0[0]}, {dl.X1[0], dl.Y1[0]},
		{dl.X2[0], dl.Y2[0]}, {dl.X3[0], dl.Y3[0]}}
	for i, v := range shipVerts {
		dx, dy := v[0]-seed.Xs[0], v[1]-seed.Ys[0]
		d2 := dx*dx + dy*dy
		want := (shipWantUnits[i] * astFP) * (shipWantUnits[i] * astFP)
		if d2 < want*8/10 || d2 > want*12/10 {
			t.Fatalf("ship vertex %d at distance^2=%d, want ~%d (%d units) — the "+
				"display-list geometry is wrong, so its cost would be meaningless",
				i, d2, want, shipWantUnits[i])
		}
	}
	// ANTI-VACUITY on the directionality itself: if every vertex were the same
	// distance out, the outline would be rotationally symmetric and the ship's
	// heading would not be readable no matter how well the renderer drew it.
	if shipVerts[0] == shipVerts[2] {
		t.Fatal("ship nose and tail coincide — the outline is degenerate")
	}
	noseD2 := func(v [2]int64) int64 {
		dx, dy := v[0]-seed.Xs[0], v[1]-seed.Ys[0]
		return dx*dx + dy*dy
	}
	if noseD2(shipVerts[0]) <= noseD2(shipVerts[1]) {
		t.Fatal("ship nose is not the furthest vertex — the outline is not " +
			"directional, so the heading is invisible on screen")
	}
	dlOps, ok := astEvalOps(t, ap, dlPath)
	if !ok {
		t.Fatal("display list is over the default op cap")
	}

	// --- port 3: the FRAMEBUFFER, swept by resolution.
	type fbResult struct {
		w, h int
		ops  int
		ok   bool
		lit  int
	}
	var fbs []fbResult
	for _, n := range []int{8, 16, 32, 64} {
		path := fmt.Sprintf("app/asteroids/f5/fb%d", n)
		if _, err := buildAsteroidsFramebuffer(ap, p.statePath, n, n).
			Build(context.Background(), path); err != nil {
			t.Fatalf("build fb %d: %v", n, err)
		}
		res := fbResult{w: n, h: n}
		ops, okc := astEvalOps(t, ap, path)
		res.ops, res.ok = ops, okc
		if okc {
			// CORRECTNESS: the frame must actually draw something. An all-zero
			// framebuffer is cheap AND wrong — exactly the vacuous measurement.
			var f astFrame
			astEvalPort(t, ap, path, &f)
			if int(f.Width) != n || len(f.Pixels) != n*n {
				t.Fatalf("fb %d: wrong shape w=%d pixels=%d", n, f.Width, len(f.Pixels))
			}
			for _, px := range f.Pixels {
				if px != astFree {
					res.lit++
				}
			}
			if res.lit == 0 {
				t.Fatalf("fb %dx%d rendered an EMPTY frame — its op cost would be "+
					"the cost of drawing nothing", n, n)
			}
		}
		fbs = append(fbs, res)
	}

	// --- the report.
	t.Logf("output-port op cost, one tick, %d actor slots (%d live):",
		astCapacity, astCapacity-astCount(seed, astFree))
	t.Logf("  %-22s %10s  %s", "port", "op_cost", "what the renderer must know")
	t.Logf("  %-22s %10d  %s", "1. state (the step)", stepOps,
		"what an asteroid IS (per-program logic in the renderer)")
	t.Logf("  %-22s %10d  %s", "2. display list", dlOps,
		"how to draw polylines (fixed vocabulary)")
	for _, f := range fbs {
		if !f.ok {
			t.Logf("  %-22s %10s  %s", fmt.Sprintf("3. framebuffer %dx%d", f.w, f.h),
				">100k", "nothing (blit) — BUT OVER THE OP CAP")
			continue
		}
		t.Logf("  %-22s %10d  %s (%d/%d px lit)",
			fmt.Sprintf("3. framebuffer %dx%d", f.w, f.h), f.ops,
			"nothing (blit)", f.lit, f.w*f.h)
	}

	// The structural findings, ASSERTED rather than eyeballed.
	//
	// (Anti-vacuity on the assertion itself: comparing against fbs[last] would
	// silently skip, because the largest framebuffer is over the cap and has no
	// measured cost. Compare against the largest one that actually MEASURED.)
	var big *fbResult
	for i := range fbs {
		if fbs[i].ok {
			big = &fbs[i]
		}
	}
	if big == nil {
		t.Fatal("no framebuffer resolution measured under the cap — nothing to compare")
	}
	if dlOps >= big.ops {
		t.Fatalf("expected the framebuffer to dominate the display list; got dl=%d fb%dx%d=%d",
			dlOps, big.w, big.h, big.ops)
	}
	// The cliff is the headline: SOME framebuffer resolution must exceed the cap,
	// otherwise this sweep never reached the interesting regime.
	overCap := false
	for _, f := range fbs {
		if !f.ok {
			overCap = true
		}
	}
	if !overCap {
		t.Fatal("VACUOUS: no framebuffer resolution exceeded the op cap — the sweep " +
			"never reached the cliff it exists to locate")
	}
	t.Logf("READ: the display list costs O(actors) and is INDEPENDENT of "+
		"resolution; the framebuffer costs O(pixels x LIVE actors), because a pure "+
		"(state)->frame function cannot scatter and must ask, at every pixel, which "+
		"actors cover it. Both are cheap here — the framebuffer only becomes the "+
		"loser as resolution x actor count grows (see F6's sweep and the report's "+
		"Doom extrapolation). The step's own cost (%d) is the floor under all "+
		"three: a derived port is ADDITIONAL work per tick, not a substitute.",
		stepOps)
}

// --- F6: WHERE the framebuffer's ops actually go ---------------------------
//
// F5 priced an 8x8 framebuffer at 87,514 ops and that number is not credible on
// its face: 64 pixels should not cost more than a 24-actor physics step. "Take
// my word for it" is not a result, so this test decomposes the cost until the
// arithmetic is visible, and separates three different things that F5 conflated:
//
//	(a) the per-pixel floor — what a map over P pixels costs before any actor
//	    is consulted at all;
//	(b) the GATHER — P x CAP, the term that is inherent to a pure
//	    (state) -> frame function (you cannot scatter into a mutable buffer,
//	    so every output pixel must ask "which actors cover me?");
//	(c) the AUTHOR'S CONSTANT — how much of (b) was a sloppy lowering rather
//	    than the port's real price.
//
// Ops are counted per NODE EVALUATION (one Evaluate call = one op,
// ext/compute/eval.go:60), which is what makes this decomposition meaningful:
// the number is a property of the expression graph, not of the engine.
func TestExpAsteroidsF6_FramebufferCostDecomposition(t *testing.T) {
	if testing.Short() {
		t.Skip("decomposition bisects the budget; slow")
	}
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeedDuel(7)
	seed.Kinds[2] = astBullet
	seed.Xs[2] = astWorld / 2
	seed.Ys[2] = astWorld/2 - 20*astFP
	seed.Ttls[2] = astBulletTTL
	p := astSetup(t, ap, "app/asteroids/f6", seed)

	c := ap.Compute()
	sc := c.LookupScope
	const n = 8
	pix := make([]uint64, n*n)
	for i := range pix {
		pix[i] = uint64(i)
	}

	// (a) the floor: a map over the same P pixels that consults nothing.
	floorExpr := c.Construct(astFrameType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(n)),
		"height": c.Literal(uint64(n)),
		"pixels": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(pix),
			"fn":         c.Lambda([]string{"p"}, c.Literal(uint64(0))),
		}),
	})
	if _, err := floorExpr.Build(context.Background(), "app/asteroids/f6/floor"); err != nil {
		t.Fatal(err)
	}
	floorOps, ok := astEvalOps(t, ap, "app/asteroids/f6/floor")
	if !ok {
		t.Fatal("even the empty framebuffer is over the cap")
	}

	// (a2) the floor + per-pixel coordinate math, still consulting no actor.
	coordExpr := c.Construct(astFrameType, map[string]*entitysdk.Builder{
		"width":  c.Literal(uint64(n)),
		"height": c.Literal(uint64(n)),
		"pixels": c.BuiltinsCall("map", map[string]*entitysdk.Builder{
			"collection": c.Literal(pix),
			"fn": c.Lambda([]string{"p"},
				c.Let(map[string]*entitysdk.Builder{
					"px": c.Arithmetic("mod", sc("p"), c.Literal(uint64(n))),
				}, c.Let(map[string]*entitysdk.Builder{
					"py": c.Arithmetic("div",
						c.Arithmetic("sub", sc("p"), sc("px")), c.Literal(uint64(n))),
				}, c.Arithmetic("add",
					c.Arithmetic("mul", sc("px"), c.Literal(astWorld/int64(n))),
					c.Arithmetic("mul", sc("py"), c.Literal(astWorld/int64(n))))))),
		}),
	})
	if _, err := coordExpr.Build(context.Background(), "app/asteroids/f6/coord"); err != nil {
		t.Fatal(err)
	}
	coordOps, ok := astEvalOps(t, ap, "app/asteroids/f6/coord")
	if !ok {
		t.Fatal("the coordinate-only framebuffer is over the cap")
	}

	// (b)+(c): the three real lowerings.
	lower := []struct {
		name        string
		hoist, live bool
		path        string
	}{
		{"naive", false, false, "app/asteroids/f6/naive"},
		{"hoist", true, false, "app/asteroids/f6/hoist"},
		{"hoist+live", true, true, "app/asteroids/f6/live"},
	}
	ops := map[string]int{}
	frames := map[string]astFrame{}
	for _, l := range lower {
		if _, err := buildAsteroidsFramebufferV(ap, p.statePath, n, n, l.hoist, l.live).
			Build(context.Background(), l.path); err != nil {
			t.Fatal(err)
		}
		o, ok := astEvalOps(t, ap, l.path)
		if !ok {
			t.Fatalf("8x8 framebuffer (%s) over the cap", l.name)
		}
		ops[l.name] = o
		var f astFrame
		astEvalPort(t, ap, l.path, &f)
		frames[l.name] = f
	}
	naiveOps, hoistOps, liveOps := ops["naive"], ops["hoist"], ops["hoist+live"]

	// CORRECTNESS: every lowering must be a PURE REFACTOR of the naive one. If
	// they disagree on a single pixel, the "cheaper" numbers are meaningless —
	// the fastest way to render a frame is to render it wrong.
	fn := frames["naive"]
	lit := 0
	for _, px := range fn.Pixels {
		if px != astFree {
			lit++
		}
	}
	if lit == 0 {
		t.Fatal("VACUOUS: the frame is empty — measuring the cost of drawing nothing")
	}
	for _, l := range lower[1:] {
		f := frames[l.name]
		if len(f.Pixels) != len(fn.Pixels) {
			t.Fatalf("%s: frame length differs: %d vs %d", l.name, len(f.Pixels), len(fn.Pixels))
		}
		for i := range fn.Pixels {
			if f.Pixels[i] != fn.Pixels[i] {
				t.Fatalf("%s changed pixel %d: %d != %d — not a pure refactor, so the "+
					"cost comparison is invalid", l.name, i, f.Pixels[i], fn.Pixels[i])
			}
		}
	}

	pxCount := n * n
	t.Logf("framebuffer %dx%d (%d pixels), %d actor slots, %d px lit:",
		n, n, pxCount, astCapacity, lit)
	t.Logf("  %-34s %8s %10s", "stage", "ops", "ops/pixel")
	t.Logf("  %-34s %8d %10.1f", "(a) map over pixels, no actors",
		floorOps, float64(floorOps)/float64(pxCount))
	t.Logf("  %-34s %8d %10.1f", "(a2) + per-pixel coordinate math",
		coordOps, float64(coordOps)/float64(pxCount))
	t.Logf("  %-34s %8d %10.1f", "(b+c) + gather, NAIVE lowering",
		naiveOps, float64(naiveOps)/float64(pxCount))
	t.Logf("  %-34s %8d %10.1f", "(b) + gather, HOISTED lowering",
		hoistOps, float64(hoistOps)/float64(pxCount))
	t.Logf("  %-34s %8d %10.1f", "(b) + gather, HOIST+LIVE lowering",
		liveOps, float64(liveOps)/float64(pxCount))
	live := astCapacity - astCount(seed, astFree)
	t.Logf("  the gather term: naive %d ops over %d pixel-SLOT pairs = %.1f ops/pair",
		naiveOps-coordOps, pxCount*astCapacity,
		float64(naiveOps-coordOps)/float64(pxCount*astCapacity))
	t.Logf("  the gather term: hoist %d ops over %d pixel-SLOT pairs = %.1f ops/pair",
		hoistOps-coordOps, pxCount*astCapacity,
		float64(hoistOps-coordOps)/float64(pxCount*astCapacity))
	t.Logf("  the gather term: live  %d ops over %d pixel-LIVE pairs = %.1f ops/pair",
		liveOps-coordOps, pxCount*live,
		float64(liveOps-coordOps)/float64(pxCount*live))
	t.Logf("  AUTHOR'S CONSTANT: hoisting removes %d ops (%.0f%%); gathering over the "+
		"%d LIVE actors instead of all %d slots removes %d more. Total self-inflicted: "+
		"%.0f%% of the F5 headline.",
		naiveOps-hoistOps, 100*float64(naiveOps-hoistOps)/float64(naiveOps),
		live, astCapacity, hoistOps-liveOps,
		100*float64(naiveOps-liveOps)/float64(naiveOps))

	// The resolution sweep, on the BEST lowering: where does a useful
	// framebuffer actually sit relative to the cap? F5 answered this on the
	// naive lowering and therefore answered it wrong.
	t.Logf("resolution sweep, hoist+live lowering, %d live actors:", live)
	for _, r := range []int{8, 16, 32, 64} {
		path := fmt.Sprintf("app/asteroids/f6/sweep%d", r)
		if _, err := buildAsteroidsFramebufferV(ap, p.statePath, r, r, true, true).
			Build(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		o, ok := astEvalOps(t, ap, path)
		if !ok {
			t.Logf("  %3dx%-3d (%5d px)  >100k — OVER THE CAP", r, r, r*r)
			continue
		}
		t.Logf("  %3dx%-3d (%5d px)  %6d ops  (%.1f ops/px)", r, r, r*r, o,
			float64(o)/float64(r*r))
	}

	// The claim under test, stated so it can FAIL: the gather dominates. If the
	// per-pixel floor were the problem, resolution would be the enemy; it is not
	// — the pixel x actor product is.
	if coordOps >= hoistOps/2 {
		t.Fatalf("expected the gather to dominate the per-pixel floor; floor+coords=%d "+
			"vs hoisted total=%d — the cost story in the report is wrong",
			coordOps, hoistOps)
	}
	if hoistOps >= naiveOps {
		t.Fatalf("the hoist did not help (naive=%d hoist=%d) — the claim that half "+
			"the cost was self-inflicted is FALSE and the report must be corrected",
			naiveOps, hoistOps)
	}
	if liveOps >= hoistOps {
		t.Fatalf("gathering over the live list did not help (hoist=%d live=%d) — the "+
			"O(pixels x LIVE) claim is FALSE and the report must be corrected",
			hoistOps, liveOps)
	}
}

// --- the shard rig ---------------------------------------------------------

// astShardRig splits one tick into k slot-range evals, each on its own fresh
// budget, and stitches the k fragments host-side into one state entity.
type astShardRig struct {
	ap                   *entitysdk.AppPeer
	statePath, inputPath string
	shards               []string // step path per shard, in slot order
	bounds               [][2]int
}

func newAstShardRig(t testing.TB, ap *entitysdk.AppPeer, root string, k int, seed astState) *astShardRig {
	t.Helper()
	r := &astShardRig{
		ap:        ap,
		statePath: root + "/state",
		inputPath: root + "/input",
	}
	ent, err := astStateEntity(seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(r.statePath, ent); err != nil {
		t.Fatal(err)
	}
	e, err := astInputEntity(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.PutEntity(r.inputPath, e); err != nil {
		t.Fatal(err)
	}
	per := (astCapacity + k - 1) / k
	for j := 0; j < k; j++ {
		i0 := j * per
		i1 := i0 + per
		if i1 > astCapacity {
			i1 = astCapacity
		}
		if i0 >= i1 {
			break
		}
		path := fmt.Sprintf("%s/shard%d", root, j)
		if _, err := buildAsteroidsStepRange(ap, r.statePath, r.inputPath, i0, i1).
			Build(context.Background(), path); err != nil {
			t.Fatalf("build shard %d: %v", j, err)
		}
		r.shards = append(r.shards, path)
		r.bounds = append(r.bounds, [2]int{i0, i1})
	}
	return r
}

func (r *astShardRig) writeInput(t testing.TB, keys uint64) {
	t.Helper()
	ent, err := astInputEntity(keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ap.PutEntity(r.inputPath, ent); err != nil {
		t.Fatal(err)
	}
}

// evalShard evaluates one shard, returning its FRAGMENT (arrays cover the
// shard's slots only).
func (r *astShardRig) evalShard(path string) (*astState, error) {
	req, err := entitysdk.PrimitiveAny(map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	resp, err := r.ap.Executor().ExecuteOnResource("system/compute", "eval", req,
		&types.ResourceTarget{Targets: []string{path}})
	if err != nil {
		return nil, fmt.Errorf("eval dispatch: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("eval status %d (type=%s)", resp.Status, resp.Type)
	}
	if resp.Type == types.TypeComputeError {
		var ed types.ComputeErrorData
		_ = ecf.Decode(resp.Data, &ed)
		return nil, fmt.Errorf("compute/error code=%s message=%q", ed.Code, ed.Message)
	}
	var f astState
	if err := ecf.Decode(resp.Data, &f); err != nil {
		return nil, fmt.Errorf("decode fragment: %w", err)
	}
	return &f, nil
}

// tickParallel runs the k shard evals CONCURRENTLY and stitches in slot order.
// Order is restored by writing each shard's result into its own slot, so
// completion order cannot affect the output — determinism by construction, not
// by luck (the Life rig's reasoning, unchanged).
//
// rng/score/status are whole-state reductions: every shard computes them from
// the same frozen previous state and therefore agrees. The host takes shard 0's
// and asserts the others match — which is itself a determinism check.
func (r *astShardRig) tickParallel() (*astState, hash.Hash, error) {
	frags := make([]*astState, len(r.shards))
	errs := make([]error, len(r.shards))
	var wg sync.WaitGroup
	for j, path := range r.shards {
		wg.Add(1)
		go func(j int, path string) {
			defer wg.Done()
			frags[j], errs[j] = r.evalShard(path)
		}(j, path)
	}
	wg.Wait()
	for j, err := range errs {
		if err != nil {
			return nil, hash.Hash{}, fmt.Errorf("shard %d: %w", j, err)
		}
	}
	out := astState{
		Kinds: make([]uint64, 0, astCapacity),
		Xs:    make([]int64, 0, astCapacity),
		Ys:    make([]int64, 0, astCapacity),
		Vxs:   make([]int64, 0, astCapacity),
		Vys:   make([]int64, 0, astCapacity),
		Rots:  make([]uint64, 0, astCapacity),
		Szs:   make([]uint64, 0, astCapacity),
		Ttls:  make([]uint64, 0, astCapacity),
	}
	for j, f := range frags {
		if got, want := len(f.Kinds), r.bounds[j][1]-r.bounds[j][0]; got != want {
			return nil, hash.Hash{}, fmt.Errorf(
				"shard %d returned %d slots, want %d", j, got, want)
		}
		if f.RNG != frags[0].RNG || f.Score != frags[0].Score || f.Status != frags[0].Status {
			return nil, hash.Hash{}, fmt.Errorf(
				"shard %d disagrees on the whole-state reduction: rng/score/status "+
					"(%d/%d/%d) != shard 0 (%d/%d/%d)",
				j, f.RNG, f.Score, f.Status, frags[0].RNG, frags[0].Score, frags[0].Status)
		}
		out.Kinds = append(out.Kinds, f.Kinds...)
		out.Xs = append(out.Xs, f.Xs...)
		out.Ys = append(out.Ys, f.Ys...)
		out.Vxs = append(out.Vxs, f.Vxs...)
		out.Vys = append(out.Vys, f.Vys...)
		out.Rots = append(out.Rots, f.Rots...)
		out.Szs = append(out.Szs, f.Szs...)
		out.Ttls = append(out.Ttls, f.Ttls...)
	}
	out.RNG, out.Score, out.Status = frags[0].RNG, frags[0].Score, frags[0].Status

	ent, err := astStateEntity(out)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	h, err := r.ap.PutEntity(r.statePath, ent)
	if err != nil {
		return nil, hash.Hash{}, err
	}
	return &out, h, nil
}

// --- F4: sharding stays boundary-hash-identical with CROSS-ACTOR reads ------
//
// The load-bearing determinism result (arch §5.1). Life sharded cleanly because
// each cell read only the frozen previous grid. The open question was whether
// that survives when actors read EACH OTHER and the set changes length.
//
// It should — the reads are of frozen state — but "should" is what this track
// keeps disproving under measurement, so: prove it at every k, against both the
// unsharded tick AND the Go oracle, over a run that actually spawns and splits.
func TestExpAsteroidsF4_ShardBoundaryIdentical(t *testing.T) {
	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Close() })

	seed := astSeedDuel(7)
	fireAt := map[int]bool{1: true}
	// The tick-1 shot reaches the asteroid's hit radius at tick 15, so 18 ticks
	// is the shortest run that actually GROWS the set. That matters: each shard
	// eval costs nearly a whole tick (it reads the full state and runs the same
	// O(N^2) scans; only the actor map narrows), so the run is ticks x (1+sum k)
	// evals — the tick budget is what keeps this test minutes, not hours.
	const ticks = 18

	// the reference: the unsharded whole tick.
	ref := astSetup(t, ap, "app/asteroids/f4ref", seed)
	refChain := make([]hash.Hash, 0, ticks)
	cur := seed
	seedAst := astCount(seed, astAsteroid)
	maxAst := seedAst
	for tk := 1; tk <= ticks; tk++ {
		var keys uint64
		if fireAt[tk] {
			keys = astKeys(astKeyFire)
		}
		ref.writeInput(t, keys)
		got, h, err := ref.tick()
		if err != nil {
			t.Fatalf("ref tick %d: %v", tk, err)
		}
		want := astNext(cur, keys)
		astAssertState(t, tk, got, h, want)
		refChain = append(refChain, h)
		if n := astCount(*got, astAsteroid); n > maxAst {
			maxAst = n
		}
		cur = want
	}
	// ANTI-VACUITY: if the set never grew, this run is Life with extra steps and
	// proves nothing about sharding a VARIABLE set.
	if maxAst <= seedAst {
		t.Fatalf("VACUOUS: reference run never split (max asteroids %d <= seed %d) — "+
			"the variable-set path never ran, so shard identity here would only "+
			"re-prove the fixed-set result", maxAst, seedAst)
	}

	for _, k := range []int{2, 5, 8} {
		r := newAstShardRig(t, ap, fmt.Sprintf("app/asteroids/f4k%d", k), k, seed)
		for tk := 1; tk <= ticks; tk++ {
			var keys uint64
			if fireAt[tk] {
				keys = astKeys(astKeyFire)
			}
			r.writeInput(t, keys)
			_, h, err := r.tickParallel()
			if err != nil {
				t.Fatalf("k=%d tick %d: %v", k, tk, err)
			}
			if h != refChain[tk-1] {
				t.Fatalf("k=%d tick %d: sharded state hash != unsharded\n got  %x\n want %x",
					k, tk, h, refChain[tk-1])
			}
		}
		t.Logf("  k=%-2d (%d shards): %d ticks, every tick's stitched hash == the "+
			"unsharded tick's hash", k, len(r.shards), ticks)
	}
	t.Logf("PASS F4: sharding is boundary-hash-identical at k=2,5,8 (5 divides "+
		"%d unevenly — the last shard is short) with CROSS-ACTOR reads and a "+
		"VARIABLE actor set (grew %d -> %d asteroids). Actors read the frozen "+
		"previous state, so the per-actor step stays independent over read-only "+
		"data — the property that let Life shard, and it survives heterogeneity.",
		astCapacity, seedAst, maxAst)
}
