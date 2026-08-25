package entitysdk

import (
	"context"
	"sync"
	"sync/atomic"

	"fmt"
	"time"

	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/hash"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/tree"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/attestation"
	"go.entitychurch.org/entity-core-go/ext/clock"
	"go.entitychurch.org/entity-core-go/ext/compute"
	"go.entitychurch.org/entity-core-go/ext/content"
	"go.entitychurch.org/entity-core-go/ext/continuation"
	"go.entitychurch.org/entity-core-go/ext/discovery"
	discoverymdns "go.entitychurch.org/entity-core-go/ext/discovery/mdns"
	"go.entitychurch.org/entity-core-go/ext/handlers"
	"go.entitychurch.org/entity-core-go/ext/history"
	"go.entitychurch.org/entity-core-go/ext/identity"
	"go.entitychurch.org/entity-core-go/ext/inbox"
	"go.entitychurch.org/entity-core-go/ext/localfiles"
	extnetwork "go.entitychurch.org/entity-core-go/ext/network"
	"go.entitychurch.org/entity-core-go/ext/query"
	"go.entitychurch.org/entity-core-go/ext/quorum"
	"go.entitychurch.org/entity-core-go/ext/registry"
	"go.entitychurch.org/entity-core-go/ext/registry/localname"
	"go.entitychurch.org/entity-core-go/ext/revision"
	"go.entitychurch.org/entity-core-go/ext/role"
	"go.entitychurch.org/entity-core-go/ext/signaling"
	signalingnode "go.entitychurch.org/entity-core-go/ext/signaling/node"
	"go.entitychurch.org/entity-core-go/ext/subscription"
)

// AppPeer is the primary entry point for entity-native applications in Go.
//
// It wraps an entity-core-go Peer with a pre-wired query extension,
// executor, peer context, and event log. All operations go through
// the entity protocol — the AppPeer never exposes internal storage or
// indexes directly.
//
// Renderers and applications create an AppPeer once and use its accessors
// to build their UI and business logic on top.
type AppPeer struct {
	peer     *peer.Peer
	executor *Executor
	store    *Store
	peerCtx  *PeerContext
	eventLog *EventLog

	// Identity-stack handler refs — populated when the IdentityStack
	// extension is enabled. BootstrapIdentity uses these to drive the
	// L0 identity ceremony. Nil when IdentityStack is disabled.
	attHandler      *attestation.Handler
	quorumHandler   *quorum.Handler
	identityHandler *identity.Handler

	// discoveryHandler is set when the discovery substrate was wired
	// (auto-on when ListenAddr is non-empty). Announce / DiscoverPeers
	// / OnDiscoveredPeerChange route through this.
	discoveryHandler *discovery.Handler

	// subEngine is set when the subscription extension is enabled
	// via PeerConfig.Extensions.Subscription. Nil otherwise. The
	// subscription bridge (AppPeer.Subscribe, pass 2 step 3) needs
	// direct access to it.
	subEngine *subscription.Engine

	// cancelSubEngine stops the subscription engine's notify loop
	// on AppPeer.Close. Nil when subscription isn't enabled.
	cancelSubEngine context.CancelFunc

	// generation is the tree-mutation counter (SDK-OPERATIONS §6.4).
	// Incremented via a sync hook on every location-index write. Used
	// by callers that want to cheaply detect "has anything changed
	// since I last looked" without subscribing to a stream. Stored as
	// a pointer so buildPeerOptions can register a hook that closes
	// over it before the AppPeer value exists.
	generation *atomic.Uint64

	// ownerCap is the peer-owner wildcard self-capability minted at
	// peer construction. Exposed via OwnerCapability() so callers
	// building continuations / capabilities can reference it as the
	// dispatch_capability (it's already authority-walkable from the
	// local identity).
	ownerCap entity.Entity

	// localFilesHandler is the registered local/files extension
	// handler when enabled. Exposed via LocalFilesHandler() so the
	// shell's mount verb can StartWatching on it post-construction.
	// Nil when the extension is disabled.
	localFilesHandler *localfiles.Handler

	// nameResolution records whether the EXTENSION-REGISTRY substrate was
	// wired at construction. Exposed via NameResolutionEnabled() rather
	// than the handler pointers: callers ask "can this peer resolve
	// names", never "give me the meta-resolver", and a handler accessor
	// would invite app code to bypass the executor and call the backend
	// directly — the protocol-first rule the SDK exists to hold.
	nameResolution bool

	// prSeqFloor is the per-publisher published-root freshness floor
	// (published_root.go): the highest seq accepted for each peer-id,
	// used to reject rollbacks. Process-lifetime only — see
	// PublishedRootSeqFloor for why it is not persisted here.
	prSeqMu    sync.Mutex
	prSeqFloor map[string]uint64
}

// OwnerCapability returns the peer-owner self-capability entity
// minted at peer construction. Its content hash is suitable as a
// continuation's `dispatch_capability` for any operation the local
// peer would normally be authorized to perform — the chain-root
// check walks granter → local identity and succeeds.
//
// The cap is already persisted to the local content store, so for
// local Install the dispatch_capability is resolvable without
// passing the entity via the Included map.
func (a *AppPeer) OwnerCapability() entity.Entity { return a.ownerCap }

// LocalFilesHandler returns the wired local/files extension handler,
// or nil when the extension is disabled. The shell's `local-files
// mount` verb (Phase E) calls StartWatching on this handler to
// install an fsnotify watcher on a configured root.
func (a *AppPeer) LocalFilesHandler() *localfiles.Handler { return a.localFilesHandler }

// NameResolutionEnabled reports whether the EXTENSION-REGISTRY substrate
// is wired on this peer — the meta-resolver plus the local-name backend.
// When false, ResolveName / BindLocalName / ListLocalNames and friends
// all fail through longest-prefix-miss with a 404 naming a handler path,
// which is an accurate answer and a terrible message.
//
// A UI surfacing name resolution should check this once and say "name
// resolution is off on this peer" rather than letting a user read a
// dispatch 404 as "that name does not exist" — the two are opposite
// diagnoses, and the second sends them looking for a typo in a name that
// was never consulted.
func (a *AppPeer) NameResolutionEnabled() bool { return a.nameResolution }

// RawContentStore returns the core-go ContentStore backing this peer.
// Exposed for extension-aware code (Phase E mount verb, etc.) that
// needs to call into core-go APIs requiring the underlying store
// rather than the SDK's wrapped Store. Most callers should use Store()
// instead.
func (a *AppPeer) RawContentStore() store.ContentStore { return a.peer.Store() }

// RawLocationIndex returns the core-go LocationIndex backing this
// peer. Same role as RawContentStore — for extension-aware code that
// needs to bridge to core-go APIs.
func (a *AppPeer) RawLocationIndex() store.LocationIndex { return a.peer.LocationIndex() }

// RawPeer returns the underlying core-go *peer.Peer. Used by integration
// tests that need to dispatch synthetic chain steps via the dispatcher's
// RemoteExecute with explicit AsyncDelivery.Bounds (e.g., the WB-27
// cross-peer chain-error marker reproducer).
func (a *AppPeer) RawPeer() *peer.Peer { return a.peer }

// Generation returns the local tree-mutation counter (SDK-OPERATIONS
// §6.4). The counter increments on every location-index write, so
// callers can poll it to cheaply detect "has anything changed since
// I last looked." Wraps at 2^64 (never in practice).
func (a *AppPeer) Generation() uint64 {
	return a.generation.Load()
}

// EntityCount returns the number of distinct entities in the peer's
// content store (SDK-OPERATIONS §8.4 entity_count).
func (a *AppPeer) EntityCount() int {
	return a.store.EntityCount()
}

// PathCount returns the number of paths in the peer's location index
// (SDK-OPERATIONS §8.4 path_count). Distinct from EntityCount because
// multiple paths can share the same content hash.
func (a *AppPeer) PathCount() int {
	return a.store.PathCount()
}

// CreatePeer builds a local peer from an SDK PeerConfig, wiring the
// SDK's standard surface (executor, direct store, peer context, event
// log, watch hub) around it. Matches SDK-OPERATIONS §8.1.
//
// The zero-value PeerConfig is valid — it produces a local, in-memory
// peer with a fresh keypair, no custom handlers, no listener, and
// open-access grants.
func CreatePeer(cfg PeerConfig) (*AppPeer, error) {
	opts, err := buildPeerOptions(cfg)
	if err != nil {
		return nil, err
	}
	return assembleAppPeer(opts)
}

// NewAppPeer creates an AppPeer from raw entity-core-go peer.Options.
//
// Deprecated: prefer CreatePeer(PeerConfig), which exposes a stable
// SDK-owned config surface and doesn't leak entity-core-go types. This
// form remains for internal/test use while consumers migrate.
func NewAppPeer(opts ...peer.Option) (*AppPeer, error) {
	return CreatePeer(PeerConfig{RawOptions: opts})
}

type builtOptions struct {
	core      []peer.Option
	watchSink chan store.TreeChangeEvent

	// generation is the tree-mutation counter (§6.4). Allocated here so
	// we can register a sync hook over it during buildPeerOptions and
	// hand the pointer to the finished AppPeer in assembleAppPeer.
	generation *atomic.Uint64

	// subEngine and subEvents are non-nil when the subscription
	// extension is enabled. The engine's Deliver func and Start loop
	// must be wired after peer.New() (they need the peer's dispatcher
	// and final location index), which is why they're carried through
	// builtOptions into assembleAppPeer.
	subEngine *subscription.Engine
	subEvents chan store.TreeChangeEvent
	subCancel context.CancelFunc
	subCtx    context.Context

	// roleHandler is non-nil when the role extension is enabled.
	// SetupStore + SetupAuthority must be called after peer.New() so
	// the role handler can sign role-derived caps with the peer's
	// keypair and write to the location index.
	roleHandler *role.Handler

	// Identity-stack handlers — wired together when
	// ExtensionsConfig.IdentityStack is enabled. attestation +
	// quorum + identity compose per SDK-IDENTITY-INFRASTRUCTURE §3;
	// post-peer.New() each gets SetupStore + identity gets
	// SetupAuthority + SetupSubstrate(att, q) which also registers
	// the "identity-resolved" signer resolver against quorum.
	attHandler      *attestation.Handler
	quorumHandler   *quorum.Handler
	identityHandler *identity.Handler

	// pendingBundle, when non-nil, is an identity bundle loaded via
	// PeerConfig.Identity that needs to have its ceremony re-applied
	// after peer.New. assembleAppPeer consumes this and clears it.
	pendingBundle *IdentityBundle

	// Stable extension handlers — populated when the corresponding
	// ExtensionsConfig fields are enabled (default-on for all).
	// Each gets the appropriate post-peer.New() setup call in
	// assembleAppPeer.
	clockHandler    *clock.Handler
	handlersHandler *handlers.Handler
	historyRecorder *history.Recorder

	// Revision extension surface: rootTracker (tracks settled trie
	// roots for snapshot short-circuits + the auto-versioner), and
	// autoVersioner (writes RevisionTransition entities on tree
	// mutation when a RevisionConfigData matches the path). Both
	// need post-peer.New() peer-id + location-index wiring + Load.
	rootTracker   *tree.RootTracker
	autoVersioner *revision.AutoVersioner

	// Compute extension surface: the reactive engine + the handler
	// driving its eval/install/uninstall ops. Engine needs peer-id +
	// location-index post-construction, then RebuildDependencyIndex
	// to scan installed processes. The handler also wires
	// p.Dispatcher().EvaluateExpression for V7 entity-native dispatch.
	computeEngine  *compute.Engine
	computeHandler *compute.Handler

	// localFilesHandler is set when the LocalFiles extension is
	// enabled. The shell's `local-files mount` verb (Phase E) calls
	// StartWatching on this handler post-construction. Nil when the
	// extension is disabled.
	localFilesHandler *localfiles.Handler

	// discoveryHandler is set when ListenAddr is non-empty — the
	// substrate is opt-in-on-listen because Announce needs a port to
	// advertise. SetupStore + mDNS backend registration run in
	// assembleAppPeer after the live peer's PeerID is known.
	discoveryHandler *discovery.Handler

	// networkHandler is non-nil when the system/network extension is
	// enabled. Bind must run after peer.New() — the handler needs the
	// live peer to dial, evict, and self-execute the §4.1 lifecycle
	// continuations.
	networkHandler *extnetwork.Handler

	// registryHandler + localNameHandler back the `name → peer_id`
	// resolution rung (GUIDE-RESOLUTION §4). The meta-resolver
	// (system/registry:resolve) and the local-name backend
	// (system/registry/local-name) are registered in buildPeerOptions;
	// localNameHandler.SetupStore + registryHandler.RegisterBackend run
	// in assembleAppPeer once the live peer-id + store exist.
	registryHandler  *registry.Handler
	localNameHandler *localname.Handler
}

func buildPeerOptions(cfg PeerConfig) (*builtOptions, error) {
	// Storage: select content store + location index based on cfg.Storage.Kind.
	// Empty/"memory" gets the in-process MemoryContentStore (peer.New supplies
	// a MemoryLocationIndex by default). "sqlite" opens core-go's SqliteStore
	// at cfg.Storage.Path, which provides both a ContentStore and a
	// LocationIndex backed by the same *sql.DB; the DB handle is closed via
	// peer.WithCloseFunc when the AppPeer shuts down. Path ":memory:" selects
	// the SQL in-memory variant — useful for tests that want to exercise the
	// SQL code path without a temp file.
	var cs store.ContentStore
	var li store.LocationIndex
	var sqliteHandle *store.SqliteStore
	switch cfg.Storage.Kind {
	case "", "memory":
		cs = store.NewMemoryContentStore()
	case "sqlite":
		if cfg.Storage.Path == "" {
			return nil, NewError(400, "invalid_storage",
				"storage kind \"sqlite\" requires a non-empty Path")
		}
		// Pre-flight schema-version check. Runs BEFORE NewSqliteStore
		// because the constructor currently rewrites user_version on
		// every open (core-go forward-compat hazard tracked
		// separately). Catches the "future binary wrote this DB"
		// case before any state would get clobbered.
		if err := checkSqliteSchemaVersion(cfg.Storage.Path); err != nil {
			return nil, err
		}
		var s *store.SqliteStore
		var err error
		if cfg.Storage.Path == ":memory:" {
			s, err = store.NewSqliteStoreInMemory()
		} else {
			s, err = store.NewSqliteStore(cfg.Storage.Path)
		}
		if err != nil {
			return nil, WrapError(500, "storage_open_failed",
				"open sqlite at "+cfg.Storage.Path, err)
		}
		sqliteHandle = s
		cs = s.ContentStore()
		li = s.LocationIndex()
	default:
		return nil, NewError(501, "storage_not_supported",
			"storage kind "+cfg.Storage.Kind+" is not yet implemented")
	}

	// Identity binding: if PeerConfig.Identity.Name is set and no
	// explicit Keypair was supplied, dispatch on the on-disk shape:
	// directory → identity-aware bundle; flat file → V7-only keypair.
	var pendingBundle *IdentityBundle
	if cfg.Identity != nil && cfg.Identity.Name != "" && cfg.Keypair == nil {
		isBundle, err := IsIdentityBundleDir(cfg.Identity.Name)
		if err != nil {
			return nil, WrapError(500, "identity_bundle_stat_failed",
				"check identity dir", err)
		}
		if isBundle {
			b, err := LoadIdentityBundle(cfg.Identity.Name)
			if err != nil {
				return nil, err
			}
			kp := b.ControllerKeypair
			cfg.Keypair = &kp
			pendingBundle = &b
		} else {
			id, err := LoadIdentity(cfg.Identity.Name)
			if err != nil {
				return nil, err
			}
			kp := id.Keypair
			cfg.Keypair = &kp
		}
	}

	// Query extension is always wired (standard peer configuration).
	queryMaintainer := query.NewIndexMaintainer(cs)
	queryHandler := query.NewHandler(
		queryMaintainer.TypeIndex(),
		queryMaintainer.ReverseHashIndex(),
		queryMaintainer.PathLinkIndex(),
		cs,
	)

	// Tree-event sink powering the Store.Watch fanout hub. Registered
	// first so the sink is always present even if the caller passes
	// additional WithTreeEventSink options via RawOptions.
	watchSink := make(chan store.TreeChangeEvent, 256)

	// Generation counter hook (§6.4) — a sync hook that just bumps
	// an atomic on every tree mutation.
	gen := new(atomic.Uint64)
	genHook := func(store.TreeChangeEvent) *store.ConsumerResult {
		gen.Add(1)
		return nil
	}

	opts := []peer.Option{
		peer.WithStore(cs),
		peer.WithNamedSyncHook("query", queryMaintainer.OnTreeChange),
		peer.WithNamedSyncHook("generation", genHook),
		peer.WithHandler("system/query", queryHandler),
		peer.WithTreeEventSink(watchSink),
	}
	if li != nil {
		opts = append(opts, peer.WithLocationIndex(li))
	}
	if sqliteHandle != nil {
		s := sqliteHandle
		opts = append(opts, peer.WithCloseFunc(func() { _ = s.Close() }))
	}

	built := &builtOptions{watchSink: watchSink, generation: gen}

	// Subscription extension: wire engine + dedicated event sink +
	// handler + cancel-on-close. Per SDK-ALIGNMENT §7.2, ordering
	// between extensions is the builder's problem; for now,
	// subscription's event sink fires after the query sync hook,
	// which is the correct order.
	if cfg.Extensions.Subscription == nil || !cfg.Extensions.Subscription.Disabled {
		engine := subscription.NewEngine(cs, nil, cfg.DebugLog)
		subEvents := make(chan store.TreeChangeEvent, 256)
		engineCtx, cancelEngine := context.WithCancel(context.Background())

		// Stock system/inbox handler is a hard dependency of
		// subscription: the engine's delivery function dispatches
		// notification EXECUTEs to inbox URIs, and if nothing is
		// registered under system/inbox the dispatch fails. A future
		// SDK subscription bridge (pass 2 step 3) will register
		// per-subscription handlers at system/inbox/{id} which take
		// precedence via longest-prefix match; the stock handler here
		// covers anything else (plain mailbox behavior).
		opts = append(opts,
			peer.WithHandler("system/inbox", inbox.NewHandler()),
			peer.WithHandler("system/subscription", subscription.NewHandler(engine)),
			peer.WithNamedSyncHook("subscription/notification", engine.OnTreeChange),
			peer.WithCloseFunc(cancelEngine),
		)

		built.subEngine = engine
		built.subEvents = subEvents
		built.subCancel = cancelEngine
		built.subCtx = engineCtx
	}

	// Role extension: nil or non-Disabled config wires the handler.
	// Default-on per the convention in ExtensionsConfig doc; opt-out
	// via &RoleConfig{Disabled: true}.
	if cfg.Extensions.Role == nil || !cfg.Extensions.Role.Disabled {
		roleH := role.NewHandler()
		opts = append(opts,
			peer.WithHandler("system/role", roleH),
			peer.WithNamedSyncHook("role/exclusion-sweep", roleH.OnTreeChange),
		)
		built.roleHandler = roleH
	}

	// Stable extensions wired default-on per the
	// nil-as-default-on convention. Each gets the minimum wiring
	// here; post-construction setup happens in assembleAppPeer.

	if cfg.Extensions.Clock == nil || !cfg.Extensions.Clock.Disabled {
		clockH := clock.NewHandler()
		opts = append(opts,
			peer.WithHandler("system/clock", clockH),
			peer.WithNamedSyncHook("clock/advancement", clockH.OnTreeChange),
		)
		built.clockHandler = clockH
	}
	if cfg.Extensions.Continuation == nil || !cfg.Extensions.Continuation.Disabled {
		opts = append(opts,
			peer.WithHandler("system/continuation", continuation.NewHandler()),
		)
	}
	if cfg.Extensions.Content == nil || !cfg.Extensions.Content.Disabled {
		opts = append(opts,
			peer.WithHandler("system/content", content.NewHandler()),
		)
	}
	// EXTENSION-REGISTRY name-resolution substrate (GUIDE-RESOLUTION §4,
	// the `name → peer_id` rung). Opt-in (default OFF) at the SDK tier:
	// pass &RegistryConfig{} to wire it. The one reason left is that most
	// peers never resolve names, and a library should not spend a
	// namespace on a surface its embedder did not ask for.
	//
	// **The second reason is withdrawn — it was measured false on
	// 2026-08-19.** This comment used to claim the local-name handler's
	// default-grant caps were "re-minted (not deduplicated) on every
	// bootstrap", growing a peer's rebootstrap footprint linearly, and
	// that claim is what kept `entity-shell` with no name resolution at
	// all: shellboot never set this field, so the verb had no substrate to
	// stand on. registry_bootstrap_cost_test.go runs the operation across
	// a five-restart SQLite series with the keypair pinned, in both arms.
	// The registry's marginal per-restart cost is ZERO on both counters;
	// its whole cost is +8 paths / +8 entities, once, at install. The
	// growth originally observed is the +2 entities/restart that a
	// registry-LESS peer pays too — core-go's, in the same family as the
	// waived identity-rebootstrap leak, and not this extension's.
	//
	// shellboot enables it by default on the strength of that measurement
	// (shellboot.Config.DisableRegistry opts out). The meta-resolver consults
	// registered backends per the resolver-config; the local-name backend
	// is registered onto it in assembleAppPeer once the live peer-id +
	// store exist. EnableLocalNameResolver writes the chain entry that
	// activates local-name. The `resolve()` seam this substrate composes
	// lives in resolve_chain.go (PROPOSAL-UNIVERSAL-RESOLUTION).
	if cfg.Extensions.Registry != nil && !cfg.Extensions.Registry.Disabled {
		regH := registry.NewHandler()
		lnH := localname.NewHandler()
		opts = append(opts,
			peer.WithHandler(registry.HandlerPattern, regH),
			peer.WithHandler(localname.HandlerPattern, lnH),
		)
		built.registryHandler = regH
		built.localNameHandler = lnH
	}
	// Network extension: the system/network handler — maintain-peer /
	// release-peer / status / close plus the §4.1 reconnect
	// continuation graph (EXTENSION-NETWORK §3-§4).
	//
	// Default-on, and cheap to leave on: registering it starts nothing.
	// core/peer already owns the imperative half of liveness (it writes
	// `connected` at handshake, `suspect` at the dispatch seam,
	// `disconnected` on keepalive miss) whether this handler exists or
	// not. What the handler adds is the REACTIVE half — the
	// continuation graph that watches those writes and dials back — and
	// that graph is installed per-peer by a maintain-peer call, not at
	// construction. A peer nobody calls maintain-peer on carries the
	// handler and does nothing with it.
	//
	// It is also what makes AppPeer honest about being a peer: without
	// it, system/network ops 404 through longest-prefix-miss on a
	// surface every other cohort implementation answers.
	if cfg.Extensions.Network == nil || !cfg.Extensions.Network.Disabled {
		netH := extnetwork.NewHandler()
		if cfg.DebugLog != nil {
			netH.SetDebugLog(cfg.DebugLog)
		}
		opts = append(opts, peer.WithHandler(extnetwork.HandlerPattern, netH))
		built.networkHandler = netH
	}

	// SIGNALING node — the rendezvous mailbox, opt-in (see
	// SignalingNodeConfig for why this one is not default-on). The node
	// is mode-blind by construction: it stores opaque 33-byte keys and
	// never derives one, so it cannot tell a tag meeting from a pair
	// meeting and has no way to act on the difference (§2.2).
	if cfg.Extensions.SignalingNode != nil {
		nodeOpts := []signalingnode.Option{}
		if ep := cfg.Extensions.SignalingNode.Endpoint; ep != "" {
			nodeOpts = append(nodeOpts, signalingnode.WithEndpoint(ep))
		}
		if lc := cfg.Extensions.SignalingNode.LobbyConstant; len(lc) > 0 {
			nodeOpts = append(nodeOpts, signalingnode.WithLobbyConstant(lc))
		}
		opts = append(opts, peer.WithHandler(signaling.HandlerPattern, signalingnode.New(nodeOpts...)))
	}

	// LocalFiles extension: handler registered at "local/files".
	// Watch is opt-in per mount via the shell's `local-files mount`
	// verb (Phase E), which calls StartWatching on the handler ref.
	// The handler's RegisterTypes is invoked by the peer builder.
	if cfg.Extensions.LocalFiles == nil || !cfg.Extensions.LocalFiles.Disabled {
		lfH := localfiles.NewHandler(cfg.DebugLog)
		opts = append(opts,
			peer.WithHandler("local/files", lfH),
			// Wire Handler.Close into AppPeer.Close so watcher inotify
			// instances release on peer shutdown. Closes WB-24.
			peer.WithCloseFunc(func() { _ = lfH.Close() }),
		)
		built.localFilesHandler = lfH
	}
	if cfg.Extensions.Handlers == nil || !cfg.Extensions.Handlers.Disabled {
		hH := handlers.NewHandler()
		opts = append(opts,
			peer.WithHandler("system/handler", hH),
		)
		built.handlersHandler = hH
	}
	if cfg.Extensions.History == nil || !cfg.Extensions.History.Disabled {
		// Recorder is constructed with empty peer-id; SetLocalPeerID
		// runs post-peer.New(). Pattern mirrors entity-peer/main.go.
		recorder := history.NewRecorder(cs, "", nil)
		hH := history.NewHandler(cs, recorder)
		opts = append(opts,
			peer.WithHandler("system/history", hH),
			peer.WithNamedSyncHook("history/recorder", recorder.OnTreeChange),
		)
		built.historyRecorder = recorder
	}

	// Revision extension: trie-root tracker + auto-versioner +
	// system/revision handler. The three are wired as a unit because
	// the auto-versioner reads the settled root the tracker maintains
	// (EXTENSION-REVISION §6.1 hook order: tree/root-tracker fires
	// before revision/auto-version on every mutation). Both need
	// post-peer.New() Setup + Load; see assembleAppPeer.
	if cfg.Extensions.Revision == nil || !cfg.Extensions.Revision.Disabled {
		rootTracker := tree.NewRootTracker(cs, "", nil)
		autoVersioner := revision.NewAutoVersioner(cs, rootTracker, nil)
		revHandler := revision.NewHandler()
		// Wire AV into the revision handler so version-transcription
		// operations (merge / FF / checkout / cherry-pick / revert) can
		// hold AV's per-prefix mutex across their binding-apply phase.
		// Without this wire, the handler's lock-helper is a no-op
		// (nil-safe fallback), and the phantom-marker race fires under
		// concurrent Put + merge. F10 part 7 / PROPOSAL-DELETION-MARKERS
		// Amendment 2 per-prefix-scope invariant.
		revHandler.SetAutoVersioner(autoVersioner)
		opts = append(opts,
			peer.WithRootTracker(rootTracker),
			peer.WithNamedSyncHook("tree/root-tracker", rootTracker.OnTreeChange),
			peer.WithNamedSyncHook("revision/auto-version", autoVersioner.OnTreeChange),
			peer.WithHandler("system/revision", revHandler),
		)
		built.rootTracker = rootTracker
		built.autoVersioner = autoVersioner
	}

	// Compute extension: reactive engine + handler. The engine watches
	// tree mutations and re-evaluates installed processes whose
	// dependencies changed. The handler exposes eval/install/uninstall
	// and the engine's EvaluateAtPath which wires into the dispatcher
	// for V7 entity-native dispatch (handler manifests with
	// expression_path) — that wiring lands in assembleAppPeer.
	if cfg.Extensions.Compute == nil || !cfg.Extensions.Compute.Disabled {
		// The engine needs the location index from peer.New, but it
		// only consults it through SetLocationIndex post-construction.
		// Pass nil here; assembleAppPeer wires it through.
		engine := compute.NewEngine(cs, nil, nil)
		handler := compute.NewHandler(engine)
		opts = append(opts,
			peer.WithNamedSyncHook("compute/reactive", engine.OnTreeChange),
			peer.WithHandler("system/compute", handler),
		)
		built.computeEngine = engine
		built.computeHandler = handler
	}

	// Identity stack: attestation + quorum + identity wired together.
	// Per SDK-IDENTITY-INFRASTRUCTURE §3 the three compose; partial
	// wiring produces a peer that can't validate cert chains. Hook
	// names + ordering mirror entity-peer/main.go:176-179.
	if cfg.Extensions.IdentityStack == nil || !cfg.Extensions.IdentityStack.Disabled {
		attH := attestation.NewHandler()
		quorumH := quorum.NewHandler()
		identityH := identity.NewHandler()
		opts = append(opts,
			peer.WithNamedSyncHook("attestation/index-maintainer", attH.OnTreeChange),
			peer.WithNamedSyncHook("quorum/cache-invalidator", quorumH.OnTreeChange),
			peer.WithNamedSyncHook("identity/process-attestation", identityH.OnTreeChange),
			peer.WithHandler("system/attestation", attH),
			peer.WithHandler("system/quorum", quorumH),
			peer.WithHandler("system/identity", identityH),
		)
		built.attHandler = attH
		built.quorumHandler = quorumH
		built.identityHandler = identityH
	}

	if cfg.Keypair != nil {
		opts = append(opts, peer.WithIdentity(*cfg.Keypair))
	}
	if cfg.ListenAddr != "" {
		opts = append(opts, peer.WithListenAddr(cfg.ListenAddr))
		// EXTENSION-DISCOVERY substrate, opt-in-on-listen. The mDNS
		// backend needs the local peer-id and the bound port, both of
		// which are only available post-peer.New(); the handler is
		// registered here so dispatch can reach it (it returns 503 until
		// SetupStore runs in assembleAppPeer).
		discoveryH := discovery.NewHandler()
		opts = append(opts, peer.WithHandler(discovery.HandlerPattern, discoveryH))
		built.discoveryHandler = discoveryH
	}
	if cfg.DebugLog != nil {
		opts = append(opts, peer.WithDebugLog(cfg.DebugLog))
	}

	for _, reg := range cfg.Handlers {
		if reg.Pattern == "" {
			return nil, NewError(400, "invalid_handler", "handler registration missing pattern")
		}
		if reg.Handler == nil {
			return nil, NewError(400, "invalid_handler",
				"handler registration for "+reg.Pattern+" has nil handler")
		}
		opts = append(opts, peer.WithHandler(reg.Pattern, reg.Handler))
	}

	// Grants are not yet enforced — they'll land with the capability
	// surface work. The field is preserved for forward compatibility
	// with config files.
	_ = cfg.Grants

	// RawOptions last — caller escape hatch, allowed to override.
	opts = append(opts, cfg.RawOptions...)

	built.core = opts
	built.pendingBundle = pendingBundle
	return built, nil
}

func assembleAppPeer(bo *builtOptions) (*AppPeer, error) {
	p, err := peer.New(bo.core...)
	if err != nil {
		return nil, WrapError(500, "peer_build_failed", "peer.New", err)
	}

	eventLog := NewEventLog(500)

	ex := NewExecutor(p.Registry(), p.Store(), p.LocationIndex(), p.PeerID())
	ex.SetAuthorHash(p.Identity().ContentHash)
	// Mint the peer-owner self-capability so RL2-enforcing handlers
	// (system/role:define, :assign, etc.) see a covering caller cap on
	// local L1 dispatch. Open-grants Level 0 mode per SDK-OPERATIONS
	// §11.2A — wildcard on all four scope dimensions.
	ownerCap, err := mintOwnerSelfCap(p)
	if err != nil {
		return nil, WrapError(500, "owner_cap_mint_failed", "mint peer-owner self-cap", err)
	}
	ex.SetCallerCapability(ownerCap)
	ex.SetLog(eventLog)
	// Wire the dispatcher's remote-execute path so EXECUTEs whose handler URI
	// targets a non-local peer-id route through the local peer's pooled
	// connections (dialing on demand from RegisterRemote-seeded addresses).
	ex.setRemoteExecute(p.Dispatcher().RemoteExecute)
	// Wire the dispatcher's local-execute path so EXECUTEs whose handler URI
	// targets the local peer go through the V7 §6.6 tree-walk pipeline. This
	// makes entity-native handlers (system/handler entities with
	// expression_path) reachable in-process — without this, local dispatch
	// would consult only the in-memory registry and tree-stored entity-
	// native handlers would be invisible to SDK callers.
	ex.setLocalExecute(p.Dispatcher().DispatchLocalExecute)

	st := NewStore(p.Store(), p.LocationIndex())
	st.SetLog(eventLog)
	st.peerID = string(p.PeerID())
	st.watchHub = newWatchHub(bo.watchSink)

	peerCtx := NewPeerContext(ex, st)

	ap := &AppPeer{
		peer:              p,
		executor:          ex,
		store:             st,
		peerCtx:           peerCtx,
		eventLog:          eventLog,
		generation:        bo.generation,
		attHandler:        bo.attHandler,
		quorumHandler:     bo.quorumHandler,
		identityHandler:   bo.identityHandler,
		ownerCap:          ownerCap,
		localFilesHandler: bo.localFilesHandler,
		nameResolution:    bo.registryHandler != nil && bo.localNameHandler != nil,
	}

	// Finish role extension wiring — SetupStore + SetupAuthority must
	// run post-peer.New() because they need the peer's location index
	// and keypair-as-identity. Mirrors entity-peer/main.go:257-258.
	if bo.roleHandler != nil {
		bo.roleHandler.SetupStore(p.Store(), p.LocationIndex(), p.PeerID())
		bo.roleHandler.SetupAuthority(p.Keypair(), p.Identity())
	}

	// Clock advancement needs the peer's keypair + identity hash to
	// sign clock-state advances. Mirrors entity-peer/main.go:221-222.
	if bo.clockHandler != nil {
		bo.clockHandler.SetupAdvancement(
			p.Store(), p.LocationIndex(),
			string(p.PeerID()), p.Identity().ContentHash, nil)
	}

	// The network handler's Bind is the imperative seam: it holds the
	// peer it dials, evicts and self-executes through. Ops return 500
	// until it lands. Mirrors entity-peer/main.go:589.
	if bo.networkHandler != nil {
		bo.networkHandler.Bind(p)
	}

	// Handlers extension's authority is the peer's keypair — used
	// to sign each grant emitted by register/unregister so dispatch-
	// time validation per V7 §6.8 can verify granter chain to local
	// peer. Mirrors entity-peer/main.go:237.
	if bo.handlersHandler != nil {
		bo.handlersHandler.SetupAuthority(p.Keypair(), p.Identity())
	}

	// History recorder needs the peer-id, identity hash, and
	// location-index post-construction; Load() rebuilds in-memory
	// state from the existing tree (no-op for fresh peers).
	// Mirrors entity-peer/main.go:316-341.
	if bo.historyRecorder != nil {
		bo.historyRecorder.SetLocalPeerID(string(p.PeerID()), p.Identity().ContentHash)
		bo.historyRecorder.SetLocationIndex(p.LocationIndex())
		bo.historyRecorder.Load()
	}

	// Revision: root tracker first, then auto-versioner — auto-version
	// reads the settled root the tracker computes during Load(), so
	// the order matters. Mirrors entity-peer/main.go:344-352.
	if bo.rootTracker != nil {
		bo.rootTracker.SetLocalPeerID(string(p.PeerID()), p.Identity().ContentHash)
		bo.rootTracker.SetLocationIndex(p.LocationIndex())
		bo.rootTracker.Load()
	}
	if bo.autoVersioner != nil {
		bo.autoVersioner.SetLocalPeerID(string(p.PeerID()), p.Identity().ContentHash)
		bo.autoVersioner.SetLocationIndex(p.LocationIndex())
		bo.autoVersioner.Load()
	}

	// Compute: peer-id + location-index after peer.New, then
	// RebuildDependencyIndex scans system/compute/processes/* and
	// re-registers dependencies for any installed expressions
	// (no-op for fresh peers). Mirrors entity-peer/main.go:225-227.
	// EvaluateExpression is wired into the dispatcher so V7
	// entity-native handler dispatch evaluates compute expressions
	// at handler-resolve time (V7 §3.7, §6.6) — entity-peer/main.go:232.
	if bo.computeEngine != nil {
		bo.computeEngine.SetLocalPeerID(string(p.PeerID()))
		bo.computeEngine.SetLocationIndex(p.LocationIndex())
		bo.computeEngine.RebuildDependencyIndex()
	}
	if bo.computeHandler != nil {
		p.Dispatcher().EvaluateExpression = bo.computeHandler.EvaluateAtPath
	}

	// pendingBundle, if any, gets carried through to AppPeer so the
	// identity-aware re-application happens after the identity-stack
	// handlers are fully wired below.
	pendingBundle := bo.pendingBundle

	// Finish identity-stack wiring per entity-peer/main.go:241-251.
	// Order matters: attestation first (no deps), quorum next
	// (SetupAttestation links it to the attestation index), identity
	// last (SetupSubstrate registers the "identity-resolved" signer
	// resolver against quorum's hook per §6.1).
	if bo.attHandler != nil {
		bo.attHandler.SetupStore(p.Store(), p.LocationIndex(), p.PeerID())
	}
	if bo.quorumHandler != nil {
		bo.quorumHandler.SetupStore(p.Store(), p.LocationIndex(), p.PeerID())
		if bo.attHandler != nil {
			bo.quorumHandler.SetupAttestation(bo.attHandler)
		}
	}
	if bo.identityHandler != nil {
		bo.identityHandler.SetupAuthority(p.Keypair(), p.Identity())
		bo.identityHandler.SetupStore(p.Store(), p.LocationIndex(), p.PeerID())
		if bo.attHandler != nil && bo.quorumHandler != nil {
			if err := bo.identityHandler.SetupSubstrate(bo.attHandler, bo.quorumHandler); err != nil {
				return nil, WrapError(500, "identity_substrate_setup_failed",
					"identity SetupSubstrate", err)
			}
		}
	}

	// Identity-aware load: when the caller passed PeerConfig.Identity
	// referencing a directory bundle, replay the bootstrap ceremony
	// against the bundle's keypairs so the in-memory store mirrors
	// the persisted identity. The deterministic ceremony (RFC 8032
	// signatures + canonical CBOR) re-mints the same content hashes,
	// so the peer comes up indistinguishable from the original
	// bootstrap.
	if pendingBundle != nil {
		if _, err := ap.ApplyIdentityBundle(
			context.Background(),
			*pendingBundle,
			nil, // default wildcard grants — matches how the bundle was bootstrapped
		); err != nil {
			_ = ap.Close()
			return nil, err
		}
	}

	// Finish discovery extension wiring — the substrate needs the
	// live store binder, and the mDNS backend needs both the peer-id
	// and a profile resolver that can read the bound listener port
	// at announce-time. Mirrors entity-peer/main.go:502-531.
	if bo.discoveryHandler != nil {
		bo.discoveryHandler.SetupStore(discovery.NewPeerBinder(p.Store(), p.LocationIndex()))
		ap.discoveryHandler = bo.discoveryHandler

		// Resolver reads the bound listener port lazily. Returning an
		// error if the listener hasn't bound yet keeps Announce honest
		// instead of advertising a phantom port.
		mdnsResolver := func(profileRef string) (int, []string, error) {
			addr := p.Addr()
			if addr == nil {
				return 0, nil, fmt.Errorf("mdns: peer not listening (Addr() returned nil)")
			}
			host, portStr, splitErr := splitHostPort(addr.String())
			_ = host
			if splitErr != nil {
				return 0, nil, fmt.Errorf("mdns: bad listen addr %q: %w", addr.String(), splitErr)
			}
			port, parseErr := atoiPort(portStr)
			if parseErr != nil {
				return 0, nil, fmt.Errorf("mdns: parse port %q: %w", portStr, parseErr)
			}
			switch profileRef {
			case "tcp", "ws":
				return port, []string{profileRef}, nil
			default:
				return 0, nil, fmt.Errorf("mdns: unknown profile_ref %q (v1: tcp, ws)", profileRef)
			}
		}
		mdnsBackend := discoverymdns.New(string(p.PeerID()), mdnsResolver)
		bo.discoveryHandler.RegisterBackend(mdnsBackend)
	}

	// Finish registry substrate wiring. The local-name backend needs the
	// live store + location index + local peer-id to write/read bindings;
	// the meta-resolver needs that backend registered so a resolver-config
	// chain entry naming "local-name" resolves to it (registry.go
	// lookupBackend). nil clock → backend's default wall-clock for
	// issued_at stamping. ResolveName/BindLocalName (resolve.go) dispatch
	// through these handlers.
	if bo.registryHandler != nil && bo.localNameHandler != nil {
		bo.localNameHandler.SetupStore(p.Store(), p.LocationIndex(), p.PeerID(), nil)
		bo.registryHandler.RegisterBackend(bo.localNameHandler)
	}

	// Finish subscription extension wiring — the delivery function
	// and engine location index both need the fully-constructed peer.
	// Pattern mirrors entity-core-go/cmd/entity-peer/main.go:169-176.
	if bo.subEngine != nil {
		bo.subEngine.SetLocationIndex(p.LocationIndex())
		bo.subEngine.Deliver = subscription.MakeDeliveryFunc(
			p.Keypair(), p.Identity(), p.Store(), p.LocationIndex(), p.Dispatcher(),
		)
		bo.subEngine.StartDelivery(bo.subCtx)
		ap.subEngine = bo.subEngine
		ap.cancelSubEngine = bo.subCancel
	}

	return ap, nil
}

// PeerContext returns the cached data access layer.
func (a *AppPeer) PeerContext() *PeerContext { return a.peerCtx }

// Executor returns the protocol-level peer access layer.
func (a *AppPeer) Executor() *Executor { return a.executor }

// Store returns the Level 0 direct-store accessor. Use this when the
// calling code is the peer's own code (configured the peer, holds the
// standing grant) and wants to skip dispatch overhead. For
// capability-enforced operations, use the Level 1 dispatched ops via
// Executor.Execute or the typed wrappers (coming). See SDK-OPERATIONS §2.7.
func (a *AppPeer) Store() *Store { return a.store }

// EventLog returns the application event log.
func (a *AppPeer) EventLog() *EventLog { return a.eventLog }

// TreeEvents returns the raw tree change event stream for the local
// peer — every mutation, unfiltered. This is the SDK-OPERATIONS §6.3
// raw event stream. It is the lowest-level observation primitive and
// shares the L0 bypass properties of Store.Watch (no dispatch, no
// capability check). Prefer Store.Watch for pattern filtering of
// local observation, or the upcoming AppPeer.Subscribe for dispatched
// (capability-checked, cross-peer-capable) notifications.
func (a *AppPeer) TreeEvents() <-chan store.TreeChangeEvent { return a.peer.TreeEvents() }

// Note: pattern-filtered change notifications live on two surfaces
// per SDK-ALIGNMENT §7.1:
//
//   - AppPeer.Store().Watch(pattern) — L0 raw-sink observation
//     (bypasses dispatch; named honestly).
//   - AppPeer.Subscribe(pattern, ...) — L1/L3 dispatched subscription
//     (coming in pass 2 step 3).
//
// There is no top-level AppPeer.Watch; it was deliberately retired
// when we moved the raw-sink form down to Store to match the L0/L1
// tree-op split (spec §2.7).

// PeerID returns the local peer's identity.
func (a *AppPeer) PeerID() string { return string(a.peer.PeerID()) }

// IdentityHash returns the content hash of the local peer's identity
// entity. This is the per-peer-stable hash used as the assignee
// identifier in role assignments, exclusion entities, attestations,
// and any other SDK call that takes "this peer's identity hash"
// rather than the peer-id string.
func (a *AppPeer) IdentityHash() hash.Hash { return a.peer.Identity().ContentHash }

// mintOwnerSelfCap builds a wildcard self-capability for the local
// peer (granter == grantee == local identity hash, wildcard on all
// four scope dimensions). Persists the cap entity and its signature
// in the content store so chain-walking validators can resolve them.
//
// This is the SDK-level analog of the open-grants Level 0 progression
// (SDK-OPERATIONS §11.2A). Handlers enforcing RL2 see this cap on
// every local L1 dispatch and treat the local SDK caller as fully
// authorized — matching the conformance posture documented in §2.7
// "Conformance under open-grants mode."
//
// Once §11 grant enforcement lands kernel-side and CallerCapability
// can be derived from a real identity ceremony (Cut 2+), this helper
// becomes opt-in / overridable rather than the default.
func mintOwnerSelfCap(p *peer.Peer) (entity.Entity, error) {
	id := p.Identity()
	now := uint64(time.Now().UnixMilli())
	tok := types.CapabilityTokenData{
		Grants: []types.GrantEntry{{
			Handlers:   types.CapabilityScope{Include: []string{"*"}},
			Resources:  types.CapabilityScope{Include: []string{"*"}},
			Operations: types.CapabilityScope{Include: []string{"*"}},
		}},
		Granter:   types.SingleSigGranter(id.ContentHash),
		Grantee:   id.ContentHash,
		CreatedAt: now,
	}
	capEnt, err := tok.ToEntity()
	if err != nil {
		return entity.Entity{}, fmt.Errorf("encode owner self-cap: %w", err)
	}
	cs := p.Store()
	if _, err := cs.Put(capEnt); err != nil {
		return entity.Entity{}, fmt.Errorf("persist owner self-cap: %w", err)
	}
	// Sign and persist the signature so chain validators that walk
	// the cap can resolve a sibling signature reference.
	kp := p.Keypair()
	sig := kp.Sign(capEnt.ContentHash.Bytes())
	sigData := types.SignatureData{
		Target:    capEnt.ContentHash,
		Signer:    id.ContentHash,
		Algorithm: "ed25519",
		Signature: sig,
	}
	sigEnt, err := sigData.ToEntity()
	if err != nil {
		return entity.Entity{}, fmt.Errorf("encode owner self-cap signature: %w", err)
	}
	if _, err := cs.Put(sigEnt); err != nil {
		return entity.Entity{}, fmt.Errorf("persist owner self-cap signature: %w", err)
	}
	return capEnt, nil
}

// subscriptionEngine returns the wired subscription engine, or nil
// when the extension isn't enabled. Package-internal — exposed for
// the subscription bridge (pass 2 step 3) and integration tests.
func (a *AppPeer) subscriptionEngine() *subscription.Engine { return a.subEngine }

// SubscriptionEngine returns the wired subscription engine, or nil
// when the extension isn't enabled. Public accessor for inspect
// consumers that need to attach emit/deliver hooks per GUIDE-
// INSPECTABILITY v1.1 §2.1 #5 + #6. The hook surface lives on the
// engine (not on the peer builder) per the core/ext dependency DAG.
func (a *AppPeer) SubscriptionEngine() *subscription.Engine { return a.subEngine }

// Close shuts down the peer gracefully per SDK-OPERATIONS §8.2:
// stops accepting new connections, flushes pending async deliveries,
// closes active connections, stops engines, releases storage
// resources. Returns a 500 SDK Error on shutdown failure (partial
// cleanup).
func (a *AppPeer) Close() error {
	if err := a.peer.Close(); err != nil {
		return WrapError(500, "close_failed", "peer close", err)
	}
	return nil
}
