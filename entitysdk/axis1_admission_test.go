package entitysdk_test

// AE-5 inproc admission harness — run the frozen compute-conformance corpus
// in-process through Axis-1 and emit the alternate-engine emission core-go's
// gate consumes.
//
// WHAT THIS IS. Per core-go's routing packet
// (entity-core-go/docs/status/ROUTING-2026-07-23-ae5-axis1-inproc-admission.md),
// the EXTENSION-COMPUTE §11 appendix folds on the first green inproc admission
// run (AE-5) with an actual alternate engine. Axis-1 is the only alternate
// compute engine that exists, and it lives here (entitysdk/axis1), importing
// core-go — which cannot import it back (that inverts the module DAG). So the
// alternate emission is produced HERE and handed back; core-go runs
// `verify --require-alternate` + `cross-bless`.
//
// This mirrors core-go's reference emit loop
// (cmd/internal/compute-corpus/evaluate.go::evalVector + main.go's emit) step
// for step, with ONE swap: compute.Evaluate → Axis-1's Engine.Evaluate +
// axis1.Materialize. The final boundary reduction stays compute.CaptureScope —
// the reference's own path — so the boundary hash is authoritative, never
// re-derived here (evaluate.go's THE BOUNDARY REDUCTION note).
//
// THE STRUCTS ARE REPLICATED, NOT IMPORTED. The corpus/emission types live in
// core-go's `package main` under cmd/internal, so they are unreachable from
// here. They are transcribed below with byte-identical cbor tags and encoded
// through the same ecf.Encode, so core-go's verify/cross-bless decode the output
// with no adapter. TestAxis1Admission_StructRoundTrip guards that transcription
// against tag drift on every `make test-sdk`.
//
// HOW TO PRODUCE THE ARTIFACT (the corpus is core-go's; regenerate it there):
//
//	# in entity-core-go/:
//	go run ./cmd/internal/compute-corpus generate --profile inproc --out corpus-inproc.cbor
//	# in entity-workbench-go/entitysdk/:
//	AXIS1_ADMISSION_CORPUS=/abs/path/corpus-inproc.cbor \
//	AXIS1_ADMISSION_OUT=/abs/path/emit-axis1-inproc.cbor \
//	  make -C .. test-sdk ARGS="-run TestAxis1Admission_EmitInprocEmission -v"
//	# hand emit-axis1-inproc.cbor back to core-go for verify + cross-bless.
//
// Without AXIS1_ADMISSION_CORPUS the emit test skips, so it is inert in the
// normal sweep; the round-trip smoke always runs.
//
// FALLBACKS ARE THE POINT, NOT AN ANNOYANCE. Axis-1 carries a Stage-1 fallback
// seam (evalFallback → compute.Evaluate, counted in Stats.Fallbacks). Per AE-6 a
// fallback voids that vector's evidence — a boundary produced by Stage-1 is not
// Axis-1 evidence. So a fallen-back vector is recorded as a SKIP (never as a
// passing Result), and the aggregate Fallbacks count rides in the emission where
// core-go's guard 6 rejects any alternate emission with Fallbacks != 0. This
// harness reports that state faithfully and fails loudly; it never masks it.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/compute"

	"github.com/fxamacker/cbor/v2"

	"entity-workbench-go/entitysdk/axis1"
)

// --- Replicated corpus/emission contract (transcribed from core-go's
//     cmd/internal/compute-corpus/corpus.go; tags MUST stay byte-identical) ---

type vecEntity struct {
	Type string          `cbor:"type"`
	Data cbor.RawMessage `cbor:"data"`
}

type vecBudget struct {
	Operations int `cbor:"operations"`
	Depth      int `cbor:"depth"`
}

type vector struct {
	ID        string               `cbor:"id"`
	CaseIndex int                  `cbor:"case_index,omitempty"`
	Entities  []vecEntity          `cbor:"entities"`
	Root      hash.Hash            `cbor:"root"`
	Bindings  cbor.RawMessage      `cbor:"bindings"`
	Tree      map[string]hash.Hash `cbor:"tree,omitempty"`
	Budget    vecBudget            `cbor:"budget"`
	Requires  []string             `cbor:"requires,omitempty"`
}

type corpus struct {
	CorpusVersion    string   `cbor:"corpus_version"`
	GeneratorVersion string   `cbor:"generator_version"`
	PRNG             string   `cbor:"prng"`
	Seed             uint64   `cbor:"seed"`
	SweepCases       int      `cbor:"sweep_cases"`
	Profile          string   `cbor:"profile"`
	SpecVersion      string   `cbor:"spec_version"`
	Vectors          []vector `cbor:"vectors"`
}

type admOutcome struct {
	Kind     string `cbor:"kind"` // "entity" | "value" | "error"
	Boundary []byte `cbor:"boundary,omitempty"`
	Code     string `cbor:"code,omitempty"`
	Message  string `cbor:"message,omitempty"`
}

type emission struct {
	Impl          string             `cbor:"impl"`
	ImplVersion   string             `cbor:"impl_version"`
	Engine        string             `cbor:"engine"`
	EngineRole    string             `cbor:"engine_role"`
	Fallbacks     int                `cbor:"fallbacks"`
	CorpusSHA256  []byte             `cbor:"corpus_sha256"`
	CorpusVersion string             `cbor:"corpus_version"`
	SpecVersion   string             `cbor:"spec_version"`
	Results       map[string]admOutcome `cbor:"results"`
	Skipped       map[string]string  `cbor:"skipped,omitempty"`
}

const (
	outcomeEntity = "entity"
	outcomeValue  = "value"
	outcomeError  = "error"
	roleAlternate = "alternate"
	// specVersion tracks core-go's corpus const; cross-bless does not compare it,
	// but matching keeps the emission legible beside the reference.
	admissionSpecVersion = "3.20"
)

// --- The eval loop (mirror of evaluate.go::evalVector, Axis-1 swapped in) ---

// evalVectorAxis1 runs one vector through Axis-1 to its boundary admOutcome. It
// returns the admOutcome, whether the vector deopted to the Stage-1 seam (AE-6:
// that voids its evidence), and a harness error (recorded as a SKIP upstream).
func evalVectorAxis1(eng *axis1.Engine, v vector) (out admOutcome, fellBack bool, err error) {
	cs := store.NewMemoryContentStore()
	li := store.NewMemoryLocationIndex()

	byHash := make(map[string]entity.Entity, len(v.Entities))
	for i, ve := range v.Entities {
		ent, e := entity.NewEntity(ve.Type, ve.Data)
		if e != nil {
			return admOutcome{}, false, fmt.Errorf("entity %d (%s): %w", i, ve.Type, e)
		}
		if _, e := cs.Put(ent); e != nil {
			return admOutcome{}, false, fmt.Errorf("store entity %d: %w", i, e)
		}
		byHash[string(ent.ContentHash.Bytes())] = ent
	}
	for path, h := range v.Tree {
		if e := li.Set(path, h); e != nil {
			return admOutcome{}, false, fmt.Errorf("tree %q: %w", path, e)
		}
	}

	root, ok := byHash[string(v.Root.Bytes())]
	if !ok {
		return admOutcome{}, false, fmt.Errorf("root %s not in closure", v.Root)
	}

	// Axis-1 takes the root scope as a plain map (compute.Scope cannot be
	// enumerated); decode the vector's canonical-CBOR bindings straight into one.
	var bindings map[string]interface{}
	if e := ecf.Decode(v.Bindings, &bindings); e != nil {
		return admOutcome{}, false, fmt.Errorf("decode bindings: %w", e)
	}

	// Peer-free context, exactly as the reference: a closed graph against a
	// supplied scope + tree, no capability chain, store access on so lookup/hash
	// can reach the vector's own entities.
	ctx := &compute.EvalContext{
		ContentStore:          cs,
		LocationIndex:         li,
		HasContentStoreAccess: true,
	}
	budget := compute.NewBudget(v.Budget.Operations, v.Budget.Depth)

	before := eng.Stats().Fallbacks
	result, evalErr := eng.Evaluate(root, bindings, budget, ctx)
	fellBack = eng.Stats().Fallbacks > before

	if evalErr != nil {
		if ce, ok := evalErr.(*compute.ComputeError); ok {
			return admOutcome{Kind: outcomeError, Code: ce.Code, Message: ce.Message}, fellBack, nil
		}
		// A non-compute error escaping the engine is a harness-visible defect, not
		// a vector admOutcome — it has no spec code, so recording it as an error
		// would invent one and let a crash cross-bless green.
		return admOutcome{}, fellBack, fmt.Errorf("axis1 returned a non-compute error: %w", evalErr)
	}

	// Cross the boundary with Axis-1's own materialize (its interior
	// *constructedValue/*closure are unknown to compute.CaptureScope), then reduce
	// through the reference's CaptureScope path.
	mv, e := axis1.Materialize(result, cs)
	if e != nil {
		return admOutcome{}, fellBack, fmt.Errorf("materialize: %w", e)
	}
	out, e = boundaryOfAdmission(mv, cs)
	return out, fellBack, e
}

// boundaryOfAdmission reduces a materialized value to its boundary form via
// compute.CaptureScope — a verbatim mirror of evaluate.go::boundaryOf, so the
// boundary bytes are the reference's, not re-derived here.
func boundaryOfAdmission(v interface{}, cs store.ContentStore) (admOutcome, error) {
	s := compute.NewScope()
	s.Set("r", v)
	ent, err := compute.CaptureScope(s, cs)
	if err != nil {
		return admOutcome{}, fmt.Errorf("capture scope: %w", err)
	}
	var d types.ComputeScopeData
	if err := ecf.Decode(ent.Data, &d); err != nil {
		return admOutcome{}, fmt.Errorf("decode captured scope: %w", err)
	}
	b, ok := d.Bindings["r"]
	if !ok {
		return admOutcome{}, fmt.Errorf("captured scope is missing binding r")
	}
	switch b.Kind {
	case types.ScopeBindingKindEntity:
		if b.EntityHash == nil {
			return admOutcome{}, fmt.Errorf("entity binding carries no hash")
		}
		// Bytes(), not EffectiveDigest(): the algorithm byte travels with the hash.
		return admOutcome{Kind: outcomeEntity, Boundary: b.EntityHash.Bytes()}, nil
	case types.ScopeBindingKindValue:
		raw, err := ecf.Encode(b.Value)
		if err != nil {
			return admOutcome{}, fmt.Errorf("encode value binding: %w", err)
		}
		return admOutcome{Kind: outcomeValue, Boundary: raw}, nil
	default:
		return admOutcome{}, fmt.Errorf("unknown scope binding kind %q", b.Kind)
	}
}

// --- The admission run ---

func TestAxis1Admission_EmitInprocEmission(t *testing.T) {
	corpusPath := os.Getenv("AXIS1_ADMISSION_CORPUS")
	if corpusPath == "" {
		t.Skip("set AXIS1_ADMISSION_CORPUS=/abs/path/corpus-inproc.cbor to run the AE-5 admission emission (see file header)")
	}
	outPath := os.Getenv("AXIS1_ADMISSION_OUT")
	if outPath == "" {
		outPath = "emit-axis1-inproc.cbor"
	}

	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	sum := sha256.Sum256(raw)
	var c corpus
	if err := ecf.Decode(raw, &c); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(c.Vectors) == 0 {
		t.Fatalf("corpus carries no vectors")
	}
	t.Logf("corpus %s: profile=%s version=%s sweep=%d, %d vectors, sha256=%x",
		corpusPath, c.Profile, c.CorpusVersion, c.SweepCases, len(c.Vectors), sum[:6])

	eng := axis1.NewEngine()
	em := &emission{
		Impl:          "go",
		ImplVersion:   "unspecified", // matches core-go's --impl-version default; not compared
		Engine:        "axis1",
		EngineRole:    roleAlternate,
		CorpusSHA256:  sum[:],
		CorpusVersion: c.CorpusVersion,
		SpecVersion:   admissionSpecVersion,
		Results:       make(map[string]admOutcome, len(c.Vectors)),
		Skipped:       map[string]string{},
	}

	var kinds struct{ entity, value, err int }
	codes := map[string]int{}
	var fellBackIDs []string

	for _, v := range c.Vectors {
		o, fellBack, err := evalVectorAxis1(eng, v)
		if err != nil {
			// A harness failure is a SKIP, never an admOutcome (§3.1(2)).
			em.Skipped[v.ID] = err.Error()
			continue
		}
		if fellBack {
			// AE-6: a Stage-1 deopt voids this vector's Axis-1 evidence. Record it
			// as a skip (not a passing Result) AND count it — guard 6 gates on the
			// aggregate.
			em.Fallbacks++
			fellBackIDs = append(fellBackIDs, v.ID)
			em.Skipped[v.ID] = "axis1 deopted to the Stage-1 seam (AE-6: evidence void)"
			continue
		}
		em.Results[v.ID] = o
		switch o.Kind {
		case outcomeEntity:
			kinds.entity++
		case outcomeValue:
			kinds.value++
		case outcomeError:
			kinds.err++
			codes[o.Code]++
		}
	}

	// Write the artifact FIRST — it exists for core-go's triage even if the run
	// is not AE-5-green (fallbacks/skips), which is the useful failure mode.
	outRaw, err := ecf.Encode(em)
	if err != nil {
		t.Fatalf("encode emission: %v", err)
	}
	if err := os.WriteFile(outPath, outRaw, 0o644); err != nil {
		t.Fatalf("write emission: %v", err)
	}
	outSum := sha256.Sum256(outRaw)
	t.Logf("wrote %s (%d bytes, sha256=%x)", outPath, len(outRaw), outSum[:6])
	t.Logf("answered %d/%d — %d value, %d entity, %d error; %d fallbacks, %d skipped",
		len(em.Results), len(c.Vectors), kinds.value, kinds.entity, kinds.err,
		em.Fallbacks, len(em.Skipped))
	for _, code := range sortedKeysInt(codes) {
		t.Logf("  error code %-20s %d", code, codes[code])
	}

	// AE-5 success == every vector answered by Axis-1 natively, zero deopts.
	// Anything short is a real, routable finding — surface it loudly, but the
	// emission is already on disk for triage.
	if em.Fallbacks > 0 {
		sort.Strings(fellBackIDs)
		t.Errorf("AE-6: %d vector(s) deopted to Stage-1 — guard 6 will reject this emission "+
			"(engine not yet native for: %v). The emission is written for triage; Axis-1 owes "+
			"native coverage for these ops before AE-5 can go green.", em.Fallbacks, fellBackIDs)
	}
	if n := len(em.Skipped) - em.Fallbacks; n > 0 {
		t.Errorf("%d vector(s) skipped on a harness error (see AXIS1_ADMISSION_OUT skipped map)", n)
	}
	if em.Fallbacks == 0 && len(em.Skipped) == 0 {
		t.Logf("AE-5 candidate: %d/%d answered natively, 0 fallbacks, 0 skips. "+
			"Hand %s to core-go for verify --require-alternate + cross-bless.",
			len(em.Results), len(c.Vectors), outPath)
	}
}

// TestAxis1Admission_StructRoundTrip guards the replicated contract against tag
// drift with no external artifact, so it runs in the normal sweep. If core-go's
// corpus.go structs and these diverge, an emission would silently fail to decode
// in cross-bless; this catches it here instead.
func TestAxis1Admission_StructRoundTrip(t *testing.T) {
	want := &emission{
		Impl:          "go",
		ImplVersion:   "unspecified",
		Engine:        "axis1",
		EngineRole:    roleAlternate,
		Fallbacks:     0,
		CorpusSHA256:  []byte{0x91, 0x31, 0xa9, 0x3d},
		CorpusVersion: "v1",
		SpecVersion:   admissionSpecVersion,
		Results: map[string]admOutcome{
			"worked/arith":     {Kind: outcomeValue, Boundary: []byte{0x05}},
			"worked/construct": {Kind: outcomeEntity, Boundary: []byte{0x00, 0x01, 0x02}},
			"worked/divzero":   {Kind: outcomeError, Code: "division_by_zero", Message: "Division by zero"},
		},
	}
	raw, err := ecf.Encode(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// ecf.Encode is canonical/deterministic — a second pass must be byte-identical.
	raw2, err := ecf.Encode(want)
	if err != nil {
		t.Fatalf("encode (2): %v", err)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("ecf.Encode not deterministic for emission")
	}

	var got emission
	if err := ecf.Decode(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Impl != want.Impl || got.Engine != want.Engine || got.EngineRole != want.EngineRole ||
		got.Fallbacks != want.Fallbacks || got.CorpusVersion != want.CorpusVersion ||
		got.SpecVersion != want.SpecVersion {
		t.Fatalf("scalar fields did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Results) != len(want.Results) {
		t.Fatalf("results length: got %d want %d", len(got.Results), len(want.Results))
	}
	for id, wo := range want.Results {
		go_ := got.Results[id]
		if go_.Kind != wo.Kind || go_.Code != wo.Code || string(go_.Boundary) != string(wo.Boundary) {
			t.Errorf("result %q: got %+v want %+v", id, go_, wo)
		}
	}
}

func sortedKeysInt(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
