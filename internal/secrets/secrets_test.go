package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func mustKeyring(t *testing.T, primary string, previous ...string) *Keyring {
	t.Helper()
	k, err := NewKeyring(primary, previous)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// here is the context most tests seal for.
var here = RepositoryContext("6f1c0a52-8f7e-4d1e-9a51-0c2b9d3e4f10", "proxy.auth.password")

// goldenKey and goldenLegacy are a key and a value sealed with it by Hangar
// v0.1.9 (internal/secrets at 9ca4207, SealString("legacy-secret")): the
// hgs1 format as databases hold it.
const (
	goldenKey    = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	goldenLegacy = "hgs1.f6071d8aba5d.ji5r9tgsv8ssNEImwRAVVS0EJARM9dJ_0WeHijSMuDD7ef4fnizyWnhqak51IitBiGVFvjj2n34kBPw9." +
		"neI11ZcKnTD1YEu5OpOgo925wZ44G7aKVKc8UFqKJERCBjhj4gfjSPI"
)

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, KeySize)
	for name, s := range map[string]string{
		"hex":        hex.EncodeToString(raw),
		"std":        base64.StdEncoding.EncodeToString(raw),
		"raw std":    base64.RawStdEncoding.EncodeToString(raw),
		"url":        base64.URLEncoding.EncodeToString(raw),
		"whitespace": " " + base64.StdEncoding.EncodeToString(raw) + "\n",
	} {
		got, err := ParseKey(s)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s: %x %v", name, got, err)
		}
	}
	for _, bad := range []string{"", "short", "correct horse battery staple", base64.StdEncoding.EncodeToString(raw[:16]), hex.EncodeToString(raw[:31])} {
		if _, err := ParseKey(bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("ParseKey(%q) = %v", bad, err)
		}
	}
}

func TestSealOpen(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	s, err := k.SealString(here, "p@ss:w/ord")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(s) || IsLegacy(s) || !regexp.MustCompile(`^hgs2\.[0-9a-f]{12}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`).MatchString(s) {
		t.Fatalf("sealed form %q", s)
	}
	if id, _ := KeyIDOf(s); id != k.PrimaryID() {
		t.Errorf("key id %s, primary %s", id, k.PrimaryID())
	}
	if got, err := k.OpenString(here, s); err != nil || got != "p@ss:w/ord" {
		t.Errorf("open = %q %v", got, err)
	}
	// Every seal uses a fresh data key and nonce.
	if s2, _ := k.SealString(here, "p@ss:w/ord"); s2 == s {
		t.Error("sealing is deterministic")
	}
	if strings.Contains(s, "p@ss") {
		t.Error("plaintext visible")
	}
	// Empty values seal and open too.
	if e, err := k.SealString(here, ""); err != nil {
		t.Fatal(err)
	} else if got, err := k.OpenString(here, e); err != nil || got != "" {
		t.Errorf("empty = %q %v", got, err)
	}
}

// An hgs2 value opens only at the place it was sealed for: another table,
// row or field (or a context that differs only in where the parts split)
// fails as malformed, and an incomplete context is a programming error.
func TestContextBinding(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	tests := []struct {
		name string
		at   Context
		err  error
	}{
		{"same place", here, nil},
		{"other row", RepositoryContext("0b8e5a77-1111-4222-8333-944455556666", here.Field), ErrMalformed},
		{"other field", RepositoryContext(here.Row, "proxy.auth.token"), ErrMalformed},
		{"other table", TaskContext(here.Row, here.Field), ErrMalformed},
		{"parts shifted", Context{Table: "repositories" + here.Row[:1], Row: here.Row[1:], Field: here.Field}, ErrMalformed},
		{"no row", Context{Table: "repositories", Field: here.Field}, ErrNoContext},
		{"no field", Context{Table: "repositories", Row: here.Row}, ErrNoContext},
		{"zero", Context{}, ErrNoContext},
	}
	s, err := k.SealString(here, "bound")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		got, err := k.OpenString(tc.at, s)
		if !errors.Is(err, tc.err) || (tc.err == nil && got != "bound") {
			t.Errorf("%s: open = %q, %v; want err %v", tc.name, got, err, tc.err)
		}
		if tc.err == nil {
			continue
		}
		if err := k.Check(tc.at, s); !errors.Is(err, tc.err) {
			t.Errorf("%s: check = %v", tc.name, err)
		}
		if _, err := k.Rewrap(tc.at, s); !errors.Is(err, tc.err) {
			t.Errorf("%s: rewrap = %v", tc.name, err)
		}
		if _, err := k.Upgrade(tc.at, s); !errors.Is(err, tc.err) {
			t.Errorf("%s: upgrade = %v", tc.name, err)
		}
	}
	if _, err := k.SealString(Context{Table: "repositories", Row: here.Row}, "x"); !errors.Is(err, ErrNoContext) {
		t.Errorf("seal without a field: %v", err)
	}
}

// Values sealed by Hangar before v0.2.0 still open (wherever they are, as
// they always did), are recognised as legacy, and Rewrap and Upgrade turn
// them into hgs2 values bound to their place.
func TestLegacy(t *testing.T) {
	k := mustKeyring(t, goldenKey)
	if !IsSealed(goldenLegacy) || !IsLegacy(goldenLegacy) || !k.NeedsRewrap(goldenLegacy) {
		t.Fatal("golden value not recognised as a legacy sealed value")
	}
	for _, at := range []Context{here, SettingContext("auth.ldap", "bind_password")} {
		if got, err := k.OpenString(at, goldenLegacy); err != nil || got != "legacy-secret" {
			t.Fatalf("open legacy at %s = %q %v", at, got, err)
		}
	}
	if _, err := k.OpenString(Context{}, goldenLegacy); !errors.Is(err, ErrNoContext) {
		t.Errorf("legacy open without a context: %v", err)
	}
	for name, convert := range map[string]func(Context, string) (string, error){"rewrap": k.Rewrap, "upgrade": k.Upgrade} {
		up, err := convert(here, goldenLegacy)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(up, Prefix) || k.NeedsRewrap(up) {
			t.Errorf("%s: %q is not current", name, up)
		}
		if got, err := k.OpenString(here, up); err != nil || got != "legacy-secret" {
			t.Errorf("%s: open upgraded = %q %v", name, got, err)
		}
		if _, err := k.OpenString(SettingContext("auth.ldap", "bind_password"), up); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: upgraded value opens elsewhere: %v", name, err)
		}
	}
	// A legacy value whose prefix is swapped for the current one does not
	// open: the wrapped data key is bound to the format.
	if _, err := k.Open(here, Prefix+goldenLegacy[len(LegacyPrefix):]); !errors.Is(err, ErrMalformed) {
		t.Errorf("relabelled legacy value: %v", err)
	}
	// Downgrade goes back to hgs1 for older servers.
	cur, _ := k.SealString(here, "downgrade-me")
	down, err := k.Downgrade(here, cur)
	if err != nil || !IsLegacy(down) {
		t.Fatalf("downgrade = %q %v", down, err)
	}
	if got, err := k.OpenString(TaskContext("any", "source.token"), down); err != nil || got != "downgrade-me" {
		t.Errorf("open downgraded = %q %v", got, err)
	}
	if again, err := k.Downgrade(here, down); err != nil || again != down {
		t.Errorf("downgrade of an hgs1 value under the primary changed it: %v", err)
	}
	if _, err := k.Downgrade(RepositoryContext("other", here.Field), cur); !errors.Is(err, ErrMalformed) {
		t.Errorf("downgrade at the wrong place: %v", err)
	}
}

func TestTamperAndKeys(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	s, _ := k.SealString(here, "secret")
	parts := strings.Split(s, ".")
	flip := func(p string) string {
		b := []byte(p)
		if b[5] == 'A' {
			b[5] = 'B'
		} else {
			b[5] = 'A'
		}
		return string(b)
	}
	for name, bad := range map[string]string{
		"wrapped key": strings.Join([]string{parts[0], parts[1], flip(parts[2]), parts[3]}, "."),
		"ciphertext":  strings.Join([]string{parts[0], parts[1], parts[2], flip(parts[3])}, "."),
		"truncated":   s[:len(s)-4],
		"parts":       "hgs2." + parts[1] + "." + parts[2],
		"not base64":  strings.Join([]string{parts[0], parts[1], "!!!", parts[3]}, "."),
		"as legacy":   LegacyPrefix + s[len(Prefix):],
		"no prefix":   strings.Join(parts[1:], "."),
	} {
		if _, err := k.Open(here, bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A key id swapped in for another key does not open.
	other := mustKeyring(t, newKey(t))
	if _, err := other.Open(here, s); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("foreign key: %v", err)
	}
	// Nil keyring: no key configured.
	var none *Keyring
	if _, err := none.Seal(here, []byte("x")); !errors.Is(err, ErrNoKey) || !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("nil seal: %v", err)
	}
	if v, err := none.Reveal(here, "legacy plaintext"); err != nil || v != "legacy plaintext" {
		t.Errorf("nil reveal plaintext: %q %v", v, err)
	}
	if _, err := none.Reveal(here, s); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil reveal sealed: %v", err)
	}
	if _, err := none.Rewrap(here, s); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil rewrap: %v", err)
	}
	if k, err := NewKeyring("", nil); k != nil || err != nil {
		t.Errorf("empty keyring = %v %v", k, err)
	}
	if _, err := NewKeyring("", []string{newKey(t)}); err == nil {
		t.Error("previous keys without a primary accepted")
	}
	if _, err := NewKeyring("not a key", nil); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad primary: %v", err)
	}
}

func TestRotation(t *testing.T) {
	oldKey, newK := newKey(t), newKey(t)
	old := mustKeyring(t, oldKey)
	s, _ := old.SealString(here, "token-1")
	legacy, _ := old.sealLegacy([]byte("token-0"))

	rotated := mustKeyring(t, newK, oldKey)
	if got, err := rotated.OpenString(here, s); err != nil || got != "token-1" {
		t.Fatalf("previous key does not open: %q %v", got, err)
	}
	if !rotated.NeedsRewrap(s) || !rotated.NeedsRewrap(legacy) {
		t.Error("NeedsRewrap = false for a previous key")
	}
	// Upgrade leaves hgs2 values under a previous key to Rewrap.
	if kept, err := rotated.Upgrade(here, s); err != nil || kept != s {
		t.Errorf("upgrade of a current-format value changed it: %v", err)
	}
	r, err := rotated.Rewrap(here, s)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.NeedsRewrap(r) {
		t.Error("rewrapped value still under the previous key")
	}
	// The ciphertext is kept, only the envelope changes.
	if strings.Split(r, ".")[3] != strings.Split(s, ".")[3] {
		t.Error("ciphertext changed on rewrap")
	}
	if again, _ := rotated.Rewrap(here, r); again != r {
		t.Error("rewrap of a current value changed it")
	}
	// A legacy value under the previous key comes out as hgs2 under the new one.
	rl, err := rotated.Rewrap(here, legacy)
	if err != nil || IsLegacy(rl) || rotated.NeedsRewrap(rl) {
		t.Fatalf("rewrap legacy = %q %v", rl, err)
	}
	onlyNew := mustKeyring(t, newK)
	for want, v := range map[string]string{"token-1": r, "token-0": rl} {
		if got, err := onlyNew.OpenString(here, v); err != nil || got != want {
			t.Errorf("after removing the old key: %q %v", got, err)
		}
	}
	if _, err := onlyNew.Open(here, s); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("old value after removing the old key: %v", err)
	}
	if len(rotated.KeyIDs()) != 2 || rotated.KeyIDs()[0] != rotated.PrimaryID() {
		t.Errorf("key ids = %v", rotated.KeyIDs())
	}
}

func TestRepoConfig(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	const repoID, otherID = "1d6a3c1e-0000-4000-8000-00000000000a", "1d6a3c1e-0000-4000-8000-00000000000b"
	cfg := core.RepoConfig{Proxy: &core.ProxyConfig{
		RemoteURL: "https://u:remote%2Fpw@repo.example/maven2/",
		Auth:      &core.UpstreamAuth{Kind: core.UpstreamAuthHeader, HeaderName: "Private-Token", Token: "tok"},
		HTTP:      core.UpstreamHTTP{Proxy: "socks5://p:proxy-pw@socks.corp:1080"},
	}}
	if err := k.SealRepoConfig(&cfg, repoID); err != nil {
		t.Fatal(err)
	}
	p := cfg.Proxy
	if !IsSealed(p.Auth.Token) || strings.Contains(p.RemoteURL, "remote") || strings.Contains(p.HTTP.Proxy, "proxy-pw") ||
		!strings.Contains(p.RemoteURL, "@repo.example/maven2/") || !strings.HasPrefix(p.HTTP.Proxy, "socks5://p:hgs2.") {
		t.Fatalf("sealed = %+v %+v", p, p.Auth)
	}
	if err := (core.UpstreamHTTP{Proxy: p.HTTP.Proxy}).ValidateProxy(); err != nil {
		t.Errorf("sealed proxy URL does not validate: %v", err)
	}
	sealedToken := p.Auth.Token
	if err := k.SealRepoConfig(&cfg, repoID); err != nil || cfg.Proxy.Auth.Token != sealedToken {
		t.Errorf("sealing twice changed the value: %v", err)
	}
	open, err := k.OpenProxyConfig(cfg.Proxy, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if open.Auth.Token != "tok" || open.RemoteURL != "https://u:remote%2Fpw@repo.example/maven2/" ||
		open.HTTP.Proxy != "socks5://p:proxy-pw@socks.corp:1080" {
		t.Errorf("opened = %+v %+v", open, open.Auth)
	}
	if cfg.Proxy.Auth.Token != sealedToken {
		t.Error("OpenProxyConfig modified its input")
	}
	// The configuration of one repository does not open as another's, and
	// a secret moved to another field of the same repository neither opens
	// nor is accepted when the configuration is saved.
	if _, err := k.OpenProxyConfig(cfg.Proxy, otherID); !errors.Is(err, ErrMalformed) {
		t.Errorf("opened as another repository: %v", err)
	}
	moved := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://repo.example/",
		Auth: &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "u", Password: sealedToken}}}
	if _, err := k.OpenProxyConfig(moved.Proxy, repoID); !errors.Is(err, ErrMalformed) {
		t.Errorf("token opened as the password: %v", err)
	}
	if err := k.SealRepoConfig(&moved, repoID); !errors.Is(err, ErrMalformed) || !errors.Is(err, core.ErrInvalid) {
		t.Errorf("token stored as the password: %v", err)
	}
	copied := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://repo.example/",
		Auth: &core.UpstreamAuth{Kind: core.UpstreamAuthBearer, Token: sealedToken}}}
	if err := k.SealRepoConfig(&copied, otherID); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("token stored in another repository: %v", err)
	}
	// A legacy value carried over on save is upgraded for its place.
	legacy, _ := k.sealLegacy([]byte("old-token"))
	up := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://repo.example/",
		Auth: &core.UpstreamAuth{Kind: core.UpstreamAuthBearer, Token: legacy}}}
	if err := k.SealRepoConfig(&up, repoID); err != nil || !strings.HasPrefix(up.Proxy.Auth.Token, Prefix) {
		t.Fatalf("legacy token not upgraded: %q %v", up.Proxy.Auth.Token, err)
	}
	if got, err := k.OpenString(RepositoryContext(repoID, "proxy.auth.token"), up.Proxy.Auth.Token); err != nil || got != "old-token" {
		t.Errorf("upgraded token = %q %v", got, err)
	}
	// A nil keyring refuses secrets but accepts configurations without any.
	var none *Keyring
	plain := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://repo.example/"}}
	if err := none.SealRepoConfig(&plain, repoID); err != nil {
		t.Errorf("no secrets, no key: %v", err)
	}
	withPW := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://u:pw@repo.example/"}}
	if err := none.SealRepoConfig(&withPW, repoID); !errors.Is(err, ErrNoKey) {
		t.Errorf("secret, no key: %v", err)
	}
}

func TestStoreConfig(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	const storeID = "5e0b7d2a-0000-4000-8000-000000000001"
	sealed, _ := k.SealString(BlobStoreContext(storeID, "secret_access_key_sealed"), "SK")
	in := map[string]any{"bucket": "b", "access_key_id": "AK", "secret_access_key_sealed": sealed}
	out, err := k.OpenStoreConfig(in, storeID)
	if err != nil {
		t.Fatal(err)
	}
	if out["secret_access_key"] != "SK" || out["secret_access_key_sealed"] != nil || in["secret_access_key"] != nil {
		t.Errorf("opened = %v (input %v)", out, in)
	}
	// Another store's id, or the session token field, does not open it.
	if _, err := k.OpenStoreConfig(in, "5e0b7d2a-0000-4000-8000-000000000002"); !errors.Is(err, ErrMalformed) {
		t.Errorf("opened for another store: %v", err)
	}
	asToken := map[string]any{"bucket": "b", "session_token_sealed": sealed}
	if _, err := k.OpenStoreConfig(asToken, storeID); !errors.Is(err, ErrMalformed) {
		t.Errorf("opened as the session token: %v", err)
	}
	doc := map[string]any{"bucket": "b", "secret_access_key": "legacy", "session_token": ""}
	seal := func(field, v string) (string, error) { return k.SealString(BlobStoreContext(storeID, field), v) }
	changed, err := mapStoreDoc(doc, seal, func(_, v string) (string, error) { return v, nil })
	if err != nil || !changed {
		t.Fatalf("mapStoreDoc = %v %v", changed, err)
	}
	if _, ok := doc["secret_access_key"]; ok {
		t.Errorf("plaintext key kept: %v", doc)
	}
	if v, _ := k.OpenString(BlobStoreContext(storeID, "secret_access_key_sealed"), doc["secret_access_key_sealed"].(string)); v != "legacy" {
		t.Errorf("sealed legacy key = %q", v)
	}
}

// Sealed values are refused as input, in either format: a legacy value
// opens wherever it is stored, and any value sent back into its own field
// would be opened and sent to the submitter's host.
func TestCheckInput(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	sealed, err := k.SealString(here, "victim")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		err     error
		refused bool
	}{
		{"plain", CheckInput("f", "hunter2"), false},
		{"empty", CheckInput("f", ""), false},
		{"sealed", CheckInput("f", sealed), true},
		{"sealed, padded", CheckInput("f", "  "+sealed), true},
		{"legacy", CheckInput("f", goldenLegacy), true},
		{"forged", CheckInput("f", "hgs2.x.y.z"), true},
		{"forged legacy", CheckInput("f", "hgs1.x.y.z"), true},
		{"url plain", CheckURLInput("u", "https://u:pw@h.example/"), false},
		{"url no password", CheckURLInput("u", "https://u@h.example/"), false},
		{"url sealed", CheckURLInput("u", "https://u:"+sealed+"@h.example/"), true},
		{"url legacy", CheckURLInput("u", "https://u:"+goldenLegacy+"@h.example/"), true},
	}
	for _, tc := range tests {
		if got := errors.Is(tc.err, ErrSealedInput) && errors.Is(tc.err, core.ErrInvalid); got != tc.refused {
			t.Errorf("%s: err = %v, want refused %v", tc.name, tc.err, tc.refused)
		}
		if tc.err != nil && (strings.Contains(tc.err.Error(), sealed) || strings.Contains(tc.err.Error(), goldenLegacy)) {
			t.Errorf("%s: error echoes the value: %v", tc.name, tc.err)
		}
	}
}
