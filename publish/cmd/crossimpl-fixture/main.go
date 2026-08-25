// crossimpl-fixture emits a deterministic Go-published site for the
// cross-implementation consumer check (ADR-0012: a signed-root result
// verified only by the same language's reader is cohort-consistent, not
// independent convergence).
//
// It is NOT a shipped binary. It exists so the Rust arm — or any other
// consumer — can be pointed at a real Go emission instead of at a
// fixture the consumer's own publisher built. Every claim in
// reviews/CROSSIMPL-PUBLISH-CONSUME-2026-08-18.md is reproduced by
// running this and reading the tree it writes.
//
// Determinism: the keypair is seeded from a fixed 32-byte constant and
// the publish instant is pinned (FixtureInstant), so EVERY byte the
// fixture emits is stable across runs and across machines. Verified by
// re-running into a second directory and diffing.
//
// The instant has to be pinned, and the earlier claim that only
// `advertised_at` drifted was wrong. `published_at` is a field of the
// published-root entity, so a fresh clock moves the root's content
// hash, which moves the signature entity's name
// (`system/signature/{root_hex}.bin`), which moves two content shards
// and the {out}/manifest bytes. Only the trie root and the entities
// beneath it were ever stable — which is to say, not the artifacts a
// consumer's reader enters through. Measured across two runs on
// 2026-08-18, after the re-cut arch asked for.
//
//	go run ./cmd/crossimpl-fixture -out /tmp/go-published-site
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.entitychurch.org/entity-core-go/core/crypto"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/publish"
)

// FixtureSeed is the pinned Ed25519 seed. Changing it changes the
// peer-id and every hash in the fixture, which invalidates every
// citation in the cross-impl packet — treat it as a wire constant.
var FixtureSeed = [32]byte{
	0x77, 0x6f, 0x72, 0x6b, 0x62, 0x65, 0x6e, 0x63,
	0x68, 0x2d, 0x67, 0x6f, 0x2d, 0x63, 0x72, 0x6f,
	0x73, 0x73, 0x69, 0x6d, 0x70, 0x6c, 0x2d, 0x66,
	0x69, 0x78, 0x74, 0x75, 0x72, 0x65, 0x00, 0x01,
}

// FixtureInstant is the pinned publish instant, stamped into
// `published_at` on the signed root and `advertised_at` on the
// transport profile. Like FixtureSeed, it is a wire constant: changing
// it changes the published-root content hash and therefore the
// signature path and two content shards, invalidating every citation
// in the cross-impl packet.
var FixtureInstant = time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)

// The published pages. Deliberately includes a nested path so the trie
// closure has interior nodes rather than a single leaf — a flat site
// can pass a walk that a nested one fails.
var pages = map[string]string{
	"docs/index":            "# Home\n\nauthored bytes from the Go arm\n",
	"docs/intro":            "# Intro\n\nsecond page\n",
	"docs/deep/one":         "# Deep\n\nnested one level\n",
	"docs/deep/two/leaf":    "# Leaf\n\nnested two levels\n",
	"private/not-published": "this sits outside -prefix and MUST NOT appear\n",
}

func main() {
	out := flag.String("out", "./crossimpl-fixture-out", "output directory")
	prefix := flag.String("prefix", "docs/", "tree prefix to publish")
	origin := flag.String("origin", "https://go-arm.example", "origin URL")
	flag.Parse()

	kp := crypto.FromSeed(FixtureSeed)

	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &kp})
	if err != nil {
		fail("CreatePeer: %v", err)
	}
	defer ap.Close()

	// L0 store writes: this fixture is about the EMITTED BYTES, not
	// about dispatch. Store().Put is the documented back door (D2) and
	// is deliberate here — it also keeps the fixture reproducible while
	// the kernel's entry-point resource drop has L1 writes red.
	paths := make([]string, 0, len(pages))
	for p := range pages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if _, err := ap.Store().Put(p, "test/note", map[string]string{"body": pages[p]}); err != nil {
			fail("seed %s: %v", p, err)
		}
	}

	res, err := publish.Publish(context.Background(), publish.Opts{
		Peer:      ap,
		Prefix:    *prefix,
		OutputDir: *out,
		OriginURL: *origin,
		At:        FixtureInstant,
	})
	if err != nil {
		fail("Publish: %v", err)
	}

	fmt.Printf("peer_id:   %s\n", ap.PeerID())
	fmt.Printf("out:       %s\n", res.OutputDir)
	fmt.Printf("prefix:    %s\n", *prefix)
	fmt.Println()
	fmt.Println("emitted tree (files only):")
	_ = filepath.Walk(*out, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(*out, p)
		fmt.Printf("  %s\n", rel)
		return nil
	})
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "crossimpl-fixture: "+f+"\n", a...)
	os.Exit(1)
}
