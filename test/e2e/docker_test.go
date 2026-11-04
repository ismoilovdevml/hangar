//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Docker / OCI acceptance test (docs/FORMATS.md, Docker / OCI): docker login,
// push and pull against hosted, group and proxy repositories, multi-arch
// docker buildx --push, and (when installed) crane copy, cosign sign/verify
// and helm push oci://.
//
// Needs HANGAR_E2E_URL and an administrator able to create repositories:
// HANGAR_E2E_USER / HANGAR_E2E_PASSWORD (the password may be a token). The
// Docker daemon must trust the registry host: use localhost, TLS, or list it
// under "insecure-registries". The Docker Hub proxy check is skipped when
// HANGAR_E2E_OFFLINE is set. Blob store: HANGAR_E2E_BLOB_STORE (default "default").

func dockerE2ECreds(t *testing.T) (string, string) {
	t.Helper()
	u, p := os.Getenv("HANGAR_E2E_USER"), os.Getenv("HANGAR_E2E_PASSWORD")
	if u == "" || p == "" {
		t.Skip("HANGAR_E2E_USER and HANGAR_E2E_PASSWORD not set")
	}
	return u, p
}

func dockerE2ERun(t *testing.T, dir string, stdin string, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+filepath.Join(dir, ".docker"))
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func dockerE2EMust(t *testing.T, dir, stdin, bin string, args ...string) string {
	t.Helper()
	out, err := dockerE2ERun(t, dir, stdin, bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return out
}

func dockerE2ECreateRepo(t *testing.T, base, user, pass string, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/repositories", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(resp.Body)
		t.Fatalf("create repository %v: %d %s", body["name"], resp.StatusCode, msg.String())
	}
}

func TestDocker(t *testing.T) {
	base := baseURL(t)
	user, pass := dockerE2ECreds(t)
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Host
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	sfx := fmt.Sprintf("%d", time.Now().Unix()%100000)
	hosted, hub, group := "docker-e2e-h"+sfx, "docker-e2e-hub"+sfx, "docker-e2e-g"+sfx
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": hosted, "format": "docker", "type": "hosted",
		"blob_store": store, "config": map[string]any{"hosted": map[string]any{"write_policy": "allow"}}})
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": hub, "format": "docker", "type": "proxy",
		"blob_store": store, "config": map[string]any{"proxy": map[string]any{"remote_url": "https://registry-1.docker.io", "metadata_ttl": "1h"}}})
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": group, "format": "docker", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, hub}, "write_member": hosted}}})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from hangar "+sfx+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// docker login (token flow) and push/pull to hosted.
	if out, err := dockerE2ERun(t, dir, "", dockerBin, "info"); err != nil {
		t.Skipf("docker daemon not available: %v\n%s", err, out)
	}
	out, err := dockerE2ERun(t, dir, pass, dockerBin, "login", host, "-u", user, "--password-stdin")
	if err != nil && strings.Contains(out, "connection refused") {
		// The daemon runs elsewhere (a VM or remote host) and cannot reach this address.
		t.Skipf("docker daemon cannot reach %s: %s", host, out)
	}
	if err != nil || !strings.Contains(out, "Login Succeeded") {
		t.Fatalf("login: %s", out)
	}
	img := host + "/" + hosted + "/e2e/hello:1"
	dockerE2EMust(t, dir, "", dockerBin, "build", "-t", img, ".")
	dockerE2EMust(t, dir, "", dockerBin, "push", img)
	dockerE2EMust(t, dir, "", dockerBin, "rmi", img)
	dockerE2EMust(t, dir, "", dockerBin, "pull", img)

	// Pull and push through the group.
	gimg := host + "/" + group + "/e2e/hello:1"
	dockerE2EMust(t, dir, "", dockerBin, "pull", gimg)
	dockerE2EMust(t, dir, "", dockerBin, "tag", img, host+"/"+group+"/e2e/via-group:1")
	dockerE2EMust(t, dir, "", dockerBin, "push", host+"/"+group+"/e2e/via-group:1")
	dockerE2EMust(t, dir, "", dockerBin, "pull", host+"/"+hosted+"/e2e/via-group:1")

	// Multi-arch buildx --push with a docker-container builder that trusts the registry.
	if _, err := dockerE2ERun(t, dir, "", dockerBin, "buildx", "version"); err == nil {
		cfg := filepath.Join(dir, "buildkitd.toml")
		toml := fmt.Sprintf("[registry.%q]\n  http = %t\n  insecure = true\n", host, u.Scheme == "http")
		if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		builder := "hangar-e2e-" + sfx
		dockerE2EMust(t, dir, "", dockerBin, "buildx", "create", "--name", builder, "--driver", "docker-container",
			"--driver-opt", "network=host", "--config", cfg)
		t.Cleanup(func() { _, _ = dockerE2ERun(t, dir, "", dockerBin, "buildx", "rm", "-f", builder) })
		multi := host + "/" + hosted + "/e2e/multi:1"
		dockerE2EMust(t, dir, "", dockerBin, "buildx", "build", "--builder", builder,
			"--platform", "linux/amd64,linux/arm64", "-t", multi, "--push", ".")
		out = dockerE2EMust(t, dir, "", dockerBin, "buildx", "imagetools", "inspect", multi)
		if !strings.Contains(out, "linux/amd64") || !strings.Contains(out, "linux/arm64") {
			t.Fatalf("multi-arch index: %s", out)
		}
	} else {
		t.Log("docker buildx not available; multi-arch push skipped")
	}

	// Docker Hub through the proxy (library/ prefix, token dance) and via the group.
	if os.Getenv("HANGAR_E2E_OFFLINE") == "" {
		dockerE2EMust(t, dir, "", dockerBin, "pull", host+"/"+hub+"/busybox:1.36")
		dockerE2EMust(t, dir, "", dockerBin, "pull", host+"/"+group+"/library/alpine:3.20")
	}

	// Optional clients.
	if crane, err := exec.LookPath("crane"); err == nil {
		insecure := []string{}
		if u.Scheme == "http" {
			insecure = append(insecure, "--insecure")
		}
		args := append([]string{"copy", img, host + "/" + hosted + "/e2e/copied:1"}, insecure...)
		dockerE2EMust(t, dir, "", crane, args...)
	}
	if cosign, err := exec.LookPath("cosign"); err == nil {
		t.Setenv("COSIGN_PASSWORD", "")
		dockerE2EMust(t, dir, "", cosign, "generate-key-pair")
		flags := []string{"--key", "cosign.key", "--yes", "--tlog-upload=false"}
		if u.Scheme == "http" {
			flags = append(flags, "--allow-insecure-registry", "--allow-http-registry")
		}
		dockerE2EMust(t, dir, "", cosign, append(append([]string{"sign"}, flags...), img)...)
		vflags := []string{"--key", "cosign.pub", "--insecure-ignore-tlog=true"}
		if u.Scheme == "http" {
			vflags = append(vflags, "--allow-insecure-registry", "--allow-http-registry")
		}
		dockerE2EMust(t, dir, "", cosign, append(append([]string{"verify"}, vflags...), img)...)
	}
	if helm, err := exec.LookPath("helm"); err == nil {
		dockerE2EMust(t, dir, "", helm, "create", "e2echart")
		dockerE2EMust(t, dir, "", helm, "package", "e2echart")
		login := []string{"registry", "login", host, "-u", user, "--password-stdin"}
		push := []string{"push", "e2echart-0.1.0.tgz", "oci://" + host + "/" + hosted + "/charts"}
		if u.Scheme == "http" {
			login = append(login, "--insecure")
			push = append(push, "--plain-http")
		}
		dockerE2EMust(t, dir, pass, helm, login...)
		dockerE2EMust(t, dir, "", helm, push...)
	}
}
