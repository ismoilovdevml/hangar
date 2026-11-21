package cac

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	npmToken      = "npm-token-7f3a9c1e"
	proxyPassword = "proxy-pass-55b2e0"
)

// platform is a multi-resource configuration: an outbound proxy, a blob
// store, a role, hosted, proxy and nested group repositories and a cleanup
// policy, in an order apply has to fix (groups before their members).
const platform = `
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-outer
spec:
  format: npm
  type: group
  config:
    group:
      members: [npm-all]
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-all
  labels:
    team: web
spec:
  format: npm
  type: group
  config:
    group:
      members: [npm-hosted, npm-proxy]
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-proxy
spec:
  format: npm
  type: proxy
  blob_store: cac-store
  config:
    proxy:
      remote_url: https://registry.npmjs.org
      metadata_ttl: 30m
      auth:
        kind: bearer
        token: {valueFrom: {env: CAC_NPM_TOKEN}}
      http:
        proxy_name: corp
    routing:
      block: ["@acme/*"]
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-hosted
spec:
  format: npm
  type: hosted
  description: Internal packages
  blob_store: cac-store
  config:
    hosted:
      write_policy: allow_once
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: raw-hosted
spec:
  format: raw
  type: hosted
  blob_store: cac-store
  config:
    hosted: {}
---
apiVersion: hangar/v1
kind: BlobStore
metadata:
  name: cac-store
spec:
  kind: fs
  fs:
    path: STORE_PATH
  soft_quota_bytes: 10737418240
---
apiVersion: hangar/v1
kind: OutboundProxy
metadata:
  name: corp
spec:
  scheme: http
  host: proxy.corp.example
  port: 3128
  username: hangar
  password: {valueFrom: {env: CAC_PROXY_PASSWORD}}
  no_proxy: [.corp.example]
---
apiVersion: hangar/v1
kind: Role
metadata:
  name: ci-deployer
spec:
  description: CI pipelines
  privileges: ["repo:raw-*:write", "repo:npm-hosted:write", "repo:*:read"]
---
apiVersion: hangar/v1
kind: CleanupPolicy
metadata:
  name: raw-old
spec:
  description: Old raw files
  criteria:
    older_than_days: 365
    keep_newest: 3
  repositories: [raw-hosted]
  schedule: "0 3 * * *"
`

var secretEnv = []string{"CAC_NPM_TOKEN=" + npmToken, "CAC_PROXY_PASSWORD=" + proxyPassword}

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	content = strings.ReplaceAll(content, "STORE_PATH", filepath.Join(dir, "store"))
	p := filepath.Join(dir, "platform.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func expectCode(t *testing.T, r result, code int, what string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", what, r.code, code, r.stdout, r.stderr)
	}
	for _, secret := range []string{npmToken, proxyPassword} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Fatalf("%s printed a secret:\n%s%s", what, r.stdout, r.stderr)
		}
	}
}

func expectOutput(t *testing.T, r result, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(r.stdout, w) {
			t.Fatalf("output lacks %q:\n%s\nstderr:\n%s", w, r.stdout, r.stderr)
		}
	}
}

// TestApplyLifecycle applies a configuration, applies it again (no
// changes), changes a field, prunes and checks that export reproduces it.
func TestApplyLifecycle(t *testing.T) {
	h := server(t)
	dir := t.TempDir()
	file := writeConfig(t, dir, platform)
	set := []string{"--apply-set", "platform"}

	// Not applied yet: diff reports changes with exit status 2.
	r := h.cli(t, secretEnv, append([]string{"diff", "-f", file}, set...)...)
	expectCode(t, r, 2, "first diff")
	expectOutput(t, r, "+ Repository/npm-outer create", "Plan: 9 to create, 0 to update, 0 to delete, 0 unchanged.",
		// The policy's repository does not exist yet, so it cannot be previewed.
		`preview not available: 400 invalid_argument: repository "raw-hosted" does not exist`)

	// A missing secret fails before anything is sent.
	r = h.cli(t, nil, append([]string{"apply", "-f", file}, set...)...)
	expectCode(t, r, 1, "apply without secrets")
	if !strings.Contains(r.stderr, "CAC_NPM_TOKEN is not set") {
		t.Fatalf("stderr = %s", r.stderr)
	}

	r = h.cli(t, secretEnv, append([]string{"apply", "-f", file}, set...)...)
	expectCode(t, r, 0, "first apply")
	expectOutput(t, r, "Applied 9 changes.")
	// Dependencies first: proxy and store, then members, then groups inside out.
	order := []string{"OutboundProxy/corp created", "BlobStore/cac-store created", "Role/ci-deployer created",
		"Repository/npm-hosted created", "Repository/npm-all created", "Repository/npm-outer created", "CleanupPolicy/raw-old created"}
	last := -1
	for _, o := range order {
		i := strings.Index(r.stdout, o)
		if i < last {
			t.Fatalf("%q out of order:\n%s", o, r.stdout)
		}
		last = i
	}
	if strings.Contains(r.stdout, "warning:") {
		t.Fatalf("apply warned:\n%s", r.stdout)
	}

	// The second apply changes nothing; diff agrees.
	r = h.cli(t, secretEnv, append([]string{"apply", "-f", file}, set...)...)
	expectCode(t, r, 0, "second apply")
	expectOutput(t, r, "Plan: 0 to create, 0 to update, 0 to delete, 9 unchanged.")
	r = h.cli(t, secretEnv, append([]string{"diff", "-f", file}, set...)...)
	expectCode(t, r, 0, "diff after apply")

	// The server stored the secrets: the proxy has its password.
	b, status, err := h.basic(http.MethodGet, "/api/v1/proxies/corp", nil)
	if err != nil || status != 200 || !strings.Contains(string(b), `"password_configured":true`) {
		t.Fatalf("proxy = %d %s %v", status, b, err)
	}

	// Changing a field updates only that resource.
	changed := strings.Replace(platform, "metadata_ttl: 30m", "metadata_ttl: 45m", 1)
	changed = strings.Replace(changed, "team: web", "team: frontend", 1)
	writeConfig(t, dir, changed)
	r = h.cli(t, secretEnv, append([]string{"diff", "-f", file}, set...)...)
	expectCode(t, r, 2, "diff after a change")
	expectOutput(t, r, "~ Repository/npm-proxy update", `config.proxy.metadata_ttl: "30m0s" -> "45m"`,
		`labels.team: "web" -> "frontend"`, "Plan: 0 to create, 2 to update, 0 to delete, 7 unchanged.")
	r = h.cli(t, secretEnv, append([]string{"apply", "--dry-run", "-f", file}, set...)...)
	expectCode(t, r, 2, "dry run")
	r = h.cli(t, secretEnv, append([]string{"apply", "-f", file}, set...)...)
	expectCode(t, r, 0, "apply the change")
	expectOutput(t, r, "Repository/npm-proxy updated", "Applied 2 changes.")
	r = h.cli(t, secretEnv, append([]string{"diff", "-f", file}, set...)...)
	expectCode(t, r, 0, "diff after the change")

	// A server validation error names the resource and the field.
	bad := strings.Replace(changed, "older_than_days: 365", "older_than_days: 0", 1)
	writeConfig(t, dir, bad)
	r = h.cli(t, secretEnv, append([]string{"apply", "-f", file}, set...)...)
	expectCode(t, r, 1, "apply an invalid policy")
	expectOutput(t, r, "! CleanupPolicy/raw-old update failed: 400", "older_than_days")
	writeConfig(t, dir, changed)

	testPrune(t, h, dir, changed)
	testExportRoundTrip(t, h, dir)
}

func testPrune(t *testing.T, h *harness, dir, config string) {
	// A repository created by hand carries no ownership labels: no prune
	// ever deletes it.
	if b, status, err := h.basic(http.MethodPost, "/api/v1/repositories", map[string]any{
		"name": "manual-raw", "format": "raw", "type": "hosted", "config": map[string]any{"hosted": map[string]any{}}}); err != nil || status != 201 {
		t.Fatalf("create manual-raw: %d %s %v", status, b, err)
	}
	// Drop the role, the policy and raw-hosted from the files.
	docs := strings.Split(config, "\n---\n")
	var kept []string
	for _, d := range docs {
		if strings.Contains(d, "name: ci-deployer") || strings.Contains(d, "name: raw-old") || strings.Contains(d, "name: raw-hosted") {
			continue
		}
		kept = append(kept, d)
	}
	file := writeConfig(t, dir, strings.Join(kept, "\n---\n"))

	// --prune insists on an explicit apply set.
	r := h.cli(t, secretEnv, "apply", "--prune", "-f", file)
	expectCode(t, r, 1, "prune without --apply-set")
	if !strings.Contains(r.stderr, "--prune needs an explicit --apply-set") {
		t.Fatalf("stderr = %s", r.stderr)
	}
	// Another apply set owns nothing here: it may neither prune nor adopt.
	r = h.cli(t, secretEnv, "diff", "--prune", "--apply-set", "other", "-f", file)
	expectCode(t, r, 1, "diff of another apply set")
	if !strings.Contains(r.stderr, `owned by apply set "platform", not "other"`) {
		t.Fatalf("stderr = %s", r.stderr)
	}

	r = h.cli(t, secretEnv, "diff", "--prune", "--apply-set", "platform", "-f", file)
	expectCode(t, r, 2, "prune diff")
	expectOutput(t, r, "- Repository/raw-hosted delete", "- Role/ci-deployer delete", "- CleanupPolicy/raw-old delete",
		"Plan: 0 to create, 0 to update, 3 to delete")
	if strings.Contains(r.stdout, "manual-raw") || strings.Contains(r.stdout, "Repository/default") {
		t.Fatalf("prune would touch unmanaged resources:\n%s", r.stdout)
	}
	r = h.cli(t, secretEnv, "apply", "--prune", "--apply-set", "platform", "-f", file)
	expectCode(t, r, 0, "prune")
	// The policy goes before the repository it cleans.
	if i, j := strings.Index(r.stdout, "CleanupPolicy/raw-old deleted"), strings.Index(r.stdout, "Repository/raw-hosted deleted"); i < 0 || j < i {
		t.Fatalf("prune order:\n%s", r.stdout)
	}
	for _, path := range []string{"/api/v1/repositories/raw-hosted", "/api/v1/roles/ci-deployer", "/api/v1/cleanup-policies/raw-old"} {
		if _, status, _ := h.basic(http.MethodGet, path, nil); status != http.StatusNotFound {
			t.Fatalf("GET %s = %d after prune", path, status)
		}
	}
	if _, status, _ := h.basic(http.MethodGet, "/api/v1/repositories/manual-raw", nil); status != 200 {
		t.Fatalf("manual-raw = %d after prune", status)
	}
	r = h.cli(t, secretEnv, "diff", "--prune", "--apply-set", "platform", "-f", file)
	expectCode(t, r, 0, "diff after prune")

	// A resource created by hand under a pruned name is not the pruned one.
	if b, status, err := h.basic(http.MethodPost, "/api/v1/repositories", map[string]any{
		"name": "raw-hosted", "format": "raw", "type": "hosted", "config": map[string]any{"hosted": map[string]any{}}}); err != nil || status != 201 {
		t.Fatalf("recreate raw-hosted: %d %s %v", status, b, err)
	}
	r = h.cli(t, secretEnv, "diff", "--prune", "--apply-set", "platform", "-f", file)
	expectCode(t, r, 0, "diff with a recreated name")
}

func testExportRoundTrip(t *testing.T, h *harness, dir string) {
	out := filepath.Join(dir, "export.yaml")
	r := h.cli(t, nil, "export", "--apply-set", "platform", "-o", out)
	expectCode(t, r, 0, "export")
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	exported := string(b)
	for _, want := range []string{"kind: OutboundProxy", "kind: BlobStore", "name: npm-outer", "team: frontend",
		"valueFrom:", "env: HANGAR_SECRET_REPOSITORY_NPM_PROXY_AUTH_TOKEN", "env: HANGAR_SECRET_PROXY_CORP_PASSWORD"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("export lacks %q:\n%s", want, exported)
		}
	}
	for _, bad := range []string{npmToken, proxyPassword, "manual-raw", "hangar/apply-set", "kind: Role"} {
		if strings.Contains(exported, bad) {
			t.Fatalf("export contains %q:\n%s", bad, exported)
		}
	}
	env := []string{"HANGAR_SECRET_REPOSITORY_NPM_PROXY_AUTH_TOKEN=" + npmToken, "HANGAR_SECRET_PROXY_CORP_PASSWORD=" + proxyPassword}
	r = h.cli(t, env, "diff", "--prune", "--apply-set", "platform", "-f", out)
	expectCode(t, r, 0, "diff of the export")

	// export -> edit -> apply
	edited := strings.Replace(exported, "description: Internal packages", "description: Internal npm packages", 1)
	if err := os.WriteFile(out, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.cli(t, env, "apply", "--apply-set", "platform", "-f", out)
	expectCode(t, r, 0, "apply the edited export")
	expectOutput(t, r, `description: "Internal packages" -> "Internal npm packages"`, "Applied 1 change.")

	// The whole server exports without built-in roles or the configured store.
	r = h.cli(t, nil, "export")
	expectCode(t, r, 0, "full export")
	if !strings.Contains(r.stdout, "name: manual-raw") || strings.Contains(r.stdout, "name: Admin") {
		t.Fatalf("full export:\n%s", r.stdout)
	}
}
