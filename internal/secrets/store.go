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
	// key (key rotation). Without it only plaintext values are sealed.
	Rewrap bool
	// DryRun counts without writing anything.
	DryRun bool
}

// Report is what Sweep found, counted per secret value.
type Report struct {
	// Plaintext values found (and sealed, unless DryRun or no keyring).
	Plaintext int `json:"plaintext"`
	// Rewrapped values moved from a previous key to the primary key.
	Rewrapped int `json:"rewrapped"`
	// Current values already sealed with the primary key.
	Current int `json:"current"`
	// Previous values still sealed with a previous key (Rewrap not requested).
	Previous int `json:"previous"`
	// Unreadable values that are sealed but cannot be opened, as
	// "repositories/<name> proxy.auth.token: <reason>". They are left as is.
	Unreadable []string `json:"unreadable,omitempty"`
	// Rows written (0 on a dry run).
	Rows int `json:"rows"`
}

// Sweep walks every stored secret (repositories.config and
// blob_stores.config) in one transaction, sealing plaintext values with the
// primary key and, with Rewrap, re-wrapping values sealed with previous keys.
// It is idempotent and safe to run on several nodes at once (rows are locked).
// Updated rows keep their updated_at: the secrets themselves do not change.
//
// With a nil keyring nothing is written; the report shows how many plaintext
// values are waiting to be sealed and how many sealed values cannot be read.
func Sweep(ctx context.Context, pool *pgxpool.Pool, k *Keyring, opts SweepOptions) (Report, error) {
	var rep Report
	write := k != nil && !opts.DryRun
	visit := func(where string) valueFunc {
		return func(field, v string) (string, error) {
			if !IsSealed(v) {
				rep.Plaintext++
				if k == nil {
					return v, nil
				}
				return k.SealString(v)
			}
			if err := k.Check(v); err != nil {
				// Reported, left as it is, and the sweep goes on.
				rep.Unreadable = append(rep.Unreadable, where+" "+field+": "+err.Error())
				return v, nil //nolint:nilerr // an unreadable value is a finding, not a failure of the sweep
			}
			switch {
			case !k.NeedsRewrap(v):
				rep.Current++
			case opts.Rewrap:
				rep.Rewrapped++
				return k.Rewrap(v)
			default:
				rep.Previous++
			}
			return v, nil
		}
	}
	seal := func(v string) (string, error) {
		rep.Plaintext++
		if k == nil {
			return v, nil
		}
		return k.SealString(v)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("secrets sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tables := []struct {
		table, query string
		apply        func(doc map[string]any, where string) (bool, error)
	}{
		{"repositories", `SELECT id::text, name, config FROM repositories WHERE config ? 'proxy' ORDER BY name FOR UPDATE`,
			func(doc map[string]any, where string) (bool, error) { return mapRepoDoc(doc, visit(where)) }},
		{"blob_stores", `SELECT id::text, name, config FROM blob_stores ORDER BY name FOR UPDATE`,
			func(doc map[string]any, where string) (bool, error) { return mapStoreDoc(doc, seal, visit(where)) }},
	}
	for _, t := range tables {
		updates, err := sweepTable(ctx, tx, t.table, t.query, t.apply)
		if err != nil {
			return rep, err
		}
		if !write {
			continue
		}
		for id, doc := range updates {
			if _, err := tx.Exec(ctx, `UPDATE `+t.table+` SET config = $2 WHERE id = $1`, id, doc); err != nil {
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

// sweepTable reads every row of query (id, name, config) and returns the
// changed config documents by id.
func sweepTable(ctx context.Context, tx pgx.Tx, table, query string,
	apply func(doc map[string]any, where string) (bool, error),
) (map[string][]byte, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id, name string
		var raw []byte
		if err := rows.Scan(&id, &name, &raw); err != nil {
			return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			continue // not an object: nothing to seal
		}
		changed, err := apply(doc, table+"/"+name)
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
		out[id] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets sweep: read %s: %w", table, err)
	}
	return out, nil
}

// Err summarises a report's unreadable values as an error (nil if none).
func (r Report) Err() error {
	if len(r.Unreadable) == 0 {
		return nil
	}
	return fmt.Errorf("%d stored secret(s) cannot be opened with the configured keys: %w", len(r.Unreadable), errUnreadable)
}

var errUnreadable = errors.New("unreadable secrets")
