package entitysdk

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.entitychurch.org/entity-core-go/core/crypto"
)

// AdvertiseTransport self-publishes a transport profile for THIS peer
// at `system/peer/transport/{our-peer-id}/{profile-id}` —
// EXTENSION-NETWORK §6.5.1a D1 self-publication, which is a SHOULD:
// "a peer SHOULD publish a profile entity for each transport it accepts
// on."
//
// This is the *other* direction from AppPeer.Connect, which registers a
// profile for the peer we just dialed (the address-book direction). We
// had that one and not this one, so nothing in this tree ever told
// another peer how to reach us — a browser peer, in particular, has no
// way to learn that a Go peer accepts a WebSocket, which is the only
// transport that can carry server→client push to a page (§6.5.2b).
//
// dialURL is the address a PEER dials, which is not necessarily the one
// we bound: a listener on 0.0.0.0 is unreachable as advertised, and
// callers are expected to pass the routable form. The scheme selects
// the profile:
//
//	tcp://host:port      → system/peer/transport/tcp        (profile-id `primary`)
//	ws:// | wss://host/p → system/peer/transport/websocket   (profile-id `primary-ws`)
//	http:// | https://   → system/peer/transport/http        (profile-id `primary-http`)
//
// The profile-ids are core-go's per-transport defaults, distinct on
// purpose so the three coexist on one peer (§6.5.1a selection sorts by
// `(priority asc, profile-id lex)`).
//
// Republishing the same transport overwrites its profile — the entity
// carries `advertised_at`, so a re-advertise is how a peer refreshes a
// stale entry.
func (a *AppPeer) AdvertiseTransport(dialURL string) error {
	if dialURL == "" {
		return NewError(400, "invalid_url", "AdvertiseTransport: dialURL is empty")
	}
	if err := checkRoutableAdvertisement(dialURL); err != nil {
		return err
	}
	self := crypto.PeerID(a.PeerID())

	switch {
	case strings.HasPrefix(dialURL, "ws://"), strings.HasPrefix(dialURL, "wss://"):
		if err := a.peer.RegisterRemoteWS(self, dialURL); err != nil {
			return WrapError(500, "advertise_failed", "publish websocket transport profile", err)
		}
	case strings.HasPrefix(dialURL, "http://"), strings.HasPrefix(dialURL, "https://"):
		if err := a.peer.RegisterRemoteHTTP(self, dialURL); err != nil {
			return WrapError(500, "advertise_failed", "publish http transport profile", err)
		}
	case strings.HasPrefix(dialURL, "tcp://"):
		// core-go's RegisterRemote rejects a pre-schemed address
		// because the cohort wire shape pins the scheme position; it
		// prepends `tcp://` itself.
		if err := a.peer.RegisterRemote(self, strings.TrimPrefix(dialURL, "tcp://")); err != nil {
			return WrapError(500, "advertise_failed", "publish tcp transport profile", err)
		}
	default:
		return NewError(400, "unsupported_scheme",
			fmt.Sprintf("AdvertiseTransport: %q carries no known scheme; want tcp://, ws://, wss://, http:// or https://", dialURL))
	}
	return nil
}

// checkRoutableAdvertisement rejects the advertisement that is worse
// than none: a wildcard bind address.
//
// A profile is a durable claim that a peer can be reached at this
// address. `0.0.0.0` / `[::]` / an empty host is what a listener BINDS
// to, never what a peer DIALS — a consumer that reads it fails to
// connect and, under §6.5.1a D1 selection, has no way to tell a wrong
// address from an unreachable peer. Fail at the publish site, where the
// operator can still fix it, rather than at every consumer.
//
// Loopback is deliberately allowed: `ws://127.0.0.1:9100/ws` is exactly
// right for a browser page and a Go peer on one machine, which is the
// first cross-implementation loop we expect to run.
func checkRoutableAdvertisement(dialURL string) error {
	raw := dialURL
	if !strings.Contains(raw, "://") {
		return NewError(400, "unsupported_scheme",
			fmt.Sprintf("AdvertiseTransport: %q has no scheme", dialURL))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return WrapError(400, "invalid_url", "AdvertiseTransport: parse "+dialURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return NewError(400, "unroutable_advertisement",
			fmt.Sprintf("AdvertiseTransport: %q names no host; a profile carries the address peers dial", dialURL))
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return NewError(400, "unroutable_advertisement",
			fmt.Sprintf("AdvertiseTransport: %q advertises the wildcard bind address; "+
				"publish the address a peer can dial (§6.5.1a D1), not the one the listener bound", dialURL))
	}
	// Port 0 is "let the kernel choose", which is a bind-time
	// instruction and never a dial target. It reaches here when a
	// caller derives the advertisement from the requested address
	// rather than the resolved one — and for the WebSocket listener
	// there is currently no resolved one to read (core-go's
	// Peer.Addr() is nil for a ws listener; only ListenReady sets
	// p.listener). Refusing keeps that gap visible instead of
	// publishing a profile pointing at port 0.
	if p := u.Port(); p == "0" {
		return NewError(400, "unroutable_advertisement",
			fmt.Sprintf("AdvertiseTransport: %q advertises port 0; that is a bind instruction, not a dial target", dialURL))
	}
	return nil
}
