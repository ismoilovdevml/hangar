package cac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Doer sends one REST API request: method and path under /api/v1, body
// encoded as JSON (nil for none), the JSON answer decoded into out (nil to
// discard it). Errors from the server are *APIError.
type Doer interface {
	Do(ctx context.Context, method, path string, body, out any) error
}

// Retry policy. The API asks clients to retry 429 and 5xx answers with
// backoff and never other 4xx (api/openapi.yaml, info.description). Only
// requests that cannot have taken effect twice are retried: reads, and any
// request refused with 429 before it was handled.
const (
	maxAttempts   = 3
	retryBackoff  = time.Second      // doubled per attempt
	maxRetryAfter = 30 * time.Second // longest Retry-After honoured
	maxAnswer     = 64 << 20         // largest answer read
)

// ClientOptions configure a Client.
type ClientOptions struct {
	// Server is the base URL of the Hangar server, e.g. https://hangar.example.com.
	Server string
	// Token is a personal access or service-account token, sent as a
	// Bearer credential and never printed.
	Token string
	// Timeout bounds each attempt of each request.
	Timeout time.Duration
	// UserAgent identifies the client.
	UserAgent string
	// Transport overrides http.DefaultTransport (tests).
	Transport http.RoundTripper
}

// Client talks to the Hangar REST API.
type Client struct {
	base    string // scheme://host[/prefix]/api/v1
	token   string
	timeout time.Duration
	agent   string
	http    *http.Client
	// sleep waits between attempts; replaced in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

var _ Doer = (*Client)(nil)

// NewClient checks the options and returns a client.
func NewClient(o ClientOptions) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(o.Server, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("server %q: want an http:// or https:// URL", o.Server)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("server: pass the base URL without credentials, query or fragment")
	}
	if o.Token == "" {
		return nil, errors.New("no API token: set HANGAR_TOKEN or pass --token-file")
	}
	if o.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	base := strings.TrimSuffix(u.String(), "/api/v1") + "/api/v1"
	rt := o.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &Client{
		base: base, token: o.Token, timeout: o.Timeout, agent: o.UserAgent,
		http: &http.Client{
			Transport: rt,
			// A redirect could carry the token to another host; the API
			// never redirects, so one means --server is not the API's address.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("the server redirected to %s; pass the address it redirects to as --server", req.URL.Redacted())
			},
		},
		sleep: sleepCtx,
	}, nil
}

// Server is the API base URL, for messages.
func (c *Client) Server() string { return strings.TrimSuffix(c.base, "/api/v1") }

// APIError is an error answer of the API (schema Error).
type APIError struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
	// Fields maps request fields to what is wrong with them
	// (details.fields), when the server names them.
	Fields map[string]string
}

// Error renders "METHOD path: status code: message".
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	code := e.Code
	if code == "" {
		code = strings.ToLower(strings.ReplaceAll(http.StatusText(e.Status), " ", "_"))
	}
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, code, msg)
}

// isStatus reports whether err is an API answer with status code.
func isStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

// Do implements Doer.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		payload = b
	}
	for attempt := 1; ; attempt++ {
		status, header, answer, err := c.once(ctx, method, path, payload)
		wait, retry := c.retryAfter(ctx, method, status, header, err, attempt)
		if retry {
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		switch {
		case err != nil:
			return err
		case status >= http.StatusBadRequest:
			return apiError(method, path, status, answer)
		case out != nil && status != http.StatusNoContent && len(answer) > 0:
			if err := json.Unmarshal(answer, out); err != nil {
				return fmt.Errorf("%s %s: decode answer: %w", method, path, err)
			}
		}
		return nil
	}
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.agent != "" {
		req.Header.Set("User-Agent", c.agent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: read answer: %w", method, path, err)
	}
	if len(answer) > maxAnswer {
		return 0, nil, nil, fmt.Errorf("%s %s: answer larger than %d bytes", method, path, maxAnswer)
	}
	return resp.StatusCode, resp.Header, answer, nil
}

// retryAfter decides whether attempt is retried and how long to wait first.
func (c *Client) retryAfter(ctx context.Context, method string, status int, header http.Header, err error, attempt int) (time.Duration, bool) {
	if attempt >= maxAttempts || ctx.Err() != nil {
		return 0, false
	}
	read := method == http.MethodGet
	switch {
	case err != nil:
		if !read {
			return 0, false
		}
	case status == http.StatusTooManyRequests:
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		if !read {
			return 0, false
		}
	default:
		return 0, false
	}
	wait := retryBackoff << (attempt - 1)
	if s, perr := strconv.Atoi(header.Get("Retry-After")); perr == nil && s >= 0 {
		wait = min(time.Duration(s)*time.Second, maxRetryAfter)
	}
	return wait, true
}

func apiError(method, path string, status int, answer []byte) error {
	e := &APIError{Method: method, Path: path, Status: status}
	var body struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if json.Unmarshal(answer, &body) == nil {
		e.Code, e.Message = body.Code, body.Message
		if fields, ok := body.Details["fields"].(map[string]any); ok {
			e.Fields = map[string]string{}
			for k, v := range fields {
				e.Fields[k] = fmt.Sprint(v)
			}
		}
	}
	return e
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// escapePath escapes one path segment (names may hold spaces: role names).
func escapePath(s string) string { return url.PathEscape(s) }
