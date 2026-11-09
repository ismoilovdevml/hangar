package auth

import (
	"fmt"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Config tunes authentication. The zero value is completed by WithDefaults.
type Config struct {
	// Argon2 sets the password hashing cost. Stored hashes made with other
	// parameters keep working and are re-hashed on the next successful login.
	Argon2 Argon2Params `yaml:"argon2"`
	// MaxConcurrentHashes bounds simultaneous argon2 computations so a burst
	// of logins cannot exhaust memory (each one uses Argon2.Memory KiB).
	MaxConcurrentHashes int `yaml:"max_concurrent_hashes"`

	Password PasswordPolicy `yaml:"password"`
	Lockout  Lockout        `yaml:"lockout"`

	// Session configures browser sessions.
	Session SessionConfig `yaml:"session"`

	// SessionTokenTTL is the default lifetime of tokens minted by Issue
	// (registry bearer tokens, CLI logins).
	SessionTokenTTL time.Duration `yaml:"session_token_ttl"`
	// MaxSessionTokenTTL caps TokenRequest.TTL.
	MaxSessionTokenTTL time.Duration `yaml:"max_session_token_ttl"`
	// MaxTokenLifetime caps the expiry of personal and service-account tokens.
	MaxTokenLifetime time.Duration `yaml:"max_token_lifetime"`
	// TokenExpiryWarning is how long before expiry a token is listed in the
	// session's token warnings.
	TokenExpiryWarning time.Duration `yaml:"token_expiry_warning"`

	// BasicCacheTTL is how long a verified username/password pair is
	// remembered so clients that send Basic auth on every request (Maven,
	// pip) do not pay for argon2 each time. 0 disables the cache.
	BasicCacheTTL time.Duration `yaml:"basic_cache_ttl"`
	// AuthZCacheTTL is how long authorization decisions are cached. Changes to
	// users and roles invalidate the local cache at once; other cluster nodes
	// pick them up after at most this long.
	AuthZCacheTTL time.Duration `yaml:"authz_cache_ttl"`

	// LoginRate limits login attempts per client IP.
	LoginRate RateLimit `yaml:"login_rate"`

	// UsageFlushInterval is how often token last_used_at and user
	// last_active_at are written back.
	UsageFlushInterval time.Duration `yaml:"usage_flush_interval"`

	// OIDC is single sign-on with an OpenID Connect provider.
	OIDC OIDCConfig `yaml:"oidc"`
	// LDAP is sign-in with an LDAP directory or Active Directory.
	LDAP LDAPConfig `yaml:"ldap"`
}

// Argon2Params are argon2id cost parameters.
type Argon2Params struct {
	Memory      uint32 `yaml:"memory_kib"`
	Iterations  uint32 `yaml:"iterations"`
	Parallelism uint8  `yaml:"parallelism"`
	SaltLength  uint32 `yaml:"salt_length"`
	KeyLength   uint32 `yaml:"key_length"`
}

// DefaultArgon2 follows RFC 9106 section 4's second recommended option scaled
// for a server: 64 MiB, 3 passes, 2 lanes.
var DefaultArgon2 = Argon2Params{Memory: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32}

// Lockout stops password guessing. A username is locked out for one client
// address after MaxAttempts failed attempts from that address within Window,
// for Duration; other addresses can still sign in, so nobody can lock an
// account for everyone. Failures from all addresses together only slow the
// username down: from SlowdownAfter failures within Window on, every attempt
// waits first, doubling from a quarter second up to MaxDelay.
type Lockout struct {
	MaxAttempts int           `yaml:"max_attempts"` // negative disables lockout
	Window      time.Duration `yaml:"window"`
	Duration    time.Duration `yaml:"duration"`
	// SlowdownAfter: failures of a username from all addresses within
	// Window before attempts are slowed down (0: default, negative: never).
	SlowdownAfter int `yaml:"slowdown_after"`
	// MaxDelay caps the slow-down (0: default).
	MaxDelay time.Duration `yaml:"max_delay"`
}

// SessionConfig configures cookie sessions.
type SessionConfig struct {
	// TTL is the lifetime of a session without "remember me".
	TTL time.Duration `yaml:"ttl"`
	// RememberTTL is the lifetime with "remember me".
	RememberTTL time.Duration `yaml:"remember_ttl"`
	// CookieName is the session cookie (openapi: sessionCookie).
	CookieName string `yaml:"cookie_name"`
	// CSRFCookieName is readable by the web UI, which echoes it in CSRFHeader.
	CSRFCookieName string `yaml:"csrf_cookie_name"`
	CSRFHeader     string `yaml:"csrf_header"`
	// Insecure drops the Secure attribute from cookies. Only for plain-HTTP
	// development setups other than localhost.
	Insecure bool `yaml:"insecure"`
}

// RateLimit is a token bucket: Burst attempts at once, refilled at PerMinute.
type RateLimit struct {
	PerMinute float64 `yaml:"per_minute"` // 0 disables the limit
	Burst     int     `yaml:"burst"`
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		Argon2:              DefaultArgon2,
		MaxConcurrentHashes: 4,
		Password:            DefaultPasswordPolicy,
		Lockout:             Lockout{MaxAttempts: 5, Window: 15 * time.Minute, Duration: 15 * time.Minute, SlowdownAfter: 10, MaxDelay: 4 * time.Second},
		Session: SessionConfig{
			TTL:            12 * time.Hour,
			RememberTTL:    30 * 24 * time.Hour,
			CookieName:     "hangar_session",
			CSRFCookieName: "hangar_csrf",
			CSRFHeader:     "X-CSRF-Token",
		},
		SessionTokenTTL:    15 * time.Minute,
		MaxSessionTokenTTL: 24 * time.Hour,
		MaxTokenLifetime:   366 * 24 * time.Hour,
		TokenExpiryWarning: 14 * 24 * time.Hour,
		BasicCacheTTL:      2 * time.Minute,
		AuthZCacheTTL:      5 * time.Second,
		LoginRate:          RateLimit{PerMinute: 10, Burst: 10},
		UsageFlushInterval: 30 * time.Second,
		OIDC:               OIDCConfig{AutoCreate: true},
		LDAP:               LDAPConfig{AutoCreate: true},
	}
}

// WithDefaults fills zero fields from DefaultConfig. Disabling switches
// (Lockout.MaxAttempts, LoginRate.PerMinute, BasicCacheTTL) cannot be
// expressed as zero here; use a negative value to disable them.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.Argon2 == (Argon2Params{}) {
		c.Argon2 = d.Argon2
	}
	if c.Argon2.SaltLength == 0 {
		c.Argon2.SaltLength = d.Argon2.SaltLength
	}
	if c.Argon2.KeyLength == 0 {
		c.Argon2.KeyLength = d.Argon2.KeyLength
	}
	if c.MaxConcurrentHashes <= 0 {
		c.MaxConcurrentHashes = d.MaxConcurrentHashes
	}
	if c.Password == (PasswordPolicy{}) {
		c.Password = d.Password
	}
	if c.Password.MaxLength == 0 {
		c.Password.MaxLength = d.Password.MaxLength
	}
	if c.Lockout == (Lockout{}) {
		c.Lockout = d.Lockout
	}
	if c.Lockout.Window == 0 {
		c.Lockout.Window = d.Lockout.Window
	}
	if c.Lockout.Duration == 0 {
		c.Lockout.Duration = d.Lockout.Duration
	}
	if c.Lockout.SlowdownAfter == 0 {
		c.Lockout.SlowdownAfter = d.Lockout.SlowdownAfter
	}
	if c.Lockout.MaxDelay <= 0 {
		c.Lockout.MaxDelay = d.Lockout.MaxDelay
	}
	s := &c.Session
	if s.TTL == 0 {
		s.TTL = d.Session.TTL
	}
	if s.RememberTTL == 0 {
		s.RememberTTL = d.Session.RememberTTL
	}
	if s.CookieName == "" {
		s.CookieName = d.Session.CookieName
	}
	if s.CSRFCookieName == "" {
		s.CSRFCookieName = d.Session.CSRFCookieName
	}
	if s.CSRFHeader == "" {
		s.CSRFHeader = d.Session.CSRFHeader
	}
	if c.SessionTokenTTL == 0 {
		c.SessionTokenTTL = d.SessionTokenTTL
	}
	if c.MaxSessionTokenTTL == 0 {
		c.MaxSessionTokenTTL = d.MaxSessionTokenTTL
	}
	if c.MaxTokenLifetime == 0 {
		c.MaxTokenLifetime = d.MaxTokenLifetime
	}
	if c.TokenExpiryWarning <= 0 {
		c.TokenExpiryWarning = d.TokenExpiryWarning
	}
	if c.BasicCacheTTL == 0 {
		c.BasicCacheTTL = d.BasicCacheTTL
	}
	if c.AuthZCacheTTL == 0 {
		c.AuthZCacheTTL = d.AuthZCacheTTL
	}
	if c.LoginRate == (RateLimit{}) {
		c.LoginRate = d.LoginRate
	}
	if c.LoginRate.Burst <= 0 {
		c.LoginRate.Burst = 1
	}
	if c.UsageFlushInterval <= 0 {
		c.UsageFlushInterval = d.UsageFlushInterval
	}
	c.OIDC = c.OIDC.WithDefaults()
	c.LDAP = c.LDAP.WithDefaults()
	return c
}

// Validate rejects settings that would weaken or break authentication.
func (c Config) Validate() error {
	a := c.Argon2
	switch {
	case a.Memory < 8*uint32(a.Parallelism) || a.Memory < 1024:
		return fmt.Errorf("auth.argon2.memory_kib %d: want at least 1024 and 8*parallelism: %w", a.Memory, core.ErrInvalid)
	case a.Iterations < 1:
		return fmt.Errorf("auth.argon2.iterations must be >= 1: %w", core.ErrInvalid)
	case a.Parallelism < 1:
		return fmt.Errorf("auth.argon2.parallelism must be >= 1: %w", core.ErrInvalid)
	case a.SaltLength < 8 || a.KeyLength < 16:
		return fmt.Errorf("auth.argon2: salt_length >= 8 and key_length >= 16 required: %w", core.ErrInvalid)
	case c.Password.MinLength < 8:
		return fmt.Errorf("auth.password.min_length %d: must be at least 8: %w", c.Password.MinLength, core.ErrInvalid)
	case c.Password.MaxLength < c.Password.MinLength:
		return fmt.Errorf("auth.password.max_length below min_length: %w", core.ErrInvalid)
	case c.Session.TTL <= 0 || c.Session.RememberTTL <= 0:
		return fmt.Errorf("auth.session ttl must be positive: %w", core.ErrInvalid)
	case c.MaxTokenLifetime < time.Hour:
		return fmt.Errorf("auth.max_token_lifetime %s: must be at least 1h: %w", c.MaxTokenLifetime, core.ErrInvalid)
	}
	return c.validateExternal()
}
