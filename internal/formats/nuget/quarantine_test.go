package nuget_test

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

type flatDoc struct {
	Versions []string `json:"versions"`
}

type searchDoc struct {
	TotalHits int `json:"totalHits"`
	Data      []struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	} `json:"data"`
}

func regVersions(t *testing.T, f *fixture, path string) []string {
	t.Helper()
	var reg regIndex
	f.getJSON(path, &reg)
	var out []string
	for _, p := range reg.Items {
		for _, it := range p.Items {
			out = append(out, it.CatalogEntry.Version)
		}
	}
	return out
}

// quarantineComponent stores a quarantined component for name/version in
// repo, as the scanner leaves it.
func quarantineComponent(t *testing.T, f *fixture, repo, name, version string) {
	t.Helper()
	r, err := f.env.Repos.Get(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	d, n, _ := f.env.Blob.Put(t.Context(), bytes.NewReader([]byte("pkg "+version)))
	_, _, err = f.env.Components.Publish(coretest.AsUser(t.Context(), "alice"), r, core.PublishRequest{
		Component: &core.Component{Name: name, Version: version, Quarantined: true},
		Assets:    []*core.Asset{{Path: "quarantined/" + name + "/" + version, BlobDigest: d, Size: n}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestQuarantinedVersionsNotAdvertised: a quarantined version is left out
// of the flat container list, the registration and search, so clients
// resolve to a version they can download.
func TestQuarantinedVersionsNotAdvertised(t *testing.T) {
	t.Run("hosted", func(t *testing.T) {
		f := newFixture(t)
		f.mustPush("nuget-hosted", "Acme.Q", "1.0.0", "")
		f.mustPush("nuget-hosted", "Acme.Q", "2.0.0", "")
		repo, _ := f.env.Repos.Get(t.Context(), "nuget-hosted")
		c, err := f.env.Components.GetComponent(t.Context(), repo.ID, "", "acme.q", "2.0.0")
		if err != nil {
			t.Fatal(err)
		}
		f.env.Components.SetQuarantined(c.ID, true)

		var flat flatDoc
		f.getJSON("/repository/nuget-hosted/v3-flatcontainer/acme.q/index.json", &flat)
		if !slices.Equal(flat.Versions, []string{"1.0.0"}) {
			t.Errorf("flat container versions = %v", flat.Versions)
		}
		if got := regVersions(t, f, "/repository/nuget-hosted/v3/registration/acme.q/index.json"); !slices.Equal(got, []string{"1.0.0"}) {
			t.Errorf("registration versions = %v", got)
		}
		if rec := f.get("/repository/nuget-hosted/v3/registration/acme.q/2.0.0.json"); rec.Code != http.StatusNotFound {
			t.Errorf("registration leaf of the quarantined version = %d", rec.Code)
		}
		var res searchDoc
		f.getJSON("/repository/nuget-hosted/query?q=acme.q", &res)
		if res.TotalHits != 1 || res.Data[0].Version != "1.0.0" || len(res.Data[0].Versions) != 1 {
			t.Errorf("search = %+v", res)
		}
	})

	t.Run("proxy", func(t *testing.T) {
		f, _ := newProxyFixture(t)
		quarantineComponent(t, f, "nuget-proxy", "remote.pkg", "1.0.0")
		var flat flatDoc
		f.getJSON("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/index.json", &flat)
		if !slices.Equal(flat.Versions, []string{"2.0.0"}) {
			t.Errorf("flat container versions = %v", flat.Versions)
		}
		if got := regVersions(t, f, "/repository/nuget-proxy/v3/registration-gz-semver2/remote.pkg/index.json"); !slices.Equal(got, []string{"2.0.0"}) {
			t.Errorf("registration versions = %v", got)
		}
		// The group merges what its members advertise.
		f.getJSON("/repository/nuget-group/v3-flatcontainer/remote.pkg/index.json", &flat)
		if !slices.Equal(flat.Versions, []string{"2.0.0"}) {
			t.Errorf("group flat container versions = %v", flat.Versions)
		}
		// The only version of the upstream search hit is quarantined: no hit.
		var res searchDoc
		f.getJSON("/repository/nuget-proxy/query?q=remote", &res)
		if res.TotalHits != 0 || len(res.Data) != 0 {
			t.Errorf("search = %+v", res)
		}
	})
}
