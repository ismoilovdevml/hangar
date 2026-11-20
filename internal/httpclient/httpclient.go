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
//
// The server-wide proxy and trust settings (Outbound) can be replaced while
// the server runs (SetOutbound): clients already handed out pick the new
// settings up with their next request.
//
// Each client owns its transport (connection pool). A request finds it with
// two atomic loads: the settings in effect are an immutable value swapped as
// a whole, and the transport is rebuilt only when they changed. Clients are
// cached by remote, settings and credentials, up to Options.MaxClients; the
// least recently requested one is dropped from the cache beyond that. A
// dropped client keeps working for whoever holds it, and its idle
// connections close after IdleConnTimeout like any other.
package httpclient

import (
	"container/list"
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// HTTP/2 connection health: a ping is sent after http2PingAfter without
// frames, and the connection is closed when it is not answered within
// http2PingTimeout.
const (
	http2PingAfter   = 30 * time.Second
	http2PingTimeout = 15 * time.Second
)

// Options are the server-wide defaults (config section `http`).
type Options struct {
	Proxy          string
	ProxyUsername  string
	ProxyPassword  string
	NoProxy        []string
	UseEnvironment bool
	// CAFile is a PEM bundle added to the system roots for every remote.
	CAFile string
	// CAPEM is a PEM bundle added like CAFile.
	CAPEM                 string
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
	// Connections to the operator's outbound proxies (Outbound.Proxies and
	// the environment proxy) are not checked: they may well be internal, and
	// the proxy enforces its own policy for the targets it reaches. A proxy
	// a repository sets for itself (UpstreamHTTP.Proxy) is checked like any
	// other address.
	DialGuard func(netip.Addr) error
	// DialAllowHost, when set, exempts host names from DialGuard (allow-listed
	// internal mirrors, which resolve to internal addresses by design).
	DialAllowHost func(host string) bool
	// DNS, when set, resolves the host names of every connection (remotes
	// and outbound proxies) through this cache instead of asking the system
	// resolver on each dial. DialGuard still checks each address dialled.
	DNS *DNSCache
	// MaxClients bounds the clients ForUpstream caches (default
	// DefaultMaxClients). Callers such as the README image fetcher ask for
	// a client per remote host, and repositories whose settings changed
	// leave their old client behind, so an unbounded cache only grows.
	MaxClients int

	// environ overrides os.Getenv for tests.
	environ func(string) string
}

// FileProxyName names the proxy http.proxy configures (hangar.yaml or
// HANGAR_HTTP_PROXY). It is the platform default.
const FileProxyName = "config"

// Proxy is a named outbound proxy.
type Proxy struct {
	Name string
	// URL is http://, https:// or socks5:// host:port, without credentials.
	URL      string
	Username string
	Password string
	// NoProxy lists hosts, domain suffixes (.corp.example) and CIDRs reached
	// directly.
	NoProxy []string
	// RedirectHosts, when not empty, limits the proxy to the repository's
	// remote host and these host patterns ("cdn.example.com",
	// ".cloudfront.net"); requests to other hosts, such as redirects
	// elsewhere, go direct. Empty: every host uses the proxy.
	RedirectHosts []string
	// CAPEM is a PEM bundle trusted for requests through this proxy (a
	// TLS-inspecting proxy's CA) in addition to the system roots.
	CAPEM string
}

// Outbound are the server-wide proxy and trust settings.
type Outbound struct {
	// Proxies are the named proxies repositories can choose.
	Proxies []Proxy
	// Default names the platform default proxy, used by repositories that
	// inherit. Empty: HTTPS_PROXY / HTTP_PROXY / NO_PROXY when
	// Options.UseEnvironment is set, else direct.
	Default string
	// CAFile is a PEM bundle added to the system roots for every request.
	CAFile string
}

// namedProxy is a validated Proxy.
type namedProxy struct {
	Proxy
	url   *url.URL                         // with credentials
	route func(*url.URL) (*url.URL, error) // honours NoProxy
}

// DefaultMaxClients is the default of Options.MaxClients: room for every
// proxy repository of a large installation plus the README image hosts
// recently fetched from.
const DefaultMaxClients = 1024

// Factory implements core.HTTPClientFactory. It is safe for concurrent use.
type Factory struct {
	opts Options

	// settings are the outbound settings in effect, read without a lock on
	// every request and dial.
	settings atomic.Pointer[outboundState]

	// mu guards the client cache and serializes SetOutbound.
	mu      sync.Mutex
	clients map[string]*list.Element // cacheKey -> element of lru
	lru     *list.List               // of *cachedClient, most recently requested first
}

// outboundState is one version of the server-wide settings with what is
// derived from them. SetOutbound replaces it as a whole; it is never
// modified, so readers need no lock.
type outboundState struct {
	out       Outbound
	baseRoots *x509.CertPool
	proxies   map[string]*namedProxy
	envProxy  func(*url.URL) (*url.URL, error) // nil = no environment proxy
	// proxyAddrs are the host:port of the operator's outbound proxies
	// (Outbound.Proxies and the environment proxy); connections to them skip
	// DialGuard. Per-repository proxies are never added.
	proxyAddrs map[string]bool
}

// cachedClient is an entry of the client cache.
type cachedClient struct {
	key    string
	client *http.Client
	live   *liveTransport
}

var _ core.HTTPClientFactory = (*Factory)(nil)

// New validates opts and returns a factory. A non-empty opts.Proxy becomes
// the proxy FileProxyName and the platform default.
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
	if opts.MaxClients <= 0 {
		opts.MaxClients = DefaultMaxClients
	}
	f := &Factory{opts: opts, clients: map[string]*list.Element{}, lru: list.New()}
	if err := f.SetOutbound(FileOutbound(opts)); err != nil {
		return nil, err
	}
	return f, nil
}

// FileOutbound returns the outbound settings opts configure: the proxy
// FileProxyName as the default when opts.Proxy is set.
func FileOutbound(opts Options) Outbound {
	o := Outbound{CAFile: opts.CAFile}
	if opts.Proxy != "" {
		o.Proxies = []Proxy{{Name: FileProxyName, URL: opts.Proxy, Username: opts.ProxyUsername, Password: opts.ProxyPassword,
			NoProxy: opts.NoProxy, CAPEM: opts.CAPEM}}
		o.Default = FileProxyName
	}
	return o
}

// SetOutbound replaces the proxies and trust settings. Clients returned by
// ForUpstream before keep working and use o from their next request on;
// idle connections made with the old settings are closed. On error nothing
// changes.
func (f *Factory) SetOutbound(o Outbound) error {
	o = cloneOutbound(o)
	roots, err := rootsFor(o.CAFile)
	if err != nil {
		return err
	}
	addrs := map[string]bool{}
	proxies := make(map[string]*namedProxy, len(o.Proxies))
	for _, p := range o.Proxies {
		np, err := compileProxy(p)
		if err != nil {
			return err
		}
		if _, dup := proxies[p.Name]; dup {
			return fmt.Errorf("proxy %q is defined twice: %w", p.Name, core.ErrInvalid)
		}
		proxies[p.Name] = np
		addProxyAddr(addrs, np.url)
	}
	if o.Default != "" && proxies[o.Default] == nil {
		return fmt.Errorf("default proxy %q is not defined: %w", o.Default, core.ErrInvalid)
	}
	st := &outboundState{out: o, baseRoots: roots, proxies: proxies, envProxy: f.environmentProxy(addrs), proxyAddrs: addrs}
	f.mu.Lock()
	f.settings.Store(st)
	live := make([]*liveTransport, 0, f.lru.Len())
	for e := f.lru.Front(); e != nil; e = e.Next() {
		live = append(live, e.Value.(*cachedClient).live)
	}
	f.mu.Unlock()
	// Connections made with the old settings are not reused: each client
	// builds a new transport with its next request. Closing the idle ones
	// now frees them at once for the cached clients; a client dropped from
	// the cache closes them when it next sends, or IdleConnTimeout does.
	for _, l := range live {
		l.CloseIdleConnections()
	}
	return nil
}

// Outbound returns the settings in effect.
func (f *Factory) Outbound() Outbound {
	return cloneOutbound(f.settings.Load().out)
}

// HasProxy reports whether a proxy called name is defined.
func (f *Factory) HasProxy(name string) bool {
	return f.settings.Load().proxies[name] != nil
}

func cloneOutbound(o Outbound) Outbound {
	o.Proxies = slices.Clone(o.Proxies)
	for i := range o.Proxies {
		o.Proxies[i].NoProxy = slices.Clone(o.Proxies[i].NoProxy)
		o.Proxies[i].RedirectHosts = slices.Clone(o.Proxies[i].RedirectHosts)
	}
	return o
}

func compileProxy(p Proxy) (*namedProxy, error) {
	pu, err := proxyURL(p.URL, p.Username, p.Password)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", p.Name, err)
	}
	if p.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(p.CAPEM)) {
		return nil, fmt.Errorf("proxy %q: CA bundle has no PEM certificates: %w", p.Name, core.ErrInvalid)
	}
	cfg := httpproxy.Config{HTTPProxy: pu.String(), HTTPSProxy: pu.String(), NoProxy: strings.Join(p.NoProxy, ",")}
	return &namedProxy{Proxy: p, url: pu, route: cfg.ProxyFunc()}, nil
}

// environmentProxy returns the HTTPS_PROXY / HTTP_PROXY / NO_PROXY proxy
// function (nil when unset or not used) and adds its addresses to addrs.
func (f *Factory) environmentProxy(addrs map[string]bool) func(*url.URL) (*url.URL, error) {
	if !f.opts.UseEnvironment {
		return nil
	}
	cfg := httpproxy.Config{
		HTTPProxy:  firstEnv(f.opts.environ, "HTTP_PROXY", "http_proxy"),
		HTTPSProxy: firstEnv(f.opts.environ, "HTTPS_PROXY", "https_proxy"),
		NoProxy:    firstEnv(f.opts.environ, "NO_PROXY", "no_proxy"),
	}
	if len(f.opts.NoProxy) > 0 {
		cfg.NoProxy = strings.Trim(cfg.NoProxy+","+strings.Join(f.opts.NoProxy, ","), ",")
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		return nil
	}
	for _, raw := range []string{cfg.HTTPProxy, cfg.HTTPSProxy} {
		if u, err := parseLooseProxy(raw); err == nil {
			addProxyAddr(addrs, u)
		}
	}
	return cfg.ProxyFunc()
}

// EnvironmentProxy is the proxy HTTPS_PROXY (else HTTP_PROXY) names, with
// any password masked; empty when unset or when the environment is not
// used.
func (f *Factory) EnvironmentProxy() string {
	if !f.opts.UseEnvironment {
		return ""
	}
	return core.RedactURL(firstEnv(f.opts.environ, "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"))
}

func rootsFor(caFile string) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // operator-configured CA bundle path
		if err != nil {
			return nil, fmt.Errorf("http.ca_file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("http.ca_file %s: no PEM certificates found", caFile)
		}
	}
	return roots, nil
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

// ProxyFor reports which proxy a request to target would use (nil = direct)
// for a remote whose own host is target's. Exposed for diagnostics ("why
// can't Hangar reach Maven Central?") and tests.
func (f *Factory) ProxyFor(target string, cfg core.UpstreamHTTP) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", target, core.ErrInvalid)
	}
	fn, _, err := f.route(cfg, u.Host)
	if err != nil || fn == nil {
		return nil, err
	}
	return fn(&http.Request{URL: u})
}

// route is outboundState.route under the settings in effect.
func (f *Factory) route(cfg core.UpstreamHTTP, remoteHost string) (func(*http.Request) (*url.URL, error), *namedProxy, error) {
	return f.settings.Load().route(cfg, remoteHost)
}

// route returns the proxy function for requests of a remote at remoteHost
// with cfg (nil = direct) and the named proxy it uses, if any.
func (st *outboundState) route(cfg core.UpstreamHTTP, remoteHost string) (func(*http.Request) (*url.URL, error), *namedProxy, error) {
	if err := cfg.ValidateProxy(); err != nil {
		return nil, nil, err
	}
	var np *namedProxy
	switch {
	case cfg.ProxyName != "":
		if np = st.proxies[cfg.ProxyName]; np == nil {
			return nil, nil, fmt.Errorf("proxy %q does not exist: %w", cfg.ProxyName, core.ErrInvalid)
		}
	case cfg.Proxy == core.ProxyNone:
		return nil, nil, nil
	case cfg.Proxy == "" || cfg.Proxy == core.ProxyInherit:
		if st.out.Default == "" {
			if env := st.envProxy; env != nil {
				return func(r *http.Request) (*url.URL, error) { return env(r.URL) }, nil, nil
			}
			return nil, nil, nil
		}
		np = st.proxies[st.out.Default]
	default:
		pu, err := proxyURL(cfg.Proxy, "", "")
		if err != nil {
			return nil, nil, err
		}
		// A repository's own proxy is chosen by whoever administers the
		// repository, not by the operator: connections to it stay under
		// DialGuard (it is not added to proxyAddrs), or a repository could
		// name an internal service or the cloud metadata endpoint as its
		// "proxy" and reach it.
		return http.ProxyURL(pu), nil, nil
	}
	remote := strings.ToLower(remoteHost)
	return func(r *http.Request) (*url.URL, error) {
		if len(np.RedirectHosts) > 0 && !strings.EqualFold(r.URL.Host, remote) && !core.MatchHost(np.RedirectHosts, r.URL.Host) {
			return nil, nil
		}
		return np.route(r.URL)
	}, np, nil
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
	if c := f.cached(key); c != nil {
		return c, nil
	}
	// The transport is built now, outside the lock, so that invalid settings
	// fail here rather than on the first request.
	live := &liveTransport{f: f, cfg: cfg, remoteHost: remote.Host}
	if _, err := live.current(); err != nil {
		return nil, err
	}
	c := &http.Client{
		Transport: &decorate{base: live, scheme: remote.Scheme, host: remote.Host, auth: auth, userAgent: f.opts.UserAgent},
		// No overall Timeout: large artifacts stream for minutes. Callers bound
		// requests with their context; headers are bounded by ReadTimeout.
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.clients[key]; ok {
		// Another caller built the same client meanwhile; ours has not
		// connected yet and is dropped.
		f.lru.MoveToFront(e)
		return e.Value.(*cachedClient).client, nil
	}
	f.clients[key] = f.lru.PushFront(&cachedClient{key: key, client: c, live: live})
	for f.lru.Len() > f.opts.MaxClients {
		oldest := f.lru.Back()
		f.lru.Remove(oldest)
		delete(f.clients, oldest.Value.(*cachedClient).key)
	}
	return c, nil
}

// cached returns the cached client for key, marking it recently used, or nil.
func (f *Factory) cached(key string) *http.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.clients[key]
	if !ok {
		return nil
	}
	f.lru.MoveToFront(e)
	return e.Value.(*cachedClient).client
}

// liveTransport sends each request with a transport built for the settings
// in effect, so SetOutbound reaches clients already handed out. Finding it
// takes two atomic loads and no lock; it is rebuilt only after the settings
// changed.
type liveTransport struct {
	f          *Factory
	cfg        core.UpstreamHTTP
	remoteHost string

	mu  sync.Mutex // serializes rebuilds
	cur atomic.Pointer[builtTransport]
}

// builtTransport is a transport and the settings it was built for.
type builtTransport struct {
	settings *outboundState
	t        *http.Transport
}

// current returns the transport for the settings in effect, building it when
// they changed since the last request. The transport of the previous
// settings has its idle connections closed.
func (l *liveTransport) current() (*http.Transport, error) {
	st := l.f.settings.Load()
	if b := l.cur.Load(); b != nil && b.settings == st {
		return b.t, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st = l.f.settings.Load()
	if b := l.cur.Load(); b != nil && b.settings == st {
		return b.t, nil
	}
	t, err := l.f.transport(st, l.cfg, l.remoteHost)
	if err != nil {
		return nil, err
	}
	if old := l.cur.Swap(&builtTransport{settings: st, t: t}); old != nil {
		old.t.CloseIdleConnections()
	}
	return t, nil
}

func (l *liveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t, err := l.current()
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	return t.RoundTrip(r)
}

// CloseIdleConnections closes the idle connections of the current transport.
func (l *liveTransport) CloseIdleConnections() {
	if b := l.cur.Load(); b != nil {
		b.t.CloseIdleConnections()
	}
}

// transport builds a transport for cfg under the settings st.
func (f *Factory) transport(st *outboundState, cfg core.UpstreamHTTP, remoteHost string) (*http.Transport, error) {
	proxy, np, err := st.route(cfg, remoteHost)
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
	roots := st.baseRoots
	if cfg.CABundle != "" || (np != nil && np.CAPEM != "") {
		roots = st.baseRoots.Clone()
		if cfg.CABundle != "" && !roots.AppendCertsFromPEM([]byte(cfg.CABundle)) {
			return nil, fmt.Errorf("caBundle: no PEM certificates found: %w", core.ErrInvalid)
		}
		if np != nil && np.CAPEM != "" {
			roots.AppendCertsFromPEM([]byte(np.CAPEM))
		}
	}
	idlePerHost := f.opts.MaxIdleConnsPerHost
	if cfg.MaxIdleConnsPerHost > 0 {
		idlePerHost = cfg.MaxIdleConnsPerHost
	}
	// HTTP/2 is negotiated by ALPN (also through CONNECT proxies); HTTP/1.1
	// keeps working for remotes and proxies that do not offer it. Health
	// pings detect HTTP/2 connections that died silently (NAT or proxy idle
	// timeouts), which would otherwise hang every stream multiplexed on them.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if !cfg.DisableHTTP2 {
		protocols.SetHTTP2(true)
	}
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           f.dialContext(connect),
		Protocols:             protocols,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: http2PingAfter, PingTimeout: http2PingTimeout},
		MaxIdleConns:          f.opts.MaxIdleConns,
		MaxIdleConnsPerHost:   idlePerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
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
// an operator-configured proxy, guarded by DialGuard for everything else. With
// Options.DNS, host names are resolved through the cache.
func (f *Factory) dialContext(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	plain := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
		if f.opts.DNS == nil {
			return d.DialContext(ctx, network, addr)
		}
		return dialCached(ctx, d, f.opts.DNS, timeout, network, addr)
	}
	if f.opts.DialGuard == nil {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, plain, network, addr)
		}
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
		toProxy := f.settings.Load().proxyAddrs[strings.ToLower(addr)]
		if toProxy || (f.opts.DialAllowHost != nil && f.opts.DialAllowHost(strings.ToLower(strings.TrimSuffix(host, ".")))) {
			return dial(ctx, plain, network, addr)
		}
		return dial(ctx, guarded, network, addr)
	}
}

// addProxyAddr adds u's host:port (default port by scheme) to addrs.
func addProxyAddr(addrs map[string]bool, u *url.URL) {
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
	addrs[strings.ToLower(net.JoinHostPort(u.Hostname(), port))] = true
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
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%d\x00%d\x00%d\x00%d\x00%t", remote.Scheme, remote.Host, cfg.Proxy, cfg.ProxyName, cfg.CABundle,
		cfg.InsecureSkipVerify, cfg.ConnectTimeout, cfg.ReadTimeout, cfg.MaxConnsPerHost, cfg.MaxIdleConnsPerHost, cfg.DisableHTTP2)
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
