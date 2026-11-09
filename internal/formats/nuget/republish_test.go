package nuget_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
)

// TestRepublish pins what a push of an existing version does under each
// write policy: a conflict under allow_once and deny (the status
// "dotnet nuget push --skip-duplicate" skips), a replacement under allow
// that keeps an unlisted version unlisted.
func TestRepublish(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "nuget-allow", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllow}}})
	key := map[string]string{"X-NuGet-ApiKey": f.token}
	listed := func(repo string) bool {
		t.Helper()
		var reg regIndex
		f.getJSON("/repository/"+repo+"/v3/registration/acme.re/index.json", &reg)
		return reg.Items[0].Items[0].CatalogEntry.Listed
	}

	t.Run("allow_once", func(t *testing.T) {
		pkg := nupkg(t, "Acme.Re", "1.0.0", "")
		if rec := f.push("nuget-hosted", pkg, f.token); rec.Code != http.StatusCreated {
			t.Fatalf("first push = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(http.MethodDelete, "/repository/nuget-hosted/api/v2/package/Acme.Re/1.0.0", nil, key); rec.Code != http.StatusNoContent {
			t.Fatalf("unlist = %d", rec.Code)
		}
		for name, body := range map[string][]byte{
			"same bytes":      pkg,
			"different bytes": nupkg(t, "Acme.Re", "1.0.0", "<title>changed</title>"),
		} {
			if rec := f.push("nuget-hosted", body, f.token); rec.Code != http.StatusConflict {
				t.Errorf("%s: re-push = %d %s, want 409", name, rec.Code, rec.Body)
			}
		}
		if listed("nuget-hosted") {
			t.Error("a refused re-push relisted the unlisted version")
		}
	})

	t.Run("allow keeps unlisted", func(t *testing.T) {
		f.mustPush("nuget-allow", "Acme.Re", "1.0.0", "")
		if rec := f.do(http.MethodDelete, "/repository/nuget-allow/api/v2/package/Acme.Re/1.0.0", nil, key); rec.Code != http.StatusNoContent {
			t.Fatalf("unlist = %d", rec.Code)
		}
		if rec := f.push("nuget-allow", nupkg(t, "Acme.Re", "1.0.0", "<title>v2</title>"), f.token); rec.Code != http.StatusCreated {
			t.Fatalf("re-push under allow = %d %s", rec.Code, rec.Body)
		}
		if listed("nuget-allow") {
			t.Error("re-push under allow relisted the unlisted version")
		}
		if rec := f.do(http.MethodPost, "/repository/nuget-allow/api/v2/package/Acme.Re/1.0.0", nil, key); rec.Code != http.StatusOK || !listed("nuget-allow") {
			t.Errorf("explicit relist = %d", rec.Code)
		}
	})

	t.Run("deny", func(t *testing.T) {
		f.env.AddRepo(t, &core.Repository{Name: "nuget-deny", Format: nuget.Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteDeny}}})
		if rec := f.push("nuget-deny", nupkg(t, "Acme.Re", "1.0.0", ""), f.token); rec.Code != http.StatusConflict {
			t.Errorf("push to deny = %d", rec.Code)
		}
	})
}
