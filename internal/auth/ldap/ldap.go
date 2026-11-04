// Package ldap authenticates users against an LDAP directory or Active
// Directory for auth.Service (auth.Directory): a service bind, a search for
// exactly one user entry, a bind as that entry with the user's password, and
// the user's groups from memberOf or a group search (optionally resolving
// Active Directory nested groups).
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Conn is the part of an LDAP connection the directory uses.
type Conn interface {
	Bind(username, password string) error
	Search(req *goldap.SearchRequest) (*goldap.SearchResult, error)
	Close() error
}

// Dialer opens a connection, already upgraded with StartTLS when configured.
type Dialer func(ctx context.Context) (Conn, error)

// Options are optional collaborators of a Directory.
type Options struct {
	// Dial replaces the network dialer (tests).
	Dial   Dialer
	Logger *slog.Logger
}

// Directory implements auth.Directory. Each sign-in uses its own
// connection, dialed with the configured timeout.
type Directory struct {
	cfg  auth.LDAPConfig
	dial Dialer
	log  *slog.Logger

	mu      sync.Mutex
	lastErr error
	lastAt  time.Time
}

var _ auth.Directory = (*Directory)(nil)

// maxGroups bounds the groups read for one user.
const maxGroups = 1000

// New returns a directory for cfg (defaults applied by auth.Config). It
// reads ca_file but does not connect.
func New(cfg auth.LDAPConfig, opts Options) (*Directory, error) {
	d := &Directory{cfg: cfg, dial: opts.Dial, log: opts.Logger}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.dial == nil {
		dial, err := netDialer(cfg)
		if err != nil {
			return nil, err
		}
		d.dial = dial
	}
	if u, err := url.Parse(cfg.URL); err == nil && u.Scheme == "ldap" && !cfg.StartTLS {
		d.log.Warn("auth.ldap: " + cfg.URL + " is not encrypted and start_tls is off; passwords cross the network in clear text")
	}
	return d, nil
}

func netDialer(cfg auth.LDAPConfig) (Dialer, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("auth.ldap.url: %w", err)
	}
	tc := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // G402: opt-in for test directories, documented as unsafe
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth.ldap.ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auth.ldap.ca_file %s: no PEM certificates found", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	return func(ctx context.Context) (Conn, error) {
		nd := &net.Dialer{Timeout: cfg.Timeout}
		if dl, ok := ctx.Deadline(); ok {
			nd.Deadline = dl
		}
		c, err := goldap.DialURL(cfg.URL, goldap.DialWithDialer(nd), goldap.DialWithTLSConfig(tc))
		if err != nil {
			return nil, err
		}
		c.SetTimeout(cfg.Timeout)
		if cfg.StartTLS {
			if err := c.StartTLS(tc); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("start tls: %w", err)
			}
		}
		return c, nil
	}, nil
}

var errBadCredentials = fmt.Errorf("invalid directory credentials: %w", core.ErrUnauthenticated)

// Authenticate implements auth.Directory.
func (d *Directory) Authenticate(ctx context.Context, username, password string) (*auth.ExternalIdentity, error) {
	// Never send an empty password: it is an unauthenticated bind, which
	// many directories accept for any DN.
	if strings.TrimSpace(username) == "" || password == "" || !utf8.ValidString(username) {
		return nil, errBadCredentials
	}
	ctx, cancel := context.WithTimeout(ctx, 3*d.cfg.Timeout)
	defer cancel()
	id, err := d.authenticate(ctx, username, password)
	if err != nil && !errors.Is(err, core.ErrUnauthenticated) {
		d.noteResult(err)
		return nil, err
	}
	d.noteResult(nil)
	return id, err
}

func (d *Directory) authenticate(ctx context.Context, username, password string) (*auth.ExternalIdentity, error) {
	c, err := d.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", d.cfg.URL, err)
	}
	defer func() { _ = c.Close() }()

	if err := d.serviceBind(c); err != nil {
		return nil, err
	}
	attrs := []string{"dn", d.cfg.UsernameAttribute, d.cfg.EmailAttribute, d.cfg.NameAttribute}
	if d.cfg.GroupBaseDN == "" {
		attrs = append(attrs, d.cfg.GroupAttribute)
	}
	filter := strings.ReplaceAll(d.cfg.UserFilter, "{username}", goldap.EscapeFilter(username))
	res, err := c.Search(goldap.NewSearchRequest(d.cfg.UserBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		2, int(d.cfg.Timeout/time.Second), false, filter, attrs, nil))
	if err != nil && !goldap.IsErrorWithCode(err, goldap.LDAPResultSizeLimitExceeded) {
		if goldap.IsErrorWithCode(err, goldap.LDAPResultNoSuchObject) {
			return nil, fmt.Errorf("search users: user_base_dn %q does not exist: %w", d.cfg.UserBaseDN, err)
		}
		return nil, fmt.Errorf("search users: %w", err)
	}
	if res == nil || len(res.Entries) == 0 {
		return nil, errBadCredentials
	}
	if len(res.Entries) > 1 {
		d.log.WarnContext(ctx, "auth.ldap: user_filter matches more than one entry; refusing sign-in", "username", username)
		return nil, errBadCredentials
	}
	entry := res.Entries[0]
	if entry.DN == "" {
		return nil, errBadCredentials
	}
	if err := c.Bind(entry.DN, password); err != nil {
		if goldap.IsErrorWithCode(err, goldap.LDAPResultInvalidCredentials) {
			return nil, errBadCredentials
		}
		return nil, fmt.Errorf("bind as user: %w", err)
	}
	id := &auth.ExternalIdentity{
		Source:      auth.SourceLDAP,
		Username:    entry.GetAttributeValue(d.cfg.UsernameAttribute),
		Email:       entry.GetAttributeValue(d.cfg.EmailAttribute),
		DisplayName: entry.GetAttributeValue(d.cfg.NameAttribute),
	}
	if id.Username == "" {
		id.Username = username
	}
	if d.cfg.GroupBaseDN == "" {
		id.Groups = groupNames(entry.GetAttributeValues(d.cfg.GroupAttribute))
		return id, nil
	}
	// The connection is bound as the user now; search groups as the
	// service account, which is what directories grant read access to.
	if err := d.serviceBind(c); err != nil {
		return nil, err
	}
	gf := strings.NewReplacer("{dn}", goldap.EscapeFilter(entry.DN), "{username}", goldap.EscapeFilter(id.Username)).Replace(d.cfg.GroupFilter)
	gres, err := c.Search(goldap.NewSearchRequest(d.cfg.GroupBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		maxGroups, int(d.cfg.Timeout/time.Second), false, gf, []string{"dn", d.cfg.GroupNameAttribute}, nil))
	if err != nil && !goldap.IsErrorWithCode(err, goldap.LDAPResultSizeLimitExceeded) {
		return nil, fmt.Errorf("search groups: %w", err)
	}
	if gres != nil {
		for _, g := range gres.Entries {
			id.Groups = append(id.Groups, g.DN)
			if n := g.GetAttributeValue(d.cfg.GroupNameAttribute); n != "" {
				id.Groups = append(id.Groups, n)
			}
		}
	}
	id.Groups = compact(id.Groups)
	return id, nil
}

func (d *Directory) serviceBind(c Conn) error {
	if d.cfg.BindDN == "" {
		return nil // anonymous search
	}
	if err := c.Bind(d.cfg.BindDN, d.cfg.BindPassword); err != nil {
		if goldap.IsErrorWithCode(err, goldap.LDAPResultInvalidCredentials) {
			return fmt.Errorf("service bind as %q: check auth.ldap.bind_dn and bind_password: %w", d.cfg.BindDN, err)
		}
		return fmt.Errorf("service bind: %w", err)
	}
	return nil
}

// groupNames returns each group DN followed by the value of its first RDN
// ("CN=Hangar Admins,OU=Groups,DC=example,DC=com" -> also "Hangar Admins"),
// so a mapping may name either.
func groupNames(dns []string) []string {
	out := make([]string, 0, 2*len(dns))
	for _, dn := range dns {
		if len(out) >= 2*maxGroups {
			break
		}
		out = append(out, dn)
		if parsed, err := goldap.ParseDN(dn); err == nil && len(parsed.RDNs) > 0 && len(parsed.RDNs[0].Attributes) > 0 {
			out = append(out, parsed.RDNs[0].Attributes[0].Value)
		}
	}
	return compact(out)
}

func compact(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, v := range s {
		k := strings.ToLower(v)
		if v == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return slices.Clip(out)
}

func (d *Directory) noteResult(err error) {
	d.mu.Lock()
	d.lastErr, d.lastAt = err, time.Now()
	d.mu.Unlock()
}

// Status reports the outcome of the most recent directory request: nil when
// it worked or none was made yet.
func (d *Directory) Status() (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastAt, d.lastErr
}
