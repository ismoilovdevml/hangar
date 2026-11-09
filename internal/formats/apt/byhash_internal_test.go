package apt

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

// TestByHashGraceExpires: superseded by-hash files are kept for the
// previous Release and dropped once their grace has passed.
func TestByHashGraceExpires(t *testing.T) {
	env := coretest.NewEnv(t)
	repo := env.AddRepo(t, &core.Repository{Name: "apt-hosted", Format: Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write") // no delete: pruning is the server's
	env.AuthZ.Grant("system", "repo:*:admin")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kr, err := secrets.NewKeyring(base64.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := New(signing.New(signing.Options{Store: signing.NewMemStore(), Secrets: kr, Algorithm: signing.AlgorithmEd25519}))
	f.byHashGrace = time.Millisecond
	h := formats.NewHandler(env.Deps(), f)
	upload := func(name string) {
		req := httptest.NewRequest(http.MethodPut, "/repository/apt-hosted/upload",
			bytes.NewReader(apttest.Package{Name: name, Version: "1", Architecture: "amd64"}.MustBuild()))
		req.SetBasicAuth("alice", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s = %d %s", name, rec.Code, rec.Body)
		}
	}
	byHash := func() map[string]bool {
		assets, err := env.Components.ListAssets(t.Context(), core.AssetQuery{RepositoryID: repo.ID, PathPrefix: "dists/", Limit: core.MaxPageSize})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, a := range assets.Items {
			if strings.Contains(a.Path, "/by-hash/") {
				out[a.Path] = true
			}
		}
		return out
	}
	upload("one")
	first := byHash()
	upload("two")
	second := byHash()
	for p := range first {
		if !second[p] {
			t.Fatalf("by-hash file of the previous Release dropped at once: %s", p)
		}
	}
	time.Sleep(5 * time.Millisecond)
	upload("three")
	third := byHash()
	gone := 0
	for p := range first {
		if !third[p] && !strings.Contains(p, "/binary-arm64/") { // arm64 indexes never changed
			gone++
		}
	}
	if gone == 0 {
		t.Fatalf("by-hash files two generations old outlived their grace: %d files", len(third))
	}
	for p := range second {
		if !first[p] && !third[p] { // the files of the second Release
			t.Fatalf("by-hash file of the previous Release dropped: %s", p)
		}
	}
}
