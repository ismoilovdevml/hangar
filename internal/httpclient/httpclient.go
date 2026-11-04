// Package httpclient builds the outbound HTTP clients Hangar uses to reach
// proxy-repository remotes. It implements core.HTTPClientFactory.
//
// Proxy selection for a remote, first match wins:
//
//  1. UpstreamHTTP.Proxy = "none"          -> direct connection
//  2. UpstreamHTTP.Proxy = <url>           -> that proxy, always
//  3. server-wide http.proxy is set        -> that proxy, except hosts in http.no_proxy
//  4. http.use_environment (default true)  -> HTTPS_PROXY / HTTP_PROXY / NO_PROXY
//  5. otherwise                            -> direct
//
// http, https and socks5 proxies are supported; credentials go in the URL or in
// http.proxy_username / http.proxy_password. Trusted roots are the system pool
// plus http.ca_file plus the remote's own CABundle.
package httpclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Options are the server-wide defaults (config section `http`).
type Options struct {
	Proxy          string
	ProxyUsername  string
	ProxyPassword  string
	NoProxy        []string
	UseEnvironment bool
	// CAFile is a PEM bundle added to the system roots for every remote.
	CAFile                string
	ConnectTimeout        time.Duration
	ReadTimeout           time.Duration // response header timeout
	IdleConnTimeout       time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	UserAgent             string
	TLSHandshakeTimeout   time.Duration
	ExpectContinueTimeout time.Duration

	// DialGuard, when set, is called with the IP address of every direct
	// connection, after DNS resolution and right before connecting (from
	// net.Dialer.Control). An error aborts the connection. This closes the DNS
	// rebinding gap of URL checks: a name that resolved to a public address
	// when checked cannot resolve to an internal one when dialled.
	// Connections to an outbound proxy are not checked (the proxy is
	// configured by the operator and may well be internal); the proxy
	// enforces its own policy for the targets it reaches.
	DialGuard func(netip.Addr) error
	// DialAllowHost, when set, exempts host names from DialGuard (allow-listed
	// internal mirrors, which resolve to internal addresses by design).
	DialAllowHost func(host string) bool

	// environ overrides os.Getenv for tests.
	environ func(string) string
}

// Factory implements core.HTTPClientFactory. It is safe for concurrent use.
type Factory struct {
	opts      Options
	baseRoots *x509.CertPool
	global    func(*url.URL) (*url.URL, error) // nil = direct

	mu      sync.Mutex
	clients map[string]*http.Client
	// proxyAddrs are the host:port of every configured outbound proxy;
	// connections to them skip DialGuard. Guarded by mu after New.
	proxyAddrs map[string]bool
}

var _ core.HTTPClientFactory = (*Factory)(nil)

// New validates opts and returns a factory.
func New(opts Options) (*Factory, error) {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 60 * time.Second
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = 90 * time.Second
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = 200
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 32
	}
	if opts.TLSHandshakeTimeout <= 0 {
		opts.TLSHandshakeTimeout = opts.ConnectTimeout
	}
	if opts.ExpectContinueTimeout <= 0 {
		opts.ExpectContinueTimeout = time.Second
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Hangar"
	}
	if opts.environ == nil {
		opts.environ = os.Getenv
	}

	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("http.ca_file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("http.ca_file %s: no PEM certificates found", opts.CAFile)
		}
	}

	f := &Factory{opts: opts, baseRoots: roots, clients: map[string]*http.Client{}, proxyAddrs: map[string]bool{}}
	switch {
	case opts.Proxy != "":
		pu, err := proxyURL(opts.Proxy, opts.ProxyUsername, opts.ProxyPassword)
		if err != nil {
			return nil, fmt.Errorf("http.proxy: %w", err)
		}
		f.addProxyAddr(pu)
		cfg := httpproxy.Config{HTTPProxy: pu.String(), HTTPSProxy: pu.String(), NoProxy: strings.Join(opts.NoProxy, ",")}
		f.global = cfg.ProxyFunc()
	case opts.UseEnvironment:
		cfg := httpproxy.Config{
			HTTPProxy:  firstEnv(opts.environ, "HTTP_PROXY", "http_proxy"),
			HTTPSProxy: firstEnv(opts.environ, "HTTPS_PROXY", "https_proxy"),
			NoProxy:    firstEnv(opts.environ, "NO_PROXY", "no_proxy"),
		}
		if len(opts.NoProxy) > 0 {
			cfg.NoProxy = strings.Trim(cfg.NoProxy+","+strings.Join(opts.NoProxy, ","), ",")
		}
		if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
			f.global = cfg.ProxyFunc()
			for _, raw := range []string{cfg.HTTPProxy, cfg.HTTPSProxy} {
				if u, err := parseLooseProxy(raw); err == nil {
					f.addProxyAddr(u)
				}
			}
		}
	}
	return f, nil
}

func firstEnv(env func(string) string, names ...string) string {
	for _, n := range names {
		if v := env(n); v != "" {
			return v
		}
	}
	return ""
}

func proxyURL(raw, user, pass string) (*url.URL, error) {
	if err := (core.UpstreamHTTP{Proxy: raw}).ValidateProxy(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(raw)
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u, nil
}

// ProxyFor reports which proxy a request to target would use (nil = direct).
// Exposed for diagnostics ("why can't Hangar reach Maven Central?") and tests.
func (f *Factory) ProxyFor(target string, cfg core.UpstreamHTTP) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", target, core.ErrInvalid)
	}
	fn, err := f.proxyFunc(cfg)
	if err != nil || fn == nil {
		return nil, err
	}
	return fn(&http.Request{URL: u})
}

func (f *Factory) proxyFunc(cfg core.UpstreamHTTP) (func(*http.Request) (*url.URL, error), error) {
	switch cfg.Proxy {
	case core.ProxyNone:
		return nil, nil
	case "", core.ProxyInherit:
		if f.global == nil {
			return nil, nil
		}
		g := f.global
		return func(r *http.Request) (*url.URL, error) { return g(r.URL) }, nil
	default:
		pu, err := proxyURL(cfg.Proxy, "", "")
		if err != nil {
			return nil, err
		}
		return http.ProxyURL(pu), nil
	}
}

// ForUpstream implements core.HTTPClientFactory.
func (f *Factory) ForUpstream(remoteURL string, cfg core.UpstreamHTTP, auth *core.UpstreamAuth) (*http.Client, error) {
	remote, err := url.Parse(remoteURL)
	if err != nil || remote.Host == "" || (remote.Scheme != "http" && remote.Scheme != "https") {
		return nil, fmt.Errorf("remote url %q must be absolute http(s): %w", remoteURL, core.ErrInvalid)
	}
	if auth != nil {
		if err := auth.Validate(); err != nil {
			return nil, err
		}
	}
	key := cacheKey(remote, cfg, auth)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[key]; ok {
		return c, nil
	}
	tr, err := f.transport(cfg)
	if err != nil {
		return nil, err
	}
	c := &http.Client{
		Transport: &decorate{base: tr, scheme: remote.Scheme, host: remote.Host, auth: auth, userAgent: f.opts.UserAgent},
		// No overall Timeout: large artifacts stream for minutes. Callers bound
		// requests with their context; headers are bounded by ReadTimeout.
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	f.clients[key] = c
	return c, nil
}

func (f *Factory) transport(cfg core.UpstreamHTTP) (*http.Transport, error) {
	proxy, err := f.proxyFunc(cfg)
	if err != nil {
		return nil, err
	}
	connect := f.opts.ConnectTimeout
	if cfg.ConnectTimeout > 0 {
		connect = cfg.ConnectTimeout.D()
	}
	read := f.opts.ReadTimeout
	if cfg.ReadTimeout > 0 {
		read = cfg.ReadTimeout.D()
	}
	roots := f.baseRoots
	if cfg.CABundle != "" {
		roots = f.baseRoots.Clone()
		if !roots.AppendCertsFromPEM([]byte(cfg.CABundle)) {
			return nil, fmt.Errorf("caBundle: no PEM certificates found: %w", core.ErrInvalid)
		}
	}
	if cfg.Proxy != "" && cfg.Proxy != core.ProxyNone && cfg.Proxy != core.ProxyInherit {
		if pu, err := proxyURL(cfg.Proxy, "", ""); err == nil {
			f.addProxyAddr(pu) // f.mu is held by ForUpstream
		}
	}
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           f.dialContext(connect),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          f.opts.MaxIdleConns,
		MaxIdleConnsPerHost:   f.opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       f.opts.IdleConnTimeout,
		TLSHandshakeTimeout:   f.opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: read,
		ExpectContinueTimeout: f.opts.ExpectContinueTimeout,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			RootCAs:            roots,
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicit, audited per-remote opt-in
		},
	}, nil
}

// dialContext returns the transport's dial function: plain for connections to
// a configured proxy, guarded by DialGuard for everything else.
func (f *Factory) dialContext(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	plain := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if f.opts.DialGuard == nil {
		return plain.DialContext
	}
	guard := f.opts.DialGuard
	guarded := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			a, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("dial %s: not an IP address", address)
			}
			return guard(a.WithZone(""))
		}}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		f.mu.Lock()
		toProxy := f.proxyAddrs[strings.ToLower(addr)]
		f.mu.Unlock()
		if toProxy || (f.opts.DialAllowHost != nil && f.opts.DialAllowHost(strings.ToLower(strings.TrimSuffix(host, ".")))) {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
}

// addProxyAddr remembers u's host:port (default port by scheme) as a proxy.
func (f *Factory) addProxyAddr(u *url.URL) {
	if u == nil || u.Hostname() == "" {
		return
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	f.proxyAddrs[strings.ToLower(net.JoinHostPort(u.Hostname(), port))] = true
}

// parseLooseProxy parses a proxy environment value, which may omit the scheme.
func parseLooseProxy(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return url.Parse(raw)
}

func cacheKey(remote *url.URL, cfg core.UpstreamHTTP, auth *core.UpstreamAuth) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%t\x00%d\x00%d", remote.Scheme, remote.Host, cfg.Proxy, cfg.CABundle,
		cfg.InsecureSkipVerify, cfg.ConnectTimeout, cfg.ReadTimeout)
	if auth != nil {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%s\x00%s", auth.Kind, auth.Username, auth.Password, auth.Token, auth.HeaderName)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// decorate adds the User-Agent and, for the remote's own origin only, credentials.
type decorate struct {
	base      http.RoundTripper
	scheme    string
	host      string
	auth      *core.UpstreamAuth
	userAgent string
}

func (d *decorate) RoundTrip(r *http.Request) (*http.Response, error) {
	needUA := r.Header.Get("User-Agent") == ""
	needAuth := d.auth != nil && r.URL.Scheme == d.scheme && strings.EqualFold(r.URL.Host, d.host) &&
		r.Header.Get("Authorization") == "" && (d.auth.Kind != core.UpstreamAuthHeader || r.Header.Get(d.auth.HeaderName) == "")
	if !needUA && !needAuth {
		return d.base.RoundTrip(r)
	}
	r = r.Clone(r.Context()) // RoundTrippers must not modify the caller's request
	if needUA {
		r.Header.Set("User-Agent", d.userAgent)
	}
	if needAuth {
		switch d.auth.Kind {
		case core.UpstreamAuthBasic:
			r.SetBasicAuth(d.auth.Username, d.auth.Password)
		case core.UpstreamAuthBearer:
			r.Header.Set("Authorization", "Bearer "+d.auth.Token)
		case core.UpstreamAuthHeader:
			r.Header.Set(d.auth.HeaderName, d.auth.Token)
		}
	}
	return d.base.RoundTrip(r)
}
