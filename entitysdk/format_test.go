package entitysdk

import (
	"strings"
	"testing"

	"go.entitychurch.org/entity-core-go/core/hash"
)

func TestFormatValue_Null(t *testing.T) {
	fv := FormatValue(nil)
	if fv.Kind != KindNull || fv.Text != "null" {
		t.Errorf("got %v %q, want KindNull null", fv.Kind, fv.Text)
	}
}

func TestFormatValue_Bool(t *testing.T) {
	fv := FormatValue(true)
	if fv.Kind != KindBool || fv.Text != "true" {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_String(t *testing.T) {
	fv := FormatValue("hello")
	if fv.Kind != KindString || fv.Text != `"hello"` {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_StringTruncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	fv := FormatValue(long)
	if fv.Kind != KindString {
		t.Errorf("got kind %v", fv.Kind)
	}
	if !strings.HasSuffix(fv.Text, "...") {
		t.Errorf("expected truncation, got %q", fv.Text)
	}
}

func TestFormatValue_Uint64(t *testing.T) {
	fv := FormatValue(uint64(42))
	if fv.Kind != KindNumber || fv.Text != "42" {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_Int64(t *testing.T) {
	fv := FormatValue(int64(-7))
	if fv.Kind != KindNumber || fv.Text != "-7" {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_Float64(t *testing.T) {
	fv := FormatValue(float64(3.14))
	if fv.Kind != KindNumber || fv.Text != "3.14" {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_BytesEmpty(t *testing.T) {
	fv := FormatValue([]byte{})
	if fv.Kind != KindBytes || fv.Text != "bytes(0)" {
		t.Errorf("got %v %q", fv.Kind, fv.Text)
	}
}

func TestFormatValue_BytesShort(t *testing.T) {
	fv := FormatValue([]byte{0xca, 0xfe})
	if fv.Kind != KindBytes {
		t.Errorf("got kind %v", fv.Kind)
	}
	if !strings.Contains(fv.Text, "cafe") {
		t.Errorf("expected hex, got %q", fv.Text)
	}
}

func TestFormatValue_BytesLong(t *testing.T) {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	fv := FormatValue(data)
	if fv.Kind != KindBytes {
		t.Errorf("got kind %v", fv.Kind)
	}
	if !strings.HasSuffix(fv.Text, "...") {
		t.Errorf("expected truncation, got %q", fv.Text)
	}
}

func TestFormatValue_Hash(t *testing.T) {
	// Build a valid 33-byte hash: algorithm 0x00 + 32 bytes digest
	b := make([]byte, hash.HashSize)
	b[0] = hash.AlgorithmSHA256
	for i := 1; i < hash.HashSize; i++ {
		b[i] = byte(i)
	}
	fv := FormatValue(b)
	if fv.Kind != KindHash {
		t.Errorf("got kind %v, want KindHash", fv.Kind)
	}
	if fv.Hash == nil {
		t.Error("expected non-nil Hash")
	}
	if !strings.HasPrefix(fv.Text, "ecf-sha256:") {
		t.Errorf("expected hash string, got %q", fv.Text)
	}
}

func TestIsSimpleValue(t *testing.T) {
	if !IsSimpleValue("hello") {
		t.Error("string should be simple")
	}
	if !IsSimpleValue(42) {
		t.Error("int should be simple")
	}
	if !IsSimpleValue(nil) {
		t.Error("nil should be simple")
	}
	if IsSimpleValue(map[interface{}]interface{}{}) {
		t.Error("map should not be simple")
	}
	if IsSimpleValue([]interface{}{}) {
		t.Error("slice should not be simple")
	}
}

func TestSortedMapKeys(t *testing.T) {
	m := map[interface{}]interface{}{
		"charlie": 3,
		"alpha":   1,
		"bravo":   2,
	}
	keys, keyMap := SortedMapKeys(m)
	if len(keys) != 3 {
		t.Fatalf("got %d keys", len(keys))
	}
	if keys[0] != "alpha" || keys[1] != "bravo" || keys[2] != "charlie" {
		t.Errorf("keys not sorted: %v", keys)
	}
	if keyMap["alpha"] != 1 {
		t.Error("keyMap lookup failed")
	}
}

func TestFormatCBOR_SimpleMap(t *testing.T) {
	data := map[interface{}]interface{}{
		"name":  "test",
		"count": uint64(5),
	}
	lines := FormatCBOR(data)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	// Should be sorted: count before name
	if lines[0].Key.Text != "count" {
		t.Errorf("first key = %q, want count", lines[0].Key.Text)
	}
	if lines[1].Key.Text != "name" {
		t.Errorf("second key = %q, want name", lines[1].Key.Text)
	}
}

func TestFormatCBOR_NestedMap(t *testing.T) {
	data := map[interface{}]interface{}{
		"outer": map[interface{}]interface{}{
			"inner": "value",
		},
	}
	lines := FormatCBOR(data)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].Key.Text != "outer" || lines[0].Value != nil {
		t.Error("first line should be key-only for nested map")
	}
	if lines[1].Indent != 1 {
		t.Errorf("nested line indent = %d, want 1", lines[1].Indent)
	}
}

func TestFormatCBOR_Array(t *testing.T) {
	data := []interface{}{"a", "b", "c"}
	lines := FormatCBOR(data)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		if line.Index != i {
			t.Errorf("line %d index = %d", i, line.Index)
		}
	}
}

func TestFormatCBOR_EmptyArray(t *testing.T) {
	data := []interface{}{}
	lines := FormatCBOR(data)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if lines[0].Value.Text != "[]" {
		t.Errorf("empty array text = %q", lines[0].Value.Text)
	}
}

func TestRenderPlainText(t *testing.T) {
	data := map[interface{}]interface{}{
		"key": "value",
	}
	lines := FormatCBOR(data)
	text := RenderPlainText(lines)
	if !strings.Contains(text, "key") || !strings.Contains(text, `"value"`) {
		t.Errorf("unexpected plain text: %q", text)
	}
}
