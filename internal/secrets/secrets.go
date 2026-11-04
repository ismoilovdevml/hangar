// Package secrets encrypts the credentials Hangar keeps in its database:
// proxy repository upstream passwords, tokens and header values, passwords
// in remote and outbound proxy URLs, and S3 keys of blob stores.
//
// Values are sealed with envelope encryption. Every value gets its own random
// 256-bit data key; the value is encrypted with AES-256-GCM under that data
// key, and the data key is encrypted (wrapped) with AES-256-GCM under a key
// encryption key from the keyring (security.secret_key). The sealed form is a
// printable string:
//
//	hgs1.<key id>.<wrapped data key>.<ciphertext>
//
// where both binary parts are unpadded base64url of nonce || GCM output. The
// key id is derived from the key encryption key itself, so an operator never
// assigns ids: the keyring holds the primary key, which seals, and any number
// of previous keys, which only open. Rotating to a new primary key re-wraps
// each data key (Rewrap) without touching the ciphertext.
//
// The alphabet of a sealed value is [A-Za-z0-9._-], so it can stand in for a
// password inside a URL's user information unchanged.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Prefix starts every sealed value.
const Prefix = "hgs1."

// KeySize is the length of a key encryption key in bytes (AES-256).
const KeySize = 32

const (
	nonceSize   = 12
	dataKeySize = 32
	gcmTagSize  = 16
	keyIDLen    = 12 // hex characters
)

var (
	// ErrNoKey is returned when a secret must be sealed or opened but no
	// security.secret_key is configured. It wraps core.ErrNotImplemented, so
	// the API explains that the feature needs configuration (501).
	ErrNoKey = fmt.Errorf("storing secrets requires security.secret_key (HANGAR_SECRET_KEY): %w", core.ErrNotImplemented)
	// ErrUnknownKey is returned when a value was sealed with a key that is
	// not in the keyring (neither the primary nor a previous key).
	ErrUnknownKey = errors.New("secret was sealed with a key that is not configured (add it to security.previous_secret_keys)")
	// ErrMalformed is returned for a value that has the sealed prefix but
	// cannot be decrypted: truncated, altered, or not produced by Hangar.
	ErrMalformed = errors.New("sealed secret is malformed or was tampered with")
)

// IsSealed reports whether s has the sealed form (it may still fail to open).
func IsSealed(s string) bool { return strings.HasPrefix(s, Prefix) }

// KeyIDOf returns the id of the key that sealed s.
func KeyIDOf(s string) (string, bool) {
	if !IsSealed(s) {
		return "", false
	}
	id, _, ok := strings.Cut(s[len(Prefix):], ".")
	return id, ok && id != ""
}

// ParseKey decodes a key encryption key: 32 bytes as 64 hex characters or as
// standard or URL-safe base64 (padded or not), e.g. `openssl rand -base64 32`.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 2*KeySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("secret key must be %d random bytes, base64 or hex encoded (generate one with `openssl rand -base64 32`): %w",
		KeySize, core.ErrInvalid)
}

// KeyID is the id recorded in values sealed with key: a truncated SHA-256 of
// the key under a fixed label. It identifies the key without revealing it.
func KeyID(key []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte("hangar secret key id v1\x00"))
	_, _ = h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:keyIDLen]
}

type kek struct {
	id   string
	aead cipher.AEAD
}

// Keyring holds the key encryption keys. The zero value is not usable; a nil
// *Keyring is valid and means "no key configured": Seal fails with ErrNoKey,
// Open fails with ErrNoKey for sealed values, and the Reveal helpers pass
// legacy plaintext through.
type Keyring struct {
	primary *kek
	keys    map[string]*kek
}

// NewKeyring builds a keyring from the primary key and any previous keys (see
// ParseKey for the encoding). An empty primary returns nil, nil: no keyring.
// Previous keys equal to the primary are ignored.
func NewKeyring(primary string, previous []string) (*Keyring, error) {
	if strings.TrimSpace(primary) == "" {
		if len(previous) > 0 {
			return nil, fmt.Errorf("security.previous_secret_keys is set without security.secret_key: %w", core.ErrInvalid)
		}
		return nil, nil
	}
	k := &Keyring{keys: map[string]*kek{}}
	p, err := newKEK(primary)
	if err != nil {
		return nil, fmt.Errorf("security.secret_key: %w", err)
	}
	k.primary = p
	k.keys[p.id] = p
	for i, s := range previous {
		if strings.TrimSpace(s) == "" {
			continue
		}
		o, err := newKEK(s)
		if err != nil {
			return nil, fmt.Errorf("security.previous_secret_keys[%d]: %w", i, err)
		}
		if _, dup := k.keys[o.id]; !dup {
			k.keys[o.id] = o
		}
	}
	return k, nil
}

func newKEK(s string) (*kek, error) {
	key, err := ParseKey(s)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &kek{id: KeyID(key), aead: aead}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// PrimaryID is the id of the key that seals new values ("" for a nil keyring).
func (k *Keyring) PrimaryID() string {
	if k == nil {
		return ""
	}
	return k.primary.id
}

// KeyIDs lists the ids of every key in the keyring, primary first.
func (k *Keyring) KeyIDs() []string {
	if k == nil {
		return nil
	}
	out := []string{k.primary.id}
	for id := range k.keys {
		if id != k.primary.id {
			out = append(out, id)
		}
	}
	return out
}

// Seal encrypts plaintext under a fresh data key wrapped with the primary key.
func (k *Keyring) Seal(plaintext []byte) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	dek := make([]byte, dataKeySize)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("seal secret: %w", err)
	}
	wrapped, err := encrypt(k.primary.aead, dek, wrapAAD(k.primary.id))
	if err != nil {
		return "", err
	}
	aead, err := newGCM(dek)
	if err != nil {
		return "", err
	}
	ct, err := encrypt(aead, plaintext, []byte(Prefix))
	if err != nil {
		return "", err
	}
	return Prefix + k.primary.id + "." + b64(wrapped) + "." + b64(ct), nil
}

// SealString is Seal for a string value.
func (k *Keyring) SealString(s string) (string, error) { return k.Seal([]byte(s)) }

// Open decrypts a sealed value.
func (k *Keyring) Open(sealed string) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	_, dek, ct, err := k.unwrap(sealed)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	pt, err := decrypt(aead, ct, []byte(Prefix))
	if err != nil {
		return nil, ErrMalformed
	}
	return pt, nil
}

// OpenString is Open returning a string.
func (k *Keyring) OpenString(sealed string) (string, error) {
	b, err := k.Open(sealed)
	return string(b), err
}

// Reveal returns the plaintext of s: s itself when it is not sealed (a value
// stored before encryption was configured), otherwise the opened value.
func (k *Keyring) Reveal(s string) (string, error) {
	if !IsSealed(s) {
		return s, nil
	}
	return k.OpenString(s)
}

// Check verifies that a sealed value opens with this keyring.
func (k *Keyring) Check(sealed string) error {
	_, err := k.Open(sealed)
	return err
}

// NeedsRewrap reports whether sealed was sealed with a key other than the primary.
func (k *Keyring) NeedsRewrap(sealed string) bool {
	id, ok := KeyIDOf(sealed)
	return ok && k != nil && id != k.primary.id
}

// Rewrap re-encrypts the data key of sealed under the primary key, leaving
// the ciphertext as it is. A value already under the primary key is returned
// unchanged.
func (k *Keyring) Rewrap(sealed string) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	id, dek, ct, err := k.unwrap(sealed)
	if err != nil {
		return "", err
	}
	if id == k.primary.id {
		return sealed, nil
	}
	// Prove the ciphertext opens before committing to the new envelope.
	aead, err := newGCM(dek)
	if err != nil {
		return "", err
	}
	if _, err := decrypt(aead, ct, []byte(Prefix)); err != nil {
		return "", ErrMalformed
	}
	wrapped, err := encrypt(k.primary.aead, dek, wrapAAD(k.primary.id))
	if err != nil {
		return "", err
	}
	return Prefix + k.primary.id + "." + b64(wrapped) + "." + b64(ct), nil
}

// unwrap parses sealed and returns its key id, data key and ciphertext.
func (k *Keyring) unwrap(sealed string) (string, []byte, []byte, error) {
	if !IsSealed(sealed) {
		return "", nil, nil, ErrMalformed
	}
	parts := strings.Split(sealed[len(Prefix):], ".")
	if len(parts) != 3 || parts[0] == "" {
		return "", nil, nil, ErrMalformed
	}
	id := parts[0]
	kk, ok := k.keys[id]
	if !ok {
		return id, nil, nil, fmt.Errorf("key %s: %w", id, ErrUnknownKey)
	}
	wrapped, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	ct, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(wrapped) != nonceSize+dataKeySize+gcmTagSize || len(ct) < nonceSize+gcmTagSize {
		return id, nil, nil, ErrMalformed
	}
	dek, err := decrypt(kk.aead, wrapped, wrapAAD(id))
	if err != nil {
		return id, nil, nil, ErrMalformed
	}
	return id, dek, ct, nil
}

// wrapAAD binds a wrapped data key to the format version and key id.
func wrapAAD(id string) []byte { return []byte(Prefix + id) }

func encrypt(aead cipher.AEAD, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize, nonceSize+len(plaintext)+gcmTagSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal secret: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func decrypt(aead cipher.AEAD, data, aad []byte) ([]byte, error) {
	if len(data) < nonceSize+gcmTagSize {
		return nil, ErrMalformed
	}
	return aead.Open(nil, data[:nonceSize], data[nonceSize:], aad)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
