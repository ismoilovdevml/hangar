package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// Validation limits of a webhook.
const (
	maxURLLength    = 2048
	maxRepositories = 100
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	// repoPatternRE is the privilege pattern alphabet (auth.ValidatePrivileges):
	// repository names and glob characters.
	repoPatternRE = regexp.MustCompile(`^[a-z0-9*?\[\]^!-]{1,64}$`)
	uuidRE        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// stored is a row of webhooks; url is in full and secret sealed.
type stored struct {
	Webhook
	url    string
	secret string
}

const hookCols = `id::text, name, url, events, repositories, format, secret, enabled, consecutive_failures, disabled_reason,
	last_delivery_at, last_status_code, created_at, updated_at, updated_by`

func scanHook(row pgx.Row) (*stored, error) {
	var h stored
	err := row.Scan(&h.ID, &h.Name, &h.url, &h.Events, &h.Repositories, &h.Format, &h.secret, &h.Enabled,
		&h.ConsecutiveFailures, &h.DisabledReason, &h.LastDeliveryAt, &h.LastStatusCode, &h.CreatedAt, &h.UpdatedAt, &h.UpdatedBy)
	if err != nil {
		return nil, err
	}
	h.URL = RedactURL(h.url)
	h.SecretConfigured = h.secret != ""
	h.CreatedAt, h.UpdatedAt = h.CreatedAt.UTC(), h.UpdatedAt.UTC()
	if h.LastDeliveryAt != nil {
		t := h.LastDeliveryAt.UTC()
		h.LastDeliveryAt = &t
	}
	if h.Events == nil {
		h.Events = []string{}
	}
	if h.Repositories == nil {
		h.Repositories = []string{}
	}
	return &h, nil
}

// RedactURL is how the API shows a webhook URL: scheme, host and port only,
// with "/***" standing for the rest. Incoming-webhook URLs of Slack, Teams
// and most other services carry their credential in the path or query, so
// the full URL is write-only like a secret.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "***"
	}
	out := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		out += "/***"
	}
	return out
}

// List returns every webhook, by name (system:webhooks:read).
func (s *Service) List(ctx context.Context) (*WebhookList, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "webhook:*"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+hookCols+` FROM webhooks ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()
	out := &WebhookList{Items: []Webhook{}}
	for rows.Next() {
		h, err := scanHook(rows)
		if err != nil {
			return nil, fmt.Errorf("webhooks: list: %w", err)
		}
		out.Items = append(out.Items, h.Webhook)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	return out, nil
}

// Get returns one webhook (system:webhooks:read).
func (s *Service) Get(ctx context.Context, id string) (*Webhook, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "webhook:"+id); err != nil {
		return nil, err
	}
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	return &h.Webhook, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// load reads webhook id, locking the row when forUpdate (q must be a tx).
func (s *Service) load(ctx context.Context, q querier, id string, forUpdate bool) (*stored, error) {
	if !uuidRE.MatchString(id) {
		return nil, fmt.Errorf("webhook %q: %w", id, core.ErrNotFound)
	}
	sql := `SELECT ` + hookCols + ` FROM webhooks WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	h, err := scanHook(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("webhook %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: read %s: %w", id, err)
	}
	return h, nil
}

// Create stores a new webhook (system:webhooks:admin, and read access to
// the repositories it names).
func (s *Service) Create(ctx context.Context, in Input) (*Webhook, error) {
	in.normalize()
	if err := s.authorize(ctx, core.ActionAdmin, AuditCreate, target(in.Name)); err != nil {
		return nil, err
	}
	fe := FieldErrors{}
	if in.URL == "" {
		fe.add("url", "is required")
	}
	s.check(ctx, fe, &in)
	// The id is chosen before the insert: the secret is sealed for its row.
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return nil, fmt.Errorf("webhooks: new id: %w", err)
	}
	sealed, err := s.mergeSecret(fe, id, "", in, "")
	if err != nil {
		return nil, err
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	if err := s.requireCoverage(ctx, AuditCreate, in.Name, in.Repositories); err != nil {
		return nil, err
	}
	enabled := in.Enabled == nil || *in.Enabled
	_, err = s.pool.Exec(ctx, `
		INSERT INTO webhooks (id, name, url, events, repositories, format, secret, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, in.Name, in.URL, in.Events, in.Repositories, in.Format, sealed, enabled, core.PrincipalFrom(ctx).Name)
	if err != nil {
		return nil, saveErr(in.Name, err)
	}
	s.record(ctx, core.AuditEvent{Action: AuditCreate, Target: target(in.Name), Details: details(in, enabled, nil)})
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	return &h.Webhook, nil
}

// Update replaces the settings of a webhook (system:webhooks:admin, and
// read access to the repositories it names). A URL that moves to another
// scheme, host or port needs the secret again (or clear_secret): a secret is
// only ever sent to the endpoint it was entered for. Enabling a webhook
// resets its failure count.
func (s *Service) Update(ctx context.Context, id string, in Input) (*Webhook, error) {
	in.normalize()
	if err := s.authorize(ctx, core.ActionAdmin, AuditUpdate, "webhook:"+id); err != nil {
		return nil, err
	}
	old, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	if in.URL == "" {
		in.URL = old.url
	}
	fe := FieldErrors{}
	s.check(ctx, fe, &in)
	sealed, err := s.mergeSecret(fe, id, old.secret, in, movedTo(old.url, in.URL))
	if err != nil {
		return nil, err
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	if err := s.requireCoverage(ctx, AuditUpdate, in.Name, in.Repositories); err != nil {
		return nil, err
	}
	enabled := old.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE webhooks SET name = $2, url = $3, events = $4, repositories = $5, format = $6, secret = $7, enabled = $8,
			consecutive_failures = CASE WHEN $8 AND NOT enabled THEN 0 ELSE consecutive_failures END,
			disabled_reason = CASE WHEN $8 THEN '' ELSE disabled_reason END,
			updated_at = now(), updated_by = $9
		WHERE id = $1`,
		id, in.Name, in.URL, in.Events, in.Repositories, in.Format, sealed, enabled, core.PrincipalFrom(ctx).Name)
	if err != nil {
		return nil, saveErr(in.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("webhook %q: %w", id, core.ErrNotFound)
	}
	s.record(ctx, core.AuditEvent{Action: AuditUpdate, Target: target(in.Name), Details: details(in, enabled, old)})
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	return &h.Webhook, nil
}

// Delete removes a webhook and its deliveries (system:webhooks:admin).
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.authorize(ctx, core.ActionAdmin, AuditDelete, "webhook:"+id); err != nil {
		return err
	}
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("webhook %q: %w", id, core.ErrNotFound)
	}
	var name string
	err := s.pool.QueryRow(ctx, `DELETE FROM webhooks WHERE id = $1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("webhook %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("webhooks: delete %s: %w", id, err)
	}
	s.record(ctx, core.AuditEvent{Action: AuditDelete, Target: target(name)})
	return nil
}

func saveErr(name string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("webhook %q: %w", name, core.ErrAlreadyExists)
		case "23514":
			return fmt.Errorf("webhook %q: %s: %w", name, pg.Message, core.ErrInvalid)
		}
	}
	return fmt.Errorf("webhooks: save %q: %w", name, err)
}

func (in *Input) normalize() {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.Format = strings.ToLower(strings.TrimSpace(in.Format))
	if in.Format == "" {
		in.Format = FormatGeneric
	}
	in.Events = cleanList(in.Events)
	in.Repositories = cleanList(in.Repositories)
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// check adds the problems of in to fe; the URL is checked against the
// outbound address policy.
func (s *Service) check(ctx context.Context, fe FieldErrors, in *Input) {
	if !nameRE.MatchString(in.Name) {
		fe.add("name", "use 1 to 64 letters, digits, spaces, '.', '_' or '-', starting with a letter or digit")
	}
	if !slices.Contains(Formats(), in.Format) {
		fe.add("format", "choose one of "+strings.Join(Formats(), ", "))
	}
	if len(in.Events) == 0 {
		fe.add("events", "choose at least one event")
	}
	for _, e := range in.Events {
		if !slices.Contains(Events(), e) {
			fe.add("events", fmt.Sprintf("%q is not an event; choose from %s", e, strings.Join(Events(), ", ")))
		}
	}
	if len(in.Repositories) > maxRepositories {
		fe.add("repositories", fmt.Sprintf("name at most %d repositories or patterns", maxRepositories))
	}
	for _, p := range in.Repositories {
		if _, err := path.Match(p, ""); err != nil || !repoPatternRE.MatchString(p) {
			fe.add("repositories", fmt.Sprintf("%q: enter a repository name or a glob such as npm-*", p))
		}
	}
	if msg := checkURL(in.URL); msg != "" {
		fe.add("url", msg)
		return
	}
	if in.URL == "" {
		return
	}
	if err := s.opts.Guard.ValidateRemote(ctx, in.URL, false); err != nil {
		host := ""
		if u, perr := url.Parse(in.URL); perr == nil {
			host = u.Hostname()
		}
		fe.add("url", "is refused by the outbound address policy: "+host+" is an internal or reserved address "+
			"(an operator can allow it with http.allow_private_networks)")
	}
}

// checkURL returns what is wrong with a webhook URL ("" = fine). Empty is
// fine here: Create requires it separately.
func checkURL(raw string) string {
	if raw == "" {
		return ""
	}
	if err := secrets.CheckInput("url", raw); err != nil {
		return "is a sealed value, which cannot be submitted"
	}
	if len(raw) > maxURLLength {
		return fmt.Sprintf("is longer than %d characters", maxURLLength)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return "enter an absolute http:// or https:// URL"
	case u.User != nil:
		return "must not contain user information; use the secret to authenticate deliveries"
	case u.Fragment != "":
		return "must not contain a fragment (#...)"
	case strings.Contains(raw, "/***"):
		return "is the redacted URL; leave it empty to keep the stored one"
	}
	return ""
}

// mergeSecret returns the sealed secret to store: the given one (sealed),
// none (cleared), or the stored one unless the URL moved (moved names the
// move, see movedTo), which adds a field error instead. Problems go to fe.
func (s *Service) mergeSecret(fe FieldErrors, id, stored string, in Input, moved string) (string, error) {
	if err := secrets.CheckInput("secret", in.Secret); err != nil {
		fe.add("secret", "is a sealed value ("+secrets.Prefix+"...), which cannot be submitted; enter the secret itself, "+
			"or leave it empty to keep the stored one")
		return "", nil
	}
	switch {
	case in.ClearSecret:
		return "", nil
	case in.Secret == "" && stored != "" && moved != "":
		fe.add("secret", moved+": the stored secret is only sent to the endpoint it was entered for; enter it again, or clear it")
		return "", nil
	case in.Secret != "":
		if s.opts.Keyring == nil {
			fe.add("secret", "storing secrets needs security.secret_key (HANGAR_SECRET_KEY) on the server")
			return "", nil
		}
		sealed, err := s.opts.Keyring.SealString(secrets.WebhookContext(id), in.Secret)
		if err != nil {
			return "", fmt.Errorf("webhooks: seal secret: %w", err)
		}
		return sealed, nil
	}
	return stored, nil
}

// movedTo describes how next points the stored secret at another endpoint
// than old: "" when both have the same scheme, host and port (default ports
// made explicit).
func movedTo(old, next string) string {
	eo, ok1 := endpoint(old)
	en, ok2 := endpoint(next)
	if ok1 && ok2 && eo == en {
		return ""
	}
	if !ok1 {
		eo = "(invalid URL)"
	}
	if !ok2 {
		en = "(invalid URL)"
	}
	return "url changed from " + eo + " to " + en
}

// endpoint is a URL's scheme, host and port, lower case, with the default
// port of http and https made explicit.
func endpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port), true
}

// details is the audit record of a webhook change, without the secret and
// with the URL redacted.
func details(in Input, enabled bool, old *stored) map[string]any {
	d := map[string]any{"url": RedactURL(in.URL), "events": in.Events, "repositories": in.Repositories,
		"format": in.Format, "enabled": enabled}
	switch {
	case in.ClearSecret:
		d["secret"] = "cleared"
	case in.Secret != "":
		d["secret"] = "set"
	}
	if old != nil {
		var changed []string
		if old.Name != in.Name {
			changed = append(changed, "name")
		}
		if old.url != in.URL {
			changed = append(changed, "url")
		}
		if !slices.Equal(old.Events, in.Events) {
			changed = append(changed, "events")
		}
		if !slices.Equal(old.Repositories, in.Repositories) {
			changed = append(changed, "repositories")
		}
		if old.Format != in.Format {
			changed = append(changed, "format")
		}
		if in.ClearSecret || in.Secret != "" {
			changed = append(changed, "secret")
		}
		if old.Enabled != enabled {
			changed = append(changed, "enabled")
		}
		d["changed"] = changed
	}
	return d
}
