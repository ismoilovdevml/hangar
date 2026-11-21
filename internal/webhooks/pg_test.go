package webhooks_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/webhooks"
)

const envDatabaseURL = "HANGAR_TEST_DATABASE_URL"

// testPool connects to a fresh schema with the base and webhook migrations
// (00001-00006, the settings tables 00090-00099 and the sealed-secret
// guards 00330-00339 that 00340 uses, 00340-00349), dropped at cleanup.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(envDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", envDatabaseURL)
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, base, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "hangar_webhooks_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String(), db.PoolOptions{MaxConns: 8})
	if err != nil {
		t.Fatalf("connect with schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})
	src := db.Migrations()
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		t.Fatal(err)
	}
	mfs := fstest.MapFS{}
	for _, e := range entries {
		n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || !(n <= 6 || (n >= 90 && n <= 99) || (n >= 330 && n <= 349)) {
			continue
		}
		data, err := fs.ReadFile(src, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		mfs[e.Name()] = &fstest.MapFile{Data: data}
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, mfs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// coverage is a Coverage that knows, per principal, the repository patterns
// it may read.
type coverage struct {
	mu   sync.Mutex
	read map[string][]string
}

func (c *coverage) grant(name string, patterns ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read[name] = append(c.read[name], patterns...)
}

func (c *coverage) RequireCovers(ctx context.Context, _, _, what string, want []core.Privilege) error {
	c.mu.Lock()
	have := c.read[core.PrincipalFrom(ctx).Name]
	c.mu.Unlock()
	for _, w := range want {
		ok := false
		for _, h := range have {
			// The rule of auth's covers: a glob covers names it matches
			// and narrower globs that share its literal prefix.
			m, _ := path.Match(h, w.Pattern)
			prefix, glob := strings.CutSuffix(h, "*")
			if h == "*" || h == w.Pattern || (m && !strings.ContainsAny(w.Pattern, "*?[")) ||
				(glob && strings.HasPrefix(w.Pattern, prefix)) {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("cannot manage %s: %w", what, core.ErrForbidden)
		}
	}
	return nil
}

// receiver is an httptest webhook endpoint answering with a scripted list
// of status codes (the last one repeats).
type receiver struct {
	srv *httptest.Server
	url string // through "localhost", the name the test guard allows

	mu       sync.Mutex
	statuses []int
	got      []received
	block    bool // hold every request until the client gives up
}

type received struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{header: req.Header.Clone(), body: body})
		status := http.StatusOK
		if len(r.statuses) > 0 {
			status = r.statuses[0]
			if len(r.statuses) > 1 {
				r.statuses = r.statuses[1:]
			}
		}
		block := r.block
		r.mu.Unlock()
		if block {
			<-req.Context().Done()
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	r.url = strings.Replace(r.srv.URL, "127.0.0.1", "localhost", 1) + "/hook"
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

// env is a webhook service over a fresh schema, with alice as the
// administrator of webhooks who may read every repository.
type env struct {
	pool   *pgxpool.Pool
	svc    *webhooks.Service
	outbox *webhooks.Outbox
	audit  *coretest.AuditLog
	authz  *coretest.AuthZ
	cov    *coverage
}

func fastPolicy() webhooks.Policy {
	return webhooks.Policy{MaxAttempts: 3, BackoffBase: 20 * time.Millisecond, AttemptTimeout: 2 * time.Second,
		DisableAfter: 20, PollInterval: 20 * time.Millisecond, Concurrency: 2, EnqueueTimeout: 5 * time.Second}
}

func newEnv(t *testing.T, policy webhooks.Policy) *env {
	t.Helper()
	pool := testPool(t)
	guard, err := proxy.NewGuard([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	clients, err := httpclient.New(httpclient.Options{DialGuard: guard.CheckAddr, DialAllowHost: guard.AllowsHost})
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	keyring, err := secrets.NewKeyring(hex.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: pool, audit: coretest.NewAuditLog(), authz: coretest.NewAuthZ(), cov: &coverage{read: map[string][]string{}}}
	e.outbox = webhooks.NewOutbox(pool, webhooks.OutboxOptions{Instance: "https://hangar.test/", Policy: policy})
	e.svc, err = webhooks.New(pool, webhooks.Options{Outbox: e.outbox, Keyring: keyring, AuthZ: e.authz, Audit: e.audit,
		Coverage: e.cov, Guard: guard, Clients: clients, UserAgent: "Hangar-Test"})
	if err != nil {
		t.Fatal(err)
	}
	e.authz.Grant("alice", "system:webhooks:admin")
	e.cov.grant("alice", "*")
	e.svc.Start(context.Background())
	t.Cleanup(e.svc.Close)
	return e
}

func as(name string) context.Context {
	return core.WithPrincipal(context.Background(), &core.Principal{Name: name, Kind: core.PrincipalUser})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) create(t *testing.T, in webhooks.Input) *webhooks.Webhook {
	t.Helper()
	h, err := e.svc.Create(as("alice"), in)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return h
}

func (e *env) deliveries(t *testing.T, id string) []webhooks.Delivery {
	t.Helper()
	l, err := e.svc.Deliveries(as("alice"), id, "", "", 100)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	return l.Items
}

func published(repo string) core.Event {
	data, _ := json.Marshal(core.Component{ID: "c1", Repository: repo, Name: "lib", Version: "1.0.0",
		Attributes: map[string]any{"upstream_token": "s3cr3t"}})
	return core.Event{Type: core.EventComponentPublished, At: time.Now().UTC(), Actor: "bob", Repository: repo, Subject: "lib@1.0.0", Data: data}
}

func TestDeliverySignedFilteredAndRecorded(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusNoContent)
	h := e.create(t, webhooks.Input{Name: "ci", URL: rcv.url, Events: []string{core.EventComponentPublished},
		Repositories: []string{"npm-*"}, Secret: "topsecret"})
	if h.URL != strings.TrimSuffix(rcv.url, "/hook")+"/***" || !h.SecretConfigured || !h.Enabled {
		t.Fatalf("created webhook = %+v", h)
	}

	e.outbox.Notify(as("bob"), published("maven-hosted"))                                              // repository filtered out
	e.outbox.Notify(as("bob"), core.Event{Type: core.EventComponentDeleted, Repository: "npm-hosted"}) // not subscribed
	e.outbox.Notify(as("bob"), published("npm-hosted"))
	eventually(t, "the delivery", func() bool {
		d := e.deliveries(t, h.ID)
		return len(d) == 1 && d[0].State == webhooks.StateSucceeded
	})

	reqs := rcv.requests()
	if len(reqs) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if !webhooks.Verify("topsecret", r.header.Get(webhooks.HeaderTimestamp), r.header.Get(webhooks.HeaderSignature), r.body) {
		t.Fatalf("signature does not verify: %v", r.header)
	}
	d := e.deliveries(t, h.ID)[0]
	if r.header.Get(webhooks.HeaderEvent) != core.EventComponentPublished || r.header.Get(webhooks.HeaderDelivery) != d.ID ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("User-Agent") != "Hangar-Test" {
		t.Fatalf("headers = %v", r.header)
	}
	var env webhooks.Envelope
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Repository != "npm-hosted" || env.Actor != "bob" || env.Instance != "https://hangar.test" || env.ID != d.EventID {
		t.Fatalf("envelope = %+v", env)
	}
	if strings.Contains(string(r.body), "s3cr3t") {
		t.Fatalf("credential sent: %s", r.body)
	}
	if d.Attempts != 1 || d.LastStatusCode == nil || *d.LastStatusCode != http.StatusNoContent || d.LastLatencyMS == nil ||
		d.CompletedAt == nil || d.LastError != "" {
		t.Fatalf("delivery = %+v", d)
	}
	got, err := e.svc.Get(as("alice"), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatusCode == nil || *got.LastStatusCode != http.StatusNoContent || got.LastDeliveryAt == nil {
		t.Fatalf("webhook after delivery = %+v", got)
	}
}

func TestRetryOn500(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusInternalServerError, http.StatusOK)
	h := e.create(t, webhooks.Input{Name: "flaky", URL: rcv.url, Events: []string{core.EventComponentPublished}})
	e.outbox.Notify(as("bob"), published("npm-hosted"))
	eventually(t, "the retried delivery", func() bool {
		d := e.deliveries(t, h.ID)
		return len(d) == 1 && d[0].State == webhooks.StateSucceeded
	})
	d := e.deliveries(t, h.ID)[0]
	reqs := rcv.requests()
	if d.Attempts != 2 || len(reqs) != 2 {
		t.Fatalf("attempts %d, requests %d; want 2 and 2", d.Attempts, len(reqs))
	}
	if reqs[0].header.Get(webhooks.HeaderDelivery) != reqs[1].header.Get(webhooks.HeaderDelivery) {
		t.Fatal("a retry must repeat the delivery id")
	}
	if got, _ := e.svc.Get(as("alice"), h.ID); got.ConsecutiveFailures != 0 {
		t.Fatalf("a success must reset the failure count: %d", got.ConsecutiveFailures)
	}
}

func TestFailsAfterMaxAttemptsAndAutoDisables(t *testing.T) {
	p := fastPolicy()
	p.MaxAttempts, p.DisableAfter = 2, 2
	e := newEnv(t, p)
	rcv := newReceiver(t, http.StatusServiceUnavailable)
	h := e.create(t, webhooks.Input{Name: "dead", URL: rcv.url, Events: []string{core.EventComponentPublished}})
	e.outbox.Notify(as("bob"), published("npm-hosted"))
	eventually(t, "the failed delivery", func() bool {
		d := e.deliveries(t, h.ID)
		return len(d) == 1 && d[0].State == webhooks.StateFailed
	})
	d := e.deliveries(t, h.ID)[0]
	if d.Attempts != 2 || d.LastStatusCode == nil || *d.LastStatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(d.LastError, "503") || d.NextAttemptAt != nil {
		t.Fatalf("delivery = %+v", d)
	}
	got, _ := e.svc.Get(as("alice"), h.ID)
	if got.Enabled || got.ConsecutiveFailures != 2 || !strings.Contains(got.DisabledReason, "2 failed attempts") {
		t.Fatalf("webhook = %+v, want disabled", got)
	}
	ev := e.audit.Events(webhooks.AuditDisable)
	if len(ev) != 1 || ev[0].Actor != "system" || ev[0].Target != "webhook:dead" {
		t.Fatalf("audit = %+v", ev)
	}

	// Disabled: new events are not queued.
	e.outbox.Notify(as("bob"), published("npm-hosted"))
	if n := len(e.deliveries(t, h.ID)); n != 1 {
		t.Fatalf("%d deliveries for a disabled webhook", n)
	}
	// Enabling again resets the failure count.
	on := true
	got, err := e.svc.Update(as("alice"), h.ID, webhooks.Input{Name: "dead", Events: []string{core.EventComponentPublished}, Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.ConsecutiveFailures != 0 || got.DisabledReason != "" {
		t.Fatalf("re-enabled webhook = %+v", got)
	}
}

func fieldErrors(t *testing.T, err error) webhooks.FieldErrors {
	t.Helper()
	var fe webhooks.FieldErrors
	if !errors.As(err, &fe) || !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("err = %v, want field errors", err)
	}
	return fe
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, fastPolicy())
	ok := webhooks.Input{Name: "x", URL: "https://hooks.example.com/x", Events: []string{core.EventComponentPublished}}
	tests := []struct {
		name  string
		edit  func(in *webhooks.Input)
		field string
	}{
		{"loopback literal is an SSRF target", func(in *webhooks.Input) { in.URL = "http://127.0.0.1:8080/hook" }, "url"},
		{"cloud metadata endpoint", func(in *webhooks.Input) { in.URL = "http://169.254.169.254/latest/meta-data" }, "url"},
		{"private network", func(in *webhooks.Input) { in.URL = "http://10.1.2.3/hook" }, "url"},
		{"IPv6 loopback", func(in *webhooks.Input) { in.URL = "http://[::1]/hook" }, "url"},
		{"scheme", func(in *webhooks.Input) { in.URL = "ftp://hooks.example.com/x" }, "url"},
		{"user information", func(in *webhooks.Input) { in.URL = "https://u:p@hooks.example.com/x" }, "url"},
		{"missing url", func(in *webhooks.Input) { in.URL = "" }, "url"},
		{"redacted url sent back", func(in *webhooks.Input) { in.URL = "https://hooks.example.com/***" }, "url"},
		{"sealed secret", func(in *webhooks.Input) { in.Secret = secrets.Prefix + "abc.def" }, "secret"},
		{"no events", func(in *webhooks.Input) { in.Events = nil }, "events"},
		{"unknown event", func(in *webhooks.Input) { in.Events = []string{"asset.downloaded"} }, "events"},
		{"bad repository pattern", func(in *webhooks.Input) { in.Repositories = []string{"NPM Hosted"} }, "repositories"},
		{"format", func(in *webhooks.Input) { in.Format = "discord" }, "format"},
		{"name", func(in *webhooks.Input) { in.Name = "-x" }, "name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := ok
			tt.edit(&in)
			_, err := e.svc.Create(as("alice"), in)
			if fe := fieldErrors(t, err); fe[tt.field] == "" {
				t.Fatalf("field errors %v, want one for %s", fe, tt.field)
			}
		})
	}
	if _, err := e.svc.Create(as("alice"), ok); err != nil {
		t.Fatalf("valid input refused: %v", err)
	}
	if _, err := e.svc.Create(as("alice"), ok); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestSecretBoundToEndpoint(t *testing.T) {
	e := newEnv(t, fastPolicy())
	in := webhooks.Input{Name: "bound", URL: "https://hooks.example.com/a", Events: []string{core.EventRepositoryCreated}, Secret: "s1"}
	h := e.create(t, in)
	tests := []struct {
		name    string
		url     string
		secret  string
		clear   bool
		wantErr bool
		wantSec bool
	}{
		{"same endpoint, other path keeps the secret", "https://hooks.example.com/b", "", false, false, true},
		{"empty url keeps the stored url and secret", "", "", false, false, true},
		{"another host needs the secret again", "https://evil.example.net/a", "", false, true, true},
		{"another port needs the secret again", "https://hooks.example.com:8443/a", "", false, true, true},
		{"another host with the secret", "https://other.example.net/a", "s2", false, false, true},
		{"moving and clearing", "https://hooks.example.com/a", "", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.svc.Update(as("alice"), h.ID, webhooks.Input{Name: "bound", URL: tt.url, Events: in.Events,
				Secret: tt.secret, ClearSecret: tt.clear})
			if tt.wantErr {
				if fe := fieldErrors(t, err); fe["secret"] == "" {
					t.Fatalf("field errors %v, want secret", fe)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.SecretConfigured != tt.wantSec {
				t.Fatalf("secret_configured = %v, want %v", got.SecretConfigured, tt.wantSec)
			}
		})
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT url FROM webhooks WHERE id = $1`, h.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "https://hooks.example.com/a" {
		t.Fatalf("stored url %q", stored)
	}
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t, fastPolicy())
	e.authz.Grant("viewer", "system:webhooks:read")
	e.cov.grant("viewer", "*")
	e.authz.Grant("team-admin", "system:webhooks:admin")
	e.cov.grant("team-admin", "team-*")
	e.authz.Grant("nobody", "repo:*:read")
	e.cov.grant("nobody", "*")
	all := e.create(t, webhooks.Input{Name: "all", URL: "https://hooks.example.com/a", Events: []string{core.EventRepositoryCreated}})

	tests := []struct {
		name string
		who  string
		call func(ctx context.Context) error
		want error
	}{
		{"nobody lists", "nobody", func(ctx context.Context) error { _, err := e.svc.List(ctx); return err }, core.ErrForbidden},
		{"nobody creates", "nobody", func(ctx context.Context) error {
			_, err := e.svc.Create(ctx, webhooks.Input{Name: "n", URL: "https://hooks.example.com/n", Events: []string{core.EventRepositoryCreated}})
			return err
		}, core.ErrForbidden},
		{"anonymous lists", core.AnonymousName, func(context.Context) error {
			_, err := e.svc.List(context.Background())
			return err
		}, core.ErrUnauthenticated},
		{"viewer lists", "viewer", func(ctx context.Context) error { _, err := e.svc.List(ctx); return err }, nil},
		{"viewer reads deliveries", "viewer", func(ctx context.Context) error {
			_, err := e.svc.Deliveries(ctx, all.ID, "", "", 10)
			return err
		}, nil},
		{"viewer deletes", "viewer", func(ctx context.Context) error { return e.svc.Delete(ctx, all.ID) }, core.ErrForbidden},
		{"viewer tests", "viewer", func(ctx context.Context) error { _, err := e.svc.Test(ctx, all.ID); return err }, core.ErrForbidden},
		{"team admin creates for its repositories", "team-admin", func(ctx context.Context) error {
			_, err := e.svc.Create(ctx, webhooks.Input{Name: "team", URL: "https://hooks.example.com/t", Events: []string{core.EventRepositoryCreated},
				Repositories: []string{"team-a", "team-b*"}})
			return err
		}, nil},
		{"team admin cannot send every repository's events", "team-admin", func(ctx context.Context) error {
			_, err := e.svc.Create(ctx, webhooks.Input{Name: "wide", URL: "https://hooks.example.com/w", Events: []string{core.EventRepositoryCreated}})
			return err
		}, core.ErrForbidden},
		{"team admin cannot repoint a wider webhook", "team-admin", func(ctx context.Context) error {
			_, err := e.svc.Update(ctx, all.ID, webhooks.Input{Name: "all", URL: "https://hooks.example.com/mine", Events: []string{core.EventRepositoryCreated}})
			return err
		}, core.ErrForbidden},
		{"team admin cannot read a wider webhook's deliveries", "team-admin", func(ctx context.Context) error {
			_, err := e.svc.Deliveries(ctx, all.ID, "", "", 10)
			return err
		}, core.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(as(tt.who))
			if tt.want == nil && err != nil {
				t.Fatalf("err = %v, want success", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	denied := 0
	for _, ev := range e.audit.Events() {
		if ev.Outcome == core.OutcomeDenied {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("denials are not audited")
	}
}

func TestPingAndRedeliver(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusInternalServerError, http.StatusOK)
	h := e.create(t, webhooks.Input{Name: "ping", URL: rcv.url, Events: []string{core.EventRepositoryCreated}, Format: webhooks.FormatSlack})

	d, err := e.svc.Test(as("alice"), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != webhooks.StateFailed || d.Event != webhooks.EventPing || d.LastStatusCode == nil || *d.LastStatusCode != 500 {
		t.Fatalf("failed ping = %+v", d)
	}
	if got, _ := e.svc.Get(as("alice"), h.ID); got.ConsecutiveFailures != 0 {
		t.Fatal("a test must not count towards disabling")
	}
	d, err = e.svc.Test(as("alice"), h.ID)
	if err != nil || d.State != webhooks.StateSucceeded {
		t.Fatalf("ping = %+v, %v", d, err)
	}
	var slack struct {
		Text string `json:"text"`
	}
	reqs := rcv.requests()
	if err := json.Unmarshal(reqs[1].body, &slack); err != nil || slack.Text == "" || reqs[1].header.Get(webhooks.HeaderEvent) != "ping" {
		t.Fatalf("slack ping body %s (%v)", reqs[1].body, err)
	}

	again, err := e.svc.Redeliver(as("alice"), h.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RedeliveryOf == nil || *again.RedeliveryOf != d.ID || again.EventID != d.EventID || again.ID == d.ID {
		t.Fatalf("redelivery = %+v", again)
	}
	eventually(t, "the redelivery", func() bool { return len(rcv.requests()) == 3 })
	if _, err := e.svc.Redeliver(as("alice"), h.ID, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("redeliver unknown: %v", err)
	}
	full, err := e.svc.Delivery(as("alice"), h.ID, d.ID)
	if err != nil || full.Payload == nil {
		t.Fatalf("delivery with payload = %+v, %v", full, err)
	}
}

func TestDeliveriesPagination(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusOK)
	h := e.create(t, webhooks.Input{Name: "pages", URL: rcv.url, Events: []string{core.EventComponentPublished}})
	for range 5 {
		e.outbox.Notify(as("bob"), published("npm-hosted"))
	}
	seen := map[string]bool{}
	cursor := ""
	for range 10 {
		page, err := e.svc.Deliveries(as("alice"), h.ID, "", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Items {
			if seen[d.ID] {
				t.Fatalf("delivery %s twice", d.ID)
			}
			seen[d.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d deliveries, want 5", len(seen))
	}
	if _, err := e.svc.Deliveries(as("alice"), h.ID, "", "garbage", 2); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
}

// TestCloseHandsBackAttemptInFlight: stopping the worker mid-attempt
// returns the delivery to pending without counting the attempt.
func TestCloseHandsBackAttemptInFlight(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusOK)
	rcv.mu.Lock()
	rcv.block = true
	rcv.mu.Unlock()
	h := e.create(t, webhooks.Input{Name: "slow", URL: rcv.url, Events: []string{core.EventComponentPublished}})
	e.outbox.Notify(as("bob"), published("npm-hosted"))
	eventually(t, "the attempt", func() bool { return len(rcv.requests()) == 1 })
	e.svc.Close()
	d := e.deliveries(t, h.ID)
	if len(d) != 1 || d[0].State != webhooks.StatePending || d[0].Attempts != 0 {
		t.Fatalf("after close: %+v", d)
	}
}

// TestHTTPRoutes drives the REST endpoints as alice, including the :test
// and :redeliver actions.
func TestHTTPRoutes(t *testing.T) {
	e := newEnv(t, fastPolicy())
	rcv := newReceiver(t, http.StatusOK)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(core.WithPrincipal(req.Context(), &core.Principal{Name: "alice", Kind: core.PrincipalUser})))
		})
	})
	e.svc.RegisterRoutes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	call := func(method, p string, body any, want int) map[string]any {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+p, rd)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d %s, want %d", method, p, resp.StatusCode, b, want)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	h := call(http.MethodPost, "/webhooks", map[string]any{"name": "api", "url": rcv.url,
		"events": []string{"repository.created"}, "secret": "k"}, http.StatusCreated)
	id := h["id"].(string)
	if h["secret_configured"] != true || strings.Contains(fmt.Sprint(h), "/hook") {
		t.Fatalf("created = %v", h)
	}
	bad := call(http.MethodPut, "/webhooks/"+id, map[string]any{"name": "api", "events": []string{}}, http.StatusBadRequest)
	if bad["code"] != "invalid_argument" || bad["details"].(map[string]any)["fields"].(map[string]any)["events"] == nil {
		t.Fatalf("400 body = %v", bad)
	}
	call(http.MethodGet, "/webhooks/"+id, nil, http.StatusOK)
	ping := call(http.MethodPost, "/webhooks/"+id+":test", nil, http.StatusOK)
	if ping["state"] != webhooks.StateSucceeded {
		t.Fatalf("ping = %v", ping)
	}
	did := ping["id"].(string)
	list := call(http.MethodGet, "/webhooks/"+id+"/deliveries?limit=10", nil, http.StatusOK)
	if items := list["items"].([]any); len(items) != 1 || list["next_cursor"] != nil {
		t.Fatalf("deliveries = %v", list)
	}
	one := call(http.MethodGet, "/webhooks/"+id+"/deliveries/"+did, nil, http.StatusOK)
	if one["payload"].(map[string]any)["event"] != "ping" {
		t.Fatalf("delivery = %v", one)
	}
	re := call(http.MethodPost, "/webhooks/"+id+"/deliveries/"+did+":redeliver", nil, http.StatusAccepted)
	if re["redelivery_of"] != did {
		t.Fatalf("redelivery = %v", re)
	}
	call(http.MethodGet, "/webhooks/"+id+"/deliveries?limit=0", nil, http.StatusBadRequest)
	call(http.MethodDelete, "/webhooks/"+id, nil, http.StatusNoContent)
	call(http.MethodGet, "/webhooks/"+id, nil, http.StatusNotFound)
	call(http.MethodGet, "/webhooks/not-a-uuid", nil, http.StatusNotFound)
}
