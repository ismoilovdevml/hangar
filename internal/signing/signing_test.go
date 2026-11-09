package signing_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

func keyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k, err := secrets.NewKeyring(base64.StdEncoding.EncodeToString(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func readPublic(t *testing.T, armored []byte) openpgp.EntityList {
	t.Helper()
	el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	if el[0].PrivateKey != nil {
		t.Fatal("public key export carries private material")
	}
	return el
}

func TestSignAndVerify(t *testing.T) {
	for _, algo := range []string{signing.AlgorithmEd25519, signing.AlgorithmRSA4096} {
		t.Run(algo, func(t *testing.T) {
			if algo == signing.AlgorithmRSA4096 && testing.Short() {
				t.Skip("RSA 4096 generation is slow")
			}
			k, err := signing.Generate(algo, "Hangar test <test@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			if k.Algorithm() != algo || len(k.Fingerprint()) != 40 {
				t.Fatalf("algorithm %s fingerprint %s", k.Algorithm(), k.Fingerprint())
			}
			pub, _ := k.PublicArmored()
			el := readPublic(t, pub)
			bin, _ := k.PublicBinary()
			if _, err := openpgp.ReadKeyRing(bytes.NewReader(bin)); err != nil {
				t.Fatalf("binary public key: %v", err)
			}

			msg := []byte("Origin: test\nSuite: stable\n- dashed line\nSHA256:\n abc 1 main/binary-amd64/Packages\n")
			signed, err := k.ClearSign(msg, time.Unix(1700000000, 0))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := clearsign.Decode(signed)
			if b == nil {
				t.Fatalf("not a clearsigned message:\n%s", signed)
			}
			if !bytes.Equal(bytes.TrimRight(b.Plaintext, "\n"), bytes.TrimRight(msg, "\n")) {
				t.Fatalf("plaintext %q", b.Plaintext)
			}
			if _, err := b.VerifySignature(el, nil); err != nil {
				t.Fatalf("clearsign verify: %v", err)
			}

			sig, err := k.DetachSign(msg, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(msg), bytes.NewReader(sig), nil); err != nil {
				t.Fatalf("detached verify: %v", err)
			}
			if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(append(msg, 'x')), bytes.NewReader(sig), nil); err == nil {
				t.Fatal("tampered message verified")
			}
		})
	}
}

func TestParsePrivate(t *testing.T) {
	k, err := signing.Generate(signing.AlgorithmEd25519, "Import <import@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := k.PrivateArmored()
	got, err := signing.ParsePrivate(priv, "")
	if err != nil || got.Fingerprint() != k.Fingerprint() {
		t.Fatalf("round trip: %v %v", got, err)
	}

	// A passphrase-protected export, as `gpg --export-secret-keys --armor` writes.
	el, _ := openpgp.ReadArmoredKeyRing(bytes.NewReader(priv))
	if err := el[0].EncryptPrivateKeys([]byte("pass"), nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	_ = el[0].SerializePrivateWithoutSigning(w, nil)
	_ = w.Close()
	if _, err := signing.ParsePrivate(buf.Bytes(), ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("missing passphrase: %v", err)
	}
	if _, err := signing.ParsePrivate(buf.Bytes(), "wrong"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if got, err := signing.ParsePrivate(buf.Bytes(), "pass"); err != nil || got.Fingerprint() != k.Fingerprint() {
		t.Fatalf("protected import: %v", err)
	}
	pub, _ := k.PublicArmored()
	if _, err := signing.ParsePrivate(pub, ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("public key accepted as private: %v", err)
	}
	if _, err := signing.ParsePrivate([]byte("garbage"), ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("garbage: %v", err)
	}
}

func newService(t *testing.T, sealer signing.Sealer) (*signing.Service, *coretest.Env, *signing.MemStore) {
	t.Helper()
	env := coretest.NewEnv(t)
	store := signing.NewMemStore()
	opts := signing.Options{Store: store, Algorithm: signing.AlgorithmEd25519, Events: env.Events, Audit: env.Audit,
		AuthZ: env.AuthZ, Repos: env.Repos, BaseURL: "https://hangar.example.com", Formats: []string{"apt", "rpm"}}
	if sealer != nil {
		opts.Secrets = sealer
	}
	return signing.New(opts), env, store
}

func TestServiceKeys(t *testing.T) {
	kr := keyring(t)
	svc, env, store := newService(t, kr)
	ctx := context.Background()
	a := env.AddRepo(t, &core.Repository{Name: "apt-a", Format: "apt", Type: core.RepoHosted})
	b := env.AddRepo(t, &core.Repository{Name: "apt-b", Format: "apt", Type: core.RepoHosted})
	s1 := env.AddRepo(t, &core.Repository{Name: "rpm-s", Format: "rpm", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"signing_key": "shared"}}})
	s2 := env.AddRepo(t, &core.Repository{Name: "apt-s", Format: "apt", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"signing_key": "shared"}}})

	ka, err := svc.KeyFor(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := svc.KeyFor(ctx, b)
	if ka.Fingerprint() == kb.Fingerprint() {
		t.Fatal("two repositories share a key without signing_key: shared")
	}
	if again, _ := svc.KeyFor(ctx, a); again.Fingerprint() != ka.Fingerprint() {
		t.Fatal("key changed between calls")
	}
	k1, _ := svc.KeyFor(ctx, s1)
	k2, _ := svc.KeyFor(ctx, s2)
	if k1.Fingerprint() != k2.Fingerprint() {
		t.Fatal("shared key differs between repositories")
	}
	rec, _ := store.Get(ctx, "repo:"+a.ID)
	if !secrets.IsSealed(rec.PrivateKey) || strings.Contains(rec.PrivateKey, "PRIVATE KEY") {
		t.Fatal("private key stored unsealed")
	}
	if !strings.Contains(ka.UserID(), "apt-a") {
		t.Errorf("user id %q", ka.UserID())
	}

	// A fresh service (another node) opens the stored key.
	other := signing.New(signing.Options{Store: store, Secrets: kr})
	if ko, err := other.KeyFor(ctx, a); err != nil || ko.Fingerprint() != ka.Fingerprint() {
		t.Fatalf("other node: %v", err)
	}

	info, err := svc.Rotate(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.Fingerprint == ka.Fingerprint() {
		t.Fatal("rotation kept the key")
	}
	if now, _ := svc.KeyFor(ctx, a); now.Fingerprint() != info.Fingerprint {
		t.Fatal("rotated key not used")
	}
	if ev := env.Events.Published(signing.EventKeyChanged); len(ev) != 1 || ev[0].Subject != "repo:"+a.ID {
		t.Fatalf("events %+v", ev)
	}
	if au := env.Audit.Events("signing_key.rotate"); len(au) != 1 || au[0].Details["previous_fingerprint"] != ka.Fingerprint() {
		t.Fatalf("audit %+v", au)
	}

	imp, _ := signing.Generate(signing.AlgorithmEd25519, "Imported <i@example.com>")
	priv, _ := imp.PrivateArmored()
	info, err = svc.Import(ctx, b, priv, "")
	if err != nil || info.Fingerprint != imp.Fingerprint() {
		t.Fatalf("import: %v %+v", err, info)
	}
}

func TestServiceWithoutSecretKey(t *testing.T) {
	svc, env, _ := newService(t, nil)
	a := env.AddRepo(t, &core.Repository{Name: "apt-a", Format: "apt", Type: core.RepoHosted})
	if _, err := svc.KeyFor(context.Background(), a); !errors.Is(err, secrets.ErrNoKey) || core.HTTPStatus(err) != http.StatusNotImplemented {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateSetting(t *testing.T) {
	for v, ok := range map[any]bool{"shared": true, "repository": true, "other": false, 3: false} {
		r := &core.Repository{Name: "x", Config: core.RepoConfig{Format: map[string]any{"signing_key": v}}}
		if err := signing.ValidateSetting(r); (err == nil) != ok {
			t.Errorf("%v: %v", v, err)
		}
	}
}

func TestAdminAPI(t *testing.T) {
	svc, env, _ := newService(t, keyring(t))
	env.AddRepo(t, &core.Repository{Name: "apt-a", Format: "apt", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "apt-p", Format: "apt", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://deb.example.org/debian"}}})
	env.AddRepo(t, &core.Repository{Name: "raw-a", Format: "raw", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "apt-s", Format: "apt", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"signing_key": "shared"}}})
	env.Auth.AddUser("admin", "pw")
	env.Auth.AddUser("reader", "pw")
	env.Auth.AddUser("owner", "pw")
	env.Auth.AddUser("nobody", "pw")
	env.AuthZ.Grant("admin", "system:*", "repo:*:admin")
	env.AuthZ.Grant("reader", "repo:*:read")
	env.AuthZ.Grant("owner", "repo:apt-a:admin", "repo:apt-s:admin")
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u, p, _ := req.BasicAuth()
			pr, err := env.Auth.Authenticate(req.Context(), core.Credentials{Username: u, Password: p})
			if err != nil {
				http.Error(w, "auth", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, req.WithContext(core.WithPrincipal(req.Context(), pr)))
		})
	})
	svc.RegisterRoutes(r)
	do := func(user, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if user != "" {
			req.SetBasicAuth(user, "pw")
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	code, m := do("reader", "GET", "/repositories/apt-a/signing-key", "")
	if code != 200 || !strings.Contains(m["public_key"].(string), "BEGIN PGP PUBLIC KEY BLOCK") || m["key"] != "repository" {
		t.Fatalf("GET = %d %v", code, m)
	}
	if strings.Contains(m["public_key"].(string), "PRIVATE") {
		t.Fatal("private key leaked")
	}
	fp := m["fingerprint"]
	for _, c := range []struct {
		user, method, path, body string
		want                     int
	}{
		{"nobody", "GET", "/repositories/apt-a/signing-key", "", 404},
		{"", "GET", "/repositories/apt-a/signing-key", "", 401},
		{"reader", "POST", "/repositories/apt-a/signing-key:rotate", "", 403},
		{"reader", "GET", "/repositories/apt-p/signing-key", "", 404},
		{"reader", "GET", "/repositories/raw-a/signing-key", "", 404},
		{"owner", "POST", "/repositories/apt-s/signing-key:rotate", "", 403}, // shared key: system admins only
		{"owner", "PUT", "/repositories/apt-a/signing-key", `{"private_key":"nope"}`, 400},
		{"owner", "POST", "/repositories/apt-a/signing-key:rotate", `{"algorithm":"dsa"}`, 400},
		{"admin", "POST", "/repositories/apt-s/signing-key:rotate", "", 200},
	} {
		if code, m := do(c.user, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s %s = %d %v, want %d", c.user, c.method, c.path, code, m, c.want)
		}
	}
	code, m = do("owner", "POST", "/repositories/apt-a/signing-key:rotate", "")
	if code != 200 || m["fingerprint"] == fp {
		t.Fatalf("rotate = %d %v", code, m)
	}
}
