package component_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/component"
)

func TestTextQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"   ":                        "",
		"Newtonsoft.Json":            "'newtonsoft':* & 'json':*",
		"tags:Serial id:acme-core":   "'serial':* & 'acme':* & 'core':*",
		`"quoted" o'brien; DROP x--`: "'quoted':* & 'o':* & 'brien':* & 'drop':* & 'x':*",
		"#":                          "",
		"Ünïcode数据":                  "'ünïcode数据':*",
	} {
		if got := component.TextQuery(in); got != want {
			t.Errorf("TextQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchComponents(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "nuget-h", core.RepoHosted, core.WriteAllow)
	other := f.repo(t, "nuget-o", core.RepoHosted, core.WriteAllow)
	pub := func(repo *core.Repository, name, version string, attrs map[string]any) {
		t.Helper()
		d := f.blob(t, repo.Name+name+version)
		if _, _, err := f.svc.Publish(f.ctx, repo, core.PublishRequest{
			Component: comp("", name, version, attrs),
			Assets:    []*core.Asset{asset(name+"/"+version+"/"+name+".nupkg", d, core.AssetContent)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pub(r, "newtonsoft.json", "13.0.1", map[string]any{"nuget": map[string]any{"description": "Popular JSON framework", "tags": "json serializer"}})
	pub(r, "newtonsoft.json", "13.0.3", nil)
	pub(r, "acme.core", "1.0.0", map[string]any{"nuget": map[string]any{"description": "Core library of ACME"}})
	pub(r, "acme.web", "2.0.0", map[string]any{"nuget": map[string]any{"title": "ACME web helpers"}})
	pub(other, "acme.other", "1.0.0", nil)

	names := func(cs []*core.Component) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name+"@"+c.Version)
		}
		return out
	}
	empty := ""
	for _, tc := range []struct {
		text string
		max  int
		want []string
		more bool
	}{
		{"json", 0, []string{"newtonsoft.json@13.0.1", "newtonsoft.json@13.0.3"}, false}, // every version of a match
		{"Newtonsoft.Js", 0, []string{"newtonsoft.json@13.0.1", "newtonsoft.json@13.0.3"}, false},
		{"serial", 0, []string{"newtonsoft.json@13.0.1", "newtonsoft.json@13.0.3"}, false}, // attribute text
		{"acme", 0, []string{"acme.core@1.0.0", "acme.web@2.0.0"}, false},                 // not the other repository
		{"acme helpers", 0, []string{"acme.web@2.0.0"}, false},                             // all words must match
		{"acme", 1, []string{"acme.core@1.0.0"}, true},
		{"", 0, []string{"acme.core@1.0.0", "acme.web@2.0.0", "newtonsoft.json@13.0.1", "newtonsoft.json@13.0.3"}, false},
		{"nomatch", 0, nil, false},
	} {
		got, more, err := f.svc.SearchComponents(f.ctx, core.ComponentTextQuery{RepositoryID: r.ID, Namespace: &empty, Text: tc.text, MaxNames: tc.max})
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if !reflect.DeepEqual(names(got), tc.want) || more != tc.more {
			t.Errorf("%q max %d = %v (more %v), want %v (more %v)", tc.text, tc.max, names(got), more, tc.want, tc.more)
		}
		for _, c := range got {
			if c.Repository != r.Name {
				t.Errorf("component %s: repository %q", c.Name, c.Repository)
			}
		}
	}
	if _, _, err := f.svc.SearchComponents(f.ctx, core.ComponentTextQuery{}); err == nil {
		t.Error("search without repository id succeeded")
	}
}

func TestSearchComponentsHugeAttributes(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "big", core.RepoHosted, core.WriteAllow)
	big := make([]byte, 0, 3<<20)
	for i := 0; len(big) < 3<<20; i++ {
		big = fmt.Appendf(big, "word%d ", i)
	}
	// The search document is capped, so a huge attribute cannot fail the publish.
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{
		Component: comp("", "huge", "1.0.0", map[string]any{"description": string(big)}),
		Assets:    []*core.Asset{asset("huge-1.0.0.bin", f.blob(t, "huge"), core.AssetContent)},
	}); err != nil {
		t.Fatalf("publish with 3 MiB attributes: %v", err)
	}
}

func TestAssetEvents(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-ev", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "x")
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{
		asset("b/file.txt", d, core.AssetContent), asset("a/file.txt", d, core.AssetContent)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteAsset(f.ctx, r, "a/file.txt"); err != nil {
		t.Fatal(err)
	}
	pubs := f.events.Published(core.EventAssetPublished)
	if len(pubs) != 1 || pubs[0].Repository != r.Name || string(pubs[0].Data) != `{"paths":["b/file.txt","a/file.txt"]}` {
		t.Fatalf("asset.published = %+v", pubs)
	}
	dels := f.events.Published(core.EventAssetDeleted)
	if len(dels) != 1 || dels[0].Subject != "a/file.txt" {
		t.Fatalf("asset.deleted = %+v", dels)
	}
	// Component publishes keep their own event.
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "c", "1", nil),
		Assets: []*core.Asset{asset("c/1/c.bin", d, core.AssetContent)}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.events.Published(core.EventAssetPublished)); n != 1 {
		t.Fatalf("component publish emitted asset.published (%d events)", n)
	}
}
