package entitysdk

import (
	"log"

	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/peer"
)

// PeerConfig is the SDK-owned configuration for CreatePeer. It mirrors
// SDK-OPERATIONS §8.1 while staying idiomatic to Go.
//
// All fields are optional. The zero-value config creates a local,
// in-memory peer with a fresh keypair, no custom handlers, no
// listener, and open-access grants — suitable for development and
// tests.
type PeerConfig struct {
	// Keypair is the peer's identity. If nil, a fresh keypair is
	// generated at build time.
	Keypair *crypto.Keypair

	// Storage configures the content store and location index.
	// Zero value selects in-memory storage. Persistent storage is
	// spec'd (SDK-OPERATIONS §2.6) but not yet implemented in this SDK.
	Storage StorageConfig

	// Handlers are custom application handlers registered alongside
	// the system extensions. System extension handlers (subscription,
	// continuation, inbox, revision, history, query, clock) are
	// wired automatically by the builder and do not appear here.
	Handlers []HandlerRegistration

	// Extensions toggles optional system extensions. The zero value is
	// "only tree + query wired" — the baseline any SDK peer has. Enable
	// additional extensions (e.g. Subscription) by setting the matching
	// field on ExtensionsConfig. Ordering between extensions is the
	// builder's responsibility; callers should not depend on
	// registration order. See SDK-ALIGNMENT §7.2.
	Extensions ExtensionsConfig

	// ListenAddr binds an inbound TCP/WebSocket listener. Empty =
	// no listener (local/embedded peer). Not yet plumbed through
	// this SDK.
	ListenAddr string

	// Grants are the connection-time capability grants issued to
	// peers that connect to this one (SDK-OPERATIONS §11.2). Empty
	// = Level 0 open access, the single-developer default from
	// §11.2A. Not yet enforced; carried through for forward
	// compatibility with config files.
	Grants []Grant

	// RawOptions is an escape hatch for entity-core-go peer.Options
	// not yet surfaced through PeerConfig. They are applied after
	// the SDK's own options, so they can override SDK defaults.
	//
	// Deprecated: prefer the structured fields above. Raw options
	// will stop being supported once the structured surface is
	// complete.
	RawOptions []peer.Option

	// DebugLog, when non-nil, is threaded through to:
	//   - core-go's peer.WithDebugLog (protocol-level logs)
	//   - the subscription engine's debugf path
	//   - the localfiles handler's watcher logs
	//   - any other extension that accepts a *log.Logger at construction
	//
	// Use a test logger (e.g. `log.New(testWriter{t}, "", 0)`) to
	// capture the trace from a failing test. Production peers
	// SHOULD leave this nil unless diagnosing a specific issue —
	// the logs are verbose. A future iteration may expose a leveled
	// logger, but today's bar is "any logger or none."
	DebugLog *log.Logger

	// Identity, when set, binds the peer to a named on-disk identity
	// at CreatePeer time. The named identity may be either a V7-only
	// flat keypair or an identity-aware directory bundle; CreatePeer
	// auto-detects the shape and dispatches accordingly:
	//
	//   - Flat file at ~/.entity/identities/{Name}     → V7-only mode:
	//                                                     keypair loaded into PeerConfig.Keypair.
	//   - Directory bundle at ~/.entity/identities/{Name}/ → identity-aware mode:
	//                                                     bundle loaded; ceremony re-run
	//                                                     against bundle keypairs to mint
	//                                                     the in-memory quorum + cert + cap.
	//
	// An explicit PeerConfig.Keypair takes precedence over Identity.Name
	// for the keypair selection (callers can compose a custom keypair
	// with a named identity if they know what they're doing).
	Identity *IdentityBindingConfig
}

// IdentityBindingConfig binds a runtime peer to a named on-disk
// identity at CreatePeer time. See PeerConfig.Identity for the
// dispatch semantics.
type IdentityBindingConfig struct {
	// Name is the identity name under ~/.entity/identities/. May
	// reference either a V7 flat keypair or an identity-aware
	// directory bundle.
	Name string
}

// StorageConfig selects a content store backend.
//
// Kind "" or "memory" → in-memory store (tests, ephemeral peers).
// All other values are reserved for future implementations (sqlite,
// file, …) and currently produce a "not_supported" error.
type StorageConfig struct {
	Kind string
	Path string
}

// HandlerRegistration binds a handler pattern to a handler
// implementation. Matches the handler registration contract in
// SDK-OPERATIONS §11.3.
type HandlerRegistration struct {
	Pattern string
	Handler handler.Handler
}

// Grant is a connection-time capability grant. See SDK-OPERATIONS
// §11.2.
type Grant struct {
	Scope GrantScope
}

// GrantScope is the four-dimensional capability scope per
// SDK-OPERATIONS §11.2 / V7 §3.6. A dispatch must match all four
// dimensions from a single grant entry.
type GrantScope struct {
	Handlers   ScopeDimension // handler patterns
	Operations ScopeDimension // operation names
	Resources  ScopeDimension // path patterns
	Peers      ScopeDimension // peer-id patterns
}

// ScopeDimension is an include/exclude pair for one scope dimension.
// Include lists what's allowed; Exclude subtracts from it. Use
// Include: []string{"*"} for "allow everything".
type ScopeDimension struct {
	Include []string
	Exclude []string
}

// WildcardGrantScope returns the open-access grant scope — "*" on
// all four dimensions. This is the Level 0 grant progression from
// SDK-OPERATIONS §11.2A, the default for single-developer use.
func WildcardGrantScope() GrantScope {
	star := []string{"*"}
	return GrantScope{
		Handlers:   ScopeDimension{Include: star},
		Operations: ScopeDimension{Include: star},
		Resources:  ScopeDimension{Include: star},
		Peers:      ScopeDimension{Include: star},
	}
}

// WildcardGrant returns a single open-access Grant. This is what the
// SDK uses by default when PeerConfig.Grants is empty.
func WildcardGrant() Grant {
	return Grant{Scope: WildcardGrantScope()}
}

// ExtensionsConfig selects which optional system extensions get wired
// into the peer at construction. The convention for stable extensions
// is **nil = default-on**; pass &XConfig{Disabled: true} to opt out.
// New unstable extensions land as nil = off until they stabilize.
//
// Today the SDK owns ordering of any extensions it enables. Once
// entity-core-go exposes a dependency DAG (SDK-ALIGNMENT §7.2), this
// layer will delegate ordering to the core peer builder.
type ExtensionsConfig struct {
	// Subscription wires the system/subscription engine + system/inbox
	// stock handler. Default: enabled (the engine starts a notify
	// goroutine; pass &SubscriptionConfig{Disabled: true} for the
	// rare peers that genuinely don't want the runtime cost).
	Subscription *SubscriptionConfig

	// Role wires the system/role handler with its exclusion-sweep
	// sync hook and post-construction Setup{Store,Authority} calls.
	// Default: enabled. Pass &RoleConfig{Disabled: true} to opt out.
	//
	// Role works in V7-only mode: caller cap is the local peer's
	// standing grant. When the identity extension is enabled, the
	// typical caller cap becomes the local peer→controller cap per
	// SDK-IDENTITY-INFRASTRUCTURE §7.
	Role *RoleConfig

	// IdentityStack toggles the three identity-stack extensions —
	// attestation (signed-graph substrate), quorum (K-of-N node
	// primitive), and identity (convention layer). Per
	// SDK-IDENTITY-INFRASTRUCTURE the three compose: identity depends
	// on quorum + attestation. They wire as a unit; granular toggles
	// are not exposed because partial wiring produces a peer that
	// can't validate cert chains.
	//
	// Default: enabled. Pass &IdentityStackConfig{Disabled: true} to
	// opt out — useful for deeply minimal peers that don't carry an
	// identity surface.
	//
	// Until identity.Startup runs (Cut 2c bootstrap helper), identity
	// ops requiring peer-config return 503 authority_not_ready per
	// EXTENSION-IDENTITY §6.5. Substrate ops (attestation, quorum)
	// remain dispatchable from peer construction.
	IdentityStack *IdentityStackConfig

	// Clock toggles the system/clock extension (vector clock state).
	// Default: enabled. Pass &ClockConfig{Disabled: true} to opt out.
	Clock *ClockConfig

	// Continuation toggles the system/continuation extension
	// (suspended state machines for async chains). Default: enabled.
	Continuation *ContinuationConfig

	// Content toggles the system/content extension (content-by-hash
	// retrieval). Default: enabled.
	Content *ContentConfig

	// Handlers toggles the system/handler extension (the dynamic
	// handler register/unregister surface). Default: enabled — the
	// dynamic AppPeer.RegisterHandler primitive needs it.
	Handlers *HandlersConfig

	// History toggles the system/history extension (per-path
	// transition log). Default: enabled.
	History *HistoryConfig

	// Revision toggles the system/revision handler plus the
	// trie-root tracker (EXTENSION-TREE §3.4.1a) and the
	// auto-versioner (EXTENSION-REVISION §6.1). The three wire
	// together: auto-version reads the settled tracked root that
	// root-tracker maintains, and the revision handler relies on
	// both for snapshot/diff/merge ops. Default: enabled.
	//
	// Recording remains opt-in per path via system/revision/config/{name}
	// — enabling the extension only wires the machinery; nothing is
	// versioned until a config matches.
	Revision *RevisionConfig

	// Compute toggles the system/compute handler + reactive engine
	// (EXTENSION-COMPUTE). The engine evaluates installed compute
	// expressions when their declared dependencies change; the
	// handler exposes eval / install / uninstall ops and the
	// peer's dispatcher.EvaluateExpression hook so V7 entity-native
	// handlers (handlers.expression_path) resolve through the engine
	// at dispatch time. Default: enabled.
	Compute *ComputeConfig

	// LocalFiles toggles the local/files domain handler
	// (DOMAIN-LOCAL-FILES). The handler registers ops for read/write/
	// list/delete/watch over a configured filesystem root and bridges
	// fs events into the entity tree at local/files/{root}/...
	// Default: enabled. The handler is dispatchable when this is on,
	// but no filesystem watching happens until a mount is installed
	// via the shell verb `local-files mount` (Phase E).
	LocalFiles *LocalFilesConfig

	// Network toggles the system/network handler — maintain-peer /
	// release-peer / status / close and the EXTENSION-NETWORK §4.1
	// reconnect continuation graph. Default: enabled.
	//
	// Enabling it is not the same as running it: core/peer writes the
	// liveness transitions either way, and the handler's continuation
	// graph is installed per-peer by a maintain-peer call. Read the
	// resulting lifecycle state with AppPeer.PeerLivenessOf /
	// PeerLivenessAll / OnPeerLivenessChange.
	Network *NetworkConfig

	// SignalingNode wires a `system/signaling` rendezvous NODE — the
	// mailbox other peers offer into and collect from
	// (EXTENSION-SIGNALING §4/§5).
	//
	// **Default OFF, unlike the rest of this struct**, and the asymmetry
	// is the point: every other extension here is a capability the peer
	// has, while this one is a SERVICE the peer runs for other people.
	// It holds their blobs, it is what §3.4's same-provider MUST points
	// at, and a peer that starts one by accident is a rendezvous point
	// nobody chose and nobody watches. Consuming a node needs nothing
	// from this field — AppPeer.Signaling(nodePeerID) dispatches to
	// whichever node the pool selected.
	SignalingNode *SignalingNodeConfig

	// Discovery wires the EXTENSION-DISCOVERY substrate
	// (`system/discovery`) on a peer that is NOT listening.
	//
	// A listening peer gets the substrate automatically — the mDNS
	// backend needs a bound port and there is nothing to announce
	// without one. This field is the door for backends that need no
	// listener, which is the `rendezvous` case: a peer standing at a
	// signaling mailbox to be introduced typically has no reachable
	// address, and that is the situation the backend exists for.
	//
	// Default: off unless ListenAddr is set. Presence of a non-Disabled
	// struct forces it on. Registering the substrate registers no
	// backend — see AppPeer.EnableRendezvousDiscovery.
	Discovery *DiscoveryConfig

	// Registry toggles the EXTENSION-REGISTRY name-resolution substrate
	// — the meta-resolver (system/registry:resolve) plus the local-name
	// backend (system/registry/local-name) that backs the `name →
	// peer_id` rung of the resolution chain (GUIDE-RESOLUTION §4). The
	// `resolve()` seam composing it lives in resolve_chain.go.
	//
	// Default: DISABLED (opt-in). Pass &RegistryConfig{} to wire it. Off
	// by default so the common peer's bootstrap footprint is unchanged
	// (see the wiring note in app.go — the local-name default-grant caps
	// are not yet deduplicated across reloads).
	Registry *RegistryConfig
}

// NetworkConfig toggles the system/network extension. Zero value =
// enabled. Bind runs post-construction so the handler can dial and
// evict through the live peer.
type NetworkConfig struct{ Disabled bool }

// SignalingNodeConfig configures a hosted `system/signaling` node.
// Presence of the struct is the opt-in; the zero value runs a node on
// the §4.5 defaults (8192-byte blobs, 32 blobs per bucket, 60s TTL).
type SignalingNodeConfig struct {
	// Endpoint is the reachable address this node advertises to
	// clients (§4.5). Empty advertises no endpoint, which is honest
	// for an in-process node and useless for a hosted one.
	Endpoint string
	// LobbyConstant overrides §2.2's named lobby default. **Both arms
	// must use the same one**, so an override is only safe when
	// clients read it out of `advertise` rather than assuming — which
	// is why the SDK's Advertise doc says to.
	LobbyConstant []byte
}

// RegistryConfig wires the system/registry resolution substrate. A
// non-nil value with Disabled=false turns it on; nil leaves it off.
type RegistryConfig struct{ Disabled bool }

// DiscoveryConfig forces the system/discovery substrate on for a
// non-listening peer. A non-nil value with Disabled=false turns it on;
// nil leaves the listener-conditional default in place.
type DiscoveryConfig struct{ Disabled bool }

// RoleConfig toggles the system/role extension. Zero value = enabled
// with defaults. Currently no knobs; the struct exists so future
// options (initial-grant-policy seed, debug logger) can be added
// without breaking the ExtensionsConfig signature.
type RoleConfig struct {
	// Disabled, when true, prevents wiring the role extension. The
	// system/role handler will not be registered; role ops will return
	// 404 not_found via longest-prefix-miss.
	Disabled bool
}

// IdentityStackConfig toggles the attestation + quorum + identity
// extensions as a unit. Zero value = enabled. The three compose per
// SDK-IDENTITY-INFRASTRUCTURE §3 — identity depends on substrate, so
// they wire together. Future options (initial trusted quorum seeding,
// alternate signer-resolver registration, debug logger) can be added
// here without breaking ExtensionsConfig.
type IdentityStackConfig struct {
	// Disabled, when true, prevents wiring all three identity-stack
	// handlers. Identity / attestation / quorum ops return 404
	// not_found via longest-prefix-miss.
	Disabled bool
}

// ClockConfig toggles the system/clock extension. Zero value =
// enabled. SetupAdvancement runs post-construction so the clock
// handler can sign clock-state advances with the peer's keypair.
type ClockConfig struct{ Disabled bool }

// ContinuationConfig toggles the system/continuation extension. Zero
// value = enabled. No post-construction setup required.
type ContinuationConfig struct{ Disabled bool }

// ContentConfig toggles the system/content extension (retrieval of
// entities by content hash for content-addressed flows). Zero
// value = enabled. No post-construction setup required.
type ContentConfig struct{ Disabled bool }

// HandlersConfig toggles the system/handler extension — the dynamic
// register/unregister surface backing AppPeer.RegisterHandler.
// Disabling this breaks RegisterHandler; only opt out for peers
// that explicitly do not expose a handler-management surface
// (rare). Zero value = enabled.
type HandlersConfig struct{ Disabled bool }

// HistoryConfig toggles the system/history extension (per-path
// transition log + query). Zero value = enabled. The history
// recorder needs the peer-id and location-index after peer.New;
// the SDK threads both automatically.
type HistoryConfig struct{ Disabled bool }

// RevisionConfig toggles system/revision plus the trie-root tracker
// (core/tree) and auto-versioner (ext/revision). Zero value =
// enabled. The three are wired as a unit because the auto-versioner
// reads the settled root the tracker maintains; partial wiring
// produces auto-versioning that doesn't see the latest snapshot hash.
//
// Per-path versioning remains opt-in via RevisionConfigData entries
// at system/revision/config/{name}. Without a matching config the
// auto-versioner is a no-op, mirroring history's posture.
type RevisionConfig struct{ Disabled bool }

// ComputeConfig toggles the system/compute extension. Zero value =
// enabled. The reactive engine subscribes to tree changes; if
// nothing is installed at system/compute/processes/* it does no
// work. Wiring the engine also installs the dispatcher's
// EvaluateExpression hook so V7 entity-native handlers
// (handler manifests with expression_path) resolve through the
// engine at dispatch time.
type ComputeConfig struct{ Disabled bool }

// LocalFilesConfig toggles the local/files domain handler
// (DOMAIN-LOCAL-FILES). Zero value = enabled. The handler is
// dispatchable when wired, but filesystem watching only starts
// when a root is mounted via the shell's `local-files mount`
// verb (Phase E), which calls StartWatching on the handler ref
// exposed by AppPeer.LocalFilesHandler().
type LocalFilesConfig struct{ Disabled bool }

// SubscriptionConfig toggles the system/subscription extension.
// When enabled, CreatePeer wires the subscription engine, a
// dedicated tree-event sink, the engine's cancel-on-close hook, the
// system/subscription handler, and attaches a delivery function that
// dispatches notifications through the peer's local dispatcher.
//
// Zero value = enabled with defaults. Pass &SubscriptionConfig{Disabled: true}
// to opt out. Knobs like per-peer delivery limits or a debug logger
// can be added here without breaking ExtensionsConfig.
// DefaultDeliveryQueueSize is the SDK's total async delivery buffer
// capacity, and it deliberately differs from core-go's.
//
// **core-go defaults to 65536 slots, eagerly allocated at
// StartDelivery. Measured: 20.3 MB per peer, before the peer does
// anything.** That is a sound default for a production process holding
// ONE long-lived peer; it is the wrong one for a library, because the
// SDK does not know how many peers its embedder will hold. A test
// binary building ~100 of them paid ~2 GB — and none of it came back,
// because `subscription.Engine` has no Stop and nothing releases the
// ring when a peer closes. Both halves routed to core-go.
//
// 4096 is chosen against the sizing rationale core-go states for its
// own default — "sized for 1000+-file mount bursts" — leaving 4× margin
// over that documented worst case at **6% of the memory** (~1.2 MB per
// peer). It is not a guess at a smaller number: it is the documented
// requirement plus headroom.
//
// **Raise it on a peer that mounts large trees.** The queue drops when
// full, it does not block, so an undersized queue loses notifications
// silently under load. Depth buys time, not throughput — it will not
// fix the delivery-saturation cliff documented in STATUS.
const DefaultDeliveryQueueSize = 4096

type SubscriptionConfig struct {
	Disabled bool

	// DeliveryQueueSize is the TOTAL async delivery buffer capacity
	// across all shards. Zero takes DefaultDeliveryQueueSize (4096),
	// NOT core-go's 65536 — see that constant for the measurement and
	// the reasoning.
	//
	// That default is sized for a production peer absorbing a 1000+-file
	// mount burst, and for one long-lived peer it is the right call. It
	// is the wrong call for a process that holds many peers at once: a
	// test binary building ~100 of them pays ~2 GB before any of them
	// does anything, and **none of it comes back**, because
	// `subscription.Engine` has no Stop and nothing releases the ring
	// when a peer closes (routed to core-go).
	//
	// Sizing it down trades burst absorption for memory: a full queue
	// DROPS, it does not block, so an undersized queue loses
	// notifications silently under load. Do not shrink it on a peer
	// that mounts large trees.
	DeliveryQueueSize int

	// DeliveryWorkers is the number of delivery shards. Zero resolves
	// to min(NumCPU, core-go's shard cap) at StartDelivery. Each shard
	// owns one goroutine and one queue, and DeliveryQueueSize is split
	// across them — so this multiplies goroutines, not memory.
	DeliveryWorkers int
}
