package workbench

import (
	"testing"

	"github.com/fxamacker/cbor/v2"

	"go.entitychurch.org/entity-core-go/core/ecf"
	"go.entitychurch.org/entity-core-go/core/entity"
	"go.entitychurch.org/entity-core-go/core/handler"
	"go.entitychurch.org/entity-core-go/core/peer"
	"go.entitychurch.org/entity-core-go/core/store"
	"go.entitychurch.org/entity-core-go/core/types"
	"go.entitychurch.org/entity-core-go/ext/query"
)

// testPeerContext creates a PeerContext backed by in-memory store/index
// with a minimal handler registry. Returns the PeerContext and the raw
// store/index for seeding test data.
func testPeerContext(t *testing.T) (*PeerContext, store.ContentStore, store.LocationIndex) {
	t.Helper()
	s := store.NewMemoryContentStore()
	li := store.NewMemoryLocationIndex()
	reg := handler.NewRegistry()
	ex := NewExecutor(reg, s, li, "test-peer")
	st := NewStore(s, li)
	pc := NewPeerContext(ex, st)
	return pc, s, li
}

func seedStore(t *testing.T, s store.ContentStore, li store.LocationIndex, path, typ string, val interface{}) {
	t.Helper()
	raw, err := ecf.Encode(val)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := entity.NewEntity(typ, cbor.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Put(ent)
	if err != nil {
		t.Fatal(err)
	}
	li.Set(path, h)
}

func seedHandlerInterface(t *testing.T, s store.ContentStore, li store.LocationIndex, pattern, name string, ops map[string]types.HandlerOperationSpec) {
	t.Helper()
	data := types.HandlerInterfaceData{
		Pattern:    pattern,
		Name:       name,
		Operations: ops,
	}
	raw, err := ecf.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := entity.NewEntity(types.TypeHandlerInterface, cbor.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Put(ent)
	if err != nil {
		t.Fatal(err)
	}
	li.Set("system/handler/"+pattern, h)
}

func testWorkspaceState(t *testing.T) (*WorkspaceState, *PeerContext) {
	t.Helper()
	pc, _, _ := testPeerContext(t)
	_ = pc // cache removed
	ws := NewWorkspaceState(pc.Store())
	return ws, pc
}

// errCode decodes an ErrorData entity from a handler response and
// returns its Code field. Originally co-located with the
// RevisionConvergeHandler test file; promoted here when RCH retired
// (`revision:pull` REVISION §4.4.8 subsumes it) so chain-errors and
// notification-ingest tests retain the helper.
func errCode(t *testing.T, resp *handler.Response) string {
	t.Helper()
	var ed types.ErrorData
	if err := ecf.Decode(resp.Result.Data, &ed); err != nil {
		t.Fatalf("decode ErrorData: %v", err)
	}
	return ed.Code
}

// notifEntity builds an InboxNotification entity for tests that
// drive subscription-delivery code paths. Same retirement story as
// errCode above.
func notifEntity(t *testing.T, uri string) entity.Entity {
	t.Helper()
	e, err := types.SubscriptionNotificationData{
		SubscriptionID: "sub-1",
		Event:          "updated",
		URI:            uri,
	}.ToEntity()
	if err != nil {
		t.Fatalf("build notification: %v", err)
	}
	return e
}

// testPeerContextWithQuery creates a PeerContext backed by a real
// peer with the query extension handler registered. Use this for
// tests that exercise code paths going through system/query (e.g.
// MarkdownFilesModel, anything that filters by entity type).
//
// Seed data through the returned Store (via Store.Put) — writes must
// go through the peer's location index so the query extension's sync
// hook sees the new entities. Direct content-store writes wouldn't
// update the index and the query would miss them.
func testPeerContextWithQuery(t *testing.T) (*PeerContext, *Executor, *Store) {
	t.Helper()
	cs := store.NewMemoryContentStore()
	qm := query.NewIndexMaintainer(cs)
	qh := query.NewHandler(qm.TypeIndex(), qm.ReverseHashIndex(), qm.PathLinkIndex(), cs)

	p, err := peer.New(
		peer.WithStore(cs),
		peer.WithNamedSyncHook("query", qm.OnTreeChange),
		peer.WithHandler("system/query", qh),
	)
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	ex := NewExecutor(p.Registry(), p.Store(), p.LocationIndex(), p.PeerID())
	st := NewStore(p.Store(), p.LocationIndex())
	pc := NewPeerContext(ex, st)
	return pc, ex, st
}
