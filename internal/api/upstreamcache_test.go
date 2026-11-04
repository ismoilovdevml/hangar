package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func TestInvalidateRepositoryCache(t *testing.T) {
	f := newFixture(t)
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer remote.Close()
	f.env.AddRepo(t, &core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
	repo := f.env.AddRepo(t, &core.Repository{Name: "npm-proxy", Format: "npm", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote.URL}}})

	f.expect("admin", "POST", "/api/v1/repositories/npm-proxy/cache:invalidate", nil, http.StatusNotImplemented)

	clients, err := httpclient.New(httpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := proxy.NewGuard([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := proxy.New(proxy.Options{HTTPClients: clients, Guard: guard, Events: f.env.Events})
	if err != nil {
		t.Fatal(err)
	}
	f.api.UpstreamCache = m
	up, err := m.ForRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func() {
		resp, err := up.Fetch(context.Background(), core.UpstreamRequest{Path: "left-pad"})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	fetch()
	fetch()
	if hits.Load() != 1 {
		t.Fatalf("remote hits = %d, want 1 (404 remembered)", hits.Load())
	}

	// Readers may not purge; hosted repositories have nothing to purge.
	f.expect("alice", "POST", "/api/v1/repositories/npm-proxy/cache:invalidate", nil, http.StatusForbidden)
	f.expect("admin", "POST", "/api/v1/repositories/npm-hosted/cache:invalidate", nil, http.StatusBadRequest)
	f.expect("admin", "POST", "/api/v1/repositories/nope/cache:invalidate", nil, http.StatusNotFound)

	var res proxy.CacheInvalidation
	if err := json.Unmarshal(f.expect("admin", "POST", "/api/v1/repositories/npm-proxy/cache:invalidate", nil, http.StatusOK), &res); err != nil {
		t.Fatal(err)
	}
	if res.Repository != "npm-proxy" || res.NegativeEntries != 1 {
		t.Fatalf("result = %+v", res)
	}
	fetch()
	if hits.Load() != 2 {
		t.Fatalf("remote hits after invalidation = %d, want 2", hits.Load())
	}
	if evs := f.env.Audit.Events("repository.cache_invalidate"); len(evs) == 0 || evs[len(evs)-1].Outcome != core.OutcomeSuccess {
		t.Errorf("audit = %+v", evs)
	}
	if evs := f.env.Events.Published(core.EventRepositoryCacheInvalidated); len(evs) != 1 {
		t.Errorf("events = %+v", evs)
	}
}
