package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// fakeLabels is an in-memory LabelStore over a fixed set of resources.
type fakeLabels struct {
	mu     sync.Mutex
	exists map[string]bool // "kind/name"
	labels map[string]map[string]string
}

func newFakeLabels(resources ...string) *fakeLabels {
	f := &fakeLabels{exists: map[string]bool{}, labels: map[string]map[string]string{}}
	for _, r := range resources {
		f.exists[r] = true
	}
	return f
}

func (f *fakeLabels) List(_ context.Context, kind string) (map[string]map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]string{}
	for k, l := range f.labels {
		if name, ok := strings.CutPrefix(k, kind+"/"); ok && len(l) > 0 {
			out[name] = maps.Clone(l)
		}
	}
	return out, nil
}

func (f *fakeLabels) Set(_ context.Context, kind, name string, l map[string]string, _ string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := kind + "/" + name
	if !f.exists[k] {
		return nil, fmt.Errorf("%s %q: %w", kind, name, core.ErrNotFound)
	}
	before := f.labels[k]
	if before == nil {
		before = map[string]string{}
	}
	f.labels[k] = maps.Clone(l)
	return before, nil
}

func TestResourceLabelsAuthorization(t *testing.T) {
	f := newFixture(t)
	store := newFakeLabels("repository/npm-hosted", "repository/maven-releases", "role/ci")
	store.labels["repository/npm-hosted"] = map[string]string{"team": "web"}
	store.labels["repository/maven-releases"] = map[string]string{"team": "jvm"}
	store.labels["role/ci"] = map[string]string{"team": "ops"}
	f.api.Labels = store

	for _, tc := range []struct {
		name, user, method, path string
		body                     any
		code                     int
	}{
		{"kind required", "admin", "GET", "/api/v1/resource-labels", nil, 400},
		{"unknown kind", "admin", "GET", "/api/v1/resource-labels?kind=user", nil, 400},
		{"anonymous", "", "GET", "/api/v1/resource-labels?kind=repository", nil, 401},
		{"roles need system:users:read", "alice", "GET", "/api/v1/resource-labels?kind=role", nil, 403},
		{"admin reads roles", "admin", "GET", "/api/v1/resource-labels?kind=role", nil, 200},
		{"read access is not enough to label", "alice", "PUT", "/api/v1/resource-labels/repository/npm-hosted",
			map[string]any{"labels": map[string]string{"a": "b"}}, 403},
		{"roles need system:users:write", "alice", "PUT", "/api/v1/resource-labels/role/ci",
			map[string]any{"labels": map[string]string{"a": "b"}}, 403},
		{"unknown resource", "admin", "PUT", "/api/v1/resource-labels/repository/nope",
			map[string]any{"labels": map[string]string{"a": "b"}}, 404},
		{"labels required", "admin", "PUT", "/api/v1/resource-labels/repository/npm-hosted", map[string]any{}, 400},
		{"bad key", "admin", "PUT", "/api/v1/resource-labels/repository/npm-hosted",
			map[string]any{"labels": map[string]string{"-bad": "x"}}, 400},
		{"control character in value", "admin", "PUT", "/api/v1/resource-labels/repository/npm-hosted",
			map[string]any{"labels": map[string]string{"ok": "a\nb"}}, 400},
		{"unknown kind on write", "admin", "PUT", "/api/v1/resource-labels/user/alice",
			map[string]any{"labels": map[string]string{"a": "b"}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.expect(tc.user, tc.method, tc.path, tc.body, tc.code)
		})
	}
	if len(store.labels["repository/npm-hosted"]) != 1 {
		t.Fatalf("a refused request changed labels: %v", store.labels)
	}
}

// Repository labels are listed only for repositories the caller may read.
func TestResourceLabelsListFiltersRepositories(t *testing.T) {
	f := newFixture(t)
	store := newFakeLabels()
	store.labels["repository/npm-hosted"] = map[string]string{"team": "web"}
	store.labels["repository/maven-releases"] = map[string]string{"team": "jvm"}
	f.api.Labels = store
	for _, tc := range []struct {
		user string
		want []string
	}{
		{"admin", []string{"maven-releases", "npm-hosted"}},
		{"alice", []string{"npm-hosted"}},
		{"bob", nil},
	} {
		t.Run(tc.user, func(t *testing.T) {
			var got struct {
				Items []resourceLabelsJSON `json:"items"`
			}
			if err := json.Unmarshal(f.expect(tc.user, "GET", "/api/v1/resource-labels?kind=repository", nil, 200), &got); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, it := range got.Items {
				names = append(names, it.Name)
				if it.Kind != "repository" || len(it.Labels) == 0 {
					t.Fatalf("item = %+v", it)
				}
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("names = %v, want %v", names, tc.want)
			}
		})
	}
}

// A change is saved and audited with the labels before and after; an
// unchanged set is not audited again.
func TestResourceLabelsPutAudited(t *testing.T) {
	f := newFixture(t)
	f.api.Labels = newFakeLabels("blob_store/s3-main")
	body := map[string]any{"labels": map[string]string{"hangar/managed-by": "hangar-apply", "hangar/apply-set": "platform"}}
	var got resourceLabelsJSON
	if err := json.Unmarshal(f.expect("admin", "PUT", "/api/v1/resource-labels/blob_store/s3-main", body, 200), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "blob_store" || got.Name != "s3-main" || got.Labels["hangar/apply-set"] != "platform" {
		t.Fatalf("answer = %+v", got)
	}
	f.expect("admin", "PUT", "/api/v1/resource-labels/blob_store/s3-main", body, 200)
	events := f.env.Audit.Events(auditLabelsUpdate)
	if len(events) != 1 || events[0].Target != "blob_store:s3-main" || events[0].Actor != "admin" {
		t.Fatalf("audit = %+v", events)
	}
	after, _ := events[0].Details["after"].(map[string]string)
	if after["hangar/managed-by"] != "hangar-apply" {
		t.Fatalf("audit details = %+v", events[0].Details)
	}
	f.expect("alice", "PUT", "/api/v1/resource-labels/blob_store/s3-main", map[string]any{"labels": map[string]string{}}, 403)
	if denied := f.env.Audit.Events(auditLabelsUpdate); len(denied) != 2 || denied[1].Outcome != core.OutcomeDenied {
		t.Fatalf("refusal not audited: %+v", denied)
	}
}

func TestResourceLabelsWithoutStore(t *testing.T) {
	f := newFixture(t)
	f.expect("admin", "GET", "/api/v1/resource-labels?kind=repository", nil, 501)
}
