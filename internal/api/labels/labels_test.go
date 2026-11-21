package labels_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/api/labels"
	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestValidate(t *testing.T) {
	many := map[string]string{}
	for i := range labels.MaxLabels + 1 {
		many["k"+strings.Repeat("x", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		ok     bool
	}{
		{"empty", nil, true},
		{"plain key", map[string]string{"team": "web"}, true},
		{"prefixed key", map[string]string{"hangar/apply-set": "platform"}, true},
		{"dotted prefix", map[string]string{"example.com/owner": ""}, true},
		{"key starts with a dash", map[string]string{"-team": "x"}, false},
		{"empty name after prefix", map[string]string{"hangar/": "x"}, false},
		{"upper-case prefix", map[string]string{"Hangar/x": "x"}, false},
		{"space in key", map[string]string{"a b": "x"}, false},
		{"name over 63", map[string]string{strings.Repeat("a", 64): "x"}, false},
		{"value over 256", map[string]string{"a": strings.Repeat("v", 257)}, false},
		{"value with newline", map[string]string{"a": "x\ny"}, false},
		{"too many", many, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := labels.Validate(tc.labels)
			if (err == nil) != tc.ok {
				t.Fatalf("Validate(%v) = %v, want ok=%v", tc.labels, err, tc.ok)
			}
			if err != nil && !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("error %v does not wrap core.ErrInvalid", err)
			}
		})
	}
}

func TestStoreWithPostgres(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	ctx := context.Background()
	store := labels.New(pool)

	storeID := s.Store("main", "fs", nil)
	s.Repo("npm-hosted", "npm", "hosted", storeID)
	s.Repo("raw-hosted", "raw", "hosted", storeID)
	apitest.Exec(t, pool, `INSERT INTO roles (name) VALUES ('ci')`)
	apitest.Exec(t, pool, `INSERT INTO outbound_proxies (name, scheme, host, port) VALUES ('corp', 'http', 'proxy.corp', 3128)`)

	owned := map[string]string{"hangar/managed-by": "hangar-apply", "hangar/apply-set": "platform"}
	set := func(kind, name string, l map[string]string) map[string]string {
		t.Helper()
		before, err := store.Set(ctx, kind, name, l, "admin")
		if err != nil {
			t.Fatalf("Set(%s, %s): %v", kind, name, err)
		}
		return before
	}
	list := func(kind string) map[string]map[string]string {
		t.Helper()
		got, err := store.List(ctx, kind)
		if err != nil {
			t.Fatalf("List(%s): %v", kind, err)
		}
		return got
	}

	if before := set(labels.KindRepository, "npm-hosted", owned); len(before) != 0 {
		t.Fatalf("before = %v", before)
	}
	set(labels.KindBlobStore, "main", owned)
	set(labels.KindRole, "ci", owned)
	set(labels.KindProxy, "corp", owned)
	for _, kind := range []string{labels.KindRepository, labels.KindBlobStore, labels.KindRole, labels.KindProxy} {
		if got := list(kind); len(got) != 1 || !maps.Equal(got[map[string]string{
			labels.KindRepository: "npm-hosted", labels.KindBlobStore: "main", labels.KindRole: "ci", labels.KindProxy: "corp",
		}[kind]], owned) {
			t.Fatalf("List(%s) = %v", kind, got)
		}
	}
	if got := list(labels.KindCleanupPolicy); len(got) != 0 {
		t.Fatalf("cleanup policies = %v", got)
	}

	// Replacing returns the previous set; an empty set removes the row.
	if before := set(labels.KindRepository, "npm-hosted", map[string]string{"team": "web"}); !maps.Equal(before, owned) {
		t.Fatalf("before = %v", before)
	}
	set(labels.KindRepository, "npm-hosted", nil)
	if got := list(labels.KindRepository); len(got) != 0 {
		t.Fatalf("after removal = %v", got)
	}
	if n := apitest.QueryString(t, pool, `SELECT count(*) FROM resource_labels WHERE kind = 'repository'`); n != "0" {
		t.Fatalf("rows left = %s", n)
	}

	// Labels belong to the resource they were set on: one deleted and created
	// again under the same name starts without them, so a prune never acts
	// on a resource someone else created in its place.
	set(labels.KindRepository, "raw-hosted", owned)
	apitest.Exec(t, pool, `DELETE FROM repositories WHERE name = 'raw-hosted'`)
	if got := list(labels.KindRepository); len(got) != 0 {
		t.Fatalf("labels of a deleted repository listed: %v", got)
	}
	s.Repo("raw-hosted", "raw", "hosted", storeID)
	if got := list(labels.KindRepository); len(got) != 0 {
		t.Fatalf("recreated repository inherited labels: %v", got)
	}
	if before := set(labels.KindRepository, "raw-hosted", map[string]string{"team": "x"}); len(before) != 0 {
		t.Fatalf("recreated repository reported old labels %v", before)
	}
	apitest.Exec(t, pool, `DELETE FROM outbound_proxies WHERE name = 'corp'`)
	apitest.Exec(t, pool, `INSERT INTO outbound_proxies (name, scheme, host, port) VALUES ('corp', 'http', 'proxy.corp', 3128)`)
	if got := list(labels.KindProxy); len(got) != 0 {
		t.Fatalf("recreated proxy inherited labels: %v", got)
	}

	// Unknown resources and kinds.
	if _, err := store.Set(ctx, labels.KindCleanupPolicy, "nope", owned, "admin"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Set on a missing policy = %v", err)
	}
	if _, err := store.List(ctx, "user"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("List(user) = %v", err)
	}
	if _, err := store.Set(ctx, labels.KindRole, "ci", map[string]string{"bad key": "x"}, "admin"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Set with a bad key = %v", err)
	}
}
