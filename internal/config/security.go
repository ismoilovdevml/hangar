package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// EnvSecretKey is a short alias of HANGAR_SECURITY_SECRET_KEY, which wins
// when both are set.
const EnvSecretKey = "HANGAR_SECRET_KEY"

// Security configures encryption of stored secrets and HTTP hardening.
type Security struct {
	// SecretKey encrypts the credentials Hangar stores in the database (proxy
	// upstream passwords and tokens, proxy URL passwords, S3 keys): 32 random
	// bytes, base64 or hex encoded (`openssl rand -base64 32`). Set it through
	// HANGAR_SECRET_KEY. Every node of a cluster needs the same keys. Without
	// it, configurations carrying such credentials are refused.
	SecretKey string `yaml:"secret_key"` // from env; masked by Redacted
	// PreviousSecretKeys still open values sealed before a key rotation
	// (HANGAR_SECURITY_PREVIOUS_SECRET_KEYS, comma-separated). Remove a key
	// once `hangar admin rotate-secrets` has re-encrypted everything.
	PreviousSecretKeys []string `yaml:"previous_secret_keys"` // masked by Redacted

	HSTS      HSTS       `yaml:"hsts"`
	CORS      CORS       `yaml:"cors"`
	RateLimit RateLimits `yaml:"rate_limit"`
	Limits    Limits     `yaml:"limits"`
}

// HSTS is the Strict-Transport-Security header, sent on HTTPS requests only
// (TLS, or X-Forwarded-Proto: https from a trusted proxy).
type HSTS struct {
	// MaxAge is the max-age directive; 0 disables the header.
	MaxAge time.Duration `yaml:"max_age"`
	// IncludeSubdomains adds includeSubDomains. Only enable it when every
	// subdomain of the host is served over HTTPS.
	IncludeSubdomains bool `yaml:"include_subdomains"`
}

// CORS controls cross-origin browser access. Nothing is allowed by default:
// browsers may only call the API from Hangar's own origin.
type CORS struct {
	// AllowedOrigins are extra origins ("https://portal.example.com") allowed
	// to call the API and package endpoints from a browser, with credentials.
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// RateLimits are per-client-IP token buckets (client IPs honour
// server.trusted_proxies). A per_minute of 0 disables a limit.
type RateLimits struct {
	// API limits every /api/v1 request.
	API auth.RateLimit `yaml:"api"`
	// FailedAuth limits failed authentications (401) on the API and the
	// package endpoints. When a client IP runs out, requests from it that
	// carry credentials answer 429 until the bucket refills; anonymous reads
	// keep working.
	FailedAuth auth.RateLimit `yaml:"failed_auth"`
}

// Limits bound request sizes and slow clients.
type Limits struct {
	// MaxHeaderBytes bounds the request line and headers.
	MaxHeaderBytes int `yaml:"max_header_bytes"`
	// APIBodyBytes bounds JSON/YAML bodies of /api/v1 requests.
	APIBodyBytes int64 `yaml:"api_body_bytes"`
	// UploadBodyBytes bounds bodies sent to package endpoints; 0 = unlimited
	// (uploads are streamed to the blob store, never buffered).
	UploadBodyBytes int64 `yaml:"upload_body_bytes"`
	// BodyIdleTimeout aborts a request whose body sends no data for this long
	// (slow-body attacks); 0 disables. Active transfers are never cut off.
	BodyIdleTimeout time.Duration `yaml:"body_idle_timeout"`
}

// DefaultSecurity returns the security defaults.
func DefaultSecurity() Security {
	return Security{
		HSTS: HSTS{MaxAge: 365 * 24 * time.Hour},
		RateLimit: RateLimits{
			API:        auth.RateLimit{PerMinute: 1200, Burst: 300},
			FailedAuth: auth.RateLimit{PerMinute: 30, Burst: 30},
		},
		Limits: Limits{
			MaxHeaderBytes:  128 << 10,
			APIBodyBytes:    1 << 20,
			BodyIdleTimeout: 2 * time.Minute,
		},
	}
}

// Keyring builds the secret keyring; nil when no secret key is configured.
func (s Security) Keyring() (*secrets.Keyring, error) {
	return secrets.NewKeyring(s.SecretKey, s.PreviousSecretKeys)
}

// validate reports security settings an operator must fix.
func (s Security) validate() []error {
	var errs []error
	if _, err := s.Keyring(); err != nil {
		errs = append(errs, err)
	}
	if s.HSTS.MaxAge < 0 {
		errs = append(errs, errors.New("security.hsts.max_age must not be negative"))
	}
	for _, o := range s.CORS.AllowedOrigins {
		if _, err := ParseOrigin(o); err != nil {
			errs = append(errs, fmt.Errorf("security.cors.allowed_origins: %w", err))
		}
	}
	for name, l := range map[string]auth.RateLimit{"api": s.RateLimit.API, "failed_auth": s.RateLimit.FailedAuth} {
		if l.PerMinute < 0 || l.Burst < 0 {
			errs = append(errs, fmt.Errorf("security.rate_limit.%s must not be negative", name))
		}
	}
	if s.Limits.MaxHeaderBytes < 4<<10 {
		errs = append(errs, fmt.Errorf("security.limits.max_header_bytes %d: want at least 4096", s.Limits.MaxHeaderBytes))
	}
	if s.Limits.APIBodyBytes < 4<<10 {
		errs = append(errs, fmt.Errorf("security.limits.api_body_bytes %d: want at least 4096", s.Limits.APIBodyBytes))
	}
	if s.Limits.UploadBodyBytes < 0 || s.Limits.BodyIdleTimeout < 0 {
		errs = append(errs, errors.New("security.limits.upload_body_bytes and body_idle_timeout must not be negative"))
	}
	return errs
}

// ParseOrigin normalises a browser origin: scheme://host[:port], http or
// https, without path, query or credentials.
func ParseOrigin(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q is not an origin (want scheme://host[:port])", s)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// redacted masks the keys.
func (s Security) redacted() Security {
	if s.SecretKey != "" {
		s.SecretKey = "REDACTED"
	}
	if len(s.PreviousSecretKeys) > 0 {
		masked := make([]string, len(s.PreviousSecretKeys))
		for i := range masked {
			masked[i] = "REDACTED"
		}
		s.PreviousSecretKeys = masked
	}
	return s
}

// applySecretKeyAlias honours HANGAR_SECRET_KEY unless the long form is set.
func applySecretKeyAlias(cfg *Config, lookup func(string) (string, bool)) {
	if _, long := lookup(EnvPrefix + "SECURITY_SECRET_KEY"); long {
		return
	}
	if v, ok := lookup(EnvSecretKey); ok {
		cfg.Security.SecretKey = v
	}
}
