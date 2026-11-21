package importer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// tokenService records the Authorization header of every token request.
type tokenService struct {
	mu   sync.Mutex
	auth []string
}

func (ts *tokenService) handler(w http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	ts.auth = append(ts.auth, r.Header.Get("Authorization"))
	ts.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"token":"tok"}`))
}

func (ts *tokenService) seen() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.auth...)
}

// A sealed value cannot be submitted as a source credential: the server
// would open it and send the plaintext to the submitter's source. Leaving
// the credentials empty keeps the stored ones.
func TestSealedSourceCredentialsRefused(t *testing.T) {
	k, err := secrets.NewKeyring("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{opts: Options{Secrets: k}}
	at := sourceAt{testTaskID, importSourcePath}
	victim, err := k.SealString(at.context("password"), "victim-password")
	if err != nil {
		t.Fatal(err)
	}
	const src = "https://attacker.example.com/npm/"
	for _, in := range []Source{
		{URL: src, Username: "x", Password: victim},
		{URL: src, Token: victim},
		{URL: src, Token: " " + victim},
	} {
		// Even with the same value stored for the same URL and user.
		prev := in
		if err := s.sealSource(&in, &prev, at); !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "sealed value") {
			t.Errorf("%+v: err = %v, want ErrInvalid", in, err)
		}
	}
	// Keeping the stored credential still works.
	prev := Source{URL: src, Username: "x", Password: victim}
	in := Source{URL: src, Username: "x"}
	if err := s.sealSource(&in, &prev, at); err != nil || in.Password != victim {
		t.Fatalf("keep stored: err = %v, password kept = %v", err, in.Password == victim)
	}
}

// The source's credentials are sent to a token service only when the source
// itself asked, and only over TLS unless the token service is the source's
// own origin.
func TestChallengeCredentialsOrigin(t *testing.T) {
	var plainTS, tlsTS tokenService
	plainRealm := httptest.NewServer(http.HandlerFunc(plainTS.handler))
	defer plainRealm.Close()
	tlsRealm := httptest.NewTLSServer(http.HandlerFunc(tlsTS.handler))
	defer tlsRealm.Close()

	// other (a host the source redirects to) answers with a challenge naming
	// an https token service.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tlsRealm.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()

	var realm string // the realm the source names in its challenges
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"own"}`))
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case r.Header.Get("Authorization") != "Bearer own" && r.Header.Get("Authorization") != "Bearer tok":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer reg.Close()

	newSrc := func() *source {
		base, _ := url.Parse(reg.URL + "/")
		return &source{base: base, client: tlsRealm.Client(), creds: Source{Username: "ci", Password: "s3cret"},
			limit: newLimiter(1000), agent: "test", tokens: map[string]string{}}
	}
	tests := []struct {
		name     string
		realm    string
		path     string
		wantErr  bool
		ts       *tokenService
		wantAuth bool
	}{
		{"https realm on another host", tlsRealm.URL + "/token", "/v2/", false, &tlsTS, true},
		{"plain http realm on another host", plainRealm.URL + "/token", "/v2/", true, &plainTS, false},
		{"challenge from a redirect target", "", "/redirect", true, &tlsTS, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			realm = tc.realm
			before := len(tc.ts.seen())
			resp, err := newSrc().do(context.Background(), http.MethodGet, tc.path, http.Header{})
			if resp != nil {
				drain(resp)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, core.ErrUnauthenticated) {
				t.Errorf("err = %v, want ErrUnauthenticated", err)
			}
			got := tc.ts.seen()[before:]
			withAuth := false
			for _, a := range got {
				withAuth = withAuth || a != ""
			}
			if withAuth != tc.wantAuth {
				t.Errorf("token service got Authorization %q, want credentials sent = %v", got, tc.wantAuth)
			}
		})
	}

	// A plain http token service on the source's own origin gets them (the
	// source itself is plain http, so nothing new is exposed).
	realm = reg.URL + "/token"
	resp, err := newSrc().do(context.Background(), http.MethodGet, "/v2/", http.Header{})
	if err != nil {
		t.Fatalf("same-origin realm: %v", err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin realm: status %d", resp.StatusCode)
	}
}
