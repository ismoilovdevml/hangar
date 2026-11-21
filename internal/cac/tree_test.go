package cac

import (
	"reflect"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	j, err := toJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return j.(map[string]any)
}

func diffPaths(ds []FieldDiff) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Path.String())
	}
	return strings.Join(out, ",")
}

func TestDiffSpec(t *testing.T) {
	repo, role := kindByName(KindRepository), kindByName(KindRole)
	for _, tc := range []struct {
		name       string
		k          *kind
		want, have map[string]any
		diffs      string // paths that differ, comma-separated
	}{
		{"equal", repo, map[string]any{"description": "x"}, map[string]any{"description": "x"}, ""},
		{"changed scalar", repo, map[string]any{"description": "y"}, map[string]any{"description": "x"}, "description"},
		{"field left out is not managed", repo, map[string]any{}, map[string]any{"description": "x"}, ""},
		{"zero equals absent", repo, map[string]any{"description": "", "online": false, "config": map[string]any{"routing": map[string]any{}}},
			map[string]any{}, ""},
		{"value against absent", repo, map[string]any{"online": true}, map[string]any{}, "online"},
		{"duration spelled differently", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"metadata_ttl": "24h"}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"metadata_ttl": "24h0m0s"}}}, ""},
		{"duration changed", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"metadata_ttl": "1h"}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"metadata_ttl": "24h0m0s"}}}, "config.proxy.metadata_ttl"},
		{"numbers by value", repo, map[string]any{"config": map[string]any{"connector": map[string]any{"http_port": 8080}}},
			map[string]any{"config": map[string]any{"connector": map[string]any{"http_port": 8080.0}}}, ""},
		{"nested server defaults ignored", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"remote_url": "https://r"}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"remote_url": "https://r", "content_ttl": "0s"}}}, ""},
		{"write-only secret not compared", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "bearer", "token": "s3cret"}}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "bearer"}}}}, ""},
		{"atomic object compared exactly", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "bearer", "token": "t"}}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "basic", "username": "u"}}}},
			"config.proxy.auth"},
		{"atomic object with an extra live key", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "header", "token": "t"}}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "header", "header_name": "X-Key"}}}},
			"config.proxy.auth"},
		{"null removes", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": nil}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": map[string]any{"kind": "basic"}}}}, "config.proxy.auth"},
		{"null equals absent", repo, map[string]any{"config": map[string]any{"proxy": map[string]any{"auth": nil}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{}}}, ""},
		{"URL password compared redacted", repo,
			map[string]any{"config": map[string]any{"proxy": map[string]any{"remote_url": "https://u:pw@r.example/"}}},
			map[string]any{"config": map[string]any{"proxy": map[string]any{"remote_url": "https://u:REDACTED@r.example/"}}}, ""},
		{"ordered list", repo,
			map[string]any{"config": map[string]any{"group": map[string]any{"members": []any{"b", "a"}}}},
			map[string]any{"config": map[string]any{"group": map[string]any{"members": []any{"a", "b"}}}}, "config.group.members"},
		{"unordered list", role, map[string]any{"privileges": []any{"repo:*:read", "repo:a:write"}},
			map[string]any{"privileges": []any{"repo:a:write", "repo:*:read"}}, ""},
		{"unordered list changed", role, map[string]any{"privileges": []any{"repo:*:read"}},
			map[string]any{"privileges": []any{"repo:a:write", "repo:*:read"}}, "privileges"},
		{"type mismatch", repo, map[string]any{"online": "yes"}, map[string]any{"online": true}, "online"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := diffPaths(tc.k.diffSpec(mustJSON(t, tc.want), mustJSON(t, tc.have)))
			if got != tc.diffs {
				t.Fatalf("diffs = %q, want %q", got, tc.diffs)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	repo := kindByName(KindRepository)
	live := mustJSON(t, map[string]any{"description": "kept", "config": map[string]any{"proxy": map[string]any{
		"remote_url": "https://r", "metadata_ttl": "1h0m0s",
		"auth": map[string]any{"kind": "basic", "username": "u"}}}})
	want := mustJSON(t, map[string]any{"config": map[string]any{"proxy": map[string]any{
		"metadata_ttl": "30m", "auth": map[string]any{"kind": "bearer", "token": "t"}}}})
	got := repo.merge(nil, live, want)
	expect := mustJSON(t, map[string]any{"description": "kept", "config": map[string]any{"proxy": map[string]any{
		"remote_url": "https://r", "metadata_ttl": "30m",
		// atomic: the basic username does not leak into the bearer auth
		"auth": map[string]any{"kind": "bearer", "token": "t"}}}})
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("merge = %v\nwant %v", got, expect)
	}
	if live["config"].(map[string]any)["proxy"].(map[string]any)["metadata_ttl"] != "1h0m0s" {
		t.Fatal("merge modified the live value")
	}
}

func TestMaskAndRender(t *testing.T) {
	repo := kindByName(KindRepository)
	auth := mustJSON(t, map[string]any{"kind": "bearer", "token": "s3cret"})
	got := render(repo.mask(Path{"config", "proxy", "auth"}, auth, nil), true)
	if strings.Contains(got, "s3cret") || !strings.Contains(got, secretText) {
		t.Fatalf("masked auth = %s", got)
	}
	ref := render(repo.mask(Path{"description"}, "from-env", []Path{{"description"}}), true)
	if ref != secretText {
		t.Fatalf("referenced value = %s", ref)
	}
	u := render(repo.mask(Path{"config", "proxy", "remote_url"}, "https://u:pw@r/", nil), true)
	if strings.Contains(u, "pw") {
		t.Fatalf("URL = %s", u)
	}
	if render(nil, false) != "(unset)" || len(render(strings.Repeat("x", 500), true)) > 120 {
		t.Fatal("render limits")
	}
}
