package trivy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ScanPath is the adapter's scan endpoint.
const ScanPath = "/v1/scan"

// ScanRequest asks the adapter to scan one image manifest (schema of the
// adapter's POST /v1/scan).
type ScanRequest struct {
	// Image is "<registry host>/<repository>/<image>@sha256:<hex>": always
	// by digest.
	Image string `json:"image"`
	// RegistryToken is a short-lived bearer token for pulling Image.
	RegistryToken string `json:"registry_token"`
	// Insecure allows a plain-HTTP (or self-signed) registry.
	Insecure bool `json:"insecure,omitempty"`
	// Server is the Trivy server (client/server mode) and ServerToken its
	// --token, if it has one.
	Server      string `json:"server"`
	ServerToken string `json:"server_token,omitempty"`
	// TimeoutSeconds bounds the scan (Trivy's --timeout).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// ErrorResponse is the adapter's answer to a failed request.
type ErrorResponse struct {
	Error string `json:"error"`
}

// VersionInfo is the Trivy server's GET /version answer.
type VersionInfo struct {
	Version         string     `json:"Version"`
	VulnerabilityDB *DBVersion `json:"VulnerabilityDB,omitempty"`
	JavaDB          *DBVersion `json:"JavaDB,omitempty"`
}

// DBVersion describes one of the server's databases.
type DBVersion struct {
	Version      int        `json:"Version"`
	UpdatedAt    *time.Time `json:"UpdatedAt,omitempty"`
	NextUpdate   *time.Time `json:"NextUpdate,omitempty"`
	DownloadedAt *time.Time `json:"DownloadedAt,omitempty"`
}

// Options configure a Client.
type Options struct {
	// ServerURL is the Trivy server, e.g. http://trivy:4954. Required.
	ServerURL string
	// ServerToken is the server's --token (optional).
	ServerToken string
	// AdapterURL is the scan adapter, e.g. http://hangar-scanner:4955. Required.
	AdapterURL string
	// AdapterToken authenticates Hangar to the adapter. Required.
	AdapterToken string
	// Timeout bounds one image scan (default DefaultTimeout).
	Timeout time.Duration
	// HTTPClient is used for both (default: a client without proxy).
	HTTPClient *http.Client
}

// DefaultTimeout bounds one image scan.
const DefaultTimeout = 15 * time.Minute

// maxReport bounds a report read from the adapter.
const maxReport = 64 << 20

// Client calls the scan adapter and the Trivy server.
type Client struct {
	opts Options
}

// New validates opts and returns a client.
func New(opts Options) (*Client, error) {
	for name, raw := range map[string]string{"scanner.trivy_url": opts.ServerURL, "scanner.adapter_url": opts.AdapterURL} {
		if err := checkURL(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if opts.AdapterToken == "" {
		return nil, fmt.Errorf("scanner.adapter_token is required with scanner.adapter_url: %w", core.ErrInvalid)
	}
	opts.ServerURL = strings.TrimSuffix(opts.ServerURL, "/")
	opts.AdapterURL = strings.TrimSuffix(opts.AdapterURL, "/")
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.HTTPClient == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil // internal services: never through the outbound proxy
		opts.HTTPClient = &http.Client{Transport: t}
	}
	return &Client{opts: opts}, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%q must be an http:// or https:// URL: %w", raw, core.ErrInvalid)
	}
	return nil
}

// ServerURL is the Trivy server the scans use.
func (c *Client) ServerURL() string { return c.opts.ServerURL }

// Scan scans one image by digest. image must be "<host>/<path>@sha256:...".
func (c *Client) Scan(ctx context.Context, image, registryToken string, insecure bool) (*Report, error) {
	req := ScanRequest{
		Image: image, RegistryToken: registryToken, Insecure: insecure,
		Server: c.opts.ServerURL, ServerToken: c.opts.ServerToken,
		TimeoutSeconds: int(c.opts.Timeout / time.Second),
	}
	body, _ := json.Marshal(req)
	// The adapter's own timeout is Trivy's; leave it a minute to answer.
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout+time.Minute)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.AdapterURL+ScanPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.opts.AdapterToken)
	resp, err := c.opts.HTTPClient.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("scan adapter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReport+1))
	if err != nil {
		return nil, fmt.Errorf("scan adapter: read answer: %w", err)
	}
	if len(data) > maxReport {
		return nil, fmt.Errorf("scan adapter: report larger than %d MiB", maxReport>>20)
	}
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("scan adapter: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("scan adapter: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(firstBytes(data, 300)))
	}
	return ParseReport(data)
}

// ParseReport decodes a Trivy JSON report.
func ParseReport(data []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("trivy report: %w", err)
	}
	if r.SchemaVersion != 2 {
		return nil, fmt.Errorf("trivy report: schema version %d, want 2", r.SchemaVersion)
	}
	return &r, nil
}

// Version asks the Trivy server for its version and database dates.
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opts.ServerURL+"/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.opts.HTTPClient.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("trivy server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("trivy server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trivy server /version: HTTP %d", resp.StatusCode)
	}
	var v VersionInfo
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("trivy server /version: %w", err)
	}
	if v.Version == "" {
		return nil, errors.New("trivy server /version: no version in the answer")
	}
	return &v, nil
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
