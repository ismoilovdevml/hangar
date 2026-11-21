package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepOptions select what Sweep changes.
type SweepOptions struct {
	// Rewrap re-encrypts values sealed with a previous key under the primary
	// key (key rotation). Without it plaintext values are sealed and legacy
	// values upgraded, and hgs2 values under a previous key are only counted.
	Rewrap bool
	// Downgrade writes every value in the legacy hgs1 format under the
	// primary key, which Hangar before v0.2.0 reads, giving up the binding of
	// each value to its row and field. For downgrading a database only: a
	// v0.2.0 server upgrades the values again on its next start.
	Downgrade bool
	// DryRun counts without writing anything.
	DryRun bool
}

// Report is what Sweep found, counted per secret value. Counts of changes
// are what was (or, on a dry run, would be) changed.
type Report struct {
	// Plaintext values found (and sealed, unless DryRun or no keyring).
	Plaintext int `json:"plaintext"`
	// Upgraded legacy hgs1 values sealed again as hgs2 for their row and
	// field under the primary key.
	Upgraded int `json:"upgraded"`
	// Rewrapped hgs2 values moved from a previous key to the primary key.
	Rewrapped int `json:"rewrapped"`
	// Downgraded values written in the legacy hgs1 format (Downgrade).
	Downgraded int `json:"downgraded,omitempty"`
	// Current values already in the target form: hgs2 under the primary key
	// (hgs1 under the primary key with Downgrade).
	Current int `json:"current"`
	// Previous hgs2 values still sealed with a previous key (Rewrap not
	// requested).
	Previous int `json:"previous"`
	// Unreadable values that are sealed but cannot be opened where they are
	// stored, as "repositories/<name> proxy.auth.token: <reason>": sealed
	// with a key that is not configured, altered, or copied from another
	// row or field. They are left as is.
	Unreadable []string `json:"unreadable,omitempty"`
	// Rows written (0 on a dry run).
	Rows int `json:"rows"`
}

// Sweep walks every stored secret (repositories.config, blob_stores.config,
// the source credentials of import tasks, the sign-in settings, the outbound
// proxy passwords, the repository signing keys and the webhook signing
// secrets) in one transaction,
// sealing plaintext values with the primary key, upgrading legacy hgs1 values
// to hgs2 bound to their row and field, and, with Rewrap, re-wrapping values
// sealed with previous keys. It is idempotent and safe to run on several
// nodes at once (rows are locked). Updated rows keep their updated_at: the
// secrets themselves do not change.
//
// With a nil keyring nothing is written; the report shows how many plaintext
// values are waiting to be sealed and how many sealed values cannot be read.
func Sweep(ctx context.Context, pool *pgxpool.Pool, k *Keyring, opts SweepOptions) (Report, error) {
	var rep Report
	write := k != nil && !opts.DryRun
	seal := func(c Context, v string) (string, error) {
		rep.Plaintext++
		switch {
		case k == nil:
			return v, nil
		case opts.Downgrade:
			return k.sealLegacy([]byte(v))
		}
		return k.SealString(c, v)
	}
	// visit returns the valueFunc for one row: where names it in reports,
	// place gives the Context of each of its fields.
	visit := func(where string, place func(field string) Context) valueFunc {
		return func(field, v string) (string, error) {
			c := place(field)
			if !IsSealed(v) {
				return seal(c, v)
			}
			if err := k.Check(c, v); err != nil {
				// Reported, left as it is, and the sweep goes on.
				rep.Unreadable = append(rep.Unreadable, where+" "+field+": "+err.Error())
				return v, nil //nolint:nilerr // an unreadable value is a finding, not a failure of the sweep
			}
			id, _ := KeyIDOf(v)
			primary := id == k.PrimaryID()
			switch {
			case opts.Downgrade && IsLegacy(v) && primary:
				rep.Current++
			case opts.Downgrade:
				rep.Downgraded++
				return k.Downgrade(c, v)
			case IsLegacy(v):
				rep.Upgraded++
				return k.Rewrap(c, v)
			case primary:
				rep.Current++
			case opts.Rewrap:
				rep.Rewrapped++
				return k.Rewrap(c, v)
			default:
				rep.Previous++
			}
			return v, nil
		}
	}
	plain := func(place func(string) Context) valueFunc {
		return func(field, v string) (string, error) { return seal(place(field), v) }
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("secrets sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Every query returns (row key, name for reports, document). The row key
	// is the Row of the values' Context and the key the update matches.
	tables := []struct {
		table, query, update string
		apply                func(doc map[string]any, where, key string) (bool, error)
	}{
		{"repositories", `SELECT id::text, name, config FROM repositories WHERE config ? 'proxy' ORDER BY name FOR UPDATE`,
			`UPDATE repositories SET config = $2 WHERE id = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapRepoDoc(doc, visit(where, func(f string) Context { return RepositoryContext(key, f) }))
			}},
		{"blob_stores", `SELECT id::text, name, config FROM blob_stores ORDER BY name FOR UPDATE`,
			`UPDATE blob_stores SET config = $2 WHERE id = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				place := func(f string) Context { return BlobStoreContext(key, f) }
				return mapStoreDoc(doc, plain(place), visit(where, place))
			}},
		{"settings", `SELECT key, key, value FROM settings ORDER BY key FOR UPDATE`,
			`UPDATE settings SET value = $2 WHERE key = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapFields(doc, visit(where, func(f string) Context { return SettingContext(key, f) }), "", "client_secret", "bind_password")
			}},
		{"outbound_proxies", `SELECT name, name, jsonb_build_object('password', password) FROM outbound_proxies
			WHERE password <> '' ORDER BY name FOR UPDATE`,
			`UPDATE outbound_proxies SET password = $2::jsonb ->> 'password' WHERE name = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapFields(doc, visit(where, func(string) Context { return OutboundProxyContext(key) }), "", "password")
			}},
		{"signing_keys", `SELECT name, name, jsonb_build_object('private_key', private_key) FROM signing_keys ORDER BY name FOR UPDATE`,
			`UPDATE signing_keys SET private_key = $2::jsonb ->> 'private_key' WHERE name = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapFields(doc, visit(where, func(string) Context { return SigningKeyContext(key) }), "", "private_key")
			}},
		{"webhooks", `SELECT id::text, name, jsonb_build_object('secret', secret) FROM webhooks
			WHERE secret <> '' ORDER BY name FOR UPDATE`,
			`UPDATE webhooks SET secret = $2::jsonb ->> 'secret' WHERE id = $1::uuid`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapFields(doc, visit(where, func(string) Context { return WebhookContext(key) }), "", "secret")
			}},
		{"tasks", `SELECT id::text, name, config FROM tasks WHERE config ? 'source' OR config ? 'inventory' ORDER BY name FOR UPDATE`,
			`UPDATE tasks SET config = $2 WHERE id = $1`,
			func(doc map[string]any, where, key string) (bool, error) {
				return mapTaskDoc(doc, visit(where, func(f string) Context { return TaskContext(key, f) }))
			}},
	}
	// Tables of later migrations are skipped when absent (schemas migrated in part).
	optional := map[string]bool{"signing_keys": true, "webhooks": true}
	for _, t := range tables {
		if optional[t.table] {
			var present bool
			if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, t.table).Scan(&present); err != nil {
				return rep, fmt.Errorf("secrets sweep: %s: %w", t.table, err)
			}
			if !present {
				continue
			}
		}
		updates, err := sweepTable(ctx, tx, t.table, t.query, t.apply)
		if err != nil {
			return rep, err
		}
		if !write {
			continue
		}
		for id, doc := range updates {
			if _, err := tx.Exec(ctx, t.update, id, doc); err != nil {
				return rep, fmt.Errorf("secrets sweep: update %s %s: %w", t.table, id, err)
			}
			rep.Rows++
		}
	}
	if !write {
		return rep, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("secrets sweep: commit: %w", err)
	}
	return rep, nil
}

// sweepTable reads every row of query (key, name, document) and returns the
// changed documents by key.
func sweepTable(ctx context.Context, tx pgx.Tx, table, query string,
	apply func(doc map[string]any, where, key string) (bool, error),
) (map[string][]byte, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var key, name string
		var raw []byte
		if err := rows.Scan(&key, &name, &raw); err != nil {
			return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			continue // not an object: nothing to seal
		}
		changed, err := apply(doc, table+"/"+name, key)
		if err != nil {
			return nil, fmt.Errorf("secrets sweep: %s %q: %w", table, name, err)
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("secrets sweep: encode %s %q: %w", table, name, err)
		}
		out[key] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
	}
	return out, nil
}

// mapTaskDoc applies fn to the source credentials of an import or pre-warm
// task config (source, inventory.source); fn gets their JSON paths
// ("source.password", "inventory.source.token").
func mapTaskDoc(doc map[string]any, fn valueFunc) (bool, error) {
	type src struct {
		path string
		doc  any
	}
	srcs := []src{{"source.", doc["source"]}}
	if inv, ok := doc["inventory"].(map[string]any); ok {
		srcs = append(srcs, src{"inventory.source.", inv["source"]})
	}
	changed := false
	for _, s := range srcs {
		m, ok := s.doc.(map[string]any)
		if !ok {
			continue
		}
		c, err := mapFields(m, fn, s.path, "password", "token")
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	return changed, nil
}

// mapFields applies fn to the non-empty string fields of doc named by
// fields, reporting whether any changed. fn gets prefix + the field name.
func mapFields(doc map[string]any, fn valueFunc, prefix string, fields ...string) (bool, error) {
	changed := false
	for _, f := range fields {
		v, ok := doc[f].(string)
		if !ok || v == "" {
			continue
		}
		nv, err := fn(prefix+f, v)
		if err != nil {
			return false, err
		}
		if nv != v {
			doc[f], changed = nv, true
		}
	}
	return changed, nil
}

// Err summarises a report's unreadable values as an error (nil if none).
func (r Report) Err() error {
	if len(r.Unreadable) == 0 {
		return nil
	}
	return fmt.Errorf("%d stored secret(s) cannot be opened with the configured keys where they are stored: %w",
		len(r.Unreadable), errUnreadable)
}

var errUnreadable = errors.New("unreadable secrets")
