package docker

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Connectors
//
// OCI clients address a registry by host only: the image name follows the
// host directly. On the shared /v2/ endpoint the first path segment selects
// the Hangar repository (hangar.example.com/docker-hosted/team/app). A
// connector gives one docker repository an entry point of its own, where
// image names carry no repository prefix:
//
//   - connector.host: requests whose effective Host (X-Forwarded-Host only
//     from server.trusted_proxies) is that name: cr.example.com/team/app.
//   - connector.http_port: a plain-HTTP listener on that port (TLS terminated
//     in front) serving only that repository's /v2/ API.
//
// Registry tokens follow the naming of the entry point they were requested
// for: the challenge carries a service of its own (the connector host, or
// "<base_url host>:<port>"), the scopes name images without the repository
// prefix, and a token is only accepted on the entry point it was issued for.
// The token realm of a host connector is <base_url scheme>://<host>/v2/token,
// so clients that can only reach cr.example.com get their tokens there; port
// connectors use the main realm (<base_url>/v2/token), which recognises the
// connector by the service parameter.

// connectorRefresh bounds how long a loaded connector table is trusted;
// repository events invalidate it earlier.
const connectorRefresh = 30 * time.Second

// hostRE is a DNS host name, optionally with a port.
var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// naming is how /v2/ names map to repositories on one entry point.
type naming struct {
	repo    string // connector repository; "" = "<repository>/<image>" names
	service string // token audience and challenge service
	realm   string // token endpoint advertised in challenges
}

// split maps an OCI name to the repository and image path.
func (n naming) split(name string) (repo, image string, ok bool) {
	if n.repo == "" {
		return splitName(name)
	}
	if name == "" || len(name) > maxNameLength || !nameRE.MatchString(name) {
		return "", "", false
	}
	return n.repo, name, true
}

// public renders the name a client uses for image in repo on this entry point.
func (n naming) public(repo, image string) (string, bool) {
	if n.repo == "" {
		return repo + "/" + image, true
	}
	return image, repo == n.repo
}

type namingKey struct{}

func withNaming(ctx context.Context, n naming) context.Context {
	return context.WithValue(ctx, namingKey{}, n)
}

func namingFrom(ctx context.Context) (naming, bool) {
	n, ok := ctx.Value(namingKey{}).(naming)
	return n, ok
}

// connectorTable is the connectors of all docker repositories.
type connectorTable struct {
	hosts    map[string]string // lower-case host[:port] -> repository
	ports    map[int]string    // port -> repository
	services map[string]string // token service -> repository
	loaded   time.Time
}

// connectors loads and caches the connector table.
type connectors struct {
	repos core.RepositoryService
	now   func() time.Time

	mu      sync.Mutex
	table   *connectorTable
	stale   bool
	changed chan struct{} // closed and replaced on every invalidation
	group   singleflight.Group
}

func newConnectors(repos core.RepositoryService, now func() time.Time) *connectors {
	return &connectors{repos: repos, now: now, changed: make(chan struct{})}
}

// invalidate forces a reload on next use and wakes port watchers.
func (c *connectors) invalidate() {
	c.mu.Lock()
	c.stale = true
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func (c *connectors) get(ctx context.Context) (*connectorTable, error) {
	c.mu.Lock()
	t, stale := c.table, c.stale
	c.mu.Unlock()
	if t != nil && !stale && c.now().Sub(t.loaded) < connectorRefresh {
		return t, nil
	}
	v, err, _ := c.group.Do("load", func() (any, error) {
		c.mu.Lock()
		c.stale = false
		c.mu.Unlock()
		t, err := c.load(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.table = t
		c.mu.Unlock()
		return t, nil
	})
	if err != nil {
		if t != nil {
			return t, nil // keep routing with the last table while the database hiccups
		}
		return nil, err
	}
	return v.(*connectorTable), nil
}

func (c *connectors) load(ctx context.Context) (*connectorTable, error) {
	t := &connectorTable{hosts: map[string]string{}, ports: map[int]string{}, services: map[string]string{}, loaded: c.now()}
	q := core.RepositoryQuery{Format: Name, Limit: listPage}
	var repos []*core.Repository
	for {
		page, err := c.repos.List(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("list docker repositories: %w", err)
		}
		repos = append(repos, page.Items...)
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	slices.SortFunc(repos, func(a, b *core.Repository) int { return strings.Compare(a.Name, b.Name) })
	for _, r := range repos {
		cc := r.Config.Connector
		if r.Format != Name || cc == nil {
			continue
		}
		// Duplicates are rejected by Validate; if one slips in (older data),
		// the first repository by name keeps the entry point.
		if h := normalizeHost(cc.Host); h != "" {
			if _, dup := t.hosts[h]; !dup {
				t.hosts[h] = r.Name
			}
		}
		if cc.HTTPPort > 0 {
			if _, dup := t.ports[cc.HTTPPort]; !dup {
				t.ports[cc.HTTPPort] = r.Name
			}
		}
	}
	return t, nil
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// portService is the token service of a port connector.
func portService(baseHost string, port int) string {
	return baseHost + ":" + strconv.Itoa(port)
}

// localPort is the port of the listener that accepted r (0 if unknown).
func localPort(r *http.Request) int {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return 0
	}
	if ta, ok := addr.(*net.TCPAddr); ok {
		return ta.Port
	}
	_, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// defaultNaming is the naming of the main /v2/ endpoint.
func (h *handler) defaultNaming(r *http.Request) naming {
	base := h.baseURL(r)
	service := "hangar"
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		service = u.Host
	}
	return naming{service: service, realm: base + "/v2/token"}
}

// baseHostScheme returns the scheme and host name (no port) of base_url.
func (h *handler) baseHostScheme(r *http.Request) (scheme, host string) {
	u, err := url.Parse(h.baseURL(r))
	if err != nil || u.Host == "" {
		return "http", "hangar"
	}
	return u.Scheme, u.Hostname()
}

// namingFor resolves the entry point of r: a connector port, a connector
// host, or the main endpoint.
func (h *handler) namingFor(r *http.Request) (naming, error) {
	def := h.defaultNaming(r)
	t, err := h.conns.get(r.Context())
	if err != nil {
		return def, err
	}
	if len(t.ports) == 0 && len(t.hosts) == 0 {
		return def, nil
	}
	scheme, baseHost := h.baseHostScheme(r)
	if p := localPort(r); p > 0 {
		if repo, ok := t.ports[p]; ok {
			return naming{repo: repo, service: portService(baseHost, p), realm: def.realm}, nil
		}
	}
	host := core.RequestInfoFrom(r.Context()).Host
	if host == "" {
		host = r.Host
	}
	host = normalizeHost(host)
	repo, ok := t.hosts[host]
	if !ok {
		if hn, _, err := net.SplitHostPort(host); err == nil {
			host = hn
			repo, ok = t.hosts[hn]
		}
	}
	if ok {
		return naming{repo: repo, service: host, realm: scheme + "://" + host + "/v2/token"}, nil
	}
	return def, nil
}

// namingForService finds the connector whose token service is service (for
// token requests that reach the main endpoint).
func (h *handler) namingForService(r *http.Request, service string) (naming, bool) {
	t, err := h.conns.get(r.Context())
	if err != nil || service == "" {
		return naming{}, false
	}
	def := h.defaultNaming(r)
	scheme, baseHost := h.baseHostScheme(r)
	service = normalizeHost(service)
	if repo, ok := t.hosts[service]; ok {
		return naming{repo: repo, service: service, realm: scheme + "://" + service + "/v2/token"}, true
	}
	for p, repo := range t.ports {
		if portService(baseHost, p) == service {
			return naming{repo: repo, service: service, realm: def.realm}, true
		}
	}
	return naming{}, false
}

// validateConnector checks the connector of a docker repository: syntax,
// and (once the format serves requests) that no other repository and not
// the main endpoint already use the host or port.
func (f *Format) validateConnector(repo *core.Repository) error {
	cc := repo.Config.Connector
	if cc == nil {
		return nil
	}
	if cc.Host != "" {
		h := normalizeHost(cc.Host)
		if !hostRE.MatchString(h) || len(h) > 253 {
			return fmt.Errorf("repository %q: connector host %q is not a valid host name: %w", repo.Name, cc.Host, core.ErrInvalid)
		}
	}
	if cc.HTTPPort < 0 || cc.HTTPPort > 65535 {
		return fmt.Errorf("repository %q: connector http_port %d out of range: %w", repo.Name, cc.HTTPPort, core.ErrInvalid)
	}
	f.mu.Lock()
	h := f.h
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	ctx := context.Background()
	if cc.Host != "" && h.deps.BaseURL != "" {
		if u, err := url.Parse(h.deps.BaseURL); err == nil && (normalizeHost(u.Host) == normalizeHost(cc.Host) || normalizeHost(u.Hostname()) == normalizeHost(cc.Host)) {
			return fmt.Errorf("repository %q: connector host %q is the server's own base_url host: %w", repo.Name, cc.Host, core.ErrInvalid)
		}
	}
	t, err := h.conns.load(ctx)
	if err != nil {
		return err
	}
	if other, ok := t.hosts[normalizeHost(cc.Host)]; cc.Host != "" && ok && other != repo.Name {
		return fmt.Errorf("repository %q: connector host %q is already used by %q: %w", repo.Name, cc.Host, other, core.ErrAlreadyExists)
	}
	if other, ok := t.ports[cc.HTTPPort]; cc.HTTPPort > 0 && ok && other != repo.Name {
		return fmt.Errorf("repository %q: connector http_port %d is already used by %q: %w", repo.Name, cc.HTTPPort, other, core.ErrAlreadyExists)
	}
	return nil
}

// WatchConnectorPorts calls apply with the connector ports of all docker
// repositories (port -> repository name) now and whenever they change, until
// ctx ends. The server opens a listener per port; requests arriving on one
// are served by that repository only. It returns immediately if the format
// has not been mounted.
func (f *Format) WatchConnectorPorts(ctx context.Context, apply func(map[int]string)) {
	f.mu.Lock()
	h := f.h
	f.mu.Unlock()
	if h == nil {
		return
	}
	var last map[int]string
	tick := time.NewTicker(connectorRefresh)
	defer tick.Stop()
	for {
		h.conns.mu.Lock()
		changed := h.conns.changed
		h.conns.mu.Unlock()
		if t, err := h.conns.get(ctx); err != nil {
			h.log.Warn("docker: cannot load connector ports", "err", err)
		} else if last == nil || !maps.Equal(last, t.ports) {
			last = maps.Clone(t.ports)
			apply(maps.Clone(t.ports))
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-tick.C:
			h.conns.invalidate()
		}
	}
}
