package secrets

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// legacyEverywhere stores a legacy (hgs1) value in every sealed column, as a
// v0.1.x database holds them, and returns the plaintexts by context.
func legacyEverywhere(t *testing.T, pool *pgxpool.Pool, k *Keyring) map[Context]string {
	t.Helper()
	ctx := context.Background()
	want := map[Context]string{}
	seal := func(plain string) string {
		t.Helper()
		v, err := k.sealLegacy([]byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	fsID := pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	s3ID := pgtest.AddBlobStore(t, pool, "s3", "s3", map[string]any{"bucket": "b", "access_key_id": "AK",
		"secret_access_key_sealed": seal("s3-secret")})
	want[BlobStoreContext(s3ID, "secret_access_key_sealed")] = "s3-secret"
	repoID := q(`INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('npm-proxy', 'npm', 'proxy', $1,
		jsonb_build_object('proxy', jsonb_build_object('remote_url', 'https://u:' || $2 || '@registry.example/',
			'auth', jsonb_build_object('kind', 'bearer', 'token', $3::text)))) RETURNING id::text`,
		fsID, seal("url-pw"), seal("repo-token"))
	want[RepositoryContext(repoID, "proxy.remote_url")] = "url-pw"
	want[RepositoryContext(repoID, "proxy.auth.token")] = "repo-token"
	q(`INSERT INTO settings (key, value) VALUES ('auth.oidc', jsonb_build_object('client_secret', $1::text)) RETURNING key`, seal("oidc-secret"))
	want[SettingContext("auth.oidc", "client_secret")] = "oidc-secret"
	q(`INSERT INTO outbound_proxies (name, scheme, host, port, password) VALUES ('corp', 'http', 'proxy.corp', 3128, $1) RETURNING name`,
		seal("proxy-pw"))
	want[OutboundProxyContext("corp")] = "proxy-pw"
	q(`INSERT INTO signing_keys (name, algorithm, fingerprint, public_key, private_key) VALUES ('shared', 'ed25519', 'F', 'P', $1)
		RETURNING name`, seal("private-key"))
	want[SigningKeyContext("shared")] = "private-key"
	taskID := q(`INSERT INTO tasks (name, type, config) VALUES ('warm', 'proxy_prewarm', jsonb_build_object(
		'source', jsonb_build_object('url', 'https://a.example', 'password', $1::text),
		'inventory', jsonb_build_object('source', jsonb_build_object('url', 'https://b.example', 'token', $2::text))))
		RETURNING id::text`, seal("task-pw"), seal("inv-token"))
	want[TaskContext(taskID, "source.password")] = "task-pw"
	want[TaskContext(taskID, "inventory.source.token")] = "inv-token"
	return want
}

// stored reads the sealed value at c from the database.
func stored(t *testing.T, pool *pgxpool.Pool, c Context) string {
	t.Helper()
	queries := map[string]string{
		"repositories":     `SELECT config #>> $2 FROM repositories WHERE id::text = $1`,
		"blob_stores":      `SELECT config #>> $2 FROM blob_stores WHERE id::text = $1`,
		"settings":         `SELECT value #>> $2 FROM settings WHERE key = $1`,
		"outbound_proxies": `SELECT password FROM outbound_proxies WHERE name = $1 AND $2::text[] IS NOT NULL`,
		"signing_keys":     `SELECT private_key FROM signing_keys WHERE name = $1 AND $2::text[] IS NOT NULL`,
		"tasks":            `SELECT config #>> $2 FROM tasks WHERE id::text = $1`,
	}
	var v string
	if err := pool.QueryRow(context.Background(), queries[c.Table], c.Row, strings.Split(c.Field, ".")).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", c, err)
	}
	if u, err := url.Parse(v); err == nil && u.User != nil {
		pw, _ := u.User.Password()
		return pw
	}
	return v
}

// TestSweepUpgradesLegacy: the first start of v0.2.0 turns every stored hgs1
// value into an hgs2 value bound to its row and field, once; a downgrade
// turns them back, and the next start upgrades them again.
func TestSweepUpgradesLegacy(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	k := mustKeyring(t, newKey(t))
	want := legacyEverywhere(t, pool, k)

	if rep, err := Sweep(ctx, pool, k, SweepOptions{DryRun: true}); err != nil || rep.Upgraded != len(want) || rep.Rows != 0 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	rep, err := Sweep(ctx, pool, k, SweepOptions{})
	if err != nil || rep.Upgraded != len(want) || rep.Rows != 6 || len(rep.Unreadable) != 0 {
		t.Fatalf("upgrade: %+v %v", rep, err)
	}
	for c, plain := range want {
		v := stored(t, pool, c)
		if !strings.HasPrefix(v, Prefix) {
			t.Errorf("%s: %q is not hgs2", c, v)
			continue
		}
		if got, err := k.OpenString(c, v); err != nil || got != plain {
			t.Errorf("%s: open = %q %v", c, got, err)
		}
	}
	if rep, err := Sweep(ctx, pool, k, SweepOptions{}); err != nil || rep.Current != len(want) || rep.Upgraded != 0 || rep.Rows != 0 {
		t.Fatalf("second sweep: %+v %v", rep, err)
	}

	rep, err = Sweep(ctx, pool, k, SweepOptions{Downgrade: true, Rewrap: true})
	if err != nil || rep.Downgraded != len(want) || rep.Rows != 6 {
		t.Fatalf("downgrade: %+v %v", rep, err)
	}
	for c, plain := range want {
		v := stored(t, pool, c)
		if got, err := k.OpenString(TaskContext("anywhere", "x"), v); !IsLegacy(v) || err != nil || got != plain {
			t.Errorf("%s: downgraded %q opens as %q %v", c, v, got, err)
		}
	}
	if rep, err := Sweep(ctx, pool, k, SweepOptions{Downgrade: true}); err != nil || rep.Current != len(want) || rep.Rows != 0 {
		t.Fatalf("second downgrade: %+v %v", rep, err)
	}
	if rep, err := Sweep(ctx, pool, k, SweepOptions{}); err != nil || rep.Upgraded != len(want) {
		t.Fatalf("upgrade after downgrade: %+v %v", rep, err)
	}
}

// TestSweepFindsCopiedSecret: an hgs2 value copied in the database into
// another field of its row, or into another table, does not open there; the
// sweep reports it and leaves it alone.
func TestSweepFindsCopiedSecret(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	k := mustKeyring(t, newKey(t))
	want := legacyEverywhere(t, pool, k)
	if _, err := Sweep(ctx, pool, k, SweepOptions{}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	for c := range want {
		if c.Table == "tasks" {
			taskID = c.Row
		}
	}
	token := stored(t, pool, TaskContext(taskID, "inventory.source.token"))
	if _, err := pool.Exec(ctx, `UPDATE tasks SET config = jsonb_set(config, '{source,password}', to_jsonb($1::text)) WHERE id::text = $2`,
		token, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbound_proxies SET password = $1 WHERE name = 'corp'`, token); err != nil {
		t.Fatal(err)
	}
	rep, err := Sweep(ctx, pool, k, SweepOptions{Rewrap: true})
	if err != nil || len(rep.Unreadable) != 2 || rep.Err() == nil || rep.Rows != 0 {
		t.Fatalf("copied values: %+v %v", rep, err)
	}
	for _, u := range rep.Unreadable {
		if !strings.Contains(u, "another row or field") {
			t.Errorf("unreadable entry = %q", u)
		}
	}
	if v := stored(t, pool, OutboundProxyContext("corp")); v != token {
		t.Error("the sweep changed an unreadable value")
	}
}

// TestSealedGuardsAcceptHGS2: the database guards (migrations 00050, 00090,
// 00285, extended by 00330) take both formats and still refuse plaintext.
func TestSealedGuardsAcceptHGS2(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	k := mustKeyring(t, newKey(t))
	cur, _ := k.SealString(OutboundProxyContext("p2"), "pw")
	legacy, _ := k.sealLegacy([]byte("pw"))
	storeID := pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	tests := []struct {
		name, sql string
		args      []any
		ok        bool
	}{
		{"proxy hgs2", `INSERT INTO outbound_proxies (name, scheme, host, port, password) VALUES ('p2', 'http', 'h', 1, $1)`, []any{cur}, true},
		{"proxy hgs1", `INSERT INTO outbound_proxies (name, scheme, host, port, password) VALUES ('p1', 'http', 'h', 1, $1)`, []any{legacy}, true},
		{"proxy plaintext", `INSERT INTO outbound_proxies (name, scheme, host, port, password) VALUES ('p3', 'http', 'h', 1, 'plain')`, nil, false},
		{"repo hgs2 url", `INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('r2', 'npm', 'proxy', $1,
			jsonb_build_object('proxy', jsonb_build_object('remote_url', 'https://u:' || $2 || '@h.example/')))`, []any{storeID, cur}, true},
		{"repo plaintext url", `INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('r3', 'npm', 'proxy', $1,
			'{"proxy":{"remote_url":"https://u:plain@h.example/"}}')`, []any{storeID}, false},
		{"repo hgs2 token", `INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('r4', 'npm', 'proxy', $1,
			jsonb_build_object('proxy', jsonb_build_object('auth', jsonb_build_object('token', $2::text))))`, []any{storeID, cur}, true},
		{"task plaintext", `INSERT INTO tasks (name, type, config) VALUES ('t1', 'repository_import',
			'{"source":{"url":"https://x.example","token":"plain"}}')`, nil, false},
		{"task hgs2", `INSERT INTO tasks (name, type, config) VALUES ('t2', 'repository_import',
			jsonb_build_object('source', jsonb_build_object('url', 'https://x.example', 'token', $1::text)))`, []any{cur}, true},
		{"store plaintext", `INSERT INTO blob_stores (name, kind, config) VALUES ('s1', 's3', '{"secret_access_key_sealed":"plain"}')`, nil, false},
		{"store hgs2", `INSERT INTO blob_stores (name, kind, config) VALUES ('s2', 's3', jsonb_build_object('secret_access_key_sealed', $1::text))`,
			[]any{cur}, true},
	}
	for _, tc := range tests {
		_, err := pool.Exec(ctx, tc.sql, tc.args...)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want accepted %v", tc.name, err, tc.ok)
		}
	}
}
