using System;
using System.Runtime.InteropServices;

namespace EntityAvalonia;

// P/Invoke surface for libbridge.so — must match the //export
// declarations in ../bridge/main.go exactly.
//
// Phase I Session 2 multi-peer pivot: every per-peer op now takes a
// leading `long peerHandle`. The C# side acquires its peer handle from
// `Bridge.DefaultPeer()` after `Bridge.Init(...)` and threads it through
// every subsequent call. Session 3 will add a peer manager UI (tab
// strip + new-peer modal) that uses PeerCreate / PeerDestroy / PeerList
// / PeerConfig directly to manage multiple peers concurrently.
public static class Bridge
{
    private const string Lib = "bridge";

    // void cb(int64_t handle, const char* event_json)
    [UnmanagedFunctionPointer(CallingConvention.Cdecl)]
    public delegate void WatchCallback(long handle, IntPtr eventJsonPtr);

    // BridgeInit boots a default peer per the JSON config. Returns NULL
    // on success; on failure returns an error string the caller must
    // release with FreeString.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "BridgeInit")]
    public static extern IntPtr Init([MarshalAs(UnmanagedType.LPStr)] string configJson);

    // BridgeDefaultPeer returns the handle of the peer booted by
    // BridgeInit (or 0 if init hasn't run). Call once post-init.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "BridgeDefaultPeer")]
    public static extern long DefaultPeer();

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "BridgeShutdown")]
    public static extern void Shutdown();

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "Hello")]
    public static extern IntPtr Hello();

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "FreeString")]
    public static extern void FreeString(IntPtr p);

    // --- Peer manager (Phase I S2) --------------------------------------
    //
    // PeerCreate boots a new peer. Envelope: {ok, handle} or {ok:false, error}.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerCreate")]
    public static extern IntPtr PeerCreate([MarshalAs(UnmanagedType.LPStr)] string configJson);

    // PeerDestroy tears down peer h. Cascades through trees + watches.
    // Envelope: {ok} or {ok:false, error}.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerDestroy")]
    public static extern IntPtr PeerDestroy(long peerHandle);

    // PeerList enumerates live peers. Envelope:
    // {ok:true, peers:[{handle,peer_id,alias,identity,storage_kind,
    //                   listen,is_system,connections,added_at},...]}
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerList")]
    public static extern IntPtr PeerList();

    // PeerConfig returns the bootstrap config snapshot for peer h.
    // Envelope: {ok, config:{identity,alias,storage,storage_path,listen,open_access}}.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerConfig")]
    public static extern IntPtr PeerConfig(long peerHandle);

    // PeerListenAddr returns the bound listener address (if any) for
    // peer h. Envelope:
    //   {ok:true, result:{listening:true|false, scheme:"tcp"|"ws", addr:"..."}}
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerListenAddr")]
    public static extern IntPtr PeerListenAddr(long peerHandle);

    // BridgeRestorePeers reads the system peer's roster and respawns
    // every non-ephemeral peer that isn't already hosted. Call after
    // Init. Envelope:
    //   {ok:true, restored:[handle,...]}
    //   {ok:true, restored:[...], warning:"N of M failed (...)"}
    //   {ok:false, error:"..."}
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "BridgeRestorePeers")]
    public static extern IntPtr RestorePeers();

    // --- Per-peer ops ---------------------------------------------------

    // DispatchLine runs a single shell input line through peer h's
    // shell. Envelope: { ok, lines: [{text,kind}...], prompt: "entity:…:/… > " }
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "DispatchLine")]
    public static extern IntPtr DispatchLine(long peerHandle, [MarshalAs(UnmanagedType.LPStr)] string line);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellPrompt")]
    public static extern IntPtr ShellPrompt(long peerHandle);

    // Complete returns candidate completions for the last token of the
    // line on peer h's shell. Envelope: { ok, candidates: [..], tokenStart }.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "Complete")]
    public static extern IntPtr Complete(long peerHandle, [MarshalAs(UnmanagedType.LPStr)] string line);

    // EntityGet runs `get <path>` against peer h, no prompt-echo line.
    // Envelope: { ok, lines: [{text,kind}, ...] }.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "EntityGet")]
    public static extern IntPtr EntityGet(long peerHandle, [MarshalAs(UnmanagedType.LPStr)] string path);

    // PeerSummary returns the per-peer status snapshot for h.
    // Envelope: { ok, alias, peer_id, identity, connections }.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerSummary")]
    public static extern IntPtr PeerSummary(long peerHandle);

    // WatchSubscribe attaches a watch to peer h's store. Returns
    // {ok:true, handle:N} — the WATCH handle is distinct from the peer
    // handle and is what WatchUnsubscribe consumes.
    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "WatchSubscribe")]
    public static extern IntPtr WatchSubscribe(long peerHandle, [MarshalAs(UnmanagedType.LPStr)] string pattern, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "WatchUnsubscribe")]
    public static extern void WatchUnsubscribe(long watchHandle);

    // --- Tree-browser ops -----------------------------------------------
    //
    // Tree handles also live in a flat namespace (distinct from peer
    // handles + watch handles). TreeOpen takes the peer it's bound to;
    // every subsequent tree op takes only the tree handle.

    [UnmanagedFunctionPointer(CallingConvention.Cdecl)]
    public delegate void TreeWakeCallback(long handle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeOpen")]
    public static extern IntPtr TreeOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeRegisterWake")]
    public static extern IntPtr TreeRegisterWake(long treeHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeRender")]
    public static extern IntPtr TreeRender(long treeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeToggleExpand")]
    public static extern IntPtr TreeToggleExpand(long treeHandle, int index);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeSetSearch")]
    public static extern IntPtr TreeSetSearch(long treeHandle, [MarshalAs(UnmanagedType.LPStr)] string text);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "TreeClose")]
    public static extern void TreeClose(long treeHandle);

    // --- PeerInfo panel -------------------------------------------------
    //
    // Per-peer statistics (entity count, path count, sorted path list).
    // Same handle pattern as the tree: Open → handle, RegisterWake →
    // wake-fanout goroutine wired to a C callback, Render → snapshot,
    // Close → tear down. Cascades on peer destroy.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerInfoOpen")]
    public static extern IntPtr PeerInfoOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerInfoRegisterWake")]
    public static extern IntPtr PeerInfoRegisterWake(long peerInfoHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerInfoRender")]
    public static extern IntPtr PeerInfoRender(long peerInfoHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "PeerInfoClose")]
    public static extern void PeerInfoClose(long peerInfoHandle);

    // --- Log viewer panel ----------------------------------------------
    //
    // Wraps wb.LogFilterModel. Wake fires per EventLog append (via
    // EventLog.OnAppend). LogCycleDisplayLevel cycles the per-panel
    // filter; LogCycleCollectionLevel cycles the global verbosity.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogOpen")]
    public static extern IntPtr LogOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogRegisterWake")]
    public static extern IntPtr LogRegisterWake(long logHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogRender")]
    public static extern IntPtr LogRender(long logHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogCycleDisplayLevel")]
    public static extern IntPtr LogCycleDisplayLevel(long logHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogCycleCollectionLevel")]
    public static extern IntPtr LogCycleCollectionLevel(long logHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LogClose")]
    public static extern void LogClose(long logHandle);

    // --- Markdown view panel -------------------------------------------
    //
    // Read-mode renderer for doc/markdown-file entities. LoadPath binds
    // a path + rebinds the per-path Store.Watch. Wake fires on path
    // change + on entity content mutation. Edit/save not yet exposed.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownViewOpen")]
    public static extern IntPtr MarkdownViewOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownViewRegisterWake")]
    public static extern IntPtr MarkdownViewRegisterWake(long markdownHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownViewLoadPath")]
    public static extern IntPtr MarkdownViewLoadPath(long markdownHandle, [MarshalAs(UnmanagedType.LPStr)] string path);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownViewRender")]
    public static extern IntPtr MarkdownViewRender(long markdownHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownViewClose")]
    public static extern void MarkdownViewClose(long markdownHandle);

    // --- Markdown files panel ------------------------------------------
    //
    // Tree-shaped browser filtered to doc/markdown-file under "docs/".
    // Same open/wake/render/toggleExpand/close shape as the main tree.
    // C# fires the host's PublishSelectedPath when a leaf is selected.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownFilesOpen")]
    public static extern IntPtr MarkdownFilesOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownFilesRegisterWake")]
    public static extern IntPtr MarkdownFilesRegisterWake(long mdFilesHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownFilesRender")]
    public static extern IntPtr MarkdownFilesRender(long mdFilesHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownFilesToggleExpand")]
    public static extern IntPtr MarkdownFilesToggleExpand(long mdFilesHandle, int index);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "MarkdownFilesClose")]
    public static extern void MarkdownFilesClose(long mdFilesHandle);

    // --- Query browser panel -------------------------------------------
    //
    // Pull-only — no wake source. Set filters, call Execute, render
    // result page. SelectNext/Prev navigate; NextPage advances.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QueryOpen")]
    public static extern IntPtr QueryOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QuerySetFilters")]
    public static extern IntPtr QuerySetFilters(long queryHandle,
        [MarshalAs(UnmanagedType.LPStr)] string typeFilter,
        [MarshalAs(UnmanagedType.LPStr)] string pathPrefix);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QueryExecute")]
    public static extern IntPtr QueryExecute(long queryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QuerySelectNext")]
    public static extern IntPtr QuerySelectNext(long queryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QuerySelectPrev")]
    public static extern IntPtr QuerySelectPrev(long queryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QueryNextPage")]
    public static extern IntPtr QueryNextPage(long queryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QueryRender")]
    public static extern IntPtr QueryRender(long queryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "QueryClose")]
    public static extern void QueryClose(long queryHandle);

    // --- Handler browser panel -----------------------------------------
    //
    // Wake-driven on the `system/handler/` prefix: a handler registered
    // at runtime shows up without a poll. Execution is always explicit —
    // selecting a handler or an operation dispatches nothing, only
    // ExecuteSelected / ExecuteCustom do.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersOpen")]
    public static extern IntPtr HandlersOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersRegisterWake")]
    public static extern IntPtr HandlersRegisterWake(long handlersHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersRender")]
    public static extern IntPtr HandlersRender(long handlersHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersSelectHandler")]
    public static extern IntPtr HandlersSelectHandler(long handlersHandle, long index);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersSelectOperation")]
    public static extern IntPtr HandlersSelectOperation(long handlersHandle, long index);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersExecuteSelected")]
    public static extern IntPtr HandlersExecuteSelected(long handlersHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersExecuteCustom")]
    public static extern IntPtr HandlersExecuteCustom(long handlersHandle,
        [MarshalAs(UnmanagedType.LPStr)] string uri,
        [MarshalAs(UnmanagedType.LPStr)] string op,
        [MarshalAs(UnmanagedType.LPStr)] string resource);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "HandlersClose")]
    public static extern void HandlersClose(long handlersHandle);

    // --- Site view panel -----------------------------------------------
    //
    // Read-projection of the SITE convention (app/site-manifest +
    // app/site-page, v0.5). Single-shot per Navigate; wake fires on
    // model state change (Navigate, GoBack, Invalidate). Navigate's
    // envelope carries `kind` — "navigated" (model state moved) or
    // "external" (target is an http/mailto link the OS should handle).

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteOpen")]
    public static extern IntPtr SiteOpen(long peerHandle,
        [MarshalAs(UnmanagedType.LPStr)] string peerId,
        [MarshalAs(UnmanagedType.LPStr)] string siteId);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteRegisterWake")]
    public static extern IntPtr SiteRegisterWake(long siteHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteNavigate")]
    public static extern IntPtr SiteNavigate(long siteHandle,
        [MarshalAs(UnmanagedType.LPStr)] string target);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteGoBack")]
    public static extern IntPtr SiteGoBack(long siteHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteRender")]
    public static extern IntPtr SiteRender(long siteHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SiteClose")]
    public static extern void SiteClose(long siteHandle);

    // --- Snake game panel ------------------------------------------------
    //
    // Display/input driver for the Snake hostable compute program
    // (wb.SnakeGameModel — the compute-program runtime contract's first
    // GUI consumer). Render returns the output-port frame as JSON;
    // Input writes the snapshot input port (0=up 1=right 2=down 3=left);
    // Start/Stop/Restart drive the host tick clock. Wake fires after
    // every tick and run-state change.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeOpen")]
    public static extern IntPtr SnakeOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeRegisterWake")]
    public static extern IntPtr SnakeRegisterWake(long snakeHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeInput")]
    public static extern IntPtr SnakeInput(long snakeHandle, long direction);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeStart")]
    public static extern IntPtr SnakeStart(long snakeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeStop")]
    public static extern IntPtr SnakeStop(long snakeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeRestart")]
    public static extern IntPtr SnakeRestart(long snakeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeRender")]
    public static extern IntPtr SnakeRender(long snakeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "SnakeClose")]
    public static extern void SnakeClose(long snakeHandle);

    // --- Asteroids game panel --------------------------------------------
    //
    // Display driver for the Asteroids hostable compute program
    // (wb.AsteroidsGameModel) — the first heterogeneous, variable-actor-set
    // program, and the first to bind a DISPLAY-LIST output port.
    //
    // Two things differ from Snake, and both are the point:
    //   - AsteroidsInput takes a held-key SET as a bitmask (bit 0 = left,
    //     1 = right, 2 = thrust, 3 = fire), not one direction. Snake's port
    //     is a single last-write-wins value and cannot express "thrust +
    //     rotate + fire at once"; a bitmask can, and it is still a SNAPSHOT
    //     port — no new port kind was needed.
    //   - AsteroidsRender returns a DISPLAY LIST (world-space quads + kind
    //     tags), not game state. The panel draws polylines and knows nothing
    //     about asteroids.
    // See docs/architecture/reviews/COMPUTE-ASTEROIDS-PORT-TAXONOMY-2026-07-16.md.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsOpen")]
    public static extern IntPtr AsteroidsOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsRegisterWake")]
    public static extern IntPtr AsteroidsRegisterWake(long asteroidsHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsInput")]
    public static extern IntPtr AsteroidsInput(long asteroidsHandle, long keyBitmask);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsStart")]
    public static extern IntPtr AsteroidsStart(long asteroidsHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsStop")]
    public static extern IntPtr AsteroidsStop(long asteroidsHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsRestart")]
    public static extern IntPtr AsteroidsRestart(long asteroidsHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsRender")]
    public static extern IntPtr AsteroidsRender(long asteroidsHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "AsteroidsClose")]
    public static extern void AsteroidsClose(long asteroidsHandle);

    // --- Life game panel -------------------------------------------------
    //
    // Display driver for the Conway's Life hostable compute program
    // (wb.LifeGameModel). Same surface as Snake minus the input port —
    // Life is a closed program (state₀ + step is the whole thing), so
    // there is no LifeInput. Render returns the output-port frame as
    // JSON; Start/Stop/Restart drive the host tick clock. Wake fires
    // after every generation and run-state change.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeOpen")]
    public static extern IntPtr LifeOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeRegisterWake")]
    public static extern IntPtr LifeRegisterWake(long lifeHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeStart")]
    public static extern IntPtr LifeStart(long lifeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeStop")]
    public static extern IntPtr LifeStop(long lifeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeRestart")]
    public static extern IntPtr LifeRestart(long lifeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeRender")]
    public static extern IntPtr LifeRender(long lifeHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "LifeClose")]
    public static extern void LifeClose(long lifeHandle);

    // --- The generic compute-program host -------------------------------
    //
    // ONE seam for every compute program, replacing what the three blocks
    // above do per-program. The split is deliberate and visible:
    //
    //   ProgramAuthor(peer, name) -> descriptor path   ; per-program, once
    //   ProgramMount(peer, path)  -> handle            ; generic, always
    //
    // A front-end holding a descriptor fetched from another peer by hash
    // calls ProgramMount alone and never touches ProgramAuthor. That is the
    // transferable-compute story in two signatures.
    //
    // ProgramRender returns every output port decoded BY SHAPE, so the C#
    // side binds shapes (text, display-list) and never a program.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramAuthor")]
    public static extern IntPtr ProgramAuthor(long peerHandle,
        [MarshalAs(UnmanagedType.LPUTF8Str)] string name);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramMount")]
    public static extern IntPtr ProgramMount(long peerHandle,
        [MarshalAs(UnmanagedType.LPUTF8Str)] string descriptorPath);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramRegisterWake")]
    public static extern IntPtr ProgramRegisterWake(long programHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramStart")]
    public static extern IntPtr ProgramStart(long programHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramStop")]
    public static extern IntPtr ProgramStop(long programHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramRestart")]
    public static extern IntPtr ProgramRestart(long programHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramRender")]
    public static extern IntPtr ProgramRender(long programHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramInputKeys")]
    public static extern IntPtr ProgramInputKeys(long programHandle,
        [MarshalAs(UnmanagedType.LPUTF8Str)] string portName, long keys);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramInputDirection")]
    public static extern IntPtr ProgramInputDirection(long programHandle,
        [MarshalAs(UnmanagedType.LPUTF8Str)] string portName, long dir);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ProgramClose")]
    public static extern void ProgramClose(long programHandle);

    // --- Per-panel shell ------------------------------------------------
    //
    // PHASE-I-DESKTOP-RENDERER-PLAN §I.5 — each ShellPanel owns its
    // own shellcmd.Shell over the peer's shared workspace, so multiple
    // panels can have independent WD / history / completion. The peer-
    // keyed DispatchLine / Complete / ShellPrompt above remain for
    // programmatic ingress paths (smoke driver, tests).

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellOpen")]
    public static extern IntPtr ShellOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellClose")]
    public static extern void ShellClose(long shellHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellDispatchLine")]
    public static extern IntPtr ShellDispatchLine(long shellHandle,
        [MarshalAs(UnmanagedType.LPStr)] string line);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellComplete")]
    public static extern IntPtr ShellComplete(long shellHandle,
        [MarshalAs(UnmanagedType.LPStr)] string line);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ShellPromptForHandle")]
    public static extern IntPtr ShellPromptForHandle(long shellHandle);

    // --- Peer connections panel ---------------------------------------
    //
    // PHASE-I-PEER-CONNECTIONS-PLAN B-1. Wakes fire whenever the
    // peer's `system/peer/transport/` prefix changes (i.e. any
    // Connect/Disconnect from this panel, a shell panel, or any other
    // surface). Connect/Disconnect dispatch through the shared shell
    // workspace, so alias bindings are visible to ShellPanels on the
    // same peer.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsOpen")]
    public static extern IntPtr ConnectionsOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsRegisterWake")]
    public static extern IntPtr ConnectionsRegisterWake(long connsHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsRender")]
    public static extern IntPtr ConnectionsRender(long connsHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsConnect")]
    public static extern IntPtr ConnectionsConnect(long connsHandle,
        [MarshalAs(UnmanagedType.LPStr)] string alias,
        [MarshalAs(UnmanagedType.LPStr)] string address);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsDisconnect")]
    public static extern IntPtr ConnectionsDisconnect(long connsHandle,
        [MarshalAs(UnmanagedType.LPStr)] string alias);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "ConnectionsClose")]
    public static extern void ConnectionsClose(long connsHandle);

    // --- Discovery (mDNS) -----------------------------------------------
    // Handle lifecycle mirrors Connections* — Open returns {ok,handle};
    // RegisterWake hooks the C# OnWake delegate; Render returns the
    // current "Nearby peers" snapshot; Close tears down. The handle is
    // scoped to one peer; auto-cleaned when the peer is destroyed via
    // the bridge's cascadeDiscoveries OnPeerDestroyed hook.

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "DiscoveryOpen")]
    public static extern IntPtr DiscoveryOpen(long peerHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "DiscoveryRegisterWake")]
    public static extern IntPtr DiscoveryRegisterWake(long discoveryHandle, IntPtr callback);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "DiscoveryRender")]
    public static extern IntPtr DiscoveryRender(long discoveryHandle);

    [DllImport(Lib, CallingConvention = CallingConvention.Cdecl, EntryPoint = "DiscoveryClose")]
    public static extern void DiscoveryClose(long discoveryHandle);

    // TakeString copies a C-string allocated by Go into a managed
    // string and immediately frees the Go-side allocation. Returns
    // empty string when given IntPtr.Zero (Go's NULL return).
    public static string TakeString(IntPtr p)
    {
        if (p == IntPtr.Zero) return "";
        var s = Marshal.PtrToStringAnsi(p) ?? "";
        FreeString(p);
        return s;
    }
}
