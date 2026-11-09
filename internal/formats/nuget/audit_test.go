package nuget_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
)

// TestDotnetAudit: "dotnet restore" against a hosted repository reports
// the vulnerabilities this server knows about its packages (NuGet Audit),
// through the repository and through a group.
func TestDotnetAudit(t *testing.T) {
	d := newDotnet(t)
	if testing.Short() {
		t.Skip("real-client test skipped with -short")
	}
	s := newServer(t)
	hosted := s.env.AddRepo(t, &core.Repository{Name: "nuget-hosted", Format: nuget.Name, Type: core.RepoHosted})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted"}}}})
	format := nuget.New()
	format.SetAdvisories(fakeAdvisories{hosted.ID: {
		{ID: "Acme.Risky", Version: "1.0.0", Severity: nuget.SeverityCritical, URL: "https://osv.dev/vulnerability/GHSA-hngr-test-0001"},
	}})
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	s.env.BaseURL = srv.URL // links in documents
	h = formats.NewHandler(s.env.Deps(), format)
	token := s.env.Auth.AddToken("ci")
	s.env.AuthZ.Grant("ci", "repo:nuget-*:read", "repo:nuget-*:write")
	source := func(repo string) string { return srv.URL + "/repository/" + repo + "/index.json" }

	cfg := filepath.Join(d.root, "push.config")
	writeConfig(t, cfg, source("nuget-hosted"), "ci", token)
	pkg := d.pack("Acme.Risky", "1.0.0", cfg, nil)
	d.run(d.root, "nuget", "push", "--allow-insecure-connections", pkg, "-s", source("nuget-hosted"), "-k", token)

	for _, repo := range []string{"nuget-hosted", "nuget-group"} {
		app := filepath.Join(d.root, "app-"+repo)
		if err := os.MkdirAll(app, 0o755); err != nil {
			t.Fatal(err)
		}
		proj := `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net10.0</TargetFramework>
    <NuGetAudit>true</NuGetAudit><NuGetAuditMode>all</NuGetAuditMode></PropertyGroup>
  <ItemGroup><PackageReference Include="Acme.Risky" Version="1.0.0" /></ItemGroup>
</Project>
`
		if err := os.WriteFile(filepath.Join(app, "app.csproj"), []byte(proj), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app, "Program.cs"), []byte("System.Console.WriteLine(Acme.Risky.Hello.Say());\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, filepath.Join(app, "nuget.config"), source(repo), "ci", token)
		out := d.run(app, "restore", "--force-evaluate")
		if !strings.Contains(out, "NU1904") || !strings.Contains(out, "GHSA-hngr-test-0001") || strings.Contains(out, "NU1900") {
			t.Errorf("%s: restore did not report the critical vulnerability:\n%s", repo, out)
		}
	}
}
