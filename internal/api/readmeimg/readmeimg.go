// Package readmeimg loads the images package READMEs reference, so the web
// UI can show them under its strict content security policy.
//
// The package info API rewrites every image of a README into a same-origin
// URL signed with a server key (Sign): either a file inside the package or
// a remote URL that the server fetches on the browser's behalf (Fetch). Only
// URLs the server itself put into README output carry a valid signature, so
// the endpoint is not an open proxy. Remote fetches go through the shared
// outbound HTTP client (platform default proxy, trust settings) and the SSRF
// guard on the URL and every redirect hop; they are size, time and redirect
// bounded, send no cookies, credentials or referrer, and keep only content
// that sniffs as an image. Results, failures included, are cached in memory
// and in PostgreSQL, so every node shares them and a README's badges are
// fetched once a day, not once per view.
package readmeimg

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Mode is how README images are shown (ui.readme_images).
type Mode string

// Modes.
const (
	// ModeProxy loads remote images through the server (the default).
	ModeProxy Mode = "proxy"
	// ModeDirect lets browsers load https images from their hosts; the web
	// UI's content security policy allows https: images.
	ModeDirect Mode = "direct"
	// ModeOff shows no remote images, only their alternative text. Images
	// stored inside the package are still shown: they come from Hangar.
	ModeOff Mode = "off"
)

// ParseMode accepts "", proxy, direct or off ("" is proxy).
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeProxy, nil
	case ModeProxy, ModeDirect, ModeOff:
		return m, nil
	}
	return "", fmt.Errorf("readme images mode %q: want proxy, direct or off: %w", s, core.ErrInvalid)
}

// Source kinds a signature covers.
const (
	// KindURL is a remote image, fetched by the server.
	KindURL = "u"
	// KindMember is a file inside the package archive.
	KindMember = "p"
)

// Defaults of Options.
const (
	DefaultMaxBytes     = 5 << 20
	DefaultTimeout      = 15 * time.Second
	DefaultMaxRedirects = 5
	DefaultTTL          = 24 * time.Hour
	DefaultFailureTTL   = 15 * time.Minute
	DefaultMemoryBytes  = 32 << 20
	maxURLLen           = 4096
)

// Errors of Fetch. They wrap core.ErrUpstream.
var (
	// ErrNotImage: the content is not a served image type.
	ErrNotImage = errors.New("not an image")
	// ErrTooLarge: the image exceeds the size limit.
	ErrTooLarge = errors.New("image too large")
	// ErrNotAllowed: the URL's scheme is not allowed.
	ErrNotAllowed = errors.New("url not allowed")
)

// Clients builds outbound HTTP clients (implemented by *httpclient.Factory).
type Clients interface {
	ForUpstream(remoteURL string, cfg core.UpstreamHTTP, auth *core.UpstreamAuth) (*http.Client, error)
}

// ProxyRouter reports the outbound proxy a request would use (implemented
// by *httpclient.Factory). Optional: without it every request counts as
// direct, so host names must resolve locally to pass the guard.
type ProxyRouter interface {
	ProxyFor(target string, cfg core.UpstreamHTTP) (*url.URL, error)
}

// Guard is the SSRF guard (implemented by *proxy.Guard).
type Guard interface {
	Check(ctx context.Context, u *url.URL, proxied bool) error
}

// Options configure a Service.
type Options struct {
	Mode Mode
	// AllowHTTP fetches http:// images as written. Otherwise they are
	// upgraded to https://, and redirects to http:// are refused.
	AllowHTTP bool
	// Key signs image URLs. Empty: the key is read from (or created in) the
	// database, else a random per-process key is used.
	Key []byte
	// Pool caches images and holds the signing key; nil caches in memory only.
	Pool    *pgxpool.Pool
	Clients Clients
	Router  ProxyRouter
	Guard   Guard
	// MaxBytes, Timeout, MaxRedirects, TTL, FailureTTL and MemoryBytes
	// default to the Default* constants.
	MaxBytes     int64
	Timeout      time.Duration
	MaxRedirects int
	TTL          time.Duration
	FailureTTL   time.Duration
	MemoryBytes  int64
	Logger       *slog.Logger
	Now          func() time.Time
}

// Image is a fetched image.
type Image struct {
	ContentType string
	Body        []byte
}

// Service signs image URLs and fetches remote images.
type Service struct {
	opts  Options
	key   []byte
	mem   *lru
	group singleflight.Group
	log   *slog.Logger
	// lastPrune is when this node last removed expired database rows.
	pruneMu   sync.Mutex
	lastPrune time.Time
}

// DeriveKey derives the URL signing key from the server secret key
// (security.secret_key), so every node agrees without another setting.
func DeriveKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, "hangar readme images v1", 32)
}

// New returns a service. Without Options.Key the signing key is loaded from
// the database, which creates it on first use.
func New(ctx context.Context, o Options) (*Service, error) {
	mode, err := ParseMode(string(o.Mode))
	if err != nil {
		return nil, err
	}
	o.Mode = mode
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.FailureTTL <= 0 {
		o.FailureTTL = DefaultFailureTTL
	}
	if o.MemoryBytes <= 0 {
		o.MemoryBytes = DefaultMemoryBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Service{opts: o, key: o.Key, mem: newLRU(o.MemoryBytes), log: o.Logger}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if len(s.key) == 0 {
		if s.key, err = loadKey(ctx, o.Pool); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// loadKey reads the stored signing key, creating it if there is none. A nil
// pool gives a random key.
func loadKey(ctx context.Context, pool *pgxpool.Pool) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if pool == nil {
		return k, nil
	}
	if _, err := pool.Exec(ctx, `INSERT INTO readme_image_key (id, key) VALUES (true, $1) ON CONFLICT (id) DO NOTHING`, k); err != nil {
		return nil, fmt.Errorf("readme image key: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT key FROM readme_image_key WHERE id`).Scan(&k); err != nil {
		return nil, fmt.Errorf("readme image key: %w", err)
	}
	return k, nil
}

// Mode returns the configured mode.
func (s *Service) Mode() Mode { return s.opts.Mode }

// AllowHTTP reports whether http:// images are loaded as written.
func (s *Service) AllowHTTP() bool { return s.opts.AllowHTTP }

// Sign returns the signature of source (kind KindURL or KindMember) for
// component componentID.
func (s *Service) Sign(componentID, kind, source string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("v1\x00" + componentID + "\x00" + kind + "\x00" + source))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// Verify reports whether sig is Sign(componentID, kind, source).
func (s *Service) Verify(componentID, kind, source, sig string) bool {
	return hmac.Equal([]byte(sig), []byte(s.Sign(componentID, kind, source)))
}

// NormalizeURL returns the URL Fetch loads for raw, or false when raw is
// not an http(s) URL that may be loaded: http:// is upgraded to https://
// unless AllowHTTP is set; URLs with credentials are refused.
func (s *Service) NormalizeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxURLLen {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !s.opts.AllowHTTP {
			u.Scheme = "https"
			if u.Port() == "80" {
				u.Host = u.Hostname()
			}
		}
	default:
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment, u.RawFragment = "", ""
	return u.String(), true
}

// Fetch returns the image at rawURL (already normalized), from the cache
// or the remote. Failures are cached briefly too, so a broken badge is not
// requested again on every view.
func (s *Service) Fetch(ctx context.Context, rawURL string) (*Image, error) {
	key := sha256.Sum256([]byte(rawURL))
	now := s.opts.Now()
	if e, ok := s.mem.get(string(key[:]), now); ok {
		return e.image()
	}
	v, err, _ := s.group.Do(string(key[:]), func() (any, error) {
		// One fetch serves every waiting request, so it does not end with
		// the first caller's request.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.Timeout+5*time.Second)
		defer cancel()
		if e, ok := s.dbGet(fctx, key[:]); ok {
			s.mem.put(string(key[:]), e)
			return e, nil
		}
		img, ferr := s.fetchRemote(fctx, rawURL)
		e := &entry{expires: s.opts.Now().Add(s.opts.TTL)}
		if ferr != nil {
			e.err = ferr.Error()
			e.expires = s.opts.Now().Add(s.opts.FailureTTL)
		} else {
			e.contentType, e.body = img.ContentType, img.Body
		}
		s.mem.put(string(key[:]), e)
		s.dbPut(fctx, key[:], rawURL, e)
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*entry).image()
}

// dbGet reads a cache row that has not expired.
func (s *Service) dbGet(ctx context.Context, key []byte) (*entry, bool) {
	if s.opts.Pool == nil {
		return nil, false
	}
	e := &entry{}
	err := s.opts.Pool.QueryRow(ctx, `SELECT content_type, body, error, expires_at FROM readme_image_cache WHERE url_hash = $1 AND expires_at > $2`,
		key, s.opts.Now()).Scan(&e.contentType, &e.body, &e.err, &e.expires)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.WarnContext(ctx, "readme image cache read", "error", err)
		}
		return nil, false
	}
	return e, true
}

// dbPut stores e and, at most once an hour per node, prunes expired rows.
func (s *Service) dbPut(ctx context.Context, key []byte, rawURL string, e *entry) {
	if s.opts.Pool == nil {
		return
	}
	now := s.opts.Now()
	if _, err := s.opts.Pool.Exec(ctx, `
INSERT INTO readme_image_cache (url_hash, url, content_type, body, error, fetched_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (url_hash) DO UPDATE SET content_type = EXCLUDED.content_type, body = EXCLUDED.body, error = EXCLUDED.error,
    fetched_at = EXCLUDED.fetched_at, expires_at = EXCLUDED.expires_at`,
		key, rawURL, e.contentType, e.body, e.err, now, e.expires); err != nil {
		s.log.WarnContext(ctx, "readme image cache write", "error", err)
		return
	}
	s.pruneMu.Lock()
	prune := now.Sub(s.lastPrune) > time.Hour
	if prune {
		s.lastPrune = now
	}
	s.pruneMu.Unlock()
	if prune {
		if _, err := s.opts.Pool.Exec(ctx, `DELETE FROM readme_image_cache WHERE expires_at < $1`, now); err != nil {
			s.log.WarnContext(ctx, "readme image cache prune", "error", err)
		}
	}
}
