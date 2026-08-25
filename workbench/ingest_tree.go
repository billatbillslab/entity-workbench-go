package workbench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/content"
	"go.entitychurch.org/entity-core-go/ext/content/chunker"
)

// MarkdownFileType is the entity type used for files ingested via
// IngestMarkdownTree, written by the Phase E mount transform, and
// queried by MarkdownFilesModel. Preserves the source folder
// structure and per-file metadata for browsing and versioning.
const MarkdownFileType = "doc/markdown-file"

// MarkdownFilesPrefix is the canonical prefix where ingested markdown
// files live. Used by MarkdownFilesModel as the subscription prefix.
// Today equal to "docs/" — the ingest target convention. If a future
// panel kind wants markdown-files from a different region of the tree,
// pass an explicit prefix to NewMarkdownFilesModel instead.
const MarkdownFilesPrefix = "docs/"

// extractFirstHeading returns the text of the first level-1 markdown
// heading in body, or "" if none is found. Recognizes "# Title" form
// (not setext-style "Title\n=====").
func extractFirstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		}
		// Allow leading whitespace before # but only one #
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "##") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

// TreeIngestResult reports the outcome of a structured ingest run.
type TreeIngestResult struct {
	Created int      // markdown files successfully written
	Skipped int      // non-markdown files passed over (counted, not errors)
	Errors  []string // per-file error messages (continued on, not fatal)
	BytesIn int64    // total file bytes read
	Prefix  string   // tree prefix used (for caller display)
	SrcRoot string   // source directory walked (cleaned absolute)
}

// IngestMarkdownTree walks srcDir for .md files and writes each one
// at {treePrefix}{relpath} — preserving the source folder structure
// in the entity tree. Each file becomes a `doc/markdown-file` entity
// carrying {path, title, content, size}.
//
// Designed for ingesting a whole repository's worth of docs (the
// entity-systems markdown corpus, ~1k files) for browsing and
// versioning through the entity shell. Pair with a `revision config`
// on the same prefix and you get a versioned snapshot of the
// repository's current state.
//
// Differences from IngestMarkdownDirectory (the flat-slug
// knowledge-base form):
//   - Preserves directory structure rather than flattening to slugs.
//   - Keeps the .md extension on tree paths so navigation maps
//     directly to filesystem paths.
//   - Writes via Store.Put (L0); use AppPeer.Put if dispatch +
//     capability checks are required for the deployment.
func IngestMarkdownTree(store *Store, srcDir, treePrefix string) (TreeIngestResult, error) {
	result := TreeIngestResult{Prefix: treePrefix}

	abs, err := filepath.Abs(srcDir)
	if err != nil {
		return result, fmt.Errorf("abs %s: %w", srcDir, err)
	}
	result.SrcRoot = abs

	info, err := os.Stat(abs)
	if err != nil {
		return result, fmt.Errorf("stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("%s is not a directory", abs)
	}

	if treePrefix != "" && !strings.HasSuffix(treePrefix, "/") {
		treePrefix += "/"
	}

	walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", path, walkErr))
			return nil
		}
		if d.IsDir() {
			// Skip common noise dirs early — saves a lot of walk time
			// on full-repo ingests with .git / node_modules / etc.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			result.Skipped++
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", path, err))
			result.Skipped++
			return nil
		}

		rel, err := filepath.Rel(abs, path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: rel: %s", path, err))
			result.Skipped++
			return nil
		}
		// Path segments: forward-slash on the wire regardless of host OS.
		treePath := treePrefix + filepath.ToSlash(rel)

		title := extractFirstHeading(string(body))
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}

		// Chunk + persist via CONTENT v3.6 substrate. The typed entity
		// carries only the blob hash; chunks live in the content store.
		// Cross-handler dedup is structural — identical bytes through any
		// path produce identical chunks and blob hash.
		ranges := chunker.ChunkFastCDC(body, types.DefaultChunkSize)
		blobHash, err := content.IngestBlob(body, ranges, types.ChunkingFastCDC, types.DefaultChunkSize, store.ContentStore())
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: ingest blob: %s", path, err))
			result.Skipped++
			return nil
		}

		md := MarkdownFileData{
			Path:    rel,
			Title:   title,
			Content: blobHash,
			Size:    int64(len(body)),
		}
		if _, err := store.Put(treePath, MarkdownFileType, md); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: store.put %s: %s", path, treePath, err))
			result.Skipped++
			return nil
		}
		result.Created++
		result.BytesIn += int64(len(body))
		return nil
	})
	if walkErr != nil {
		return result, walkErr
	}

	return result, nil
}
