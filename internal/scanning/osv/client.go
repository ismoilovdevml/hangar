// Package osv is a client for the OSV vulnerability database API
// (https://google.github.io/osv.dev/api/): batched queries by package
// version and advisory details by id. It works against api.osv.dev or any
// mirror that serves the same API.
//
// Requests are rate limited (one request per MinInterval), and failures that
// can pass (network errors, 429, 5xx) are retried with exponential backoff,
// honouring Retry-After.
package osv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// DefaultURL is the public OSV API.
const DefaultURL = "https://api.osv.dev"

// MaxBatch is the largest number of queries the API accepts in one querybatch request.
const MaxBatch = 1000

// Defaults for Options.
const (
	DefaultMinInterval = 100 * time.Millisecond
	DefaultMaxAttempts = 4
	DefaultBaseBackoff = time.Second
	DefaultMaxBackoff  = 30 * time.Second
	DefaultMaxPages    = 20
	DefaultTimeout     = time.Minute
	// maxResponse bounds a response body.
	maxResponse = 64 << 20
)

// Options configure a Client. Every field is optional.
type Options struct {
	// BaseURL is the API root without /v1 (default DefaultURL).
	BaseURL string
	// HTTPClient sends the requests; it should honour the server's outbound
	// proxy settings. Default http.DefaultClient.
	HTTPClient *http.Client
	// BatchSize is the number of queries per querybatch request (1..MaxBatch).
	BatchSize int
	// MinInterval is the minimum time between the starts of two requests.
	MinInterval time.Duration
	// MaxAttempts per request, including the first.
	MaxAttempts int
	// BaseBackoff is the wait before the first retry; it doubles per attempt
	// up to MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxPages bounds the result pages followed for one query.
	MaxPages int
	// Timeout bounds one request attempt, body included.
	Timeout time.Duration
}

// Client calls the OSV API. It is safe for concurrent use.
type Client struct {
	base string
	opts Options

	mu   sync.Mutex
	next time.Time // earliest start of the next request
}

// New returns a client.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultURL
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("osv url %q must be an absolute http(s) URL: %w", opts.BaseURL, core.ErrInvalid)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.BatchSize <= 0 || opts.BatchSize > MaxBatch {
		opts.BatchSize = MaxBatch
	}
	if opts.MinInterval < 0 {
		opts.MinInterval = 0
	} else if opts.MinInterval == 0 {
		opts.MinInterval = DefaultMinInterval
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = DefaultBaseBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = DefaultMaxPages
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	return &Client{base: strings.TrimSuffix(opts.BaseURL, "/"), opts: opts}, nil
}

// BaseURL is the API root the client talks to.
func (c *Client) BaseURL() string { return c.base }

// Package names a package in an OSV ecosystem.
type Package struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
	PURL      string `json:"purl,omitempty"`
}

// Query asks for the advisories affecting one package version.
type Query struct {
	Package   Package `json:"package"`
	Version   string  `json:"version"`
	PageToken string  `json:"page_token,omitempty"`
}

// VulnRef is an advisory id with its last modification, as querybatch
// returns them (details come from Vuln).
type VulnRef struct {
	ID       string `json:"id"`
	Modified string `json:"modified"`
}

type batchRequest struct {
	Queries []Query `json:"queries"`
}

type batchResponse struct {
	Results []struct {
		Vulns         []VulnRef `json:"vulns"`
		NextPageToken string    `json:"next_page_token"`
	} `json:"results"`
}

// QueryBatch returns the advisories of every query, in query order. Queries
// are sent in batches of BatchSize, and paginated results are followed.
func (c *Client) QueryBatch(ctx context.Context, queries []Query) ([][]VulnRef, error) {
	out := make([][]VulnRef, len(queries))
	for start := 0; start < len(queries); start += c.opts.BatchSize {
		end := min(start+c.opts.BatchSize, len(queries))
		if err := c.queryChunk(ctx, queries[start:end], out[start:end]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// queryChunk runs one batch and follows next_page_token for the queries
// that have more results.
func (c *Client) queryChunk(ctx context.Context, queries []Query, out [][]VulnRef) error {
	pending := make([]int, len(queries)) // index into queries of each request entry
	req := make([]Query, len(queries))
	for i, q := range queries {
		pending[i] = i
		req[i] = Query{Package: q.Package, Version: q.Version}
	}
	for page := 0; len(req) > 0; page++ {
		if page >= c.opts.MaxPages {
			return fmt.Errorf("osv querybatch: more than %d result pages for one query", c.opts.MaxPages)
		}
		var resp batchResponse
		if err := c.do(ctx, http.MethodPost, "/v1/querybatch", batchRequest{Queries: req}, &resp); err != nil {
			return err
		}
		if len(resp.Results) != len(req) {
			return fmt.Errorf("osv querybatch: %d results for %d queries", len(resp.Results), len(req))
		}
		var nextReq []Query
		var nextPending []int
		for i, r := range resp.Results {
			qi := pending[i]
			out[qi] = append(out[qi], r.Vulns...)
			if r.NextPageToken != "" {
				q := queries[qi]
				nextReq = append(nextReq, Query{Package: q.Package, Version: q.Version, PageToken: r.NextPageToken})
				nextPending = append(nextPending, qi)
			}
		}
		req, pending = nextReq, nextPending
	}
	return nil
}

// Vuln returns the full advisory id (core.ErrNotFound if the database does
// not know it).
func (c *Client) Vuln(ctx context.Context, id string) (*Vuln, error) {
	if id == "" {
		return nil, fmt.Errorf("osv vuln: empty id: %w", core.ErrInvalid)
	}
	var v Vuln
	if err := c.do(ctx, http.MethodGet, "/v1/vulns/"+url.PathEscape(id), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// statusError is a non-2xx answer.
type statusError struct {
	code       int
	body       string
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	msg := fmt.Sprintf("osv answered %d", e.code)
	if e.body != "" {
		msg += ": " + e.body
	}
	return msg
}

func (e *statusError) Unwrap() error {
	if e.code == http.StatusNotFound {
		return core.ErrNotFound
	}
	return core.ErrUpstream
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code == http.StatusRequestTimeout || se.code >= 500
	}
	return true // transport errors
}

// do sends one request with rate limiting and retries, and decodes the JSON answer into out.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("osv: encode request: %w", err)
		}
	}
	var last error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		if attempt > 1 {
			wait := c.backoff(attempt - 1)
			var se *statusError
			if errors.As(last, &se) && se.retryAfter > 0 {
				wait = min(se.retryAfter, c.opts.MaxBackoff)
			}
			if err := sleep(ctx, wait); err != nil {
				return fmt.Errorf("osv %s %s: %w (after: %w)", method, path, err, last)
			}
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
		last = c.once(ctx, method, path, body, out)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil || !retryable(last) {
			break
		}
	}
	return fmt.Errorf("osv %s %s: %w", method, path, last)
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		se := &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			se.retryAfter = time.Duration(s) * time.Second
		}
		return se
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponse))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	return nil
}

// backoff is BaseBackoff * 2^(n-1), capped, with up to 20% jitter.
func (c *Client) backoff(n int) time.Duration {
	d := c.opts.BaseBackoff
	for i := 1; i < n && d < c.opts.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, c.opts.MaxBackoff)
	return d + time.Duration(rand.Int64N(int64(d)/5+1)) //nolint:gosec // jitter, not security
}

// wait blocks until the rate limit allows the next request.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	start := c.next
	if start.Before(now) {
		start = now
	}
	c.next = start.Add(c.opts.MinInterval)
	c.mu.Unlock()
	return sleep(ctx, time.Until(start))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
