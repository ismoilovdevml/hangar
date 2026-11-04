// Package config loads Hangar's configuration from a YAML file and HANGAR_*
// environment variables.
//
// Precedence (highest first): environment, file, defaults. Every leaf setting
// has an environment variable named HANGAR_ + its YAML path upper-cased and
// joined with "_": database.url -> HANGAR_DATABASE_URL, storage.fs.path ->
// HANGAR_STORAGE_FS_PATH. Secrets should come from the environment.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/auth"
)

// EnvPrefix prefixes every environment variable.
const EnvPrefix = "HANGAR_"

// EnvConfigFile names the config file when --config is not given.
const EnvConfigFile = "HANGAR_CONFIG"

// Config is the whole server configuration. See hangar.example.yaml.
type Config struct {
	Server   Server   `yaml:"server"`
	Log      Log      `yaml:"log"`
	Database Database `yaml:"database"`
	Storage  Storage  `yaml:"storage"`
	NATS     NATS     `yaml:"nats"`
	HTTP     HTTP     `yaml:"http"`
	// Auth tunes authentication: password hashing, lockout, sessions, token
	// lifetimes, caches, and external sign-in (auth.oidc, auth.ldap).
	Auth      auth.Config `yaml:"auth"`
	Docker    Docker      `yaml:"docker"`
	Bootstrap Bootstrap   `yaml:"bootstrap"`
	Scanner   Scanner     `yaml:"scanner"`
	Scanning  Scanning    `yaml:"scanning"`
	Metrics   Metrics     `yaml:"metrics"`
	// Security: encryption of stored secrets and HTTP hardening (security.go).
	Security Security `yaml:"security"`
	// Observability configures tracing and access log sampling.
	Observability Observability `yaml:"observability"`
	// UI configures the web UI.
	UI UI `yaml:"ui"`
}

// UI configures the web UI.
type UI struct {
	// ReadmeImages is how images in package READMEs that live on other hosts
	// are shown: "proxy" (default) fetches them through Hangar, so browsers
	// never contact those hosts; "direct" lets browsers load https images
	// from their hosts (the content security policy allows https: images;
	// those hosts see the viewer's IP address and that the README was
	// opened); "off" shows their alternative text only. Images stored
	// inside the package are always served by Hangar.
	ReadmeImages string `yaml:"readme_images"`
	// ReadmeImagesAllowHTTP loads http:// README images as written; by
	// default they are upgraded to https://.
	ReadmeImagesAllowHTTP bool `yaml:"readme_images_allow_http"`
}

// Docker configures the Docker / OCI format.
type Docker struct {
	// TokenKey signs registry bearer tokens and upload session URLs
	// (HANGAR_DOCKER_TOKEN_KEY). Every node of a cluster must use the same
	// value; use at least 32 random bytes. Empty: a random per-process key,
	// so registry tokens and unfinished uploads do not survive a restart.
	TokenKey string `yaml:"token_key"` // from env; masked by Redacted
}

// Bootstrap creates the first administrator when none exists.
type Bootstrap struct {
	// AdminUser and AdminPassword (HANGAR_BOOTSTRAP_ADMIN_USER /
	// HANGAR_BOOTSTRAP_ADMIN_PASSWORD): when both are set and no enabled
	// administrator exists, `hangar serve` creates this local admin on start.
	// Nothing changes once an administrator exists.
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"` // from env; masked by Redacted
	// RequirePasswordChange makes the administrator created from AdminUser
	// and AdminPassword, or by `hangar admin bootstrap`, set a new password
	// at the first sign-in (default true).
	RequirePasswordChange bool `yaml:"require_password_change"`
	// InitialPasswordFile (HANGAR_BOOTSTRAP_INITIAL_PASSWORD_FILE): without
	// AdminUser, the first start of an empty installation creates the local
	// administrator "admin" with a one-time password. If this file holds a
	// password, that one is used; otherwise one is generated, printed once
	// to standard error and written here (mode 0600). The file is deleted
	// after the administrator sets a new password.
	InitialPasswordFile string `yaml:"initial_password_file"`
}

// Server configures the HTTP listener.
type Server struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string `yaml:"addr"`
	// BaseURL is the external URL clients use, without trailing slash. Every
	// absolute link Hangar generates (npm tarball URLs, the OCI token realm,
	// Maven/PyPI links) is built from it, never from the request Host header.
	BaseURL string `yaml:"base_url"`
	// InstanceName is shown in the UI header and system status.
	InstanceName      string        `yaml:"instance_name"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// IdleTimeout closes idle keep-alive connections.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// ShutdownTimeout bounds graceful shutdown (draining in-flight uploads).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// TrustedProxies are CIDRs whose X-Forwarded-For is honoured for client IPs.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// NodeID names this process on the System status and System logs
	// screens; unique per node. Empty: the host name.
	NodeID string `yaml:"node_id"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json or text
	// BufferSize is how many recent records each node keeps in memory for
	// the System logs screen (100 to 100000).
	BufferSize int `yaml:"buffer_size"`
}

// Database configures PostgreSQL.
type Database struct {
	URL          string        `yaml:"url"`
	MaxConns     int32         `yaml:"max_conns"`
	MinConns     int32         `yaml:"min_conns"`
	ConnLifetime time.Duration `yaml:"conn_lifetime"`
	// MigrateOnStart applies pending migrations when `hangar serve` starts.
	MigrateOnStart bool `yaml:"migrate_on_start"`
}

// Storage configures the default blob store, created on first start.
type Storage struct {
	Kind string    `yaml:"kind"` // fs or s3
	FS   FSStorage `yaml:"fs"`
	S3   S3Storage `yaml:"s3"`
	GC   GC        `yaml:"gc"`
}

// MinGCGracePeriod is the shortest accepted storage.gc.grace_period. It must
// exceed the Docker upload session lifetime (24h), or blobs of an upload that
// is still allowed to finish could be collected before its manifest commits.
const MinGCGracePeriod = 24 * time.Hour

// GC configures blob garbage collection (job blob.gc, run daily by the leader).
type GC struct {
	// GracePeriod is how long an unreferenced blob is kept before its bytes
	// are deleted. Must be longer than MinGCGracePeriod.
	GracePeriod time.Duration `yaml:"grace_period"`
	// BatchSize is the number of blobs deleted per transaction.
	BatchSize int `yaml:"batch_size"`
	// TempMaxAge is the age after which abandoned temporary uploads are
	// removed; negative disables temp cleanup.
	TempMaxAge time.Duration `yaml:"temp_max_age"`
}

// FSStorage is the filesystem driver.
type FSStorage struct {
	Path string `yaml:"path"`
}

// S3Storage is the S3-compatible driver (AWS S3, MinIO).
type S3Storage struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	Prefix          string `yaml:"prefix"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"` // from env; masked by Redacted
	SessionToken    string `yaml:"session_token"`     // temporary credentials; masked by Redacted
	UsePathStyle    bool   `yaml:"use_path_style"`
	// CAFile is a PEM bundle trusted for the endpoint (private CA).
	CAFile string `yaml:"ca_file"`
	// PartSize is the multipart part size in bytes (5 MiB to 5 GiB, default 16 MiB).
	PartSize int64 `yaml:"part_size"`
}

// NATS configures the event bus and job queue.
type NATS struct {
	// Embedded runs a NATS server with JetStream inside the hangar process
	// (single-node mode). Otherwise URL points at an external cluster.
	Embedded bool   `yaml:"embedded"`
	URL      string `yaml:"url"`
	// DataDir holds embedded JetStream state.
	DataDir string `yaml:"data_dir"`
	// Listen exposes the embedded server on host:port; empty = in-process only.
	Listen string `yaml:"listen"`
}

// HTTP configures outbound HTTP for proxy repositories (corporate networks).
// Proxy repositories inherit it and may override proxy, CA and timeouts per remote.
type HTTP struct {
	// Proxy is an http://, https:// or socks5:// URL for all outbound requests.
	Proxy         string `yaml:"proxy"`
	ProxyUsername string `yaml:"proxy_username"`
	ProxyPassword string `yaml:"proxy_password"` // from env; masked by Redacted
	// NoProxy lists hosts, domain suffixes (.corp.example) and CIDRs reached directly.
	NoProxy []string `yaml:"no_proxy"`
	// UseEnvironment honours HTTP_PROXY / HTTPS_PROXY / NO_PROXY when Proxy is empty.
	UseEnvironment bool `yaml:"use_environment"`
	// CAFile is a PEM bundle trusted in addition to the system roots.
	CAFile              string        `yaml:"ca_file"`
	ConnectTimeout      time.Duration `yaml:"connect_timeout"`
	ReadTimeout         time.Duration `yaml:"read_timeout"`
	IdleConnTimeout     time.Duration `yaml:"idle_conn_timeout"`
	MaxIdleConns        int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int           `yaml:"max_idle_conns_per_host"`
	// UserAgent sent upstream; default "Hangar/<version>".
	UserAgent string `yaml:"user_agent"`
	// AllowPrivateNetworks lets proxy repositories reach internal addresses,
	// which the outbound address policy blocks by default (loopback, private,
	// link-local and cloud metadata ranges): CIDRs, IPs, host names or domain
	// suffixes starting with "." (e.g. an internal mirror).
	AllowPrivateNetworks []string `yaml:"allow_private_networks"`
}

// Scanner is reserved for container image scanning, which is not active
// yet. Package scanning is configured in Scanning.
type Scanner struct {
	// TrivyURL is a Trivy server (client/server mode). Not used yet.
	TrivyURL string `yaml:"trivy_url"`
}

// Scanning configures vulnerability scanning of package components (npm,
// Maven, PyPI, NuGet, Go) against an OSV advisory database.
type Scanning struct {
	// Enabled turns scanning on (arrival scans, the rescan task, scan now).
	Enabled bool `yaml:"enabled"`
	// RescanInterval is how old a component's last scan may get before the
	// hourly rescan task checks it again against new advisories.
	RescanInterval time.Duration `yaml:"rescan_interval"`
	// InUseWindow: a vulnerable version downloaded within it counts as "in
	// use" in the fix plan of the Vulnerabilities screen (1 to 90 days).
	InUseWindow time.Duration `yaml:"in_use_window"`
	OSV         OSV           `yaml:"osv"`
}

// OSV is the advisory database the scanner queries.
type OSV struct {
	// URL is the API root: https://api.osv.dev or an internal mirror serving
	// the same API (/v1/querybatch, /v1/vulns/{id}). Requests go through the
	// outbound HTTP settings (http.proxy, http.ca_file); a mirror on a
	// private address must be listed in http.allow_private_networks.
	URL string `yaml:"url"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Observability configures tracing and the access log.
type Observability struct {
	Tracing   Tracing   `yaml:"tracing"`
	AccessLog AccessLog `yaml:"access_log"`
}

// Tracing exports OpenTelemetry traces over OTLP/HTTP.
type Tracing struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP/HTTP collector URL, e.g. http://otel-collector:4318
	// (path defaults to /v1/traces). Collector auth headers come from
	// OTEL_EXPORTER_OTLP_HEADERS.
	Endpoint string `yaml:"endpoint"`
	// SampleRatio is the share of new traces recorded (0 to 1); requests with
	// a sampled W3C traceparent are always recorded.
	SampleRatio float64 `yaml:"sample_ratio"`
	// ServiceName is the service.name resource attribute.
	ServiceName string `yaml:"service_name"`
}

// AccessLog configures the per-request log line.
type AccessLog struct {
	// SampleRatio is the share of requests logged (0 to 1); server errors
	// are always logged.
	SampleRatio float64 `yaml:"sample_ratio"`
}

// Default returns the built-in defaults: single node, filesystem blobs,
// embedded NATS, data under ./data.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8080",
			BaseURL:           "http://localhost:8080",
			InstanceName:      "hangar",
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			ShutdownTimeout:   30 * time.Second,
		},
		Log:      Log{Level: "info", Format: "json", BufferSize: 5000},
		Database: Database{MaxConns: 20, MinConns: 2, ConnLifetime: time.Hour},
		Storage: Storage{Kind: "fs", FS: FSStorage{Path: "./data/blobs"},
			GC: GC{GracePeriod: 48 * time.Hour, BatchSize: 100, TempMaxAge: 24 * time.Hour}},
		Auth: auth.DefaultConfig(),
		NATS: NATS{Embedded: true, DataDir: "./data/nats"},
		HTTP: HTTP{
			UseEnvironment:      true,
			ConnectTimeout:      10 * time.Second,
			ReadTimeout:         60 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 32,
		},
		Metrics:  Metrics{Enabled: true, Path: "/metrics"},
		Scanning: Scanning{Enabled: true, RescanInterval: 24 * time.Hour, InUseWindow: 30 * 24 * time.Hour, OSV: OSV{URL: "https://api.osv.dev"}},
		Security: DefaultSecurity(),
		UI:       UI{ReadmeImages: "proxy"},
		Observability: Observability{
			Tracing:   Tracing{SampleRatio: 0.1, ServiceName: "hangar"},
			AccessLog: AccessLog{SampleRatio: 1},
		},
		Bootstrap: Bootstrap{RequirePasswordChange: true},
	}
}

// Load reads path (if non-empty) over the defaults, then applies the
// environment, then validates. If path is empty, $HANGAR_CONFIG is used; if
// that is empty too, only defaults and environment apply.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = os.Getenv(EnvConfigFile)
	}
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return cfg, fmt.Errorf("config file %s not found", path)
			}
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		// An empty file decodes as io.EOF: no settings.
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := ApplyEnv(&cfg, os.LookupEnv); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate reports configuration errors an operator must fix.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url (HANGAR_DATABASE_URL) is required"))
	}
	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is required"))
	}
	if u, err := url.Parse(c.Server.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("server.base_url %q must be an absolute URL", c.Server.BaseURL))
	}
	c.Server.BaseURL = strings.TrimSuffix(c.Server.BaseURL, "/")
	if c.Log.BufferSize < 100 || c.Log.BufferSize > 100000 {
		errs = append(errs, fmt.Errorf("log.buffer_size %d: want 100 to 100000", c.Log.BufferSize))
	}
	if len(c.Server.NodeID) > 128 {
		errs = append(errs, errors.New("server.node_id: at most 128 characters"))
	}
	switch c.Storage.Kind {
	case "fs":
		if c.Storage.FS.Path == "" {
			errs = append(errs, errors.New("storage.fs.path is required for storage.kind=fs"))
		}
	case "s3":
		if c.Storage.S3.Bucket == "" {
			errs = append(errs, errors.New("storage.s3.bucket is required for storage.kind=s3"))
		}
	default:
		errs = append(errs, fmt.Errorf("storage.kind %q: want fs or s3", c.Storage.Kind))
	}
	if c.Storage.S3.PartSize != 0 && (c.Storage.S3.PartSize < 5<<20 || c.Storage.S3.PartSize > 5<<30) {
		errs = append(errs, fmt.Errorf("storage.s3.part_size %d: want between 5 MiB and 5 GiB", c.Storage.S3.PartSize))
	}
	if c.Storage.GC.GracePeriod <= MinGCGracePeriod {
		errs = append(errs, fmt.Errorf("storage.gc.grace_period %s must be longer than %s (the docker upload lifetime)",
			c.Storage.GC.GracePeriod, MinGCGracePeriod))
	}
	if c.Storage.GC.BatchSize <= 0 {
		errs = append(errs, errors.New("storage.gc.batch_size must be positive"))
	}
	if err := c.Auth.WithDefaults().Validate(); err != nil {
		errs = append(errs, err)
	}
	if (c.Bootstrap.AdminUser == "") != (c.Bootstrap.AdminPassword == "") {
		errs = append(errs, errors.New("bootstrap.admin_user and bootstrap.admin_password must be set together"))
	}
	if c.NATS.Embedded {
		if c.NATS.DataDir == "" {
			errs = append(errs, errors.New("nats.data_dir is required for embedded NATS"))
		}
	} else if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required when nats.embedded=false"))
	}
	if c.HTTP.Proxy != "" {
		u, err := url.Parse(c.HTTP.Proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			errs = append(errs, fmt.Errorf("http.proxy %q must be an http://, https:// or socks5:// URL", redactURL(c.HTTP.Proxy)))
		}
	}
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				errs = append(errs, fmt.Errorf("server.trusted_proxies: %q is not a CIDR or IP", cidr))
			}
		}
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("log.format %q: want json or text", c.Log.Format))
	}
	if c.Metrics.Path == "" || !strings.HasPrefix(c.Metrics.Path, "/") {
		errs = append(errs, fmt.Errorf("metrics.path %q must start with /", c.Metrics.Path))
	}
	errs = append(errs, c.Security.validate()...)
	switch c.UI.ReadmeImages = strings.ToLower(strings.TrimSpace(c.UI.ReadmeImages)); c.UI.ReadmeImages {
	case "":
		c.UI.ReadmeImages = "proxy"
	case "proxy", "direct", "off":
	default:
		errs = append(errs, fmt.Errorf("ui.readme_images %q: want proxy, direct or off", c.UI.ReadmeImages))
	}
	if w := c.Scanning.InUseWindow; w < 24*time.Hour || w > 90*24*time.Hour {
		errs = append(errs, fmt.Errorf("scanning.in_use_window %s must be between 24h and 2160h (90 days)", w))
	}
	if c.Scanning.Enabled {
		if u, err := url.Parse(c.Scanning.OSV.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("scanning.osv.url %q must be an http:// or https:// URL", redactURL(c.Scanning.OSV.URL)))
		}
		if c.Scanning.RescanInterval < time.Hour {
			errs = append(errs, fmt.Errorf("scanning.rescan_interval %s must be at least 1h", c.Scanning.RescanInterval))
		}
	}
	if t := c.Observability.Tracing; t.Enabled {
		if u, err := url.Parse(t.Endpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("observability.tracing.endpoint %q must be an http:// or https:// URL when tracing is enabled",
				redactURL(t.Endpoint)))
		}
	}
	if r := c.Observability.Tracing.SampleRatio; r < 0 || r > 1 {
		errs = append(errs, fmt.Errorf("observability.tracing.sample_ratio %v: want 0 to 1", r))
	}
	if r := c.Observability.AccessLog.SampleRatio; r < 0 || r > 1 {
		errs = append(errs, fmt.Errorf("observability.access_log.sample_ratio %v: want 0 to 1", r))
	}
	return errors.Join(errs...)
}

// ApplyEnv overrides fields of cfg from environment variables found by lookup.
func ApplyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	var errs []error
	walk(reflect.ValueOf(cfg).Elem(), EnvPrefix, func(name string, v reflect.Value) {
		raw, ok := lookup(name)
		if !ok {
			return
		}
		if err := setValue(v, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	})
	applySecretKeyAlias(cfg, lookup)
	return errors.Join(errs...)
}

// EnvVars lists every supported environment variable name, for docs and tests.
func EnvVars() []string {
	var out []string
	cfg := Default()
	walk(reflect.ValueOf(&cfg).Elem(), EnvPrefix, func(name string, _ reflect.Value) { out = append(out, name) })
	return out
}

var durationType = reflect.TypeFor[time.Duration]()

func walk(v reflect.Value, prefix string, fn func(string, reflect.Value)) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + strings.ToUpper(tag)
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			walk(fv, name+"_", fn)
			continue
		}
		fn(name, fv)
	}
}

func setValue(v reflect.Value, raw string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(n)
	case reflect.Map:
		// Maps (auth.*.group_roles) are JSON objects:
		// HANGAR_AUTH_OIDC_GROUP_ROLES='{"hangar-admins":["Admin"]}'.
		m := reflect.New(v.Type())
		if err := json.Unmarshal([]byte(raw), m.Interface()); err != nil {
			return fmt.Errorf("want a JSON object: %w", err)
		}
		v.Set(m.Elem())
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice type %s", v.Type())
		}
		var items []string
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		v.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

// Redacted returns a copy safe to log: secrets and URL passwords masked.
func (c Config) Redacted() Config {
	for _, p := range []*string{&c.Storage.S3.SecretAccessKey, &c.Storage.S3.SessionToken, &c.Docker.TokenKey, &c.Bootstrap.AdminPassword,
		&c.Auth.OIDC.ClientSecret, &c.Auth.LDAP.BindPassword} {
		if *p != "" {
			*p = "REDACTED"
		}
	}
	if c.HTTP.ProxyPassword != "" {
		c.HTTP.ProxyPassword = "REDACTED"
	}
	c.HTTP.Proxy = redactURL(c.HTTP.Proxy)
	c.Database.URL = redactURL(c.Database.URL)
	c.NATS.URL = redactURL(c.NATS.URL)
	c.Security = c.Security.redacted()
	c.Observability.Tracing.Endpoint = redactURL(c.Observability.Tracing.Endpoint)
	return c
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	return u.String()
}
