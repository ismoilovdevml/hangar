package secrets

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// legacyRows writes plaintext secrets as rows from before migration 00050
// would have held them, bypassing the NOT VALID constraints the same way
// pre-existing rows do.
func legacyRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`ALTER TABLE repositories DROP CONSTRAINT repositories_secrets_sealed`)
	exec(`ALTER TABLE blob_stores DROP CONSTRAINT blob_stores_secrets_sealed`)
	storeID := pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	pgtest.AddBlobStore(t, pool, "s3-legacy", "s3", map[string]any{"bucket": "b", "access_key_id": "AK", "secret_access_key": "legacy-sk"})
	exec(`INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES
		('npm-proxy', 'npm', 'proxy', $1, '{"proxy":{"remote_url":"https://u:remote-pw@registry.example/","auth":{"kind":"bearer","token":"legacy-token"},"http":{"proxy":"http://p:proxy-pw@proxy.corp:3128"}}}'),
		('raw-hosted', 'raw', 'hosted', $1, '{"hosted":{"write_policy":"allow"}}')`, storeID)
	// Put the guards back as NOT VALID, exactly as the migration leaves them.
	exec(`ALTER TABLE repositories ADD CONSTRAINT repositories_secrets_sealed CHECK (
		hangar_secret_sealed(config #>> '{proxy,auth,password}') AND hangar_secret_sealed(config #>> '{proxy,auth,token}')
		AND hangar_url_secret_sealed(config #>> '{proxy,remote_url}') AND hangar_url_secret_sealed(config #>> '{proxy,http,proxy}')) NOT VALID`)
	exec(`ALTER TABLE blob_stores ADD CONSTRAINT blob_stores_secrets_sealed CHECK (
		coalesce(config ->> 'secret_access_key', '') = '' AND coalesce(config ->> 'session_token', '') = ''
		AND hangar_secret_sealed(config ->> 'secret_access_key_sealed') AND hangar_secret_sealed(config ->> 'session_token_sealed')) NOT VALID`)
}

func dump(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	err := pool.QueryRow(context.Background(), `
		SELECT (SELECT string_agg(config::text, ' ' ORDER BY name) FROM repositories) || ' ' ||
		       (SELECT string_agg(config::text, ' ' ORDER BY name) FROM blob_stores)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSweep(t *testing.T) {
	pool := pgtest.Pool(t)
	legacyRows(t, pool)
	ctx := context.Background()
	plain := []string{"remote-pw", "legacy-token", "proxy-pw", "legacy-sk"}

	// No key: report only.
	rep, err := Sweep(ctx, pool, nil, SweepOptions{})
	if err != nil || rep.Plaintext != 4 || rep.Rows != 0 {
		t.Fatalf("no key: %+v %v", rep, err)
	}
	oldKey := newKey(t)
	old := mustKeyring(t, oldKey)
	// Dry run writes nothing.
	if rep, err = Sweep(ctx, pool, old, SweepOptions{DryRun: true}); err != nil || rep.Plaintext != 4 || rep.Rows != 0 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if !strings.Contains(dump(t, pool), "legacy-token") {
		t.Fatal("dry run wrote")
	}
	// Seal: every plaintext value is sealed, the constraints now validate.
	if rep, err = Sweep(ctx, pool, old, SweepOptions{}); err != nil || rep.Plaintext != 4 || rep.Rows != 2 {
		t.Fatalf("seal: %+v %v", rep, err)
	}
	after := dump(t, pool)
	for _, p := range plain {
		if strings.Contains(after, p) {
			t.Errorf("%q still stored: %s", p, after)
		}
	}
	for _, c := range []string{"repositories", "blob_stores"} {
		if _, err := pool.Exec(ctx, `ALTER TABLE `+c+` VALIDATE CONSTRAINT `+c+`_secrets_sealed`); err != nil {
			t.Errorf("validate %s: %v", c, err)
		}
	}
	// Idempotent.
	if rep, err = Sweep(ctx, pool, old, SweepOptions{}); err != nil || rep.Plaintext != 0 || rep.Current != 4 || rep.Rows != 0 {
		t.Fatalf("second seal: %+v %v", rep, err)
	}

	// Rotate to a new key.
	rotated := mustKeyring(t, newKey(t), oldKey)
	if rep, err = Sweep(ctx, pool, rotated, SweepOptions{}); err != nil || rep.Previous != 4 || rep.Rows != 0 {
		t.Fatalf("without rewrap: %+v %v", rep, err)
	}
	if rep, err = Sweep(ctx, pool, rotated, SweepOptions{Rewrap: true}); err != nil || rep.Rewrapped != 4 || rep.Rows != 2 {
		t.Fatalf("rewrap: %+v %v", rep, err)
	}
	if strings.Contains(dump(t, pool), "."+old.PrimaryID()+".") {
		t.Error("values still sealed with the old key")
	}
	// A keyring that lacks the current key reports unreadable values.
	stranger := mustKeyring(t, newKey(t))
	rep, err = Sweep(ctx, pool, stranger, SweepOptions{Rewrap: true})
	if err != nil || len(rep.Unreadable) != 4 || rep.Err() == nil || rep.Rows != 0 {
		t.Fatalf("unknown key: %+v %v", rep, err)
	}
	if !strings.Contains(rep.Unreadable[0], "proxy.") && !strings.Contains(rep.Unreadable[0], "sealed") {
		t.Errorf("unreadable entry = %q", rep.Unreadable[0])
	}
}

func TestSweepTaskCredentials(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO tasks (name, type, config) VALUES
		('imp', 'repository_import', '{"repository":"r","source":{"url":"https://old.example.com","username":"u","password":"task-pw"}}'),
		('warm', 'proxy_prewarm', '{"repository":"p","inventory":{"source":{"url":"https://old.example.com","token":"task-tok"}}}')`); err != nil {
		t.Fatal(err)
	}
	k := mustKeyring(t, newKey(t))
	rep, err := Sweep(ctx, pool, k, SweepOptions{})
	if err != nil || rep.Plaintext != 2 || rep.Rows != 2 {
		t.Fatalf("sweep: %+v %v", rep, err)
	}
	var cfg string
	if err := pool.QueryRow(ctx, `SELECT string_agg(config::text, ' ') FROM tasks WHERE name IN ('imp', 'warm')`).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "task-pw") || strings.Contains(cfg, "task-tok") || strings.Count(cfg, Prefix) != 2 {
		t.Fatalf("not sealed: %s", cfg)
	}
}
