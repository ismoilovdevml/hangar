package repokit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// A group serves its own signature over everything it lists, so clients
// trust a proxy member's packages because the group vouches for them. The
// group only does that for remote metadata whose own signature it checked:
// a proxy repository names the remote's public keys in format.remote_keys,
// and a proxy member without them is refused (merging it would let anyone
// on the path to a plain-http mirror hand out packages under the group's
// key).

// RemoteKeysSetting is the format setting (config.format.remote_keys) of a
// proxy repository: the remote's ASCII-armored OpenPGP public keys.
const RemoteKeysSetting = "remote_keys"

// RemoteKeys holds the parsed remote keys of a proxy and their fingerprint
// (for cache keys: a key change re-verifies).
type RemoteKeys struct {
	Keys openpgp.EntityList
	ID   string
}

// ParseRemoteKeys parses the remote_keys setting, nil when it is empty.
func ParseRemoteKeys(armored []string) (*RemoteKeys, error) {
	if len(armored) == 0 {
		return nil, nil
	}
	var out RemoteKeys
	h := sha256.New()
	for i, a := range armored {
		el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(a))
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: not an ASCII-armored OpenPGP public key: %w: %w", RemoteKeysSetting, i, err, core.ErrInvalid)
		}
		if len(el) == 0 {
			return nil, fmt.Errorf("%s[%d]: no key in the block: %w", RemoteKeysSetting, i, core.ErrInvalid)
		}
		for _, e := range el {
			// The setting is stored and returned as plain configuration.
			if e.PrivateKey != nil {
				return nil, fmt.Errorf("%s[%d] holds a private key; give the public key only: %w", RemoteKeysSetting, i, core.ErrInvalid)
			}
		}
		out.Keys = append(out.Keys, el...)
		_, _ = h.Write([]byte(a))
		_, _ = h.Write([]byte{0})
	}
	out.ID = hex.EncodeToString(h.Sum(nil))
	return &out, nil
}

// VerifyClearsigned verifies a cleartext-signed document (an APT InRelease)
// with k and returns the signed text. Anything but one signed block
// verifying with k fails with core.ErrUpstream.
func (k *RemoteKeys) VerifyClearsigned(doc []byte, what string) ([]byte, error) {
	b, rest := clearsign.Decode(doc)
	if b == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%s is not one cleartext-signed document: %w", what, core.ErrUpstream)
	}
	if _, err := b.VerifySignature(k.Keys, nil); err != nil {
		return nil, fmt.Errorf("%s: the signature does not verify with %s: %w: %w", what, RemoteKeysSetting, err, core.ErrUpstream)
	}
	return b.Plaintext, nil
}

// VerifyDetached verifies doc against a detached signature (armored or
// binary) with k, failing with core.ErrUpstream.
func (k *RemoteKeys) VerifyDetached(doc, sig []byte, what string) error {
	var err error
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN")) {
		_, err = openpgp.CheckArmoredDetachedSignature(k.Keys, bytes.NewReader(doc), bytes.NewReader(sig), nil)
	} else {
		_, err = openpgp.CheckDetachedSignature(k.Keys, bytes.NewReader(doc), bytes.NewReader(sig), nil)
	}
	if err != nil {
		return fmt.Errorf("%s: the signature does not verify with %s: %w: %w", what, RemoteKeysSetting, err, core.ErrUpstream)
	}
	return nil
}

// MissingRemoteKeys is the error (core.ErrInvalid) of a group that would
// merge a proxy member whose remote keys are not configured.
func MissingRemoteKeys(format string, group, member *core.Repository) error {
	return fmt.Errorf("%s group %q: proxy member %q has no format.%s; the group signs what it lists with its own key, "+
		"so it merges a remote's metadata only after verifying the remote's signature: set %s on %q to the remote's "+
		"public keys (ASCII-armored): %w", format, group.Name, member.Name, RemoteKeysSetting, RemoteKeysSetting, member.Name, core.ErrInvalid)
}
