package entitysdk

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"go.entitychurch.org/entity-core-go/core/hash"
)

// ValueKind classifies a formatted value for renderer-specific styling.
type ValueKind int

const (
	KindNull    ValueKind = iota
	KindBool              // true/false
	KindString            // quoted string
	KindNumber            // int, uint, float
	KindBytes             // raw byte array
	KindHash              // entity content hash (33 bytes, algo 0x00)
	KindKey               // map key
	KindIndex             // array index
	KindUnknown           // fallback
	KindError             // error messages
	KindPath              // entity paths, prompts
)

// FormattedValue is a single rendered value with its kind, text
// representation, and optional hash data (for clickable hash links).
type FormattedValue struct {
	Kind ValueKind
	Text string     // plain text (no markup)
	Hash *hash.Hash // non-nil when Kind == KindHash
}

// FormatValue formats a single decoded CBOR value into plain text
// with a kind tag. Renderers use the kind to apply their own colors.
func FormatValue(v interface{}) FormattedValue {
	switch val := v.(type) {
	case nil:
		return FormattedValue{Kind: KindNull, Text: "null"}
	case bool:
		return FormattedValue{Kind: KindBool, Text: fmt.Sprintf("%v", val)}
	case string:
		if len(val) > 80 {
			return FormattedValue{Kind: KindString, Text: fmt.Sprintf("%q...", val[:77])}
		}
		return FormattedValue{Kind: KindString, Text: fmt.Sprintf("%q", val)}
	case []byte:
		if len(val) == hash.HashSize && val[0] == hash.AlgorithmSHA256 {
			h, err := hash.FromBytes(val)
			if err == nil {
				s := h.String()
				if len(s) > 50 {
					return FormattedValue{Kind: KindHash, Text: s[:20] + "..." + s[len(s)-12:], Hash: &h}
				}
				return FormattedValue{Kind: KindHash, Text: s, Hash: &h}
			}
		}
		if len(val) > 32 {
			return FormattedValue{Kind: KindBytes, Text: fmt.Sprintf("bytes(%d) %s...", len(val), hex.EncodeToString(val[:16]))}
		}
		if len(val) == 0 {
			return FormattedValue{Kind: KindBytes, Text: "bytes(0)"}
		}
		return FormattedValue{Kind: KindBytes, Text: fmt.Sprintf("bytes(%d) %s", len(val), hex.EncodeToString(val))}
	case uint64:
		return FormattedValue{Kind: KindNumber, Text: fmt.Sprintf("%d", val)}
	case int64:
		return FormattedValue{Kind: KindNumber, Text: fmt.Sprintf("%d", val)}
	case float64:
		return FormattedValue{Kind: KindNumber, Text: fmt.Sprintf("%g", val)}
	default:
		return FormattedValue{Kind: KindUnknown, Text: fmt.Sprintf("%v", val)}
	}
}

// IsSimpleValue returns true if the value is a leaf (not a map or slice).
func IsSimpleValue(v interface{}) bool {
	switch v.(type) {
	case map[interface{}]interface{}, []interface{}:
		return false
	default:
		return true
	}
}

// SortedMapKeys returns the string keys of a decoded CBOR map in sorted
// order, along with a lookup map from string key to value.
func SortedMapKeys(m map[interface{}]interface{}) ([]string, map[string]interface{}) {
	keys := make([]string, 0, len(m))
	keyMap := make(map[string]interface{}, len(m))
	for k, v := range m {
		ks := fmt.Sprintf("%v", k)
		keys = append(keys, ks)
		keyMap[ks] = v
	}
	sort.Strings(keys)
	return keys, keyMap
}

// FormattedLine is one line in a formatted CBOR tree.
type FormattedLine struct {
	Indent int             // nesting depth
	Key    *FormattedValue // non-nil for map entries
	Index  int             // >= 0 for array entries, -1 otherwise
	Value  *FormattedValue // non-nil for leaf values
}

// FormatCBOR flattens a decoded CBOR value into a sequence of
// FormattedLines suitable for any renderer. Each line has an indent
// level, optional key/index, and optional leaf value.
func FormatCBOR(v interface{}) []FormattedLine {
	var lines []FormattedLine
	formatCBOR(v, 0, &lines)
	return lines
}

func formatCBOR(v interface{}, indent int, lines *[]FormattedLine) {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		keys, keyMap := SortedMapKeys(val)
		for _, k := range keys {
			child := keyMap[k]
			kv := FormattedValue{Kind: KindKey, Text: k}
			if IsSimpleValue(child) {
				fv := FormatValue(child)
				*lines = append(*lines, FormattedLine{Indent: indent, Key: &kv, Index: -1, Value: &fv})
			} else {
				*lines = append(*lines, FormattedLine{Indent: indent, Key: &kv, Index: -1})
				formatCBOR(child, indent+1, lines)
			}
		}
	case []interface{}:
		if len(val) == 0 {
			fv := FormattedValue{Kind: KindNull, Text: "[]"}
			*lines = append(*lines, FormattedLine{Indent: indent, Index: -1, Value: &fv})
			return
		}
		for i, item := range val {
			if IsSimpleValue(item) {
				fv := FormatValue(item)
				*lines = append(*lines, FormattedLine{Indent: indent, Index: i, Value: &fv})
			} else {
				*lines = append(*lines, FormattedLine{Indent: indent, Index: i})
				formatCBOR(item, indent+1, lines)
			}
		}
	default:
		fv := FormatValue(v)
		*lines = append(*lines, FormattedLine{Indent: indent, Index: -1, Value: &fv})
	}
}

// RenderPlainText renders a FormatCBOR result as plain indented text.
// Useful for logging, testing, and non-UI contexts.
func RenderPlainText(lines []FormattedLine) string {
	var sb strings.Builder
	for _, line := range lines {
		prefix := strings.Repeat("  ", line.Indent)
		switch {
		case line.Key != nil && line.Value != nil:
			sb.WriteString(fmt.Sprintf("%s%s  %s\n", prefix, line.Key.Text, line.Value.Text))
		case line.Key != nil:
			sb.WriteString(fmt.Sprintf("%s%s\n", prefix, line.Key.Text))
		case line.Index >= 0 && line.Value != nil:
			sb.WriteString(fmt.Sprintf("%s[%d] %s\n", prefix, line.Index, line.Value.Text))
		case line.Index >= 0:
			sb.WriteString(fmt.Sprintf("%s[%d]\n", prefix, line.Index))
		case line.Value != nil:
			sb.WriteString(fmt.Sprintf("%s%s\n", prefix, line.Value.Text))
		}
	}
	return sb.String()
}
