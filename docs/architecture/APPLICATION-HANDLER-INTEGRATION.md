# Application Integration with the Entity Protocol

How applications integrate with the entity system through handlers,
subscriptions, and the bridge pattern. Covers the protocol mechanics,
cross-language patterns, and the convergence toward application-as-peer.

**Status**: Architecture design

---

## 1. The Current Problem

The workbench reads the raw Go `TreeEvents()` channel and refreshes
every panel on every tree event. No routing, no filtering, Go-specific.
The entity system already has subscriptions, notifications, inbox,
and continuation delivery — all working. The application should use
them.

## 2. The Protocol Pipeline (Already Implemented)

The complete flow from path change to notification delivery:

```
locIndex.Set/Remove
    → NotifyingLocationIndex emits TreeChangeEvent (store/notifying.go)
    → fan-out broadcasts to sinks (peer/fanout.go)
    → Subscription Engine matches patterns (ext/subscription/engine.go)
    → builds SubscriptionNotificationData
    → delivers via authenticated EXECUTE (ext/subscription/delivery.go)
    → Inbox Handler receives (ext/inbox/handler.go)
    → optional continuation chain (ext/continuation/handler.go)
    → target handler receives notification
```

All components live in entity-core-go and are functional. The
subscription handler at `system/subscription` accepts subscribe/cancel.
The engine does pattern matching, event filtering, rate limiting, and
token validation. Delivery builds authenticated envelopes with
capability chains.

## 3. The Application Handler

The application registers a handler at `workspace/app` on the peer.
This handler bridges protocol notifications into the application's
native event model.

### Go Implementation

```go
type AppHandler struct {
    notifications chan Notification
}

func (h *AppHandler) Handle(ctx context.Context, req *handler.Request) (*handler.Response, error) {
    if req.Operation == "receive" {
        var notif types.SubscriptionNotificationData
        ecf.Decode(req.Params.Data, &notif)
        h.notifications <- Notification{
            SubscriptionID: notif.SubscriptionID,
            Event:          notif.Event,
            Path:           notif.URI,
            Hash:           notif.Hash,
        }
        return &handler.Response{Status: 200}, nil
    }
    return handler.NewErrorResponse(400, "unknown_operation", req.Operation)
}
```

The Go channel is the bridge. Protocol on one side, Go concurrency
on the other. The UI goroutine reads the channel and refreshes panels.

### How Panels Subscribe

On startup, the application creates subscriptions for each panel's
interest patterns:

```
Execute console: subscribe to "system/handler/*"
Tree browser:    subscribe to "*"
Log viewer:      subscribe to "workspace/settings/*"
```

When matching paths change, notifications flow through the pipeline
to the app handler, through the channel, to the specific panel.

### What Changes

```
BEFORE: tree event (any) → refresh ALL panels
AFTER:  tree event at system/handler/foo
            → subscription matches → notification to workspace/app
            → channel → route to execute console only
```

## 4. The Bridge Pattern

The application handler is a bridge handler — the same pattern used
for HTTP, filesystem, database, or any external protocol integration.

### Bridge Handlers Work in Two Directions

**Outgoing** (entity system acts as client into external systems):

```
Entity System                Handler                External System

execute data/files "read"    fs handler              filesystem
    path: readme.md      →  translates to      →  os.Open("readme.md")
    ← entity result      ←  reads bytes,       ←  file contents
                             creates entity

execute http/api "get"       http client handler     HTTP server
    path: /users/123      →  builds HTTP req    →  GET /users/123
    ← entity result       ←  parses response   ←  JSON body

execute db/users "query"     database handler        PostgreSQL
    params: {age: >21}   →  builds SQL         →  SELECT * WHERE age>21
    ← entity result       ←  maps rows         ←  result set
```

Each outgoing bridge translates entity protocol operations (execute
with path + operation + params) into the external system's native
protocol. The handler is format-aware — it knows how to map between
entity types and external representations.

**Incoming** (external systems act as clients into the entity system):

```
External System              Handler                Entity System

HTTP POST /api/data          http server handler
    body: JSON           →  parses, creates    →  tree put data/item
    ← HTTP 200           ←  entity             ←  handler response

WebSocket message            ws handler
    event: subscribe     →  translates to      →  system/subscription subscribe
    ← push notification  ←  ws.send()          ←  subscription notification

gRPC PutEntity               grpc handler
    proto message        →  decodes proto      →  tree put
    ← proto response     ←  encodes            ←  handler response

stdin line                   cli handler
    "get data/foo"       →  parses command     →  execute system/tree get
    ← stdout text        ←  formats            ←  entity data
```

Each incoming bridge exposes the entity system through an external
protocol. The handler translates external requests into entity
operations and entity responses back into external format.

### The Application Handler Is Both

**Incoming** (notifications arrive):
```
subscription notification  →  app handler  →  Go channel → UI refresh
```

**Outgoing** (user acts):
```
user clicks button  →  Go channel  →  app handler  →  execute (tree put)
```

The "external protocol" the application bridges is the Go
application's concurrency model. For an HTTP bridge, it's HTTP. For
a WebSocket bridge, it's WebSocket. Same pattern, different transport.

## 5. Cross-Language Patterns

The bridge mechanism varies by language. The protocol side is identical.

### Go: Channel Bridge

```go
type AppHandler struct {
    notifications chan Notification
}

func (h *AppHandler) Handle(ctx context.Context, req *handler.Request) (*handler.Response, error) {
    if req.Operation == "receive" {
        var notif types.SubscriptionNotificationData
        ecf.Decode(req.Params.Data, &notif)
        h.notifications <- Notification{
            SubscriptionID: notif.SubscriptionID,
            Event:          notif.Event,
            Path:           notif.URI,
            Hash:           notif.Hash,
        }
        return &handler.Response{Status: 200}, nil
    }
    return handler.NewErrorResponse(400, "unknown_operation", req.Operation)
}
```

Goroutines + channels are Go's native concurrency. The handler writes
to a channel, the UI goroutine reads it. For tview, the reader calls
`QueueUpdateDraw`. For raylib, it calls `WakeRenderLoop`.

### Rust: async/await + mpsc

```rust
struct AppHandler {
    tx: mpsc::Sender<Notification>,
}

impl Handler for AppHandler {
    async fn handle(&self, req: Request) -> Result<Response> {
        let notif = decode_notification(&req.params)?;
        self.tx.send(notif).await?;
        Ok(Response::ok())
    }
}

// UI side
let (tx, mut rx) = mpsc::channel(256);
let handler = AppHandler { tx };
// register handler on peer...

// UI loop (egui, winit, etc.)
loop {
    while let Ok(notif) = rx.try_recv() {
        route_to_panel(notif);
    }
    render_frame();
}
```

Structurally identical to Go. Rust's ownership model makes the
handler → channel → UI flow safe by construction. No runtime data
races. `tokio::sync::mpsc` for async runtimes, `crossbeam::channel`
for sync code.

### Python: asyncio / callback

**Async variant:**
```python
class AppHandler:
    def __init__(self):
        self.queue = asyncio.Queue()

    async def handle(self, req):
        notif = decode_notification(req.params)
        await self.queue.put(notif)
        return Response(status=200)

# UI loop
async def ui_loop(handler):
    while True:
        notif = await handler.queue.get()
        route_to_panel(notif)
        refresh_ui()
```

**Callback variant** (for synchronous UI frameworks like tkinter):
```python
class AppHandler:
    def __init__(self, on_notify):
        self.on_notify = on_notify  # callback into UI

    def handle(self, req):
        notif = decode_notification(req.params)
        self.on_notify(notif)  # direct callback
        return Response(status=200)
```

Python challenge: the GIL means the handler and UI typically run on
the same thread (unless using multiprocessing). If the peer runs in
a thread, the handler callback needs to be thread-safe.
`asyncio.Queue` handles this for async code. `queue.Queue` from the
threading module handles it for sync code.

### Summary

| Language | Bridge Mechanism | Thread Safety | UI Integration |
|----------|-----------------|--------------|----------------|
| Go | `chan Notification` | Built-in (CSP) | QueueUpdateDraw / WakeRenderLoop |
| Rust | `tokio::sync::mpsc` | Ownership model | egui request_repaint / winit wake |
| Python | `asyncio.Queue` | asyncio event loop | tkinter after_idle / Qt signal |
| JS | `EventEmitter` | Single-threaded | requestAnimationFrame / React setState |

Each language's standard library provides the handler + bridge for
its native concurrency model. The entity protocol operations
(subscribe, deliver, execute) are the same everywhere.

### Standard Library API Shape (Cross-Language)

Every language's standard library provides the same conceptual API:

```
AppContext
├── CreatePeer() → peer with app handler registered
├── Subscribe(pattern, events) → subscription on peer
├── Notifications() → language-native event source
│   ├── Go: <-chan Notification
│   ├── Rust: mpsc::Receiver<Notification>
│   ├── Python: asyncio.Queue
│   └── JS: EventEmitter
├── State(path) → read app state via protocol
├── SetState(path, data) → write app state via protocol
├── Connect(addr) → connect to external peer
└── Close()
```

The API shape is the same. The concurrency bridge is language-native.
The protocol operations are identical because they're defined by the
entity system, not by Go/Rust/Python.

## 6. Subscription Creation Mechanics

Creating a subscription through the protocol requires capability
tokens — the subscription system enforces authorization on who can
watch what paths and where notifications get delivered.

### The Subscribe Flow

```
1. Application needs a capability token that grants access to
   system/inbox (where notifications are delivered):
   → token at system/capability/grants/system/inbox
   → this is created during peer setup (handler grants)

2. Application sends EXECUTE to system/subscription:
   path:      system/subscription
   operation: subscribe
   resource:  {targets: ["system/handler/*"]}  ← the watch pattern
   params:    SubscriptionRequestData {
                events:        ["created", "updated", "deleted"]
                deliver_to:    {uri: "workspace/app", operation: "receive"}
                deliver_token: <hash of inbox capability token>
              }
   included:  {<deliver_token entity>}  ← proves delivery authorization

3. Subscription handler validates:
   - Delivery token exists in included
   - Token grants access to deliver_to URI's handler scope
   - Token is not expired

4. Subscription registered:
   - Engine adds pattern "system/handler/*" to path index
   - Subscription entity stored at system/subscription/{id}
   - Engine matches against this pattern on every tree event

5. When system/handler/data/files changes:
   - Engine matches pattern
   - Builds SubscriptionNotificationData
   - Delivers via authenticated EXECUTE to workspace/app
   - App handler receives, writes to channel
```

### Qualification

Patterns are qualified with the peer ID during subscription:
- Bare pattern `"system/handler/*"` becomes `"{peerID}/system/handler/*"`
- This scopes the subscription to the local peer's tree
- Cross-peer patterns use full URIs: `"entity://other-peer/data/*"`

### Multiple Subscriptions

A single application handler can receive from many subscriptions.
The `SubscriptionNotificationData.SubscriptionID` identifies which
subscription fired, allowing the application to route notifications
to the correct panel.

```
subscription "sub-handlers" → pattern "system/handler/*"
subscription "sub-types"    → pattern "system/type/*"
subscription "sub-all"      → pattern "*"

notification arrives with subscription_id "sub-handlers"
    → route to execute console panel
notification arrives with subscription_id "sub-all"
    → route to tree browser panel
```

## 6b. The External Peer Problem

When the peer runs out-of-process (or on a remote machine), the
application can't register a handler on it directly. Handler
registration is an in-process operation.

### Option A: Application IS a Peer

The application creates its own peer with a listener. It connects to
the external data peer. Subscriptions on the data peer deliver back
to the application peer via the wire protocol.

```
Data Peer (external)                Application Peer (in-process)
    │                                   │
    │ subscription fires                │ handler at workspace/app
    │ deliver_to: entity://app-peer/    │
    │ workspace/app                     │
    ├──── EXECUTE over TCP ────────────►│
    │                                   │ Handle() → channel → UI
```

This is the cleanest model. The application has full peer identity
(keypair, peer ID). It participates in the protocol as an equal.
Other peers can discover it, subscribe to its state, send it commands.

Cost: the application needs a listener (port allocation). It's a
real network peer. May be overkill for a local UI tool.

### Option B: Application Polls an Inbox

The application creates subscriptions on the external peer with
`deliver_to` pointing to an inbox path on that peer. Then it polls
the inbox for new messages.

```
Data Peer (external)
    │
    │ subscription fires
    │ deliver_to: system/inbox/workspace/notifications
    │ inbox handler stores notification at
    │   system/inbox/workspace/notifications/{reqID}
    │
    │                    Application (client)
    │                        │
    │◄── tree get system/inbox/workspace/notifications/* ──│
    │                        │
    │ returns stored notifications
```

This doesn't require the application to be a peer. It's a client
that reads its inbox. But it's polling, not push. Good enough for
applications that don't need real-time updates.

### Option C: Streaming Tree Events

If the protocol supports a streaming operation (like a long-lived
EXECUTE that returns events as they happen), the application could
subscribe to a stream instead of individual notifications.

This isn't in the current protocol but is a natural extension.
WebSocket bridges could provide this.

### Option D: Mixed Model (Recommended)

In-process app peer for local state + handler registration. Connect
to external peers for data access.

```
App Peer (in-process)           Data Peer (external)
    │                               │
    │ handler: workspace/app        │ has the data
    │ state: workspace/*            │
    │                               │
    │◄── connect ──────────────────►│
    │                               │
    │ data peer's tree appears      │
    │ under data peer's namespace   │
    │ in the app peer's view        │
    │                               │
    │ app peer subscribes to        │
    │ data peer's paths through     │
    │ the connection                │
```

The app peer connects to the data peer. Remote tree data appears in
the app peer's namespaced index. Subscriptions on remote paths
deliver through the connection. The application handler on the app
peer receives notifications for both local and remote path changes.

This is probably the right model for most applications:
- App peer for local state + handler registration
- Connect to external peers for data access
- Same subscription/notification mechanism works for both
- Application handler doesn't care whether the notification came
  from a local or remote path change

## 7. The Convergence Spectrum

The architecture isn't binary. Applications exist on a spectrum:

```
Level 0: Client         — connects, sends EXECUTEs, no local state
Level 1: Cached client  — local cache, dirty tracking (PeerContext now)
Level 2: Handler        — registers handler, receives notifications
Level 3: Full peer      — listener, other peers connect to it
Level 4: Entity-native  — all logic is handlers, all state is entities
```

Each level adds protocol integration and reduces custom code.
Everything you add at a lower level (event routing, state sync,
authorization) already exists in the protocol at higher levels.

This is why the architecture keeps converging toward "application is
a peer" — every capability you add to the application bridge is
reimplementing something the protocol already provides. But you're
not required to be at Level 4. Level 2 is the practical target for
the workbench now.

### What Each Level Gets You

| Capability | L0 | L1 | L2 | L3 | L4 |
|-----------|----|----|----|----|-----|
| Read tree data | yes | cached | cached | cached | native |
| Push notifications | no | no | yes | yes | yes |
| Path-based routing | no | no | yes | yes | yes |
| Reachable by other peers | no | no | no | yes | yes |
| State persistence | no | no | partial | yes | yes |
| State sync across machines | no | no | no | yes | yes |
| Cross-peer subscriptions | no | no | local only | yes | yes |
| Capability enforcement | no | no | yes | yes | yes |
| Continuation chains | no | no | possible | yes | native |

### What Each Level Costs

| Cost | L0 | L1 | L2 | L3 | L4 |
|------|----|----|----|----|-----|
| Complexity | minimal | cache mgmt | handler + subs | listener + auth | everything is handlers |
| Dependencies | TCP client | + store types | + handler registry | + network stack | + full protocol |
| Bootstrapping | connect + go | + PeerContext | + handler reg | + listener setup | + handler design for all logic |
| Latency sensitivity | wire RTT | cache staleness | channel dispatch | wire RTT (incoming) | handler dispatch overhead |
| Debuggability | trace wire | inspect cache | trace notifications | trace both sides | trace everything |

The tradeoff at each level: more protocol integration means more
capability but more surface area to understand and debug. Level 2 is
the sweet spot for most interactive applications — you get push
notifications and path routing without the complexity of being a
full network peer.

## 8. What Is the UI?

If the application is a peer and the app handler is a bridge, then
the UI is also a handler — it bridges between the entity system and
the human.

### The UI Is a Bridge Handler

The UI translates entity state into visual representation (outgoing)
and human actions into entity operations (incoming):

```
Entity System              UI Handler              Screen
                              │
notification arrives  ──►  ui handler  ──────►  tview redraw
                           (refresh panel)       (pixels on screen)

user clicks / types   ◄──────────────────────  keyboard/mouse
                      ──►  ui handler  ──►  execute (selection change,
                                             tree put, handler dispatch)
```

### Two Bridges Composed

The workbench is two bridges composed:

```
Human ◄──► UI Bridge ◄──► Entity Protocol ◄──► App Handler ◄──► Entity System
           (tview)            (bus)              (workspace/*)      (peer)
```

But this collapses. If the entity protocol IS the bus, and the app
handler IS on the peer, then:

```
Human ◄──► UI Bridge (handler) ◄──► Peer
```

The UI is a handler on the peer. It receives notifications (things
changed, refresh), and it generates executes (user clicked, save this).
The entity protocol is the communication bus between the UI and the
rest of the system.

### Every Panel Is Conceptually a Handler

```
Peer
├── system/tree handler          (tree operations)
├── system/subscription handler  (watch paths)
├── system/inbox handler         (receive deliveries)
├── workspace/app handler        (application logic)
├── workspace/ui/tree-browser    (tree browser panel "handler")
├── workspace/ui/detail-view     (detail view panel "handler")
├── workspace/ui/execute-console (execute console panel "handler")
└── workspace/ui/log-viewer      (log viewer panel "handler")
```

Each panel subscribes to paths it cares about. Notifications arrive
through the protocol. The panel renders. User actions translate to
executes that go back through the protocol.

We don't have to literally register each panel as a handler on the
peer — the application handler at `workspace/app` can multiplex
notifications to panels internally. But the MODEL is that each panel
is a protocol endpoint. The internal routing is an optimization.

## 9. The "Application IS a Peer" Model

Three possible architectures for the application's relationship to
the entity system:

### Model A: Application Uses a Peer

```
Application → creates Peer → reads/writes through Executor
```

The application owns the peer but sits outside it. It reaches in.
This is the current workbench model (Level 1).

### Model B: Application Has a Peer

```
Application → registers handler on Peer → receives notifications
```

The application has an endpoint in the system. It participates in
the protocol. But it's still a separate thing from the peer. This
is the next step (Level 2).

### Model C: Application IS a Peer

```
Application = Peer + UI
```

The application's identity IS the peer's identity. Its handlers are
the peer's handlers. Its state is the peer's tree. The UI is just
a renderer for the peer's state.

Model C is the most entity-native. The workbench isn't a program that
manages peers — it's a peer that has a UI. Other peers interact with
it through the protocol. Its state is visible in its tree. Its
capabilities are defined by its handlers.

Model C is also the hardest to bootstrap. You need the peer
infrastructure before you can render anything. Model B is the
practical path — create a peer, register your application handler,
start participating. Over time, the boundary between "application"
and "peer" dissolves as more logic moves into handlers and more
state moves into the tree.

## 10. The Convergence

### Adding capabilities, you rebuild the protocol

Every time you add a capability to the application bridge, you're
reimplementing something the protocol already provides:

| You want... | So you add... | Protocol already has... |
|-------------|--------------|------------------------|
| Push notifications | Callback/channel | Subscriptions + delivery |
| Path-based routing | Pattern matching | Handler pattern matching |
| Event filtering | Event type filter | Subscription event types |
| Rate limiting | Throttle logic | Subscription limits |
| Authorization | Access checks | Capability tokens |
| State persistence | Local storage | Peer's content store |
| State sync | Sync protocol | Tree snapshot + merge |
| Async operations | Callback chains | Continuations + deliver_to |
| Error handling | Try/catch routing | Continuation on_error |

Each row is work you do if you're NOT a peer. If you ARE a peer,
you get it for free.

### The Recursive Nature

The entity protocol is general enough that every communication
pattern is a degenerate case of it:

- A Go channel is a zero-hop protocol connection with no serialization
- An HTTP bridge is a protocol-to-HTTP translator
- A WebSocket bridge is a protocol-to-WebSocket translator
- The UI is a handler that translates entity state to pixels and
  human actions to executes
- stdin/stdout is a protocol-to-text translator

When the application is fully at Level 4:

```
Peer
├── system/* handlers        (core protocol)
├── workspace/app handler    (application logic)
├── workspace/ui/* handlers  (UI panels — receive, emit executes)
└── data/* handlers          (domain logic)
```

The entity protocol is the system bus. Handlers communicate through
executes and notifications. The UI is a handler that bridges to
the screen. User actions are executes back into the system. State
changes emit notifications that reach interested handlers (panels).

The recursive insight: **the tool you need to build is made of the
same material as the tool you're using to build it.** You can't
escape the recursion because the protocol IS an application framework
— addressing, routing, authorization, events, state, async. That's
what application frameworks do. Any separate framework you build
on top just reinvents a subset.

This isn't a flaw — it's the nature of a sufficiently general
protocol. The practical answer: embrace it. The standard library
wraps the protocol in language-native APIs. The protocol is always
underneath. Applications that need more can reach it directly.

## 11. Design Principles for the Standard Library

Based on this analysis:

1. **The handler is the integration point.** Every language's standard
   library provides a handler that bridges into the native event model.
   This is the one essential piece.

2. **The protocol is the bus.** Don't build a separate event system,
   state sync, or notification mechanism. Use subscriptions, tree
   operations, and handler dispatch. They already work.

3. **Support the full spectrum.** Level 0 (client) through Level 4
   (entity-native) should all be possible. The standard library makes
   Level 2 easy and Level 3 straightforward.

4. **Bridges are symmetric.** Any bridge handler works in both
   directions. The HTTP bridge is also an HTTP server. The Go channel
   bridge receives notifications AND sends executes. Design bridge
   handlers with both directions in mind.

5. **Don't fight the recursion.** The application will look like a
   peer because it IS (or is becoming) a peer. The standard library
   is scaffolding that helps you get there. Some applications stay at
   Level 2. Some grow to Level 4. Both are valid architectures — not
   right or wrong, just different tradeoffs.

## 12. Security and Capability Implications

When the application registers as a handler and creates subscriptions,
it participates in the capability system.

### What the App Handler Needs

The app handler at `workspace/app` needs:
- **Handler grant** — authorizes the peer to dispatch to this handler.
  Created automatically by the peer during handler registration.
- **Inbox delivery token** — authorizes subscription notifications to
  be delivered to the handler. This is a capability token granting
  access to `system/inbox` for the delivery URI.

For an in-process app peer, these grants are created during peer
setup. The application doesn't need to manage them manually — the
standard library handles grant creation as part of `NewAppPeer()`.

### Cross-Peer Subscriptions

When the app peer subscribes to paths on a remote data peer, the
capability chain gets more interesting:
- The app peer needs a capability token from the data peer that
  grants subscription access
- The delivery token needs to authorize the data peer to deliver
  back to the app peer
- The data peer needs the app peer's inbox handler to be reachable
  (requires Level 3 — full peer with listener)

This is why the mixed model (Option D) is practical — the app peer
handles local subscriptions without cross-peer capability negotiation.
Remote data access uses the connection's existing capability context.

### Multiple Applications, Same Peer

When multiple applications connect to the same data peer:
- Each has its own subscriptions (different subscription IDs)
- Each has its own delivery URIs (different handler paths)
- They don't interfere — the subscription engine routes independently
- They CAN coordinate through the tree — e.g., shared state at a
  conventional path that both applications watch

This is the emit coordination model: applications don't talk to each
other, they share state through the tree.

## 13. Testing at Each Level

### Level 0-1 (Client / Cached Client)

Test with in-memory peer. Create peer, seed test data, create
PeerContext, call functions. This is what workbench tests do now:

```go
func TestResolveEntity(t *testing.T) {
    pc, s, li := testPeerContext(t)
    seedStore(t, s, li, "test/hello", "test/type", data)
    r, ok := pc.Resolve("test/hello")
    // assert...
}
```

### Level 2 (Handler Registration)

Test the app handler by sending it requests directly:

```go
func TestAppHandler(t *testing.T) {
    handler := &AppHandler{notifications: make(chan Notification, 10)}
    req := buildNotificationRequest("system/handler/foo", "created")
    resp, err := handler.Handle(ctx, req)
    // assert response status
    notif := <-handler.notifications
    // assert notification fields
}
```

Test subscription-based refresh by creating a peer with the full
subscription engine, registering the app handler, creating
subscriptions, writing to the tree, and asserting that the correct
notifications arrive on the channel.

### Level 3-4 (Full Peer / Entity-Native)

Integration tests with two peers connected. Subscribe across the
connection. Write on peer A, assert notification arrives at peer B's
app handler. This tests the full wire protocol path.

## 14. The Feedback Loop Question

When the application writes workspace state (selection, settings),
that triggers a tree event, which fires subscriptions, which delivers
notifications back to the application handler. Is this a problem?

No — it's the correct behavior:

1. App writes `workspace/settings/log-level` via TreePut
2. Tree event fires
3. Subscription for `workspace/settings/*` matches
4. Notification delivered to workspace/app handler
5. Handler sends to channel
6. UI receives: "settings changed" → log viewer refreshes title

This is the emit coordination model. Panels don't talk to each other.
They share state through the tree. When state changes, interested
panels get notified through the standard mechanism. The tree IS the
coordination bus.

The only concern is infinite loops: panel refresh writes state that
triggers notification that refreshes panel that writes state. Prevent
by not writing state if it hasn't changed (e.g., `lastSyncedSelection`
guard).

## 15. Practical Next Steps

For the Go workbench, moving from Level 1 to Level 2:

1. Implement `AppHandler` as `handler.Handler`, register at
   `workspace/app` on the app peer
2. Create subscriptions for panel interest patterns via
   `system/subscription` execute
3. Replace `peer.TreeEvents()` reading with notification channel
4. Route notifications to interested panels
5. Standard library wraps this as `NewAppPeer()` returning peer +
   notification channel

The subscription/notification/delivery infrastructure is already
working in entity-core-go. We're just connecting the application
to it through a handler.
