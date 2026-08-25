package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"go.entitychurch.org/entity-core-go/core/types"

	"entity-workbench-go/fetch"
)

// registry.go — `entity-fetch`'s naming mode.
//
// The rest of this binary enters through an origin somebody already told
// it. This mode enters through a **name**, which means the operator
// supplies exactly one fact out of band — the registry's peer-id — and
// everything else is fetched and checked.
//
// It stays in the same posture as the verify mode: report which link
// held, what each one proves, and never the bare word "verified".

type registryArgs struct {
	origin   string
	peerID   string
	pinned   bool
	endpoint types.TransportEndpoint

	enumerate    bool
	name         string
	path         string
	targetOrigin string
	json         bool
}

func runRegistry(a registryArgs) error {
	ctx := context.Background()

	var (
		layout fetch.Layout
		err    error
	)
	if a.pinned {
		layout, err = fetch.PinnedLayout(a.origin, a.peerID, a.endpoint)
	} else {
		layout, err = fetch.LoadLayout(ctx, a.origin, nil)
		if err == nil && layout.PeerID != a.peerID {
			// The operator pinned a key and the origin advertised a
			// different one. Refuse rather than pick: the pin is the one
			// thing here that did not come from the origin.
			return fmt.Errorf("origin %s advertises peer %s; you pinned %s — refusing rather than "+
				"choosing between them", a.origin, layout.PeerID, a.peerID)
		}
	}
	if err != nil {
		return err
	}
	reg, err := fetch.NewRegistry(layout, nil)
	if err != nil {
		return err
	}

	if a.enumerate {
		return enumerateRegistry(ctx, reg, a)
	}
	return resolveOneName(ctx, reg, a)
}

func enumerateRegistry(ctx context.Context, reg *fetch.Registry, a registryArgs) error {
	set, err := reg.Enumerate(ctx)
	if err != nil {
		return fmt.Errorf("walking the registry's signed root: %w", err)
	}

	if a.json {
		return emitRegistryJSON(map[string]any{
			"registry":        reg.PeerID(),
			"origin":          reg.Layout.Origin,
			"layout":          pinnedOrDiscovered(!a.pinned),
			"root":            set.Root.Data.RootHash.String(),
			"published_at":    set.Root.Data.PublishedAt,
			"nodes":           set.Walk.Nodes(),
			"names":           set.NameStrings(),
			"listing":         set.Listing,
			"advertised_only": set.AdvertisedOnly,
			"committed_only":  set.CommittedOnly,
			"reconciled":      set.Reconciled(),
		})
	}

	fmt.Printf("registry %s\n", reg.PeerID())
	fmt.Printf("origin   %s\n", reg.Layout.Origin)
	fmt.Printf("layout   %s\n", pinnedOrDiscovered(!a.pinned))
	fmt.Printf("root     %s  (%d CHAMP nodes walked)\n", set.Root.Data.RootHash, set.Walk.Nodes())
	fmt.Println()
	for _, e := range set.Names {
		mark := "ok  "
		note := ""
		if !contains(set.Listing, e.Name) {
			note = "  (committed, omitted from the served menu)"
		}
		fmt.Printf("  %s %-32s %s%s\n", mark, e.Name, e.BindingHash, note)
	}
	for _, n := range set.AdvertisedOnly {
		fmt.Printf("  !!   %-32s advertised in the served listing, NOT committed by the signed root\n", n)
	}
	fmt.Println()
	if set.ListingErr != nil {
		fmt.Println("the origin serves no by-name listing; the walk answered on its own")
	} else if !set.Reconciled() {
		fmt.Println("the served menu DISAGREES with the signed key set — see the marked rows above")
	}
	fmt.Printf("%d name(s), from the WALK of the signed root — verified as of published_at %d.\n",
		len(set.Names), set.Root.Data.PublishedAt)
	fmt.Println("A served listing can hide a name undetectably; a walk cannot hide one without failing.")
	fmt.Println("Resolving any of these is a separate verification: -name NAME.")
	return nil
}

func resolveOneName(ctx context.Context, reg *fetch.Registry, a registryArgs) error {
	// Enumerate first when we can: it makes step 1 read the binding hash
	// out of the SIGNED key set instead of a host-served pointer, which
	// removes the substitution the association check exists to catch
	// rather than merely detecting it. A registry too large to walk falls
	// back to the §6a.4 floor, and the report says which happened.
	var (
		res fetch.NameResolution
		err error
	)
	set, walkErr := reg.Enumerate(ctx)
	if walkErr == nil {
		res, err = reg.ResolveIn(ctx, set, a.name)
	} else {
		res, err = reg.Resolve(ctx, a.name)
	}

	if a.json {
		payload := map[string]any{
			"registry":     reg.PeerID(),
			"name":         a.name,
			"source":       string(res.Source),
			"binding":      res.BindingHash.String(),
			"issued_at":    res.IssuedAt,
			"expires_at":   res.ExpiresAt,
			"revocation":   res.Revocation.Bound(),
			"walk_error":   errString(walkErr),
			"trust_anchor": res.TrustAnchor,
		}
		if err != nil {
			payload["refused"] = err.Error()
			payload["binding_name"] = res.Binding.Name
		} else {
			payload["peer_id"] = res.PeerID()
		}
		return emitRegistryJSON(payload)
	}

	fmt.Printf("name     %s\n", a.name)
	fmt.Printf("registry %s (%s)\n", reg.PeerID(), pinnedOrDiscovered(!a.pinned))
	if walkErr != nil {
		fmt.Printf("lookup   by-name pointer — the §6a.4 floor; the registry's root could not be\n")
		fmt.Printf("         walked (%v), so WHICH binding answers this name was the host's choice\n", walkErr)
	} else {
		fmt.Printf("lookup   found in the signed key set of root %s\n", set.Root.Data.RootHash)
		fmt.Printf("         (the origin had no say in which binding answers this name)\n")
	}

	if err != nil {
		fmt.Println()
		fmt.Printf("REFUSED: %s\n", err)
		if res.Binding.Name != "" && res.Binding.Name != a.name {
			fmt.Printf("         the binding that answered was issued for %q — a valid,\n", res.Binding.Name)
			fmt.Printf("         correctly-signed, unexpired binding answering the wrong question\n")
		}
		os.Exit(1)
	}

	fmt.Printf("binding  %s\n", res.BindingHash)
	fmt.Printf("signed   by the registry (ed25519), and the body's own `name` matches what was asked\n")
	fmt.Printf("revoked  %s\n", res.Revocation.Bound())
	fmt.Printf("valid    %s → %s\n", res.IssuedAt.Format("2006-01-02T15:04:05Z"),
		res.ExpiresAt.Format("2006-01-02T15:04:05Z"))
	fmt.Printf("target   %s\n", res.PeerID())
	for _, tr := range res.Transports() {
		fmt.Printf("         transport carried %s in the binding\n", tr.Kind)
	}

	if a.path == "" {
		fmt.Println()
		fmt.Println("Resolved as of the registry's published_at — never \"fresh\": a quiet registry")
		fmt.Println("and a withholding origin are indistinguishable from here, and the bound on a")
		fmt.Println("withheld revocation is this binding's ttl. Add -path to follow it through.")
		return nil
	}
	return followToTarget(ctx, reg, res, a)
}

// followToTarget is hop 2: the binding's transport becomes a layout, and
// the TARGET's own signed root is verified — a different key, over which
// the registry's signature has no standing.
func followToTarget(ctx context.Context, reg *fetch.Registry, res fetch.NameResolution, a registryArgs) error {
	origin, err := reg.OriginFor(ctx, res, a.targetOrigin)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("origin   %s (http-poll, from the binding)\n", origin.Layout.Origin)

	c := fetch.NewConsumer(origin.Layout, nil)
	root, err := c.VerifiedRoot(ctx)
	if err != nil {
		return fmt.Errorf("the target's signed root: %w", err)
	}
	fmt.Printf("root     %s seq=%d prefix=%q\n", root.Data.RootHash, root.Data.Seq, root.Data.Prefix)

	walk, err := c.Walk(ctx, root.Data.RootHash)
	if err != nil {
		return fmt.Errorf("walking the target's trie: %w", err)
	}
	fmt.Printf("walk     %d CHAMP nodes, %d committed keys\n", walk.Nodes(), len(walk.Bindings))

	key := strings.TrimPrefix(a.path, fetch.AbsolutePrefix(root.Data.Prefix, origin.Layout.PeerID))
	h, err := walk.Lookup(key)
	if err != nil {
		return fmt.Errorf("%q is not in the %d keys the target's signed root commits to: %w",
			key, len(walk.Bindings), err)
	}
	ent, err := c.Blob(ctx, h)
	if err != nil {
		return err
	}
	fmt.Printf("path     %s\n", key)
	fmt.Printf("type     %s (%d bytes)\n", ent.Type, len(ent.Data))
	fmt.Println()
	fmt.Printf("These bytes hash to what %s's signed root committed for this path,\n", origin.Layout.PeerID)
	fmt.Printf("verified as of published_at %d. That is the whole of what a green chain claims.\n",
		root.Data.PublishedAt)
	return nil
}

// emitRegistryJSON is separate from main.go's `emitJSON`, which is typed
// to the verify report.
func emitRegistryJSON(v map[string]any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func pinnedOrDiscovered(discovered bool) string {
	if discovered {
		return "discovered from the origin's transport-profile"
	}
	return "PINNED by you — a wrong pin and a withholding origin look the same from here"
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
