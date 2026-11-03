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
	Scanner  Scanner  `yaml:"scanner"`
	Metrics  Metrics  `yaml:"metrics"`
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
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json or text
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
	UsePathStyle    bool   `yaml:"use_path_style"`
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
}

// Scanner configures vulnerability scanning.
type Scanner struct {
	// TrivyURL is the Trivy server (client/server mode); empty disables scanning.
	TrivyURL string `yaml:"trivy_url"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
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
		Log:      Log{Level: "info", Format: "json"},
		Database: Database{MaxConns: 20, MinConns: 2, ConnLifetime: time.Hour},
		Storage:  Storage{Kind: "fs", FS: FSStorage{Path: "./data/blobs"}},
		NATS:     NATS{Embedded: true, DataDir: "./data/nats"},
		HTTP: HTTP{
			UseEnvironment:      true,
			ConnectTimeout:      10 * time.Second,
			ReadTimeout:         60 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 32,
		},
		Metrics: Metrics{Enabled: true, Path: "/metrics"},
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
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
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
	if c.Storage.S3.SecretAccessKey != "" {
		c.Storage.S3.SecretAccessKey = "REDACTED"
	}
	if c.HTTP.ProxyPassword != "" {
		c.HTTP.ProxyPassword = "REDACTED"
	}
	c.HTTP.Proxy = redactURL(c.HTTP.Proxy)
	c.Database.URL = redactURL(c.Database.URL)
	c.NATS.URL = redactURL(c.NATS.URL)
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
