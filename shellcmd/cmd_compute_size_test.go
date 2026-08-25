package shellcmd

// `compute aggregate` has to read a size the shell's own `put` wrote.
//
// The two verbs sit at opposite ends of one user gesture — put an entity,
// aggregate over it — and they disagreed about what a JSON number is.
// `put` decodes with json.Unmarshal into an interface{}, so every JSON
// number arrives as a float64; CBOR core-deterministic encoding keeps it a
// float rather than folding an integral value back to an integer. The
// extractor accepted only the integer kinds, so it skipped every entity
// created the documented way and reported "N entities scanned, N skipped".
//
// The seam is what makes this worth a test rather than a one-line diff: the
// extractor's own unit coverage constructed Go ints directly and was green
// throughout. Only a case that starts from the put payload can see it.
//
// Tier: regression (cross-verb contract).

import (
	"encoding/json"
	"testing"

	"go.entitychurch.org/entity-core-go/core/ecf"
)

// putEncode mirrors cmd_tree.go::cmdPut's payload handling — json.Unmarshal
// into an interface{}, then ECF-encode — so the bytes under test are the
// bytes `put` actually stores.
func putEncode(t *testing.T, payload string) []byte {
	t.Helper()
	var data interface{}
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatalf("payload %s is not the JSON put would accept: %v", payload, err)
	}
	blob, err := ecf.Encode(data)
	if err != nil {
		t.Fatalf("ecf.Encode: %v", err)
	}
	return blob
}

func TestExtractNumericSize_ReadsWhatPutWrote(t *testing.T) {
	// Guard the premise, so a future encoder change that folds integral
	// floats back to integers reports itself here instead of making this
	// test quietly vacuous.
	var round map[string]interface{}
	if err := ecf.Decode(putEncode(t, `{"name":"a","size":100}`), &round); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, isFloat := round["size"].(float64); !isFloat {
		t.Logf("note: a put-written size no longer round-trips as float64 (%T) — "+
			"the float cases below are now belt-and-braces, not the live path", round["size"])
	}

	cases := []struct {
		name    string
		payload string
		want    uint64
		ok      bool
	}{
		{"integral float from put", `{"name":"a","size":100}`, 100, true},
		{"zero", `{"size":0}`, 0, true},
		{"large but exact", `{"size":9007199254740992}`, 9007199254740992, true},
		{"non-integral is refused, not truncated", `{"size":100.5}`, 0, false},
		{"negative is refused", `{"size":-1}`, 0, false},
		{"string is not a size", `{"size":"100"}`, 0, false},
		{"absent", `{"name":"a"}`, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractNumericSize(putEncode(t, tc.payload))
			if ok != tc.ok || got != tc.want {
				t.Errorf("extractNumericSize(%s) = (%d, %v), want (%d, %v)",
					tc.payload, got, ok, tc.want, tc.ok)
			}
		})
	}
}
