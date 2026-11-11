package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/sysmon"
)

type fakeStoreStates map[string]sysmon.StoreState

func (f fakeStoreStates) BlobStoreState(name string) (sysmon.StoreState, bool) {
	st, ok := f[name]
	return st, ok
}

type storeCatalog struct {
	fakeCatalog
	stores []*catalog.BlobStore
}

func (c storeCatalog) BlobStores(context.Context) ([]*catalog.BlobStore, catalog.Totals, error) {
	return c.stores, catalog.Totals{}, nil
}

// /system/status is the rollup of the same checks /system/nodes and
// /system/health report: a broken blob store is "down" in all of them.
// Anonymous callers get the state alone; the failing checks need sign-in,
// their detail system:cluster:read.
func TestSystemStatusAgreesWithHealth(t *testing.T) {
	f, _ := monitorFixture(t)
	now := time.Now().UTC()
	f.api.Monitor = fakeMonitor{nodes: []sysmon.Node{{ID: "hangar-1", Current: true, LastHeartbeat: now, Status: sysmon.StatusDown,
		Subsystems: []sysmon.Subsystem{
			{Name: "database", Label: "Database", CheckedAt: now, Result: sysmon.Result{Status: sysmon.StatusHealthy}},
			{Name: "blobstores", Label: "Blob storage", CheckedAt: now, Result: sysmon.Result{Status: sysmon.StatusDown,
				Detail: "s3-bad is unreachable: lookup no-such-bucket.minio: no such host"}},
		}}}}

	var nodes systemNodes
	_ = json.Unmarshal(f.expect("viewer", "GET", "/api/v1/system/nodes", nil, 200), &nodes)
	if nodes.Summary.Status != sysmon.StatusDown || nodes.Summary.Headline != "Blob storage down" {
		t.Fatalf("nodes summary = %+v", nodes.Summary)
	}

	var anon systemStatus
	body := f.expect("", "GET", "/api/v1/system/status", nil, 200)
	_ = json.Unmarshal(body, &anon)
	if anon.State != "down" || anon.Message != nodes.Summary.Headline || anon.Problems != nil || strings.Contains(string(body), "no-such-bucket") {
		t.Fatalf("anonymous status = %s", body)
	}
	if *anon.ChecksTotal != 2 || *anon.ChecksPassing != 1 || len(anon.Dependencies) != 2 || anon.Dependencies[1].Status != "fail" {
		t.Fatalf("anonymous counts = %s", body)
	}

	var user systemStatus
	body = f.expect("alice", "GET", "/api/v1/system/status", nil, 200)
	_ = json.Unmarshal(body, &user)
	if len(user.Problems) != 1 || user.Problems[0].Label != "Blob storage" || user.Problems[0].Detail != "" || strings.Contains(string(body), "no-such-bucket") {
		t.Fatalf("signed-in status = %s", body)
	}

	var viewer systemStatus
	_ = json.Unmarshal(f.expect("viewer", "GET", "/api/v1/system/status", nil, 200), &viewer)
	if len(viewer.Problems) != 1 || !strings.Contains(viewer.Problems[0].Detail, "no-such-bucket") ||
		!strings.Contains(viewer.Dependencies[1].Error, "no-such-bucket") {
		t.Fatalf("cluster reader status = %+v", viewer)
	}
}

// Blob store state comes from the health probe, with its detail; a
// repository on an unavailable store, and a proxy whose last request
// failed, are offline with the reason and the error.
func TestStoreAndUpstreamStateReachRepositories(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "raw-bad", Format: "raw", Type: core.RepoHosted, BlobStore: "s3-bad"})
	f.env.AddRepo(t, &core.Repository{Name: "raw-ok", Format: "raw", Type: core.RepoHosted, BlobStore: "default"})
	f.env.AddRepo(t, &core.Repository{Name: "npm-dead", Format: "npm", Type: core.RepoProxy, BlobStore: "default",
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://does-not-exist.invalid"}}})
	f.env.AddRepo(t, &core.Repository{Name: "npm-idle", Format: "npm", Type: core.RepoProxy, BlobStore: "default",
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry.npmjs.org"}}})
	at := time.Date(2026, 11, 11, 9, 44, 0, 0, time.UTC)
	lat := int64(2)
	f.api.BlobStoreProbe = fakeStoreStates{
		"default": {Status: sysmon.StatusHealthy, Detail: "Reachable (2 ms)", LatencyMS: &lat, CheckedAt: at, Since: at},
		"s3-bad":  {Status: sysmon.StatusDown, Detail: "Unreachable: lookup no-such-bucket.minio: no such host", CheckedAt: at, Since: at},
	}
	f.api.Catalog = storeCatalog{stores: []*catalog.BlobStore{
		{Name: "default", Kind: "fs", FS: &catalog.FSConfig{Path: "/data"}},
		{Name: "s3-bad", Kind: "s3", S3: &catalog.S3Config{Bucket: "no-such-bucket"}},
		{Name: "new", Kind: "fs", FS: &catalog.FSConfig{Path: "/new"}},
	}}
	f.api.Upstreams = fakeUpstreams{
		"npm-dead": {Name: "npm-dead", State: proxy.StateOffline, LastError: `Get "https://does-not-exist.invalid/x": dial tcp: lookup does-not-exist.invalid: no such host`},
		"npm-idle": {Name: "npm-idle", State: proxy.StateUnknown},
	}

	var stores struct {
		Items []blobStoreJSON `json:"items"`
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/blob-stores", nil, 200), &stores)
	got := map[string]blobStoreJSON{}
	for _, s := range stores.Items {
		got[s.Name] = s
	}
	if s := got["default"]; s.State != "ok" || s.WriteLatencyMS == nil || s.CheckedAt == nil {
		t.Errorf("default = %+v", s)
	}
	if s := got["s3-bad"]; s.State != "unavailable" || !strings.Contains(s.StateDetail, "no such host") || s.WriteLatencyMS != nil {
		t.Errorf("s3-bad = %+v", s)
	}
	if s := got["new"]; s.State != "unknown" || s.CheckedAt != nil {
		t.Errorf("not probed yet = %+v", s)
	}

	var list repositoryList
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories", nil, 200), &list)
	repos := map[string]repositoryJSON{}
	for _, r := range list.Items {
		repos[r.Name] = r
	}
	if r := repos["raw-bad"]; r.Status != "offline" || r.StatusReason != "blob_store_unavailable" || !strings.Contains(r.StatusDetail, "s3-bad") {
		t.Errorf("repository on a broken store = %+v", r)
	}
	if r := repos["npm-dead"]; r.Status != "offline" || r.StatusReason != "upstream_unreachable" || !strings.Contains(r.StatusDetail, "no such host") {
		t.Errorf("dead proxy = %+v", r)
	}
	if r := repos["raw-ok"]; r.Status != "online" || r.StatusReason != "" {
		t.Errorf("healthy = %+v", r)
	}
	if r := repos["npm-idle"]; r.Status != "online" {
		t.Errorf("idle proxy = %+v", r)
	}

	var up struct {
		Items []upstreamHealthJSON `json:"items"`
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/upstreams/health", nil, 200), &up)
	if len(up.Items) != 2 {
		t.Fatalf("upstreams = %+v", up.Items)
	}
	for _, u := range up.Items {
		switch u.Name {
		case "npm-dead":
			if u.State != "offline" || !strings.Contains(u.LastError, "no such host") {
				t.Errorf("dead upstream = %+v", u)
			}
		case "npm-idle":
			if u.State != "unknown" {
				t.Errorf("idle upstream = %+v", u)
			}
		}
	}
}
