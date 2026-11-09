package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

func TestRoutingAPI(t *testing.T) {
	f := newFixture(t)
	f.seedRepos()
	f.api.Catalog = fakeCatalog{}
	ctx := context.Background()

	// Empty rules read back as empty lists.
	var got routingJSON
	if err := json.Unmarshal(f.expect("alice", "GET", "/api/v1/repositories/npm-all/routing", nil, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Allow == nil || got.Block == nil || got.Members == nil || len(got.Members) != 0 {
		t.Fatalf("empty rules = %+v", got)
	}

	group := map[string]any{"members": []any{map[string]any{"member": "npm-hosted", "exclusive": []string{"@trk/*"}}}}
	// Readers may not change rules; refusals are audited.
	f.expect("alice", "PUT", "/api/v1/repositories/npm-all/routing", group, http.StatusForbidden)
	if ev := f.env.Audit.Events("repository.routing.update"); len(ev) != 1 || ev[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denied audit = %+v", ev)
	}
	// Validation: unknown member, proxy fields on a group, members on a proxy, hosted repositories.
	f.expect("admin", "PUT", "/api/v1/repositories/npm-all/routing",
		map[string]any{"members": []any{map[string]any{"member": "nope", "block": []string{"x"}}}}, http.StatusBadRequest)
	f.expect("admin", "PUT", "/api/v1/repositories/npm-all/routing", map[string]any{"block": []string{"x"}}, http.StatusBadRequest)
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy/routing", group, http.StatusBadRequest)
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy/routing", map[string]any{"block": []string{"bad pattern"}}, http.StatusBadRequest)
	f.expect("admin", "GET", "/api/v1/repositories/npm-hosted/routing", nil, http.StatusBadRequest)
	f.expect("admin", "PUT", "/api/v1/repositories/npm-all/routing", `{"members":`, http.StatusBadRequest)
	f.expect("bob", "GET", "/api/v1/repositories/npm-all/routing", nil, http.StatusNotFound)

	if err := json.Unmarshal(f.expect("admin", "PUT", "/api/v1/repositories/npm-all/routing", group, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].Exclusive[0] != "@trk/*" {
		t.Fatalf("saved = %+v", got)
	}
	stored, _ := f.env.Repos.Get(ctx, "npm-all")
	if stored.Config.Routing == nil || stored.Config.Routing.Members[0].Member != "npm-hosted" {
		t.Fatalf("stored = %+v", stored.Config.Routing)
	}
	ev := f.env.Audit.Events("repository.routing.update")
	if last := ev[len(ev)-1]; last.Outcome != core.OutcomeSuccess || last.Target != "repo:npm-all" || last.Details["after"] == nil {
		t.Fatalf("audit = %+v", last)
	}
	// The rules travel with the repository.
	var repo struct {
		Config core.RepoConfig `json:"config"`
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories/npm-all", nil, http.StatusOK), &repo)
	if repo.Config.Routing.Empty() {
		t.Fatal("GET /repositories/{name} lacks config.routing")
	}

	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy/routing", map[string]any{"block": []string{"@trk/*"}}, http.StatusOK)

	var e routing.Explanation
	if err := json.Unmarshal(f.expect("alice", "POST", "/api/v1/repositories/npm-all/routing:test",
		map[string]string{"name": "@trk/ui"}, http.StatusOK), &e); err != nil {
		t.Fatal(err)
	}
	if !e.Allowed || e.ServedBy != "npm-hosted" || len(e.Members) != 2 || e.Members[1].Allowed || e.Members[1].Block == nil {
		t.Fatalf("test = %+v", e)
	}
	_ = json.Unmarshal(f.expect("alice", "POST", "/api/v1/repositories/npm-proxy/routing:test",
		map[string]string{"name": "@trk/ui"}, http.StatusOK), &e)
	if e.Allowed || e.Block == nil || !strings.Contains(e.Block.Reason(), `"@trk/*"`) {
		t.Fatalf("proxy test = %+v", e)
	}
	f.expect("alice", "POST", "/api/v1/repositories/npm-all/routing:test", map[string]string{}, http.StatusBadRequest)

	// Clearing.
	f.expect("admin", "DELETE", "/api/v1/repositories/npm-all/routing", nil, http.StatusNoContent)
	if stored, _ := f.env.Repos.Get(ctx, "npm-all"); stored.Config.Routing != nil {
		t.Fatalf("cleared = %+v", stored.Config.Routing)
	}
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy/routing", map[string]any{}, http.StatusNoContent)
}

func TestRoutingSuggest(t *testing.T) {
	f := newFixture(t)
	hosted, _, _ := f.seedRepos()
	ctx := core.WithPrincipal(context.Background(), &core.Principal{Name: "admin", Kind: core.PrincipalUser})
	f.env.AuthZ.Grant("admin", "repo:*:write")
	for _, c := range []*core.Component{{Namespace: "@trk", Name: "ui", Version: "1.0.0"}, {Namespace: "@trk", Name: "api", Version: "1.0.0"},
		{Name: "internal-tool", Version: "2.0.0"}} {
		if _, _, err := f.env.Components.Publish(ctx, hosted, core.PublishRequest{Component: c}); err != nil {
			t.Fatal(err)
		}
	}
	f.expect("alice", "GET", "/api/v1/repositories/npm-all/routing:suggest", nil, http.StatusForbidden)
	for _, repo := range []string{"npm-all", "npm-proxy"} {
		var res struct {
			Suggestions []routingSuggestion `json:"suggestions"`
			Truncated   bool                `json:"truncated"`
		}
		if err := json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories/"+repo+"/routing:suggest", nil, http.StatusOK), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Suggestions) != 2 || res.Suggestions[0].Pattern != "@trk/*" || res.Suggestions[0].Matches != 2 ||
			res.Suggestions[0].Source != "npm-hosted" || res.Truncated {
			t.Fatalf("%s suggestions = %+v", repo, res)
		}
		if member := res.Suggestions[0].Member; (repo == "npm-all") != (member == "npm-hosted") {
			t.Fatalf("%s member = %q", repo, member)
		}
	}
}
