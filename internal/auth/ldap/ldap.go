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
	if cfg.CAFile != "" || cfg.CAPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("auth.ldap.ca_file: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("auth.ldap.ca_file %s: no PEM certificates found", cfg.CAFile)
			}
		}
		if cfg.CAPEM != "" && !pool.AppendCertsFromPEM([]byte(cfg.CAPEM)) {
			return nil, errors.New("ldap: the CA bundle has no PEM certificates")
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
	entry, err := d.findUser(ctx, c, username)
	if err != nil {
		return nil, err
	}
	if err := c.Bind(entry.DN, password); err != nil {
		if goldap.IsErrorWithCode(err, goldap.LDAPResultInvalidCredentials) {
			return nil, errBadCredentials
		}
		return nil, fmt.Errorf("bind as user: %w", err)
	}
	// The connection is bound as the user now; groups are searched as the
	// service account, which is what directories grant read access to.
	return d.identity(c, entry, username, true)
}

// findUser searches the one entry user_filter matches for username. No
// match, or more than one, is errBadCredentials.
func (d *Directory) findUser(ctx context.Context, c Conn, username string) (*goldap.Entry, error) {
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
		return nil, errAmbiguous
	}
	if res.Entries[0].DN == "" {
		return nil, errBadCredentials
	}
	return res.Entries[0], nil
}

var errAmbiguous = fmt.Errorf("user_filter matches more than one entry: %w", errBadCredentials)

// identity maps entry to an identity with its groups. rebind binds as the
// service account again before a group search.
func (d *Directory) identity(c Conn, entry *goldap.Entry, username string, rebind bool) (*auth.ExternalIdentity, error) {
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
	if rebind {
		if err := d.serviceBind(c); err != nil {
			return nil, err
		}
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

// Probe stages, in the order Probe runs them.
const (
	StageConnect = "connect"
	StageBind    = "bind"
	StageLookup  = "lookup"
)

// ProbeResult is what Probe found.
type ProbeResult struct {
	// Stage is the last stage reached: the failing one when Probe returns an
	// error, else StageBind (no username) or StageLookup.
	Stage string
	// Anonymous is true when no bind DN is configured.
	Anonymous bool
	// DN and User describe the entry found for the username (nil when no
	// entry matched or no username was given).
	DN   string
	User *auth.ExternalIdentity
}

// Probe checks the directory settings without signing anybody in: it
// connects (with StartTLS when configured), binds as the service account
// and, when username is not empty, looks the user and their groups up. The
// user's password is never needed or sent.
func (d *Directory) Probe(ctx context.Context, username string) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*d.cfg.Timeout)
	defer cancel()
	res := ProbeResult{Stage: StageConnect, Anonymous: d.cfg.BindDN == ""}
	c, err := d.dial(ctx)
	if err != nil {
		return res, fmt.Errorf("connect to %s: %w", d.cfg.URL, err)
	}
	defer func() { _ = c.Close() }()
	res.Stage = StageBind
	if err := d.serviceBind(c); err != nil {
		return res, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return res, nil
	}
	res.Stage = StageLookup
	entry, err := d.findUser(ctx, c, username)
	switch {
	case errors.Is(err, errAmbiguous):
		return res, fmt.Errorf("user_filter matches more than one entry for %q", username)
	case errors.Is(err, errBadCredentials):
		return res, nil
	case err != nil:
		return res, err
	}
	res.DN = entry.DN
	if res.User, err = d.identity(c, entry, username, false); err != nil {
		return res, err
	}
	return res, nil
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
