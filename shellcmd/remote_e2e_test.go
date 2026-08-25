package shellcmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"entity-workbench-go/entitysdk"
	"go.entitychurch.org/entity-core-go/core/crypto"
	"go.entitychurch.org/entity-core-go/core/peer"
)

// TestShell_RemoteConnectAndDispatch wires two AppPeers in-process,
// boots a Shell on top of the client peer, and exercises the
// connect → ls → cat path against the remote — proving the full
// stack (Phase 2 Track A end-to-end).
func TestShell_RemoteConnectAndDispatch(t *testing.T) {
	serverKP, _ := crypto.Generate()
	clientKP, _ := crypto.Generate()

	server, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Keypair:    &serverKP,
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &clientKP})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Seed a known entity in the server's tree at a stable path.
	if _, err := server.Store().Put("greetings/hello", "test/scalar", "world"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() { listenErr <- server.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-listenErr:
		t.Fatalf("server listen: %v", err)
	case <-time.After(1 * time.Second):
		t.Fatal("server not ready")
	}

	sh := NewShell(client, "local", "")
	reg := Default()

	// connect serv <addr>
	addr := server.Addr().String()
	res, err := reg.Dispatch(sh, "connect", []string{"serv", addr})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if res.Kind != KindMessage {
		t.Fatalf("connect: expected message result, got %v", res.Kind)
	}
	if !strings.Contains(res.Message, "connected to serv") {
		t.Errorf("connect message: %q", res.Message)
	}

	// cd serv: → working directory becomes /serverPeerID/
	if _, err := reg.Dispatch(sh, "cd", []string{"serv:"}); err != nil {
		t.Fatalf("cd serv:: %v", err)
	}
	if sh.WD.PeerID() != string(serverKP.PeerID()) {
		t.Fatalf("WD peer-id = %q, want %q", sh.WD.PeerID(), serverKP.PeerID())
	}

	// ls greetings (relative to serv:'s root, dispatches remotely)
	res, err = reg.Dispatch(sh, "ls", []string{"greetings"})
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if res.Kind != KindListing {
		t.Fatalf("ls: expected listing, got %v", res.Kind)
	}
	foundHello := false
	for _, row := range res.Listing {
		if row.Name == "hello" {
			foundHello = true
		}
	}
	if !foundHello {
		t.Errorf("ls didn't return 'hello'; got rows: %+v", res.Listing)
	}

	// cat greetings/hello (relative to serv:'s root)
	res, err = reg.Dispatch(sh, "cat", []string{"greetings/hello"})
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	if res.Kind != KindEntity {
		t.Fatalf("cat: expected entity result, got %v", res.Kind)
	}
	if res.Entity.Entity.Type != "test/scalar" {
		t.Errorf("cat: type = %q, want %q", res.Entity.Entity.Type, "test/scalar")
	}

	// disconnect serv → bookkeeping returns and resets WD.
	if _, err := reg.Dispatch(sh, "disconnect", []string{"serv"}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if _, ok := sh.Conns["serv"]; ok {
		t.Error("alias 'serv' still registered after disconnect")
	}
	if sh.WD != "/" {
		t.Errorf("WD = %q after disconnecting current peer, want '/'", sh.WD)
	}
}

// TestShell_Phase3Commands exercises put / has / rm / cp on a
// remote peer, plus the alias-path syntax (`alias:foo/bar`).
func TestShell_Phase3Commands(t *testing.T) {
	serverKP, _ := crypto.Generate()
	clientKP, _ := crypto.Generate()

	server, err := entitysdk.CreatePeer(entitysdk.PeerConfig{
		Keypair:    &serverKP,
		ListenAddr: "127.0.0.1:0",
		RawOptions: []peer.Option{peer.WithConnectionGrants(peer.OpenAccessGrants())},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &clientKP})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() { listenErr <- server.ListenReady(ctx, ready) }()
	select {
	case <-ready:
	case err := <-listenErr:
		t.Fatalf("server listen: %v", err)
	case <-time.After(1 * time.Second):
		t.Fatal("server not ready")
	}

	sh := NewShell(client, "local", "")
	reg := Default()

	addr := server.Addr().String()
	if _, err := reg.Dispatch(sh, "connect", []string{"serv", addr}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// put serv:demo/value test/scalar 7 — alias-path syntax.
	res, err := reg.Dispatch(sh, "put", []string{"serv:demo/value", "test/scalar", "7"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !strings.Contains(res.Message, "put /"+string(serverKP.PeerID())+"/demo/value") {
		t.Errorf("put result message: %q", res.Message)
	}

	// has serv:demo/value → yes.
	res, err = reg.Dispatch(sh, "has", []string{"serv:demo/value"})
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if !strings.HasPrefix(res.Message, "yes") {
		t.Errorf("has(yes) message: %q", res.Message)
	}

	// has serv:demo/missing → no.
	res, err = reg.Dispatch(sh, "has", []string{"serv:demo/missing"})
	if err != nil {
		t.Fatalf("has missing: %v", err)
	}
	if !strings.HasPrefix(res.Message, "no") {
		t.Errorf("has(no) message: %q", res.Message)
	}

	// cp serv:demo/value local:scratch/copy — cross-peer copy from
	// remote to the shell's local peer.
	res, err = reg.Dispatch(sh, "cp", []string{"serv:demo/value", "local:scratch/copy"})
	if err != nil {
		t.Fatalf("cp: %v", err)
	}
	if !strings.Contains(res.Message, "copied") {
		t.Errorf("cp result message: %q", res.Message)
	}
	if !client.Store().Has("scratch/copy") {
		t.Error("local store missing scratch/copy after cp")
	}

	// rm serv:demo/value → entity gone on the server.
	if _, err := reg.Dispatch(sh, "rm", []string{"serv:demo/value"}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if server.Store().Has("demo/value") {
		t.Error("server still has demo/value after rm")
	}

	// has after rm → no.
	res, err = reg.Dispatch(sh, "has", []string{"serv:demo/value"})
	if err != nil {
		t.Fatalf("has post-rm: %v", err)
	}
	if !strings.HasPrefix(res.Message, "no") {
		t.Errorf("has post-rm: %q", res.Message)
	}
}
