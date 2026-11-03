package coretest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Upstreams is an in-memory core.Upstreams. Register an http.Handler per proxy
// repository with Set (served in-process); repositories without one are
// fetched over real HTTP from Config.Proxy.RemoteURL (point it at an
// httptest.Server).
type Upstreams struct {
	mu       sync.RWMutex
	handlers map[string]http.Handler
	calls    map[string]int
}

var _ core.Upstreams = (*Upstreams)(nil)

// NewUpstreams returns an empty set.
func NewUpstreams() *Upstreams {
	return &Upstreams{handlers: map[string]http.Handler{}, calls: map[string]int{}}
}

// Set serves repoName's upstream with h.
func (u *Upstreams) Set(repoName string, h http.Handler) {
	u.mu.Lock()
	u.handlers[repoName] = h
	u.mu.Unlock()
}

// Calls returns how many fetches were made for repoName (to assert caching).
func (u *Upstreams) Calls(repoName string) int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.calls[repoName]
}

// ForRepository implements core.Upstreams.
func (u *Upstreams) ForRepository(_ context.Context, repo *core.Repository) (core.Upstream, error) {
	if repo.Type != core.RepoProxy || repo.Config.Proxy == nil {
		return nil, fmt.Errorf("repository %q is not a proxy: %w", repo.Name, core.ErrInvalid)
	}
	u.mu.RLock()
	h := u.handlers[repo.Name]
	u.mu.RUnlock()
	return &fakeUpstream{owner: u, repo: repo.Name, remote: strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/"), h: h}, nil
}

type fakeUpstream struct {
	owner  *Upstreams
	repo   string
	remote string
	h      http.Handler
}

func (f *fakeUpstream) Fetch(ctx context.Context, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	f.owner.mu.Lock()
	f.owner.calls[f.repo]++
	f.owner.mu.Unlock()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hreq, err := http.NewRequestWithContext(ctx, method, f.remote+"/"+strings.TrimPrefix(req.Path, "/"), nil)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w: %w", err, core.ErrInvalid)
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
	}
	if req.Conditional.ETag != "" {
		hreq.Header.Set("If-None-Match", req.Conditional.ETag)
	}
	if !req.Conditional.LastModified.IsZero() {
		hreq.Header.Set("If-Modified-Since", req.Conditional.LastModified.UTC().Format(http.TimeFormat))
	}
	var resp *http.Response
	if f.h != nil {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, hreq)
		resp = rec.Result()
	} else {
		resp, err = http.DefaultClient.Do(hreq) //nolint:bodyclose // the body is returned to the caller
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w: %w", hreq.URL, err, core.ErrUpstream)
		}
	}
	out := &core.UpstreamResponse{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		NotModified:   resp.StatusCode == http.StatusNotModified,
		ETag:          resp.Header.Get("ETag"),
	}
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		out.LastModified = lm
	} else {
		out.LastModified = time.Time{}
	}
	return out, nil
}
