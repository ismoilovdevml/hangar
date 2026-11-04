package ldap_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/ldap"
	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	serviceDN = "CN=svc-hangar,OU=Service,DC=example,DC=com"
	servicePW = "service secret"
	usersDN   = "OU=People,DC=example,DC=com"
	groupsDN  = "OU=Groups,DC=example,DC=com"
)

type user struct {
	dn, name, password, mail, display string
	memberOf                          []string
}

type group struct {
	dn, cn  string
	members []string // DNs of users or groups
}

// fakeDir is an in-memory directory that understands the filters this
// package builds from the default configuration.
type fakeDir struct {
	cfg     auth.LDAPConfig
	users   []user
	groups  []group
	down    bool
	filters []string
	binds   []string
}

type fakeConn struct {
	d     *fakeDir
	bound string
}

func (d *fakeDir) dial(context.Context) (ldap.Conn, error) {
	if d.down {
		return nil, errors.New("connection refused")
	}
	return &fakeConn{d: d}, nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Bind(dn, password string) error {
	c.d.binds = append(c.d.binds, dn)
	if password == "" {
		return errors.New("fake: unauthenticated bind attempted")
	}
	if dn == serviceDN && password == servicePW {
		c.bound = dn
		return nil
	}
	for _, u := range c.d.users {
		if u.dn == dn && u.password == password {
			c.bound = dn
			return nil
		}
	}
	return goldap.NewError(goldap.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
}

func (c *fakeConn) Search(req *goldap.SearchRequest) (*goldap.SearchResult, error) {
	c.d.filters = append(c.d.filters, req.Filter)
	res := &goldap.SearchResult{}
	switch req.BaseDN {
	case usersDN:
		if c.bound != serviceDN {
			return nil, errors.New("fake: user search needs the service account")
		}
		for _, u := range c.d.users {
			if req.Filter == strings.ReplaceAll(c.d.cfg.UserFilter, "{username}", goldap.EscapeFilter(u.name)) {
				res.Entries = append(res.Entries, goldap.NewEntry(u.dn, map[string][]string{
					"sAMAccountName": {u.name}, "mail": {u.mail}, "displayName": {u.display}, "memberOf": u.memberOf,
				}))
			}
		}
	case groupsDN:
		if c.bound != serviceDN {
			return nil, errors.New("fake: group search needs the service account")
		}
		for _, g := range c.d.groups {
			for _, u := range c.d.users {
				direct := req.Filter == "(member="+goldap.EscapeFilter(u.dn)+")" && slices.Contains(g.members, u.dn)
				nested := req.Filter == "(member:1.2.840.113556.1.4.1941:="+goldap.EscapeFilter(u.dn)+")" && c.d.inChain(g, u.dn)
				if direct || nested {
					res.Entries = append(res.Entries, goldap.NewEntry(g.dn, map[string][]string{"cn": {g.cn}}))
				}
			}
		}
	default:
		return nil, goldap.NewError(goldap.LDAPResultNoSuchObject, errors.New("no such object"))
	}
	return res, nil
}

func (d *fakeDir) inChain(g group, dn string) bool {
	for _, m := range g.members {
		if m == dn {
			return true
		}
		for _, sub := range d.groups {
			if sub.dn == m && d.inChain(sub, dn) {
				return true
			}
		}
	}
	return false
}

func newDir(t *testing.T, mutate func(*auth.LDAPConfig)) (*ldap.Directory, *fakeDir) {
	t.Helper()
	c := auth.Config{LDAP: auth.LDAPConfig{
		Enabled: true, URL: "ldaps://dc1.example.com", BindDN: serviceDN, BindPassword: servicePW, UserBaseDN: usersDN,
	}}
	if mutate != nil {
		mutate(&c.LDAP)
	}
	cfg := c.WithDefaults().LDAP
	fd := &fakeDir{cfg: cfg,
		users: []user{
			{dn: "CN=Jane Doe,OU=People,DC=example,DC=com", name: "jdoe", password: "jane pw", mail: "jdoe@example.com",
				display: "Jane Doe", memberOf: []string{"CN=Hangar Admins,OU=Groups,DC=example,DC=com", "CN=Staff,OU=Groups,DC=example,DC=com"}},
			{dn: "CN=Star (*),OU=People,DC=example,DC=com", name: "st*r", password: "star pw"},
		},
		groups: []group{
			{dn: "CN=Platform,OU=Groups,DC=example,DC=com", cn: "Platform", members: []string{"CN=Jane Doe,OU=People,DC=example,DC=com"}},
			{dn: "CN=Engineering,OU=Groups,DC=example,DC=com", cn: "Engineering", members: []string{"CN=Platform,OU=Groups,DC=example,DC=com"}},
		},
	}
	d, err := ldap.New(cfg, ldap.Options{Dial: fd.dial})
	if err != nil {
		t.Fatal(err)
	}
	return d, fd
}

func TestAuthenticateMemberOf(t *testing.T) {
	d, fd := newDir(t, nil)
	id, err := d.Authenticate(context.Background(), "jdoe", "jane pw")
	if err != nil {
		t.Fatal(err)
	}
	if id.Source != auth.SourceLDAP || id.Username != "jdoe" || id.Email != "jdoe@example.com" || id.DisplayName != "Jane Doe" || id.Subject != "" {
		t.Fatalf("identity = %+v", id)
	}
	want := []string{"CN=Hangar Admins,OU=Groups,DC=example,DC=com", "Hangar Admins", "CN=Staff,OU=Groups,DC=example,DC=com", "Staff"}
	if !slices.Equal(id.Groups, want) {
		t.Fatalf("groups = %q, want %q", id.Groups, want)
	}
	if !slices.Equal(fd.binds, []string{serviceDN, "CN=Jane Doe,OU=People,DC=example,DC=com"}) {
		t.Fatalf("binds = %q", fd.binds)
	}
	if _, serr := d.Status(); serr != nil {
		t.Fatalf("status error = %v", serr)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	d, fd := newDir(t, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct{ user, pw string }{
		"wrong password": {"jdoe", "nope"},
		"unknown user":   {"nobody", "whatever"},
		"empty password": {"jdoe", ""},
		"empty username": {"", "jane pw"},
		"wildcard":       {"*", "jane pw"},
	} {
		fd.binds = nil
		_, err := d.Authenticate(ctx, tc.user, tc.pw)
		if !errors.Is(err, core.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
		if tc.pw == "" && len(fd.binds) > 0 {
			t.Errorf("%s: bind attempted with an empty password: %q", name, fd.binds)
		}
	}
	// The filter escapes the login name: "st*r" matches only that user.
	fd.filters = nil
	if _, err := d.Authenticate(ctx, "st*r", "star pw"); err != nil {
		t.Fatalf("escaped name: %v", err)
	}
	if want := `(&(objectClass=user)(sAMAccountName=st\2ar))`; !slices.Contains(fd.filters, want) {
		t.Fatalf("filters = %q, want %q", fd.filters, want)
	}
}

func TestAuthenticateAmbiguous(t *testing.T) {
	d, fd := newDir(t, nil)
	fd.users = append(fd.users, user{dn: "CN=Jane Doe 2,OU=People,DC=example,DC=com", name: "jdoe", password: "jane pw"})
	if _, err := d.Authenticate(context.Background(), "jdoe", "jane pw"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("two matching entries: err = %v", err)
	}
}

func TestAuthenticateGroupSearch(t *testing.T) {
	d, _ := newDir(t, func(c *auth.LDAPConfig) { c.GroupBaseDN = groupsDN })
	id, err := d.Authenticate(context.Background(), "jdoe", "jane pw")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CN=Platform,OU=Groups,DC=example,DC=com", "Platform"}; !slices.Equal(id.Groups, want) {
		t.Fatalf("groups = %q, want %q", id.Groups, want)
	}
}

func TestAuthenticateNestedGroups(t *testing.T) {
	d, fd := newDir(t, func(c *auth.LDAPConfig) { c.GroupBaseDN, c.NestedGroups = groupsDN, true })
	id, err := d.Authenticate(context.Background(), "jdoe", "jane pw")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(id.Groups, "Engineering") || !slices.Contains(id.Groups, "Platform") {
		t.Fatalf("groups = %q, want Platform and Engineering (nested)", id.Groups)
	}
	// Groups are searched as the service account after the user bind.
	if n := len(fd.binds); n != 3 || fd.binds[n-1] != serviceDN {
		t.Fatalf("binds = %q", fd.binds)
	}
}

func TestDirectoryErrors(t *testing.T) {
	d, fd := newDir(t, nil)
	fd.down = true
	_, err := d.Authenticate(context.Background(), "jdoe", "jane pw")
	if err == nil || errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("directory down: err = %v, want a non-credential error", err)
	}
	if _, serr := d.Status(); serr == nil {
		t.Fatal("status does not report the failure")
	}
	fd.down = false

	bad, _ := newDir(t, func(c *auth.LDAPConfig) { c.BindPassword = "wrong" })
	if _, err := bad.Authenticate(context.Background(), "jdoe", "jane pw"); err == nil || errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("bad service account: err = %v, want a configuration error", err)
	}
}

// TestIntegration runs against a real directory when HANGAR_TEST_LDAP_URL is
// set, with HANGAR_TEST_LDAP_BIND_DN, _BIND_PASSWORD, _USER_BASE_DN,
// _USER_FILTER (optional), _USERNAME and _PASSWORD.
func TestIntegration(t *testing.T) {
	url := os.Getenv("HANGAR_TEST_LDAP_URL")
	if url == "" {
		t.Skip("HANGAR_TEST_LDAP_URL not set; skipping LDAP integration test")
	}
	env := func(k string) string { return os.Getenv("HANGAR_TEST_LDAP_" + k) }
	c := auth.Config{LDAP: auth.LDAPConfig{
		Enabled: true, URL: url, StartTLS: env("START_TLS") == "true", InsecureSkipVerify: env("INSECURE") == "true",
		BindDN: env("BIND_DN"), BindPassword: env("BIND_PASSWORD"), UserBaseDN: env("USER_BASE_DN"),
		UserFilter: env("USER_FILTER"), UsernameAttribute: env("USERNAME_ATTRIBUTE"), GroupBaseDN: env("GROUP_BASE_DN"),
	}}.WithDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := ldap.New(c.LDAP, ldap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.Authenticate(context.Background(), env("USERNAME"), env("PASSWORD"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	t.Logf("signed in as %q <%s>, groups %q", id.Username, id.Email, id.Groups)
	if _, err := d.Authenticate(context.Background(), env("USERNAME"), env("PASSWORD")+"-wrong"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("wrong password: err = %v", err)
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	d, fd := newDir(t, nil)
	res, err := d.Probe(ctx, "jdoe")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stage != ldap.StageLookup || res.DN != "CN=Jane Doe,OU=People,DC=example,DC=com" || res.User == nil || len(res.User.Groups) != 4 {
		t.Fatalf("probe = %+v", res)
	}
	if !slices.Equal(fd.binds, []string{serviceDN}) {
		t.Fatalf("binds = %q: the probe must not bind as the user", fd.binds)
	}

	if res, err = d.Probe(ctx, "nobody"); err != nil || res.User != nil || res.Stage != ldap.StageLookup {
		t.Fatalf("unknown user: %+v, %v", res, err)
	}
	if res, err = d.Probe(ctx, ""); err != nil || res.Stage != ldap.StageBind {
		t.Fatalf("no username: %+v, %v", res, err)
	}

	bad, _ := newDir(t, func(c *auth.LDAPConfig) { c.BindPassword = "wrong" })
	if res, err = bad.Probe(ctx, "jdoe"); err == nil || res.Stage != ldap.StageBind {
		t.Fatalf("wrong bind password: %+v, %v", res, err)
	}
	fd.down = true
	if res, err = d.Probe(ctx, "jdoe"); err == nil || res.Stage != ldap.StageConnect {
		t.Fatalf("down: %+v, %v", res, err)
	}
}
