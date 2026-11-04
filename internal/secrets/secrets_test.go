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
	s, err := k.SealString("p@ss:w/ord")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(s) || !regexp.MustCompile(`^hgs1\.[0-9a-f]{12}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`).MatchString(s) {
		t.Fatalf("sealed form %q", s)
	}
	if id, _ := KeyIDOf(s); id != k.PrimaryID() {
		t.Errorf("key id %s, primary %s", id, k.PrimaryID())
	}
	if got, err := k.OpenString(s); err != nil || got != "p@ss:w/ord" {
		t.Errorf("open = %q %v", got, err)
	}
	// Every seal uses a fresh data key and nonce.
	if s2, _ := k.SealString("p@ss:w/ord"); s2 == s {
		t.Error("sealing is deterministic")
	}
	if strings.Contains(s, "p@ss") {
		t.Error("plaintext visible")
	}
	// Empty values seal and open too.
	if e, err := k.SealString(""); err != nil {
		t.Fatal(err)
	} else if got, err := k.OpenString(e); err != nil || got != "" {
		t.Errorf("empty = %q %v", got, err)
	}
}

func TestTamperAndKeys(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	s, _ := k.SealString("secret")
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
		"parts":       "hgs1." + parts[1] + "." + parts[2],
		"not base64":  strings.Join([]string{parts[0], parts[1], "!!!", parts[3]}, "."),
	} {
		if _, err := k.Open(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A key id swapped in for another key does not open.
	other := mustKeyring(t, newKey(t))
	if _, err := other.Open(s); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("foreign key: %v", err)
	}
	// Nil keyring: no key configured.
	var none *Keyring
	if _, err := none.Seal([]byte("x")); !errors.Is(err, ErrNoKey) || !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("nil seal: %v", err)
	}
	if v, err := none.Reveal("legacy plaintext"); err != nil || v != "legacy plaintext" {
		t.Errorf("nil reveal plaintext: %q %v", v, err)
	}
	if _, err := none.Reveal(s); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil reveal sealed: %v", err)
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
	s, _ := old.SealString("token-1")

	rotated := mustKeyring(t, newK, oldKey)
	if got, err := rotated.OpenString(s); err != nil || got != "token-1" {
		t.Fatalf("previous key does not open: %q %v", got, err)
	}
	if !rotated.NeedsRewrap(s) {
		t.Error("NeedsRewrap = false for a previous key")
	}
	r, err := rotated.Rewrap(s)
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
	if again, _ := rotated.Rewrap(r); again != r {
		t.Error("rewrap of a current value changed it")
	}
	onlyNew := mustKeyring(t, newK)
	if got, err := onlyNew.OpenString(r); err != nil || got != "token-1" {
		t.Errorf("after removing the old key: %q %v", got, err)
	}
	if _, err := onlyNew.Open(s); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("old value after removing the old key: %v", err)
	}
	if len(rotated.KeyIDs()) != 2 || rotated.KeyIDs()[0] != rotated.PrimaryID() {
		t.Errorf("key ids = %v", rotated.KeyIDs())
	}
}

func TestRepoConfig(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	cfg := core.RepoConfig{Proxy: &core.ProxyConfig{
		RemoteURL: "https://u:remote%2Fpw@repo.example/maven2/",
		Auth:      &core.UpstreamAuth{Kind: core.UpstreamAuthHeader, HeaderName: "Private-Token", Token: "tok"},
		HTTP:      core.UpstreamHTTP{Proxy: "socks5://p:proxy-pw@socks.corp:1080"},
	}}
	if err := k.SealRepoConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Proxy
	if !IsSealed(p.Auth.Token) || strings.Contains(p.RemoteURL, "remote") || strings.Contains(p.HTTP.Proxy, "proxy-pw") ||
		!strings.Contains(p.RemoteURL, "@repo.example/maven2/") || !strings.HasPrefix(p.HTTP.Proxy, "socks5://p:hgs1.") {
		t.Fatalf("sealed = %+v %+v", p, p.Auth)
	}
	if err := (core.UpstreamHTTP{Proxy: p.HTTP.Proxy}).ValidateProxy(); err != nil {
		t.Errorf("sealed proxy URL does not validate: %v", err)
	}
	sealedToken := p.Auth.Token
	if err := k.SealRepoConfig(&cfg); err != nil || cfg.Proxy.Auth.Token != sealedToken {
		t.Errorf("sealing twice changed the value: %v", err)
	}
	open, err := k.OpenProxyConfig(cfg.Proxy)
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
	// A nil keyring refuses secrets but accepts configurations without any.
	var none *Keyring
	plain := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://repo.example/"}}
	if err := none.SealRepoConfig(&plain); err != nil {
		t.Errorf("no secrets, no key: %v", err)
	}
	withPW := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://u:pw@repo.example/"}}
	if err := none.SealRepoConfig(&withPW); !errors.Is(err, ErrNoKey) {
		t.Errorf("secret, no key: %v", err)
	}
}

func TestStoreConfig(t *testing.T) {
	k := mustKeyring(t, newKey(t))
	sealed, _ := k.SealString("SK")
	in := map[string]any{"bucket": "b", "access_key_id": "AK", "secret_access_key_sealed": sealed}
	out, err := k.OpenStoreConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if out["secret_access_key"] != "SK" || out["secret_access_key_sealed"] != nil || in["secret_access_key"] != nil {
		t.Errorf("opened = %v (input %v)", out, in)
	}
	doc := map[string]any{"bucket": "b", "secret_access_key": "legacy", "session_token": ""}
	changed, err := mapStoreDoc(doc, k.SealString, func(_, v string) (string, error) { return v, nil })
	if err != nil || !changed {
		t.Fatalf("mapStoreDoc = %v %v", changed, err)
	}
	if _, ok := doc["secret_access_key"]; ok {
		t.Errorf("plaintext key kept: %v", doc)
	}
	if v, _ := k.OpenString(doc["secret_access_key_sealed"].(string)); v != "legacy" {
		t.Errorf("sealed legacy key = %q", v)
	}
}
