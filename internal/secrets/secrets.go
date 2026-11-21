// Package secrets encrypts the credentials Hangar keeps in its database:
// proxy repository upstream passwords, tokens and header values, passwords
// in remote and outbound proxy URLs, S3 keys of blob stores, the sign-in
// secrets of the settings, the private keys that sign repository metadata,
// and the source credentials of import tasks.
//
// Values are sealed with envelope encryption. Every value gets its own random
// 256-bit data key; the value is encrypted with AES-256-GCM under that data
// key, and the data key is encrypted (wrapped) with AES-256-GCM under a key
// encryption key from the keyring (security.secret_key). The sealed form is a
// printable string:
//
//	hgs2.<key id>.<wrapped data key>.<ciphertext>
//
// where both binary parts are unpadded base64url of nonce || GCM output. The
// key id is derived from the key encryption key itself, so an operator never
// assigns ids: the keyring holds the primary key, which seals, and any number
// of previous keys, which only open. Rotating to a new primary key re-wraps
// each data key (Rewrap) without touching the ciphertext.
//
// # Encryption context
//
// An hgs2 ciphertext is bound to the place it is stored: its GCM additional
// data is the Context (table, row key, field) of the value, encoded as
//
//	"hgs2" || be32(len(table)) || table || be32(len(row)) || row || be32(len(field)) || field
//
// and the wrapped data key's additional data is "hgs2." || key id. The
// context is not part of the sealed string: the code that seals and the code
// that opens both derive it from where the value lives, so a value copied
// into another row or field (from a backup, another column, or another
// installation with the same key) fails to open there with ErrMalformed
// instead of being decrypted and sent to whatever host that row names.
//
// The row key is the row's identity, never something that can change while
// the value is stored: the id of repositories, blob_stores and tasks, the key
// of settings, and the name (primary key) of outbound_proxies and
// signing_keys. The constructors RepositoryContext, BlobStoreContext,
// SettingContext, OutboundProxyContext, SigningKeyContext and TaskContext
// name every stored secret.
//
// # Legacy values
//
// Hangar before v0.2.0 sealed values in the hgs1 format: the same envelope
// with "hgs1." in place of "hgs2." and the fixed additional data "hgs1." for
// every ciphertext, so an hgs1 value opens wherever it is stored. hgs1 values
// still open (the context must be complete but is not bound), nothing new is
// sealed in that format, Rewrap turns them into hgs2 values, and Sweep
// converts every stored one on the first start of v0.2.0. Only an explicit
// downgrade (SweepOptions.Downgrade) writes hgs1 again.
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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Prefix starts every value sealed by this version (the hgs2 format).
const Prefix = "hgs2."

// LegacyPrefix starts values sealed before v0.2.0 (the hgs1 format, not bound
// to a context). They still open; nothing new is sealed in this format except
// by an explicit downgrade.
const LegacyPrefix = "hgs1."

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
	// cannot be decrypted where it is: truncated, altered, not produced by
	// Hangar, or sealed for another row or field (see Context).
	ErrMalformed = errors.New("sealed secret is malformed, was tampered with, or was sealed for another row or field")
	// ErrNoContext is returned when a value is sealed or opened with an
	// incomplete Context: a programming error, never a property of the data.
	ErrNoContext = errors.New("secrets: incomplete encryption context (table, row and field are required)")
)

// Context names the place a sealed value is stored: its table, the key of its
// row, and the field (column or JSON path) within the row. An hgs2 value
// opens only with the Context it was sealed with. Every part must be set.
type Context struct {
	// Table is the database table, e.g. "repositories".
	Table string
	// Row is the row's identity: a key that cannot change while the value
	// is stored (an id, a settings key, a primary-key name).
	Row string
	// Field names the value within the row, e.g. "proxy.auth.password".
	Field string
}

// String renders c as table/row/field, for messages.
func (c Context) String() string { return c.Table + "/" + c.Row + "/" + c.Field }

func (c Context) complete() bool { return c.Table != "" && c.Row != "" && c.Field != "" }

// aad is the additional data of an hgs2 ciphertext. Length prefixes keep the
// encoding injective whatever the parts contain, so no two contexts share it.
func (c Context) aad() []byte {
	b := make([]byte, 0, 4+12+len(c.Table)+len(c.Row)+len(c.Field))
	b = append(b, "hgs2"...)
	for _, p := range []string{c.Table, c.Row, c.Field} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(p))) //nolint:gosec // parts are database keys, far below 4 GiB
		b = append(b, p...)
	}
	return b
}

// RepositoryContext is the context of a secret in repositories.config: row is
// the repository id, field the JSON path ("proxy.auth.password",
// "proxy.auth.token", and for the password in a URL's user information
// "proxy.remote_url" or "proxy.http.proxy").
func RepositoryContext(id, field string) Context {
	return Context{Table: "repositories", Row: id, Field: field}
}

// BlobStoreContext is the context of a secret in blob_stores.config: row is
// the blob store id, field the sealed field ("secret_access_key_sealed",
// "session_token_sealed").
func BlobStoreContext(id, field string) Context {
	return Context{Table: "blob_stores", Row: id, Field: field}
}

// SettingContext is the context of a secret in a settings document: row is
// the settings key ("auth.oidc", "auth.ldap"), field the JSON field
// ("client_secret", "bind_password").
func SettingContext(key, field string) Context {
	return Context{Table: "settings", Row: key, Field: field}
}

// OutboundProxyContext is the context of the password of the named outbound
// proxy (outbound_proxies.password).
func OutboundProxyContext(name string) Context {
	return Context{Table: "outbound_proxies", Row: name, Field: "password"}
}

// SigningKeyContext is the context of the private key of the named
// repository signing key (signing_keys.private_key).
func SigningKeyContext(name string) Context {
	return Context{Table: "signing_keys", Row: name, Field: "private_key"}
}

// TaskContext is the context of a source credential in tasks.config: row is
// the task id, field the JSON path ("source.password", "source.token",
// "inventory.source.password", "inventory.source.token").
func TaskContext(id, field string) Context {
	return Context{Table: "tasks", Row: id, Field: field}
}

// WebhookContext is the context of the signing secret of a webhook
// (webhooks.secret); row is the webhook id.
func WebhookContext(id string) Context {
	return Context{Table: "webhooks", Row: id, Field: "secret"}
}

// IsSealed reports whether s has a sealed form, current or legacy (it may
// still fail to open).
func IsSealed(s string) bool { return strings.HasPrefix(s, Prefix) || IsLegacy(s) }

// IsLegacy reports whether s is sealed in the legacy hgs1 format.
func IsLegacy(s string) bool { return strings.HasPrefix(s, LegacyPrefix) }

// ErrSealedInput is returned for a secret submitted in sealed form. A
// ciphertext copied from a backup or another row and submitted as "my
// password" must never be opened on the submitter's behalf: a legacy value
// opens wherever it is stored, and any sealed value carried back into the
// field it came from would be opened and sent, in plaintext, to whatever
// host the submitter chose. A stored secret is kept by leaving the field out
// (or empty), never by sending its ciphertext back. It wraps core.ErrInvalid
// (400).
var ErrSealedInput = fmt.Errorf("a sealed value (%s... or %s...) cannot be submitted as a secret; enter the secret itself, "+
	"or leave the field empty to keep the stored one: %w", Prefix, LegacyPrefix, core.ErrInvalid)

// CheckInput refuses a secret submitted through the API in sealed form (see
// ErrSealedInput). field names the input for the message.
func CheckInput(field, v string) error {
	if IsSealed(strings.TrimSpace(v)) {
		return fmt.Errorf("%s: %w", field, ErrSealedInput)
	}
	return nil
}

// CheckURLInput is CheckInput for the password in a URL's user information.
func CheckURLInput(field, raw string) error {
	_, err := mapURLPassword(raw, func(pw string) (string, error) { return pw, CheckInput(field+" password", pw) })
	return err
}

// KeyIDOf returns the id of the key that sealed s.
func KeyIDOf(s string) (string, bool) {
	if !IsSealed(s) {
		return "", false
	}
	// Both prefixes have the same length.
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
// Open fails with ErrNoKey for sealed values, and Reveal passes legacy
// plaintext through.
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

// Seal encrypts plaintext for the place c under a fresh data key wrapped with
// the primary key, in the hgs2 format.
func (k *Keyring) Seal(c Context, plaintext []byte) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	if !c.complete() {
		return "", ErrNoContext
	}
	return k.seal(Prefix, c.aad(), plaintext)
}

// seal writes an envelope of the format prefix whose ciphertext carries the
// additional data aad.
func (k *Keyring) seal(prefix string, aad, plaintext []byte) (string, error) {
	dek := make([]byte, dataKeySize)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("seal secret: %w", err)
	}
	aead, err := newGCM(dek)
	if err != nil {
		return "", err
	}
	ct, err := encrypt(aead, plaintext, aad)
	if err != nil {
		return "", err
	}
	return k.wrap(prefix, dek, ct)
}

// wrap encrypts dek under the primary key and assembles the envelope.
func (k *Keyring) wrap(prefix string, dek, ct []byte) (string, error) {
	wrapped, err := encrypt(k.primary.aead, dek, wrapAAD(prefix, k.primary.id))
	if err != nil {
		return "", err
	}
	return prefix + k.primary.id + "." + b64(wrapped) + "." + b64(ct), nil
}

// SealString is Seal for a string value.
func (k *Keyring) SealString(c Context, s string) (string, error) { return k.Seal(c, []byte(s)) }

// Open decrypts a value stored at c. An hgs2 value sealed for any other
// place fails with ErrMalformed; a legacy hgs1 value opens wherever it is.
func (k *Keyring) Open(c Context, sealed string) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	if !c.complete() {
		return nil, ErrNoContext
	}
	e, dek, err := k.unwrap(sealed)
	if err != nil {
		return nil, err
	}
	return e.open(dek, c)
}

// OpenString is Open returning a string.
func (k *Keyring) OpenString(c Context, sealed string) (string, error) {
	b, err := k.Open(c, sealed)
	return string(b), err
}

// Reveal returns the plaintext of s stored at c: s itself when it is not
// sealed (a value stored before encryption was configured), otherwise the
// opened value.
func (k *Keyring) Reveal(c Context, s string) (string, error) {
	if !IsSealed(s) {
		return s, nil
	}
	return k.OpenString(c, s)
}

// Check verifies that a sealed value opens at c with this keyring.
func (k *Keyring) Check(c Context, sealed string) error {
	_, err := k.Open(c, sealed)
	return err
}

// NeedsRewrap reports whether sealed is not in the current form: sealed in
// the legacy format, or with a key other than the primary.
func (k *Keyring) NeedsRewrap(sealed string) bool {
	id, ok := KeyIDOf(sealed)
	return ok && k != nil && (IsLegacy(sealed) || id != k.primary.id)
}

// Rewrap brings a value stored at c to the current form: hgs2 under the
// primary key. The value must open at c. An hgs2 value under a previous key
// keeps its ciphertext and gets its data key re-wrapped; a legacy value is
// sealed again for c under a fresh data key. A current value is returned
// unchanged.
func (k *Keyring) Rewrap(c Context, sealed string) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	if !c.complete() {
		return "", ErrNoContext
	}
	e, dek, err := k.unwrap(sealed)
	if err != nil {
		return "", err
	}
	// Prove the ciphertext opens here before committing to a new envelope.
	pt, err := e.open(dek, c)
	if err != nil {
		return "", err
	}
	switch {
	case e.legacy:
		return k.seal(Prefix, c.aad(), pt)
	case e.id == k.primary.id:
		return sealed, nil
	default:
		return k.wrap(Prefix, dek, e.ct)
	}
}

// Upgrade checks that a value stored at c opens there and returns it fit to
// be stored again: a legacy hgs1 value is sealed again as hgs2 for c, a
// current-format value is returned unchanged (moving values to a new primary
// key is Rewrap's job, run by Sweep), so a secret that is only kept on an
// update stays byte-identical.
func (k *Keyring) Upgrade(c Context, sealed string) (string, error) {
	if IsLegacy(sealed) {
		return k.Rewrap(c, sealed)
	}
	if err := k.Check(c, sealed); err != nil {
		return "", err
	}
	return sealed, nil
}

// Downgrade turns a value stored at c into the legacy hgs1 format under the
// primary key, which Hangar before v0.2.0 reads. It gives up the binding to
// c and exists only to downgrade a database (SweepOptions.Downgrade).
func (k *Keyring) Downgrade(c Context, sealed string) (string, error) {
	if k == nil {
		return "", ErrNoKey
	}
	pt, err := k.Open(c, sealed)
	if err != nil {
		return "", err
	}
	if id, _ := KeyIDOf(sealed); IsLegacy(sealed) && id == k.primary.id {
		return sealed, nil
	}
	return k.sealLegacy(pt)
}

// sealLegacy seals plaintext in the hgs1 format (downgrades only).
func (k *Keyring) sealLegacy(plaintext []byte) (string, error) {
	return k.seal(LegacyPrefix, []byte(LegacyPrefix), plaintext)
}

// envelope is a parsed sealed value.
type envelope struct {
	legacy bool
	id     string
	ct     []byte
}

// open decrypts the ciphertext of e with its data key for the place c.
func (e envelope) open(dek []byte, c Context) ([]byte, error) {
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	aad := c.aad()
	if e.legacy {
		aad = []byte(LegacyPrefix)
	}
	pt, err := decrypt(aead, e.ct, aad)
	if err != nil {
		return nil, ErrMalformed
	}
	return pt, nil
}

// unwrap parses sealed and returns it with its data key.
func (k *Keyring) unwrap(sealed string) (envelope, []byte, error) {
	var e envelope
	prefix := Prefix
	switch {
	case IsLegacy(sealed):
		e.legacy, prefix = true, LegacyPrefix
	case !strings.HasPrefix(sealed, Prefix):
		return e, nil, ErrMalformed
	}
	parts := strings.Split(sealed[len(prefix):], ".")
	if len(parts) != 3 || parts[0] == "" {
		return e, nil, ErrMalformed
	}
	e.id = parts[0]
	kk, ok := k.keys[e.id]
	if !ok {
		return e, nil, fmt.Errorf("key %s: %w", e.id, ErrUnknownKey)
	}
	wrapped, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	ct, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(wrapped) != nonceSize+dataKeySize+gcmTagSize || len(ct) < nonceSize+gcmTagSize {
		return e, nil, ErrMalformed
	}
	dek, err := decrypt(kk.aead, wrapped, wrapAAD(prefix, e.id))
	if err != nil {
		return e, nil, ErrMalformed
	}
	e.ct = ct
	return e, dek, nil
}

// wrapAAD binds a wrapped data key to the format version and key id, so a
// data key wrapped for one format cannot be spliced into the other.
func wrapAAD(prefix, id string) []byte { return []byte(prefix + id) }

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
