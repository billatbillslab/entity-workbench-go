package workbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractFirstHeading(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"atx h1", "# Title\nbody", "Title"},
		{"atx h1 no space", "#Title", "Title"},
		{"skips non-heading lines", "intro\n\n# Real Heading\nmore", "Real Heading"},
		{"h2 is not h1", "## Sub\ntext", ""},
		{"setext not recognized", "Title\n=====\n", ""},
		{"leading whitespace", "   # Indented\n", "Indented"},
		{"no heading", "just prose\nno headings here", ""},
		{"empty", "", ""},
		{"hash only", "#\n# Real\n", "Real"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractFirstHeading(c.body); got != c.want {
				t.Fatalf("extractFirstHeading(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestIngestMarkdownTree_StructureAndCounts(t *testing.T) {
	src := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("top.md", "# Top Heading\nhello")
	mustWrite("sub/nested.md", "no heading here") // title falls back to filename
	mustWrite("sub/notes.txt", "not markdown")    // skipped (counted)
	mustWrite(".git/config", "[core]")            // .git dir skipped entirely
	mustWrite("deep/a/b/c.md", "# Deep\nx")

	pc, _, _ := testPeerContext(t)
	res, err := IngestMarkdownTree(pc.Store(), src, "docs")
	if err != nil {
		t.Fatalf("IngestMarkdownTree: %v", err)
	}

	if res.Created != 3 {
		t.Fatalf("Created = %d, want 3 (top, nested, deep)", res.Created)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (the .txt)", res.Skipped)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected Errors: %v", res.Errors)
	}
	if res.Prefix != "docs" || res.SrcRoot == "" {
		t.Fatalf("result metadata: prefix=%q srcRoot=%q", res.Prefix, res.SrcRoot)
	}
	if res.BytesIn <= 0 {
		t.Fatalf("BytesIn = %d, want > 0", res.BytesIn)
	}

	// Structure preserved: each .md is at docs/{relpath}, with the
	// title taken from the first heading or the filename.
	for path, wantTitle := range map[string]string{
		"docs/top.md":        "Top Heading",
		"docs/sub/nested.md": "nested", // no heading → filename sans ext
		"docs/deep/a/b/c.md": "Deep",
	} {
		r, ok := pc.Resolve(path)
		if !ok {
			t.Fatalf("expected entity at %s", path)
		}
		if r.Entity.Type != MarkdownFileType {
			t.Fatalf("%s type = %q, want %q", path, r.Entity.Type, MarkdownFileType)
		}
		md, err := MarkdownFileDataFromEntity(r.Entity)
		if err != nil {
			t.Fatalf("%s decode: %s", path, err)
		}
		if md.Title != wantTitle {
			t.Fatalf("%s title = %q, want %q", path, md.Title, wantTitle)
		}
	}

	// The .git path must not have been ingested.
	if _, ok := pc.Resolve("docs/.git/config"); ok {
		t.Fatal(".git contents should be skipped, not ingested")
	}
}

func TestIngestMarkdownTree_NonDirIsError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile.md")
	if err := os.WriteFile(f, []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc, _, _ := testPeerContext(t)
	if _, err := IngestMarkdownTree(pc.Store(), f, "docs"); err == nil {
		t.Fatal("ingesting a file path (not a directory) must error")
	}
}
