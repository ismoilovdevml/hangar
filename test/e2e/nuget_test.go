//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NuGet acceptance (docs/FORMATS.md, "NuGet"): dotnet nuget push into a
// hosted repository, dotnet add package + dotnet restore through a group of
// hosted + api.nuget.org proxy, search, unlist.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories), the dotnet SDK and internet access for the proxy;
// HANGAR_E2E_BLOB_STORE defaults to "default".
func TestNuGet(t *testing.T) {
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip("dotnet not installed")
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "nuget-e2e-hosted-"+suffix, "nuget-e2e-proxy-"+suffix, "nuget-e2e-group-"+suffix
	pkgID := "Hangar.E2E.Tiny" + suffix

	nugetCreateRepo(t, base, token, map[string]any{"name": hosted, "format": "nuget", "type": "hosted", "blob_store": store,
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}})
	nugetCreateRepo(t, base, token, map[string]any{"name": proxy, "format": "nuget", "type": "proxy", "blob_store": store,
		"config": map[string]any{"proxy": map[string]any{"remote_url": "https://api.nuget.org", "metadata_ttl": "1h"}}})
	nugetCreateRepo(t, base, token, map[string]any{"name": group, "format": "nuget", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}, "write_member": hosted}}})

	root := t.TempDir()
	env := append(os.Environ(),
		"NUGET_PACKAGES="+filepath.Join(root, "packages"),
		"NUGET_HTTP_CACHE_PATH="+filepath.Join(root, "http-cache"),
		"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1",
	)
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "dotnet", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dotnet %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	source := func(repo string) string { return base + "/repository/" + repo + "/index.json" }
	config := func(dir, repo string) string {
		path := filepath.Join(dir, "nuget.config")
		insecure := ""
		if strings.HasPrefix(base, "http://") {
			insecure = ` allowInsecureConnections="true"`
		}
		doc := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources><clear /><add key="hangar" value="%s"%s /></packageSources>
  <packageSourceCredentials><hangar>
    <add key="Username" value="token" /><add key="ClearTextPassword" value="%s" />
  </hangar></packageSourceCredentials>
</configuration>
`, source(repo), insecure, token)
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mkdir := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// Pack a library that depends on a nuget.org package (restored through the group).
	lib := mkdir("lib")
	libCfg := config(lib, group)
	proj := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net10.0</TargetFramework>
    <PackageId>%s</PackageId>
    <Version>1.0.0</Version>
    <Authors>Hangar E2E</Authors>
    <Description>Hangar NuGet acceptance test package</Description>
  </PropertyGroup>
  <ItemGroup><PackageReference Include="Newtonsoft.Json" Version="13.0.3" /></ItemGroup>
</Project>
`, pkgID)
	if err := os.WriteFile(filepath.Join(lib, pkgID+".csproj"), []byte(proj), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "Tiny.cs"), []byte("namespace Tiny; public static class T { public static string S() => \"ok\"; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	run(lib, "pack", "-c", "Release", "-o", out, "-p:RestoreConfigFile="+libCfg)
	nupkg := filepath.Join(out, pkgID+".1.0.0.nupkg")

	pushArgs := []string{"nuget", "push", nupkg, "-s", source(hosted), "-k", token}
	if strings.HasPrefix(base, "http://") {
		pushArgs = append(pushArgs, "--allow-insecure-connections")
	}
	if o := run(root, pushArgs...); !strings.Contains(o, "Your package was pushed") {
		t.Errorf("push:\n%s", o)
	}

	// Fresh consumer, group only.
	if err := os.RemoveAll(filepath.Join(root, "packages")); err != nil {
		t.Fatal(err)
	}
	app := mkdir("app")
	run(app, "new", "console", "--framework", "net10.0", "--no-restore")
	appCfg := config(app, group)
	run(app, "add", "package", pkgID)
	run(app, "restore", "--configfile", appCfg, "--no-cache")
	run(app, "build", "--no-restore")
	for _, p := range []string{strings.ToLower(pkgID) + "/1.0.0", "newtonsoft.json/13.0.3"} {
		if _, err := os.Stat(filepath.Join(root, "packages", p)); err != nil {
			t.Errorf("not restored: %v", err)
		}
	}
	if o := run(app, "package", "search", pkgID, "--source", source(group), "--configfile", appCfg); !strings.Contains(o, pkgID) {
		t.Errorf("search:\n%s", o)
	}

	// Unlist (dotnet nuget delete) keeps the version restorable but marks it unlisted.
	admin := mkdir("admin")
	config(admin, hosted)
	run(admin, "nuget", "delete", pkgID, "1.0.0", "-s", "hangar", "-k", token, "--non-interactive")
	var reg struct {
		Items []struct {
			Items []struct {
				CatalogEntry struct {
					Listed bool `json:"listed"`
				} `json:"catalogEntry"`
			} `json:"items"`
		} `json:"items"`
	}
	nugetGetJSON(t, base+"/repository/"+hosted+"/v3/registration-gz-semver2/"+strings.ToLower(pkgID)+"/index.json", token, &reg)
	if len(reg.Items) != 1 || len(reg.Items[0].Items) != 1 || reg.Items[0].Items[0].CatalogEntry.Listed {
		t.Errorf("after unlist: %+v", reg)
	}
}

func nugetCreateRepo(t *testing.T, base, token string, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/repositories", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		t.Fatalf("create repository %v = %d %s", body["name"], resp.StatusCode, msg)
	}
}

func nugetGetJSON(t *testing.T, url, token string, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.SetBasicAuth("token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}
