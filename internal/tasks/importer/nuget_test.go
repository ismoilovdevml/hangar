package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func nupkgOf(t *testing.T, id, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(id + ".nuspec")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata><id>%s</id><version>%s</version><authors>Acme</authors><description>%s library</description></metadata>
</package>`, id, version, id)
	w, _ = zw.Create("lib/netstandard2.0/" + id + ".dll")
	_, _ = w.Write([]byte("binary " + id + " " + version))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// nugetSource serves a V3 feed: service index, registration (one inline
// page), flat container and search.
func nugetSource(t *testing.T, pkgs map[string][]string, unlisted map[string]bool) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	base := src.URL()
	src.addJSON("/v3/index.json", map[string]any{"version": "3.0.0", "resources": []any{
		map[string]string{"@id": base + "/v3/flat/", "@type": "PackageBaseAddress/3.0.0"},
		map[string]string{"@id": base + "/v3/reg/", "@type": "RegistrationsBaseUrl/3.6.0"},
		map[string]string{"@id": base + "/v3/query", "@type": "SearchQueryService/3.5.0"},
	}})
	var data []any
	for id, versions := range pkgs {
		lid := strings.ToLower(id)
		var items []any
		for i, v := range versions {
			body := nupkgOf(t, id, v)
			sum := sha512.Sum512(body)
			p := fmt.Sprintf("/v3/flat/%s/%s/%s.%s.nupkg", lid, strings.ToLower(v), lid, strings.ToLower(v))
			src.add(p, body, "application/octet-stream")
			published := time.Date(2022, 5, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
			listed := !unlisted[id+"@"+v]
			if !listed {
				published = "1900-01-01T00:00:00Z"
			}
			items = append(items, map[string]any{"packageContent": base + p, "catalogEntry": map[string]any{
				"id": id, "version": v, "published": published, "listed": listed,
				"packageHash": base64.StdEncoding.EncodeToString(sum[:]), "packageHashAlgorithm": "SHA512"}})
		}
		lower := make([]string, len(versions))
		for i, v := range versions {
			lower[i] = strings.ToLower(v)
		}
		src.addJSON("/v3/flat/"+lid+"/index.json", map[string]any{"versions": lower})
		src.addJSON("/v3/reg/"+lid+"/index.json", map[string]any{"count": 1, "items": []any{
			map[string]any{"@id": base + "/v3/reg/" + lid + "/index.json#page", "count": len(items), "items": items}}})
		data = append(data, map[string]any{"id": id})
	}
	src.addJSON("/v3/query", map[string]any{"totalHits": len(data), "data": data})
	return src
}

func TestImportNuGet(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "nuget-hosted", "nuget", core.RepoHosted, "")
	src := nugetSource(t, map[string][]string{
		"Acme.Core": {"1.0.0", "1.1.0", "2.0.0-beta.1"},
		"Acme.Json": {"3.2.1"},
		"Other.Lib": {"1.0.0"},
	}, map[string]bool{"Acme.Core@1.0.0": true})
	cfg := ImportConfig{Repository: "nuget-hosted", Source: Source{URL: src.URL() + "/v3/index.json"},
		Filters: Filters{Packages: []string{"Acme.*"}, Prereleases: true}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 4 || res.Packages != 2 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "nuget-hosted")
	c, err := h.env.Components.GetComponent(t.Context(), repo.ID, "", "acme.core", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !c.CreatedAt.Equal(time.Date(2022, 5, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("published %s", c.CreatedAt)
	}
	if listed, _ := mustComponent(t, h, repo, "acme.core", "1.0.0").Attributes["listed"].(bool); listed {
		t.Fatal("unlisted version was listed after import")
	}
	_, res, err = h.runImport(t, cfg)
	if err != nil || res.Skipped != 4 || res.Imported != 0 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip("dotnet not installed")
	}
	restoreWithDotnet(t, h.url+"/repository/nuget-hosted/index.json", "Acme.Json", "3.2.1")
}

func mustComponent(t *testing.T, h *hangar, repo *core.Repository, name, version string) *core.Component {
	t.Helper()
	c, err := h.env.Components.GetComponent(t.Context(), repo.ID, "", name, version)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// restoreWithDotnet restores a project referencing id@version from feed.
func restoreWithDotnet(t *testing.T, feed, id, version string) {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<configuration><packageSources><clear /><add key="hangar" value="%s" allowInsecureConnections="true" protocolVersion="3" /></packageSources></configuration>`, feed)
	// The SDK's own framework needs no packages from the feed.
	sdk, err := exec.CommandContext(t.Context(), "dotnet", "--version").Output()
	if err != nil {
		t.Skip("dotnet has no SDK")
	}
	major, _, _ := strings.Cut(strings.TrimSpace(string(sdk)), ".")
	proj := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net%s.0</TargetFramework></PropertyGroup>
<ItemGroup><PackageReference Include="%s" Version="%s" /></ItemGroup></Project>`, major, id, version)
	_ = os.WriteFile(filepath.Join(dir, "nuget.config"), []byte(cfg), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "app.csproj"), []byte(proj), 0o600)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dotnet", "restore", "--configfile", filepath.Join(dir, "nuget.config"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NUGET_PACKAGES="+filepath.Join(dir, "packages"), "NUGET_HTTP_CACHE_PATH="+filepath.Join(dir, "http"),
		"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "MSBUILDDISABLENODEREUSE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dotnet restore: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "packages", strings.ToLower(id), version)); err != nil {
		t.Fatalf("restored package missing: %v", err)
	}
}
