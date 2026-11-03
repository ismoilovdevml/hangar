package core

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// RepoType is how a repository gets its content.
type RepoType string

const (
	// RepoHosted: clients publish into it.
	RepoHosted RepoType = "hosted"
	// RepoProxy: fetches from a remote on demand and caches.
	RepoProxy RepoType = "proxy"
	// RepoGroup: an ordered list of member repositories of the same format behind one URL.
	RepoGroup RepoType = "group"
)

// Valid reports whether t is one of the known types.
func (t RepoType) Valid() bool {
	return t == RepoHosted || t == RepoProxy || t == RepoGroup
}

// WritePolicy controls publishing into a hosted repository.
type WritePolicy string

const (
	// WriteAllow: publish and overwrite freely.
	WriteAllow WritePolicy = "allow"
	// WriteAllowOnce: a path can be written once; re-uploading identical bytes
	// is accepted (idempotent), different bytes are rejected with ErrWriteDenied.
	// Assets of kind AssetMetadata are exempt (formats regenerate them).
	WriteAllowOnce WritePolicy = "allow_once"
	// WriteDeny: read-only.
	WriteDeny WritePolicy = "deny"
)

// Valid reports whether p is one of the known policies.
func (p WritePolicy) Valid() bool {
	return p == WriteAllow || p == WriteAllowOnce || p == WriteDeny
}

var repoNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidateRepositoryName enforces the repository naming rule `^[a-z0-9][a-z0-9-]{1,62}$`.
func ValidateRepositoryName(name string) error {
	if !repoNameRE.MatchString(name) {
		return fmt.Errorf("repository name %q must match %s: %w", name, repoNameRE.String(), ErrInvalid)
	}
	return nil
}

// Repository is a named container of one format and one type.
type Repository struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Format      string     `json:"format"` // Format.Name() of the handler, e.g. "npm"
	Type        RepoType   `json:"type"`
	Description string     `json:"description,omitempty"`
	BlobStore   string     `json:"blob_store,omitempty"` // blob store name; empty for groups
	Online      bool       `json:"online"`
	Config      RepoConfig `json:"config"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// RepoConfig is the type- and format-specific configuration of a repository.
// Exactly one of Hosted, Proxy, Group is set, matching Repository.Type.
type RepoConfig struct {
	Hosted *HostedConfig `json:"hosted,omitempty" yaml:"hosted,omitempty"`
	Proxy  *ProxyConfig  `json:"proxy,omitempty" yaml:"proxy,omitempty"`
	Group  *GroupConfig  `json:"group,omitempty" yaml:"group,omitempty"`

	// AnonymousRead lets unauthenticated principals read (repo:<name>:read).
	AnonymousRead bool `json:"anonymous_read" yaml:"anonymous_read"`
	// ScanOnArrival enqueues a vulnerability scan for every published component.
	ScanOnArrival bool `json:"scan_on_arrival" yaml:"scan_on_arrival"`
	// QuarantineCritical blocks components with critical findings.
	QuarantineCritical bool `json:"quarantine_critical" yaml:"quarantine_critical"`
	// Connector optionally exposes the repository on its own host name or port.
	Connector *ConnectorConfig `json:"connector,omitempty" yaml:"connector,omitempty"`
	// CleanupPolicies lists cleanup policy names applied to this repository.
	CleanupPolicies []string `json:"cleanup_policies,omitempty" yaml:"cleanup_policies,omitempty"`

	// Format holds format-specific settings (e.g. Maven version policy). Its
	// shape is owned by the format package, which validates it in
	// Format.Validate and reads it with DecodeFormat.
	Format map[string]any `json:"format,omitempty" yaml:"format,omitempty"`
}

// DecodeFormat decodes the format-specific settings into v (a pointer to the
// format's own config struct, with json tags).
func (c RepoConfig) DecodeFormat(v any) error {
	if len(c.Format) == 0 {
		return nil
	}
	b, err := json.Marshal(c.Format)
	if err != nil {
		return fmt.Errorf("encode format config: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode format config: %w: %w", err, ErrInvalid)
	}
	return nil
}

// HostedConfig configures a hosted repository.
type HostedConfig struct {
	WritePolicy WritePolicy `json:"write_policy" yaml:"write_policy"`
}

// ProxyConfig configures a proxy repository.
type ProxyConfig struct {
	// RemoteURL is the upstream base URL, e.g. https://registry.npmjs.org.
	RemoteURL string `json:"remote_url" yaml:"remote_url"`
	// MetadataTTL: how long index/metadata documents (packuments, maven-metadata.xml,
	// tag lists) are served from cache before revalidating upstream.
	MetadataTTL Duration `json:"metadata_ttl" yaml:"metadata_ttl"`
	// ContentTTL: how long immutable content is trusted; zero means forever.
	ContentTTL Duration `json:"content_ttl" yaml:"content_ttl"`
	// NegativeCacheTTL: how long a 404 from upstream is remembered.
	NegativeCacheTTL Duration `json:"negative_cache_ttl" yaml:"negative_cache_ttl"`
	// Auth for the remote; secrets are stored encrypted and never returned by the API.
	Auth *UpstreamAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
	// HTTP tunes the outbound client for this remote (proxy, TLS, timeouts);
	// zero values inherit the server-wide http.* configuration.
	HTTP UpstreamHTTP `json:"http,omitzero" yaml:"http,omitempty"`
	// Offline after this many consecutive failures, for CircuitOpenFor.
	CircuitFailures int      `json:"circuit_failures,omitempty" yaml:"circuit_failures,omitempty"`
	CircuitOpenFor  Duration `json:"circuit_open_for,omitempty" yaml:"circuit_open_for,omitempty"`
}

// Upstream auth kinds.
const (
	UpstreamAuthBasic  = "basic"  // Username + Password
	UpstreamAuthBearer = "bearer" // Token as "Authorization: Bearer <token>"
	UpstreamAuthHeader = "header" // Token in the header named HeaderName (e.g. "X-JFrog-Art-Api", "Private-Token")
)

// UpstreamAuth holds credentials for a proxy remote. They are sent only to the
// remote's own host, never to redirect targets (CDNs) or the outbound proxy.
type UpstreamAuth struct {
	Kind       string `json:"kind" yaml:"kind"`
	Username   string `json:"username,omitempty" yaml:"username,omitempty"`
	Password   string `json:"password,omitempty" yaml:"password,omitempty"` // never logged; encrypted at rest
	Token      string `json:"token,omitempty" yaml:"token,omitempty"`
	HeaderName string `json:"header_name,omitempty" yaml:"header_name,omitempty"`
}

// Validate checks that the fields required by Kind are present.
func (a *UpstreamAuth) Validate() error {
	switch a.Kind {
	case UpstreamAuthBasic:
		if a.Username == "" {
			return fmt.Errorf("upstream auth basic: username required: %w", ErrInvalid)
		}
	case UpstreamAuthBearer:
		if a.Token == "" {
			return fmt.Errorf("upstream auth bearer: token required: %w", ErrInvalid)
		}
	case UpstreamAuthHeader:
		if a.Token == "" || a.HeaderName == "" {
			return fmt.Errorf("upstream auth header: headerName and token required: %w", ErrInvalid)
		}
	default:
		return fmt.Errorf("upstream auth kind %q: want basic, bearer or header: %w", a.Kind, ErrInvalid)
	}
	return nil
}

// Outbound proxy modes for UpstreamHTTP.Proxy (anything else is a proxy URL).
const (
	ProxyInherit = "inherit" // server-wide http.proxy / HTTP(S)_PROXY settings (default)
	ProxyNone    = "none"    // connect directly
)

// UpstreamHTTP tunes the outbound HTTP client of one proxy repository. Zero
// values inherit the server-wide http.* configuration.
type UpstreamHTTP struct {
	// Proxy is "" or "inherit" (server-wide settings, honouring no_proxy),
	// "none" (direct), or a proxy URL: http://, https:// or socks5://, with
	// optional user:password@ (stored encrypted, never returned by the API).
	Proxy string `json:"proxy,omitempty" yaml:"proxy,omitempty"`
	// CABundle is PEM certificates trusted for this remote in addition to the
	// system roots and http.ca_file (corporate TLS-inspecting proxies, private CAs).
	CABundle string `json:"ca_bundle,omitempty" yaml:"ca_bundle,omitempty"`
	// InsecureSkipVerify disables TLS verification for this remote. Audited; avoid.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
	// ConnectTimeout bounds TCP connect + TLS handshake; ReadTimeout bounds the
	// wait for response headers. Body transfer is bounded by the request context.
	ConnectTimeout Duration `json:"connect_timeout,omitempty" yaml:"connect_timeout,omitempty"`
	ReadTimeout    Duration `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty"`
}

// ValidateProxy checks the Proxy field syntax.
func (h UpstreamHTTP) ValidateProxy() error {
	switch h.Proxy {
	case "", ProxyInherit, ProxyNone:
		return nil
	}
	u, err := url.Parse(h.Proxy)
	if err != nil || u.Host == "" {
		return fmt.Errorf("proxy %q: want inherit, none or a proxy URL: %w", RedactURL(h.Proxy), ErrInvalid)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return fmt.Errorf("proxy scheme %q: want http, https or socks5: %w", u.Scheme, ErrInvalid)
}

// RedactURL replaces the password in a URL with "REDACTED", for logs and API responses.
func RedactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	return u.String()
}

// ConnectorConfig gives a repository its own entry point, for clients that
// cannot address a repository by path. OCI clients only speak /v2/ at a host
// root, so a Docker repository is reached either through a dedicated port or
// a dedicated host name (cr.example.com) routed to Hangar by DNS/reverse proxy.
type ConnectorConfig struct {
	// Host is a virtual host name; requests whose effective Host (after trusted
	// X-Forwarded-Host) equals it are served by this repository.
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	// HTTPPort opens an extra plain-HTTP listener serving only this repository
	// (TLS terminated in front). 0 = none.
	HTTPPort int `json:"http_port,omitempty" yaml:"http_port,omitempty"`
}

// GroupConfig configures a group repository.
type GroupConfig struct {
	// Members are repository names, resolved in this order.
	Members []string `json:"members" yaml:"members"`
	// WriteMember is the hosted member that receives writes sent to the group
	// (formats that support group publishing: docker, npm). Empty = read-only.
	WriteMember string `json:"write_member,omitempty" yaml:"write_member,omitempty"`
}

// WritePolicy returns the effective write policy: the hosted policy, or
// WriteDeny for proxy and group repositories.
func (r *Repository) WritePolicy() WritePolicy {
	if r.Type == RepoHosted && r.Config.Hosted != nil && r.Config.Hosted.WritePolicy != "" {
		return r.Config.Hosted.WritePolicy
	}
	if r.Type == RepoHosted {
		return WriteAllow
	}
	return WriteDeny
}

// Validate checks the format-independent rules. Format-specific rules are
// checked by Format.Validate.
func (r *Repository) Validate() error {
	if err := ValidateRepositoryName(r.Name); err != nil {
		return err
	}
	if r.Format == "" {
		return fmt.Errorf("repository %q: format is required: %w", r.Name, ErrInvalid)
	}
	if !r.Type.Valid() {
		return fmt.Errorf("repository %q: unknown type %q: %w", r.Name, r.Type, ErrInvalid)
	}
	switch r.Type {
	case RepoHosted:
		if r.Config.Hosted != nil && r.Config.Hosted.WritePolicy != "" && !r.Config.Hosted.WritePolicy.Valid() {
			return fmt.Errorf("repository %q: unknown write policy %q: %w", r.Name, r.Config.Hosted.WritePolicy, ErrInvalid)
		}
	case RepoProxy:
		if r.Config.Proxy == nil || r.Config.Proxy.RemoteURL == "" {
			return fmt.Errorf("repository %q: proxy requires remoteUrl: %w", r.Name, ErrInvalid)
		}
		if u, err := url.Parse(r.Config.Proxy.RemoteURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("repository %q: remoteUrl must be an absolute http(s) URL: %w", r.Name, ErrInvalid)
		}
		if err := r.Config.Proxy.HTTP.ValidateProxy(); err != nil {
			return fmt.Errorf("repository %q: %w", r.Name, err)
		}
		if a := r.Config.Proxy.Auth; a != nil {
			if err := a.Validate(); err != nil {
				return fmt.Errorf("repository %q: %w", r.Name, err)
			}
		}
	case RepoGroup:
		if r.Config.Group == nil || len(r.Config.Group.Members) == 0 {
			return fmt.Errorf("repository %q: group requires at least one member: %w", r.Name, ErrInvalid)
		}
	}
	if c := r.Config.Connector; c != nil && (c.HTTPPort < 0 || c.HTTPPort > 65535) {
		return fmt.Errorf("repository %q: connector httpPort out of range: %w", r.Name, ErrInvalid)
	}
	if r.Type != RepoGroup && r.BlobStore == "" {
		return fmt.Errorf("repository %q: blobStore is required: %w", r.Name, ErrInvalid)
	}
	return nil
}

// Duration is a time.Duration that encodes as a Go duration string ("24h", "90s")
// in JSON and YAML, so repository config round-trips through config-as-code.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("duration %q: %w", string(b), ErrInvalid)
	}
	*d = Duration(v)
	return nil
}
