package osv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// fakeOSV serves /v1/querybatch from a table of package@version -> ids and
// pages the results pageSize at a time.
type fakeOSV struct {
	t        *testing.T
	vulns    map[string][]string // "ecosystem/name@version" -> ids
	pageSize int

	mu       sync.Mutex
	requests []batchRequest
}

func (f *fakeOSV) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		var req batchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		type result struct {
			Vulns         []VulnRef `json:"vulns,omitempty"`
			NextPageToken string    `json:"next_page_token,omitempty"`
		}
		out := struct {
			Results []result `json:"results"`
		}{Results: []result{}}
		for _, q := range req.Queries {
			ids := f.vulns[q.Package.Ecosystem+"/"+q.Package.Name+"@"+q.Version]
			start := 0
			if q.PageToken != "" {
				_, _ = fmt.Sscanf(q.PageToken, "p%d", &start)
			}
			end := len(ids)
			var res result
			if f.pageSize > 0 && end-start > f.pageSize {
				end = start + f.pageSize
				res.NextPageToken = fmt.Sprintf("p%d", end)
			}
			for _, id := range ids[start:end] {
				res.Vulns = append(res.Vulns, VulnRef{ID: id, Modified: "2026-01-02T03:04:05Z"})
			}
			out.Results = append(out.Results, res)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/vulns/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "MISSING-1" {
			http.Error(w, `{"code":5,"message":"Bug not found."}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(Vuln{ID: id, Summary: "summary of " + id, Modified: "2026-01-02T03:04:05Z"})
	})
	return mux
}

func newClient(t *testing.T, h http.Handler, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts.BaseURL = srv.URL
	opts.HTTPClient = srv.Client()
	if opts.MinInterval == 0 {
		opts.MinInterval = -1 // no rate limit unless the test sets one
	}
	if opts.BaseBackoff == 0 {
		opts.BaseBackoff = time.Millisecond
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 5 * time.Millisecond
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func npmQuery(name, version string) Query {
	return Query{Package: Package{Name: name, Ecosystem: "npm"}, Version: version}
}

func ids(refs []VulnRef) string {
	var s []string
	for _, r := range refs {
		s = append(s, r.ID)
	}
	return strings.Join(s, ",")
}

func TestQueryBatchSplitsIntoBatches(t *testing.T) {
	f := &fakeOSV{t: t, vulns: map[string][]string{
		"npm/a@1": {"GHSA-a"},
		"npm/c@1": {"GHSA-c1", "GHSA-c2"},
		"npm/e@1": {"GHSA-e"},
	}}
	c := newClient(t, f.handler(), Options{BatchSize: 2})
	qs := []Query{npmQuery("a", "1"), npmQuery("b", "1"), npmQuery("c", "1"), npmQuery("d", "1"), npmQuery("e", "1")}
	got, err := c.QueryBatch(context.Background(), qs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GHSA-a", "", "GHSA-c1,GHSA-c2", "", "GHSA-e"}
	for i := range want {
		if ids(got[i]) != want[i] {
			t.Errorf("result %d = %q, want %q", i, ids(got[i]), want[i])
		}
	}
	if len(f.requests) != 3 {
		t.Fatalf("requests = %d, want 3 (batches of 2)", len(f.requests))
	}
	for i, n := range []int{2, 2, 1} {
		if len(f.requests[i].Queries) != n {
			t.Errorf("batch %d has %d queries, want %d", i, len(f.requests[i].Queries), n)
		}
	}
}

func TestQueryBatchFollowsPages(t *testing.T) {
	f := &fakeOSV{t: t, pageSize: 2, vulns: map[string][]string{
		"npm/big@1":   {"V1", "V2", "V3", "V4", "V5"},
		"npm/small@1": {"S1"},
	}}
	c := newClient(t, f.handler(), Options{})
	got, err := c.QueryBatch(context.Background(), []Query{npmQuery("small", "1"), npmQuery("big", "1")})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got[0]) != "S1" || ids(got[1]) != "V1,V2,V3,V4,V5" {
		t.Fatalf("results = %q / %q", ids(got[0]), ids(got[1]))
	}
	if len(f.requests) != 3 {
		t.Fatalf("requests = %d, want 3 (one batch, two follow-up pages)", len(f.requests))
	}
	// Follow-up pages ask only for the query that has more, with its token.
	follow := f.requests[1].Queries
	if len(follow) != 1 || follow[0].Package.Name != "big" || follow[0].PageToken != "p2" {
		t.Fatalf("second request = %+v", follow)
	}

	// A runaway pagination is cut off.
	c = newClient(t, f.handler(), Options{MaxPages: 2})
	if _, err := c.QueryBatch(context.Background(), []Query{npmQuery("big", "1")}); err == nil {
		t.Fatal("expected an error after MaxPages pages")
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	f := &fakeOSV{t: t, vulns: map[string][]string{"npm/a@1": {"GHSA-a"}}}
	inner := f.handler()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "1") // capped by MaxBackoff
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			inner.ServeHTTP(w, r)
		}
	})
	c := newClient(t, h, Options{})
	start := time.Now()
	got, err := c.QueryBatch(context.Background(), []Query{npmQuery("a", "1")})
	if err != nil {
		t.Fatal(err)
	}
	if ids(got[0]) != "GHSA-a" || calls.Load() != 3 {
		t.Fatalf("result %q after %d calls", ids(got[0]), calls.Load())
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("Retry-After was not capped by MaxBackoff: %v", time.Since(start))
	}
}

func TestGivesUpAfterMaxAttemptsAndOnClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}), Options{MaxAttempts: 3})
	_, err := c.QueryBatch(context.Background(), []Query{npmQuery("a", "1")})
	if err == nil || !errors.Is(err, core.ErrUpstream) || calls.Load() != 3 {
		t.Fatalf("err = %v after %d calls, want ErrUpstream after 3", err, calls.Load())
	}

	calls.Store(0)
	c = newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad query", http.StatusBadRequest)
	}), Options{MaxAttempts: 3})
	if _, err := c.QueryBatch(context.Background(), []Query{npmQuery("a", "1")}); err == nil || calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls, want one attempt for 400", err, calls.Load())
	}
}

func TestVuln(t *testing.T) {
	f := &fakeOSV{t: t}
	c := newClient(t, f.handler(), Options{})
	v, err := c.Vuln(context.Background(), "GHSA-xxxx-yyyy")
	if err != nil || v.ID != "GHSA-xxxx-yyyy" || v.Summary != "summary of GHSA-xxxx-yyyy" {
		t.Fatalf("vuln = %+v, %v", v, err)
	}
	if _, err := c.Vuln(context.Background(), "MISSING-1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing vuln err = %v, want ErrNotFound", err)
	}
}

func TestRateLimit(t *testing.T) {
	f := &fakeOSV{t: t}
	c := newClient(t, f.handler(), Options{MinInterval: 30 * time.Millisecond})
	start := time.Now()
	for range 3 {
		if _, err := c.Vuln(context.Background(), "X"); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("3 requests took %v, want at least 2 intervals", d)
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"ftp://osv.example.com", "osv.example.com", "http://"} {
		if _, err := New(Options{BaseURL: u}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("New(%q) = %v, want ErrInvalid", u, err)
		}
	}
}
