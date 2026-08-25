package entitysdk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.entitychurch.org/entity-core-go/core/crypto"
)

// V7Identity is a named identity discovered under
// ~/.entity/identities/. May be either a V7-only flat keypair file
// or an identity-aware directory bundle (per
// SDK-OPERATIONS §15) — the Mode field disambiguates.
//
// Despite the historical name, this struct covers both modes; the
// type predates the bundle work and is kept for backward compat.
// New callers should discriminate via Mode and load the full bundle
// via LoadIdentityBundle when Mode == ModeIdentityAware.
type V7Identity struct {
	Name    string
	PeerID  string
	Keypair crypto.Keypair // controller keypair for identity-aware mode
	Mode    string         // ModeV7Flat | ModeIdentityAware
}

// Identity-mode discriminators returned by ListIdentities.
const (
	ModeV7Flat        = "v7-flat"
	ModeIdentityAware = "identity-aware"
)

// ErrIdentityNotFound is returned by LoadIdentity when the named
// identity file does not exist under ~/.entity/identities/.
var ErrIdentityNotFound = errors.New("identity not found")

// ErrIdentityExists is returned by CreateIdentity when an identity
// with the given name already exists on disk. Callers handle this
// non-fatally (e.g. the shell's `identity create` reports it as a
// user-visible message rather than a hard error).
var ErrIdentityExists = errors.New("identity already exists")

// LoadIdentity loads a V7-only flat keypair from
// ~/.entity/identities/{name}. Returns ErrIdentityNotFound (wrapped
// in *Error 404) if the file is missing.
//
// Conformant with SDK-OPERATIONS §15.1 V7-only mode. Callers pass
// the resulting Keypair into PeerConfig.Keypair to create a peer
// bound to this identity:
//
//	id, err := entitysdk.LoadIdentity("alice")
//	if err != nil { ... }
//	ap, err := entitysdk.CreatePeer(entitysdk.PeerConfig{Keypair: &id.Keypair})
func LoadIdentity(name string) (V7Identity, error) {
	if name == "" {
		return V7Identity{}, NewError(400, "invalid_name", "identity name must not be empty")
	}
	kp, err := crypto.LoadIdentity(name)
	if err != nil {
		// crypto.LoadIdentity wraps "no such file" inside its own
		// error chain; map that to a typed not-found.
		if errors.Is(err, os.ErrNotExist) ||
			(err.Error() != "" && containsNotExistMessage(err.Error())) {
			return V7Identity{}, WrapError(404, "identity_not_found",
				fmt.Sprintf("no identity %q under ~/.entity/identities/", name),
				ErrIdentityNotFound)
		}
		return V7Identity{}, WrapError(500, "load_identity_failed",
			fmt.Sprintf("load identity %q", name), err)
	}
	return V7Identity{
		Name:    name,
		PeerID:  string(kp.PeerID()),
		Keypair: kp,
	}, nil
}

// CreateIdentity generates a fresh keypair and writes it to
// ~/.entity/identities/{name} as a V7-only flat keypair file. Returns
// ErrIdentityExists (wrapped 409) if an identity by that name already
// exists.
func CreateIdentity(name string) (V7Identity, error) {
	if name == "" {
		return V7Identity{}, NewError(400, "invalid_name", "identity name must not be empty")
	}
	dir, err := identitiesDir()
	if err != nil {
		return V7Identity{}, WrapError(500, "home_dir_failed",
			"resolve identities directory", err)
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return V7Identity{}, WrapError(409, "identity_exists",
			fmt.Sprintf("identity %q already exists", name), ErrIdentityExists)
	}
	kp, err := crypto.Generate()
	if err != nil {
		return V7Identity{}, WrapError(500, "keygen_failed",
			"generate keypair", err)
	}
	if err := crypto.SaveIdentity(name, kp); err != nil {
		return V7Identity{}, WrapError(500, "save_identity_failed",
			fmt.Sprintf("save identity %q", name), err)
	}
	return V7Identity{
		Name:    name,
		PeerID:  string(kp.PeerID()),
		Keypair: kp,
	}, nil
}

// ListIdentities returns every named identity under
// ~/.entity/identities/ — both V7-only flat keypairs and
// identity-aware directory bundles. Mode disambiguates the two.
//
// For V7-only entries, V7Identity.Keypair is populated. For
// identity-aware bundles, the controller keypair is loaded into
// V7Identity.Keypair (so the listing form carries enough to
// identify the peer); use LoadIdentityBundle for full access to
// quorum members + manifest fields.
//
// Returns an empty slice (not an error) when ~/.entity/identities/
// does not exist yet.
func ListIdentities() ([]V7Identity, error) {
	dir, err := identitiesDir()
	if err != nil {
		return nil, WrapError(500, "home_dir_failed", "resolve identities dir", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, WrapError(500, "list_identities_failed",
			"list "+dir, err)
	}
	out := make([]V7Identity, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		// Skip sidecar files (.pub, .json, etc.) on the V7 side.
		if !e.IsDir() && strings.Contains(name, ".") {
			continue
		}
		if e.IsDir() {
			// Identity-aware directory bundle. Try to read the
			// manifest + controller keypair; skip silently if either
			// fails (partial writes, malformed bundles).
			b, err := LoadIdentityBundle(name)
			if err != nil {
				continue
			}
			out = append(out, V7Identity{
				Name:    name,
				PeerID:  string(b.ControllerKeypair.PeerID()),
				Keypair: b.ControllerKeypair,
				Mode:    ModeIdentityAware,
			})
			continue
		}
		// V7 flat keypair file.
		kp, err := crypto.LoadIdentity(name)
		if err != nil {
			continue
		}
		out = append(out, V7Identity{
			Name:    name,
			PeerID:  string(kp.PeerID()),
			Keypair: kp,
			Mode:    ModeV7Flat,
		})
	}
	return out, nil
}

// IdentitiesDir returns the absolute path to ~/.entity/identities/.
// Useful for shell commands that want to message the directory back
// to users.
func IdentitiesDir() (string, error) { return identitiesDir() }

func identitiesDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".entity", "identities"), nil
}

// containsNotExistMessage matches the error strings core-go's crypto
// package produces around missing files. crypto.LoadIdentity wraps
// errors via fmt.Errorf without preserving os.IsNotExist semantics,
// so we string-match as a fallback. This is a workaround; once
// core-go preserves the underlying error via %w, the os.ErrNotExist
// check above suffices and this can be deleted.
func containsNotExistMessage(s string) bool {
	return contains(s, "no such file") ||
		contains(s, "does not exist") ||
		contains(s, "cannot find")
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
