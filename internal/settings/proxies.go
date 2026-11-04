package settings

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

// ProxySettings are the editable fields of an outbound proxy.
type ProxySettings struct {
	// Scheme is how Hangar talks to the proxy: http or https.
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// NoProxy lists hosts, domain suffixes (.corp.example) and CIDRs that
	// are reached directly.
	NoProxy []string `json:"no_proxy"`
	// RedirectHosts are the hosts upstream redirects may lead to and still
	// go through this proxy (a registry's CDN). Empty: every host does.
	RedirectHosts []string `json:"redirect_hosts"`
	// CAPEM is a PEM bundle trusted for requests through this proxy.
	CAPEM string `json:"ca_pem"`
	// Default makes this the platform default proxy (at most one).
	Default bool `json:"default"`
}

// OutboundProxy is a proxy as the API returns it (schema OutboundProxy).
type OutboundProxy struct {
	Name string `json:"name"`
	ProxySettings
	PasswordConfigured bool `json:"password_configured"`
	// Source is SourceFile (http.proxy) or SourceDatabase.
	Source     string   `json:"source"`
	FileFields []string `json:"file_fields"`
	// Repositories counts the proxy repositories that select this proxy by
	// name; Inheriting those that use the platform default (default only).
	Repositories int        `json:"repositories"`
	Inheriting   int        `json:"inheriting"`
	CreatedAt    *time.Time `json:"created_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
	UpdatedBy    string     `json:"updated_by,omitempty"`
}

// ProxyList is the body of GET /proxies (schema OutboundProxyList).
type ProxyList struct {
	Items []OutboundProxy `json:"items"`
	// Default names the platform default proxy ("" = none).
	Default string `json:"default"`
	// EnvironmentProxy is the proxy taken from HTTPS_PROXY when there is no
	// default (password masked).
	EnvironmentProxy string `json:"environment_proxy,omitempty"`
	// CAFile is http.ca_file, trusted for every request.
	CAFile string `json:"ca_file,omitempty"`
	// Limited is true when the caller may not read settings: items then
	// carry only name and default.
	Limited bool `json:"limited"`
}

// ProxyInput is the body of POST /proxies and PUT /proxies/{name}. Name is
// used on create only. An empty Password keeps the stored one;
// ClearPassword removes it.
type ProxyInput struct {
	Name string `json:"name"`
	ProxySettings
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
}

var proxyFileFields = []string{"scheme", "host", "port", "username", "password", "no_proxy", "redirect_hosts", "ca_pem", "default"}

// storedProxy is a row of outbound_proxies; Password is sealed.
type storedProxy struct {
	OutboundProxy
	password string
}

const proxyCols = `name, scheme, host, port, username, password, no_proxy, redirect_hosts, ca_pem, is_default, created_at, updated_at, updated_by`

func scanProxy(row pgx.Row) (*storedProxy, error) {
	var (
		p      storedProxy
		ca, up time.Time
	)
	err := row.Scan(&p.Name, &p.Scheme, &p.Host, &p.Port, &p.Username, &p.password, &p.NoProxy, &p.RedirectHosts,
		&p.CAPEM, &p.Default, &ca, &up, &p.UpdatedBy)
	if err != nil {
		return nil, err
	}
	ca, up = ca.UTC(), up.UTC()
	p.CreatedAt, p.UpdatedAt = &ca, &up
	p.Source, p.FileFields = SourceDatabase, []string{}
	p.PasswordConfigured = p.password != ""
	p.NoProxy, p.RedirectHosts = nonNil(p.NoProxy), nonNil(p.RedirectHosts)
	return &p, nil
}

// fileProxy is the proxy http.proxy defines, or nil.
func (s *Service) fileProxy() *OutboundProxy {
	h := s.opts.File.HTTP
	if h.Proxy == "" {
		return nil
	}
	p := &OutboundProxy{Name: httpclient.FileProxyName, Source: SourceFile, FileFields: proxyFileFields,
		ProxySettings:      ProxySettings{Username: h.ProxyUsername, NoProxy: nonNil(h.NoProxy), RedirectHosts: []string{}, Default: true},
		PasswordConfigured: h.ProxyPassword != ""}
	if u, err := url.Parse(h.Proxy); err == nil {
		p.Scheme, p.Host = u.Scheme, u.Hostname()
		p.Port, _ = strconv.Atoi(u.Port())
		if p.Port == 0 {
			p.Port = defaultPort(u.Scheme)
		}
		if u.User != nil && p.Username == "" {
			p.Username = u.User.Username()
			_, p.PasswordConfigured = u.User.Password()
		}
	}
	return p
}

func defaultPort(scheme string) int {
	switch scheme {
	case "https":
		return 443
	case "socks5", "socks5h":
		return 1080
	}
	return 80
}

// loadProxies reads every stored proxy, ordered by name.
func (s *Service) loadProxies(ctx context.Context) ([]*storedProxy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+proxyCols+` FROM outbound_proxies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("settings: read proxies: %w", err)
	}
	defer rows.Close()
	var out []*storedProxy
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, fmt.Errorf("settings: read proxies: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// proxyUsage counts proxy repositories by the proxy they name; "" counts
// those that inherit the platform default.
func (s *Service) proxyUsage(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(config->'proxy'->'http'->>'proxy_name', ''), count(*) FROM repositories
		WHERE type = 'proxy' AND coalesce(config->'proxy'->'http'->>'proxy', '') IN ('', 'inherit')
		GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("settings: count proxy users: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// ListProxies returns the proxies: the file's first, then the stored ones.
// Callers that may not read settings see names only, for the repository
// proxy picker.
func (s *Service) ListProxies(ctx context.Context) (*ProxyList, error) {
	if core.PrincipalFrom(ctx).IsAnonymous() {
		return nil, fmt.Errorf("sign in to list proxies: %w", core.ErrUnauthenticated)
	}
	stored, err := s.loadProxies(ctx)
	if err != nil {
		return nil, err
	}
	list := &ProxyList{Items: []OutboundProxy{}}
	if fp := s.fileProxy(); fp != nil {
		list.Items = append(list.Items, *fp)
		list.Default = fp.Name
	}
	for _, p := range stored {
		if p.Default && list.Default == "" {
			list.Default = p.Name
		}
		p.Default = p.Name == list.Default
		list.Items = append(list.Items, p.OutboundProxy)
	}
	if !s.canRead(ctx) {
		list.Limited = true
		for i, p := range list.Items {
			list.Items[i] = OutboundProxy{Name: p.Name, Source: p.Source, FileFields: []string{},
				ProxySettings: ProxySettings{Default: p.Default, NoProxy: []string{}, RedirectHosts: []string{}}}
		}
		return list, nil
	}
	usage, err := s.proxyUsage(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		p.Repositories = usage[p.Name]
		if p.Default {
			p.Inheriting = usage[""]
		}
	}
	list.CAFile = s.opts.File.HTTP.CAFile
	if list.Default == "" && s.opts.EnvironmentProxy != nil {
		list.EnvironmentProxy = s.opts.EnvironmentProxy()
	}
	return list, nil
}

// GetProxy returns one proxy.
func (s *Service) GetProxy(ctx context.Context, name string) (*OutboundProxy, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "proxy:"+name); err != nil {
		return nil, err
	}
	list, err := s.ListProxies(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range list.Items {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("proxy %q: %w", name, core.ErrNotFound)
}

// CheckProxyName returns nil when a proxy called name exists (for the
// repository service).
func (s *Service) CheckProxyName(ctx context.Context, name string) error {
	if fp := s.fileProxy(); fp != nil && fp.Name == name {
		return nil
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbound_proxies WHERE name = $1)`, name).Scan(&ok); err != nil {
		return fmt.Errorf("settings: find proxy: %w", err)
	}
	if !ok {
		return fmt.Errorf("proxy_name: there is no outbound proxy %q: %w", name, core.ErrInvalid)
	}
	return nil
}

// CreateProxy stores a new proxy.
func (s *Service) CreateProxy(ctx context.Context, in ProxyInput) (*OutboundProxy, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := s.authorize(ctx, core.ActionAdmin, AuditProxyCreate, "proxy:"+in.Name); err != nil {
		return nil, err
	}
	fe := FieldErrors{}
	switch {
	case in.Name == "":
		fe.add("name", "is required")
	case !core.ValidProxyName(in.Name):
		fe.add("name", "use 1 to 63 lower-case letters, digits, '.', '_' or '-', starting with a letter or digit")
	case in.Name == httpclient.FileProxyName:
		fe.add("name", fmt.Sprintf("%q is reserved for the proxy of the configuration file", in.Name))
	}
	if err := s.saveProxy(ctx, nil, in, fe); err != nil {
		return nil, err
	}
	s.record(ctx, core.AuditEvent{Action: AuditProxyCreate, Target: "proxy:" + in.Name, Details: proxyDetails(in, nil)})
	if err := s.changed(ctx, "proxies"); err != nil {
		return nil, fmt.Errorf("saved, but not applied: %w", err)
	}
	return s.GetProxy(ctx, in.Name)
}

// UpdateProxy replaces the settings of a stored proxy. The name cannot
// change.
func (s *Service) UpdateProxy(ctx context.Context, name string, in ProxyInput) (*OutboundProxy, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditProxyUpdate, "proxy:"+name); err != nil {
		return nil, err
	}
	if name == httpclient.FileProxyName && s.fileProxy() != nil {
		return nil, fmt.Errorf("proxy %q is set in the configuration file (http.proxy); change it there: %w", name, core.ErrInvalid)
	}
	fe := FieldErrors{}
	if in.Name != "" && in.Name != name {
		fe.add("name", "cannot be changed; create a new proxy instead")
	}
	in.Name = name
	old, err := s.storedProxy(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.saveProxy(ctx, old, in, fe); err != nil {
		return nil, err
	}
	s.record(ctx, core.AuditEvent{Action: AuditProxyUpdate, Target: "proxy:" + name, Details: proxyDetails(in, old)})
	if err := s.changed(ctx, "proxies"); err != nil {
		return nil, fmt.Errorf("saved, but not applied: %w", err)
	}
	return s.GetProxy(ctx, name)
}

// DeleteProxy removes a stored proxy no repository selects.
func (s *Service) DeleteProxy(ctx context.Context, name string) error {
	if err := s.authorize(ctx, core.ActionAdmin, AuditProxyDelete, "proxy:"+name); err != nil {
		return err
	}
	if name == httpclient.FileProxyName && s.fileProxy() != nil {
		return fmt.Errorf("proxy %q is set in the configuration file (http.proxy); remove it there: %w", name, core.ErrInvalid)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var found string
		err := tx.QueryRow(ctx, `SELECT name FROM outbound_proxies WHERE name = $1 FOR UPDATE`, name).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("proxy %q: %w", name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("settings: find proxy: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT name FROM repositories
			WHERE type = 'proxy' AND config->'proxy'->'http'->>'proxy_name' = $1 ORDER BY name`, name)
		if err != nil {
			return fmt.Errorf("settings: find proxy users: %w", err)
		}
		users, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("settings: find proxy users: %w", err)
		}
		if len(users) > 0 {
			return &InUseError{Proxy: name, Repositories: users[:min(len(users), 5)], Total: len(users)}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM outbound_proxies WHERE name = $1`, name); err != nil {
			return fmt.Errorf("settings: delete proxy: %w", err)
		}
		return bumpRevision(ctx, tx)
	})
	if err != nil {
		return err
	}
	s.record(ctx, core.AuditEvent{Action: AuditProxyDelete, Target: "proxy:" + name})
	if err := s.changed(ctx, "proxies"); err != nil {
		return fmt.Errorf("deleted, but not applied: %w", err)
	}
	return nil
}

func (s *Service) storedProxy(ctx context.Context, name string) (*storedProxy, error) {
	p, err := scanProxy(s.pool.QueryRow(ctx, `SELECT `+proxyCols+` FROM outbound_proxies WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("proxy %q: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("settings: read proxy: %w", err)
	}
	return p, nil
}

// saveProxy validates in and inserts it (old == nil) or updates old.
func (s *Service) saveProxy(ctx context.Context, old *storedProxy, in ProxyInput, fe FieldErrors) error {
	in.normalize()
	oldSecret := ""
	if old != nil {
		oldSecret = old.password
	}
	_, sealed, sfe, err := s.mergeSecret(oldSecret, in.Password, in.ClearPassword, "password")
	if err != nil {
		return err
	}
	for k, v := range sfe {
		fe.add(k, v)
	}
	s.checkProxy(fe, in, sealed != "")
	if err := fe.err(); err != nil {
		return err
	}
	by := core.PrincipalFrom(ctx).Name
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if in.Default {
			if _, err := tx.Exec(ctx, `UPDATE outbound_proxies SET is_default = false, updated_at = now(), updated_by = $2
				WHERE is_default AND name <> $1`, in.Name, by); err != nil {
				return fmt.Errorf("settings: clear default proxy: %w", err)
			}
		}
		var tag pgconn.CommandTag
		var err error
		if old == nil {
			tag, err = tx.Exec(ctx, `
				INSERT INTO outbound_proxies (name, scheme, host, port, username, password, no_proxy, redirect_hosts, ca_pem, is_default, updated_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				in.Name, in.Scheme, in.Host, in.Port, in.Username, sealed, in.NoProxy, in.RedirectHosts, in.CAPEM, in.Default, by)
		} else {
			tag, err = tx.Exec(ctx, `
				UPDATE outbound_proxies SET scheme = $2, host = $3, port = $4, username = $5, password = $6, no_proxy = $7,
					redirect_hosts = $8, ca_pem = $9, is_default = $10, updated_at = now(), updated_by = $11
				WHERE name = $1`,
				in.Name, in.Scheme, in.Host, in.Port, in.Username, sealed, in.NoProxy, in.RedirectHosts, in.CAPEM, in.Default, by)
		}
		var pg *pgconn.PgError
		switch {
		case errors.As(err, &pg) && pg.Code == "23505":
			return fmt.Errorf("proxy %q: %w", in.Name, core.ErrAlreadyExists)
		case err != nil:
			return fmt.Errorf("settings: save proxy: %w", err)
		case tag.RowsAffected() == 0:
			return fmt.Errorf("proxy %q: %w", in.Name, core.ErrNotFound)
		}
		return bumpRevision(ctx, tx)
	})
	return err
}

func (in *ProxyInput) normalize() {
	in.Scheme = strings.ToLower(strings.TrimSpace(in.Scheme))
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	in.NoProxy = cleanList(in.NoProxy)
	in.RedirectHosts = cleanList(in.RedirectHosts)
	in.CAPEM = strings.TrimSpace(in.CAPEM)
}

// checkProxy adds the problems of in to fe.
func (s *Service) checkProxy(fe FieldErrors, in ProxyInput, hasPassword bool) {
	if in.Scheme != "http" && in.Scheme != "https" {
		fe.add("scheme", "choose http or https")
	}
	switch {
	case in.Host == "":
		fe.add("host", "is required")
	case strings.ContainsAny(in.Host, "/@: \t") && net.ParseIP(in.Host) == nil:
		fe.add("host", "enter a host name or IP address only, without scheme, port or path")
	case net.ParseIP(in.Host) == nil && !validHostname(in.Host):
		fe.add("host", "is not a valid host name")
	}
	if in.Port < 1 || in.Port > 65535 {
		fe.add("port", "must be between 1 and 65535")
	}
	if hasPassword && in.Username == "" {
		fe.add("username", "is required when a password is set")
	}
	for _, h := range in.NoProxy {
		if strings.ContainsAny(h, " \t,/") && !isCIDR(h) {
			fe.add("no_proxy", fmt.Sprintf("%q: enter a host, a domain suffix like .corp.example, an IP or a CIDR", h))
		}
	}
	for _, h := range in.RedirectHosts {
		if err := core.ValidateHostPattern(h); err != nil {
			fe.add("redirect_hosts", fmt.Sprintf("%q: enter a host or a domain suffix like .cdn.example", h))
		}
	}
	if in.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(in.CAPEM)) {
		fe.add("ca_pem", "contains no PEM certificate")
	}
	if in.Default && s.fileProxy() != nil {
		fe.add("default", "the configuration file sets the platform default (http.proxy)")
	}
}

func isCIDR(s string) bool { _, _, err := net.ParseCIDR(s); return err == nil }

func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// proxyDetails is the audit record of a proxy change, without the password.
func proxyDetails(in ProxyInput, old *storedProxy) map[string]any {
	d := map[string]any{"host": in.Host, "port": in.Port, "scheme": in.Scheme, "default": in.Default}
	switch {
	case in.ClearPassword:
		d["password"] = "cleared"
	case in.Password != "":
		d["password"] = "set"
	}
	if old != nil {
		d["changed"] = diffFields(old.ProxySettings, in.ProxySettings, in.ClearPassword || in.Password != "", "password")
	}
	return d
}

// spec turns a stored proxy into the httpclient form, password opened.
func (s *Service) spec(p *storedProxy) (httpclient.Proxy, error) {
	password, err := s.open(p.password)
	if err != nil {
		return httpclient.Proxy{}, fmt.Errorf("proxy %q password: %w", p.Name, err)
	}
	return httpclient.Proxy{
		Name: p.Name, URL: p.Scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Username: p.Username, Password: password, NoProxy: slices.Clone(p.NoProxy),
		RedirectHosts: slices.Clone(p.RedirectHosts), CAPEM: p.CAPEM,
	}, nil
}

// effectiveOutbound returns the proxies to apply: the file's proxy (the
// default) and every stored proxy.
func (s *Service) effectiveOutbound(ctx context.Context) (httpclient.Outbound, error) {
	out := httpclient.FileOutbound(s.opts.File.HTTP)
	stored, err := s.loadProxies(ctx)
	if err != nil {
		return out, err
	}
	var errs []error
	for _, p := range stored {
		spec, err := s.spec(p)
		if err != nil {
			// Still defined, so repositories that name it fail clearly
			// instead of going direct.
			errs = append(errs, err)
		}
		if spec.Name == "" {
			spec = httpclient.Proxy{Name: p.Name, URL: p.Scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
				Username: p.Username, NoProxy: p.NoProxy, RedirectHosts: p.RedirectHosts, CAPEM: p.CAPEM}
		}
		out.Proxies = append(out.Proxies, spec)
		if p.Default && out.Default == "" {
			out.Default = p.Name
		}
	}
	if len(errs) > 0 {
		s.log.ErrorContext(ctx, "settings: stored proxy passwords cannot be opened", "error", errors.Join(errs...))
	}
	return out, nil
}
