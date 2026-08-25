package shell

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"entity-workbench-go/shellcmd"
	"go.entitychurch.org/entity-core-go/core/entity"
)

// FormatText writes a renderer-neutral Result as plain text to out.
// Empty results produce no output (matches the shell convention
// where commands like cd succeed silently).
func FormatText(out io.Writer, r shellcmd.Result) {
	switch r.Kind {
	case shellcmd.KindNone:
		// silent success
	case shellcmd.KindMessage:
		fmt.Fprintln(out, r.Message)
	case shellcmd.KindPath:
		fmt.Fprintln(out, r.Path)
	case shellcmd.KindLines:
		for _, line := range r.Lines {
			fmt.Fprintln(out, line)
		}
	case shellcmd.KindListing:
		formatListingText(out, r.Listing)
	case shellcmd.KindEntity:
		formatEntityText(out, r.Entity)
	case shellcmd.KindTree:
		formatTreeText(out, r.Tree)
	case shellcmd.KindDispatch:
		formatDispatchText(out, r.Dispatch)
	case shellcmd.KindInfo:
		formatInfoText(out, r.Info)
	}
}

func formatListingText(out io.Writer, rows []shellcmd.ListingRow) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "  (empty)")
		return
	}
	for _, row := range rows {
		switch row.Kind {
		case "connection":
			fmt.Fprintf(out, "  %-12s %s\n", row.Name, row.Detail)
		default:
			suffix := ""
			if row.HasChildren {
				suffix = "/"
			}
			fmt.Fprintf(out, "  %-30s %s\n", row.Name+suffix, row.Kind)
		}
	}
}

func formatEntityText(out io.Writer, p *shellcmd.EntityPayload) {
	if p == nil {
		return
	}
	if p.Diag {
		fmt.Fprint(out, p.Entity.DiagnoseHash())
		return
	}
	fmt.Fprintf(out, "Type:  %s\n", p.Entity.Type)
	fmt.Fprintf(out, "Hash:  %s\n", p.Entity.ContentHash)
	if p.Decoded == nil {
		fmt.Fprintf(out, "Data:  (raw, %d bytes)\n", len(p.Entity.Data))
		return
	}
	fmt.Fprintln(out, "Data:")
	var b strings.Builder
	entity.FormatCBORValue(&b, "  ", p.Decoded)
	fmt.Fprint(out, b.String())
}

func formatTreeText(out io.Writer, rows []shellcmd.TreeRow) {
	for _, row := range rows {
		indent := strings.Repeat("  ", row.Depth)
		suffix := ""
		if row.HasChildren {
			suffix = "/"
		}
		if row.Entity != nil {
			fmt.Fprintf(out, "%s%s%s  [%s] %s\n", indent, row.Name, suffix, row.Entity.Type, row.Entity.ContentHash)
			if row.Decoded != nil {
				var b strings.Builder
				entity.FormatCBORValue(&b, indent+"    ", row.Decoded)
				fmt.Fprint(out, b.String())
			}
		} else {
			fmt.Fprintf(out, "%s%s%s\n", indent, row.Name, suffix)
		}
	}
}

func formatDispatchText(out io.Writer, d *shellcmd.DispatchResp) {
	if d == nil {
		return
	}
	fmt.Fprintf(out, "Status: %d\n", d.Status)
	fmt.Fprintf(out, "Type:   %s\n", d.Result.Type)
	fmt.Fprintf(out, "Hash:   %s\n", d.Result.ContentHash)
	if d.Decoded != nil {
		fmt.Fprintln(out, "Data:")
		var b strings.Builder
		entity.FormatCBORValue(&b, "  ", d.Decoded)
		fmt.Fprint(out, b.String())
	}
	if d.Included > 0 {
		fmt.Fprintf(out, "Included: %d entities\n", d.Included)
	}
}

func formatInfoText(out io.Writer, info *shellcmd.PeerInfo) {
	if info == nil {
		return
	}
	fmt.Fprintf(out, "Alias:   %s\n", info.Alias)
	fmt.Fprintf(out, "Address: %s\n", info.Address)
	fmt.Fprintf(out, "PeerID:  %s\n", info.PeerID)
	if len(info.Grants) > 0 {
		fmt.Fprintf(out, "Grants:  %d\n", len(info.Grants))
	}
}

// FormatJSON writes a Result to out as a JSON object. The shape
// mirrors the result type so consumers can reliably parse without
// reverse-engineering the text format.
func FormatJSON(out io.Writer, r shellcmd.Result) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch r.Kind {
	case shellcmd.KindNone:
		return enc.Encode(map[string]interface{}{"kind": "none"})
	case shellcmd.KindMessage:
		return enc.Encode(map[string]interface{}{"kind": "message", "message": r.Message})
	case shellcmd.KindPath:
		return enc.Encode(map[string]interface{}{"kind": "path", "path": r.Path.String()})
	case shellcmd.KindLines:
		return enc.Encode(map[string]interface{}{"kind": "lines", "lines": r.Lines})
	case shellcmd.KindListing:
		return enc.Encode(map[string]interface{}{"kind": "listing", "rows": jsonListing(r.Listing)})
	case shellcmd.KindEntity:
		return enc.Encode(jsonEntity(r.Entity))
	case shellcmd.KindTree:
		return enc.Encode(map[string]interface{}{"kind": "tree", "rows": jsonTree(r.Tree)})
	case shellcmd.KindDispatch:
		return enc.Encode(jsonDispatch(r.Dispatch))
	case shellcmd.KindInfo:
		return enc.Encode(jsonInfo(r.Info))
	}
	return nil
}

func jsonListing(rows []shellcmd.ListingRow) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		m := map[string]interface{}{
			"name":         row.Name,
			"path":         row.Path,
			"kind":         row.Kind,
			"has_children": row.HasChildren,
		}
		if row.Detail != "" {
			m["detail"] = row.Detail
		}
		if !row.Hash.IsZero() {
			m["hash"] = row.Hash.String()
		}
		out = append(out, m)
	}
	return out
}

func jsonTree(rows []shellcmd.TreeRow) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		m := map[string]interface{}{
			"name":         row.Name,
			"path":         row.Path,
			"depth":        row.Depth,
			"kind":         row.Kind,
			"has_children": row.HasChildren,
		}
		if !row.Hash.IsZero() {
			m["hash"] = row.Hash.String()
		}
		if row.Entity != nil {
			m["type"] = row.Entity.Type
		}
		out = append(out, m)
	}
	return out
}

func jsonEntity(p *shellcmd.EntityPayload) map[string]interface{} {
	if p == nil {
		return map[string]interface{}{"kind": "entity"}
	}
	return map[string]interface{}{
		"kind": "entity",
		"type": p.Entity.Type,
		"hash": p.Entity.ContentHash.String(),
		"size": len(p.Entity.Data),
	}
}

func jsonDispatch(d *shellcmd.DispatchResp) map[string]interface{} {
	if d == nil {
		return map[string]interface{}{"kind": "dispatch"}
	}
	return map[string]interface{}{
		"kind":     "dispatch",
		"status":   d.Status,
		"type":     d.Result.Type,
		"hash":     d.Result.ContentHash.String(),
		"included": d.Included,
	}
}

func jsonInfo(info *shellcmd.PeerInfo) map[string]interface{} {
	if info == nil {
		return map[string]interface{}{"kind": "info"}
	}
	return map[string]interface{}{
		"kind":    "info",
		"alias":   info.Alias,
		"address": info.Address,
		"peer_id": info.PeerID,
	}
}
