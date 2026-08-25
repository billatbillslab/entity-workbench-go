// entity-publish — producer half of the CDN release corridor.
//
// First form. A thin CLI wrapper: parse flags, construct
// an AppPeer with the configured storage, hand off to publish.Publish.
// Deliberately depends only on entitysdk + publish — no workbench,
// shellboot, or panel imports — so this binary is easy to extract
// into its own repo when the corridor wraps.
//
// The signed published-root IS emitted (since 2026-08-18): {out}/manifest
// carries it, and the http-poll transport profile moved beside the site
// at {out}/transport-profile per EXTENSION-NETWORK §6.5.4. The signed
// substitute-entry and the local/io stdout-bridge handler are still
// deferred and tagged in publish/.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/entitysdk"
	"entity-workbench-go/publish"
)

const usage = `Usage:
  entity-publish [flags]

Flags:
  -identity NAME      Named identity under ~/.entity/identities/.
                      Required when -storage=sqlite and -storage-path empty.
  -keypair PATH       Explicit keypair file (PEM seed). Overrides
                      -identity. Use this to publish AS the identity
                      that wrote a foreign store — e.g.
                      -keypair some-repo/.entity/keypair.
  -storage KIND       Storage backend: "memory" or "sqlite" (default sqlite).
  -storage-path PATH  SQLite DB path. Defaults to
                      ~/.entity/peers/{identity}/store.db when -identity set.
  -prefix PATH        Tree prefix to publish (e.g. wt/). Default "" = all.
  -out DIR            Output directory (default: ./publish-out).
  -origin URL         External HTTP origin the published directory
                      will be served from (e.g. https://my-cdn.com).
                      Embedded as tree_url_prefix in the emitted
                      http-poll manifest. Optional; if omitted, the
                      manifest is emitted with empty prefix fields
                      and a warning.
`

func main() {
	identity := flag.String("identity", "", "named identity")
	keypairPath := flag.String("keypair", "", "explicit keypair file")
	storage := flag.String("storage", "sqlite", "storage backend")
	storagePath := flag.String("storage-path", "", "sqlite DB path")
	prefix := flag.String("prefix", "", "tree prefix to publish")
	outDir := flag.String("out", "./publish-out", "output directory")
	origin := flag.String("origin", "", "external HTTP origin URL")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	ap, err := buildPeer(*storage, *storagePath, *identity, *keypairPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "entity-publish: %v\n", err)
		os.Exit(1)
	}
	defer ap.Close()

	res, err := publish.Publish(context.Background(), publish.Opts{
		Peer:      ap,
		Prefix:    *prefix,
		OutputDir: *outDir,
		OriginURL: *origin,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "entity-publish: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("done. %d entities, %d bytes → %s\n", res.Entities, res.Bytes, *outDir)
	printSummary(res)
}

// printSummary writes a human-readable block of everything a cohort
// validator needs to configure itself against this published origin:
// publisher peer-id, origin URL, all §6.5.3 manifest fields, the
// URL patterns the consumer builds, and a curl smoke-test hint.
func printSummary(res publish.Result) {
	m := res.Manifest
	const bar = "==========================================================="
	fmt.Println()
	fmt.Println(bar)
	fmt.Println(" PUBLISHED ORIGIN — http-poll profile (EXTENSION-NETWORK §6.5.3)")
	fmt.Println(bar)
	fmt.Printf(" peer_id             %s\n", res.PeerID)
	if res.OriginURL == "" {
		fmt.Println(" origin URL          (unset — pass -origin URL to populate the manifest)")
	} else {
		fmt.Printf(" origin URL          %s\n", res.OriginURL)
	}
	fmt.Printf(" output dir          %s\n", res.OutputDir)
	fmt.Printf(" paths / entities    %d / %d  (%d bytes)\n", res.Paths, res.Entities, res.Bytes)
	fmt.Println()
	fmt.Println(" URL patterns (what a Mode-A consumer builds):")
	if res.OriginURL != "" {
		fmt.Printf("   manifest          %s/manifest  (the signed published-root)\n", res.OriginURL)
		fmt.Printf("   transport profile %s/%s  (out-of-band, §6.5.4)\n", res.OriginURL, publish.TransportProfileFile)
		fmt.Printf("   tree binding      %s/%s/<path>%s\n", res.OriginURL, res.PeerID, m.Endpoint.TreeLeafSuffix)
		fmt.Printf("   content blob      %s/<wire-hex[0:2]>/<wire-hex[2:4]>/<wire-hex>\n", m.Endpoint.ContentURLPrefix)
		fmt.Println("                       (sharded-2-4; level 1 = algo byte hex,")
		fmt.Println("                        level 2 = first digest byte hex,")
		fmt.Println("                        leaf = full 66-char §3.5 wire form)")
	} else {
		fmt.Println("   (origin unset — URLs cannot be built; -origin is required for serving)")
	}
	sr := res.SignedRoot
	fmt.Println()
	fmt.Println(" signed published-root (served at {manifest_url_prefix}, §6.5.3.1):")
	fmt.Printf("   entity hash        %s\n", sr.Root.ContentHash)
	fmt.Printf("   root_hash          %s\n", sr.Data.RootHash)
	fmt.Printf("   prefix             %s\n", sr.Data.Prefix)
	fmt.Printf("   seq                %d\n", sr.Data.Seq)
	if sr.Data.Predecessor != nil {
		fmt.Printf("   predecessor        %s\n", *sr.Data.Predecessor)
	} else {
		fmt.Println("   predecessor        (none — first published root for this peer)")
	}
	fmt.Printf("   signature at       /%s/%s%s\n", res.PeerID,
		types.LocalSignaturePath(sr.Root.ContentHash), m.Endpoint.TreeLeafSuffix)
	fmt.Printf("   closure served     %d hashes (trie nodes + leaf-bound content, §6.5.3 MUST)\n", sr.ClosureSize)

	fmt.Println()
	fmt.Printf(" §6.5.3 transport-profile fields (decoded from {out}/%s):\n", publish.TransportProfileFile)
	fmt.Printf("   transport_type     %s\n", m.TransportType)
	fmt.Printf("   supported_ops      %v\n", m.SupportedOps)
	fmt.Printf("   freshness          %s\n", m.Freshness)
	fmt.Printf("   nonce_required     %t\n", m.NonceRequired)
	fmt.Printf("   cap_flow           %s\n", m.CapFlow)
	fmt.Printf("   poll_interval_ms   %d\n", m.PollIntervalMs)
	fmt.Printf("   signed_pointer     %s\n", m.SignedPointer)
	fmt.Printf("   tree_url_prefix    %s\n", m.Endpoint.TreeURLPrefix)
	fmt.Printf("   content_url_prefix %s\n", m.Endpoint.ContentURLPrefix)
	fmt.Printf("   manifest_url_prefix %s\n", m.Endpoint.ManifestURLPrefix)
	fmt.Printf("   content_layout     %s\n", m.Endpoint.ContentLayout)
	fmt.Printf("   tree_leaf_suffix   %s\n", m.Endpoint.TreeLeafSuffix)
	fmt.Printf("   tree_listing_suffix %s\n", m.Endpoint.TreeListingSuffix)
	fmt.Printf("   advertised_at      %d (epoch ms)\n", m.AdvertisedAt)
	if res.OriginURL != "" {
		fmt.Println()
		fmt.Println(" smoke test:")
		fmt.Printf("   curl -sI %s/manifest\n", res.OriginURL)
		fmt.Printf("   curl -s  %s/manifest | xxd | head\n", res.OriginURL)
		fmt.Printf("   curl -s  %s/%s | xxd | head\n", res.OriginURL, publish.TransportProfileFile)
	}
	fmt.Println(bar)
}

// buildPeer constructs an AppPeer with the requested storage. Skips
// workbench-extension wiring (notification-ingest, chain-errors,
// localfiles reload) — publish reads the store, that's all.
//
// Identity precedence:
//   - explicit -keypair path wins (load PEM, pass via PeerConfig.Keypair)
//   - -identity falls back to ~/.entity/identities/{name}
//   - neither = fresh ephemeral keypair (only useful for memory storage;
//     for sqlite it means publish runs under a DIFFERENT peer-id than
//     the writer and the NamespacedIndex filter misses everything)
func buildPeer(storageKind, storagePath, identity, keypairPath string) (*entitysdk.AppPeer, error) {
	path := storagePath
	if storageKind == "sqlite" {
		if path == "" {
			if identity == "" {
				return nil, fmt.Errorf("-storage=sqlite requires -storage-path or -identity")
			}
			p, err := entitysdk.DefaultPeerStoragePath(identity)
			if err != nil {
				return nil, fmt.Errorf("resolve storage path: %w", err)
			}
			path = p
		}
		if err := entitysdk.EnsurePeerStorageDir(path); err != nil {
			return nil, fmt.Errorf("prepare storage dir: %w", err)
		}
	}

	cfg := entitysdk.PeerConfig{
		Storage: entitysdk.StorageConfig{Kind: storageKind, Path: path},
	}
	if keypairPath != "" {
		kp, err := crypto.LoadIdentityFromFile(keypairPath)
		if err != nil {
			return nil, fmt.Errorf("load keypair: %w", err)
		}
		cfg.Keypair = &kp
	} else if identity != "" {
		cfg.Identity = &entitysdk.IdentityBindingConfig{Name: identity}
	}
	return entitysdk.CreatePeer(cfg)
}
