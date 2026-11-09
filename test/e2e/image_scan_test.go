//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Image scanning with a real Trivy server: a known-vulnerable image
// (alpine:3.10, end of life since 2021) is copied into a hosted docker
// repository with scan on arrival, once as its multi-platform index and
// once as its linux/amd64 image, and the findings must appear.
//
// With HANGAR_E2E_IMAGE_SCAN_STACK=1 the test builds and starts
// testdata/image-scan/compose.yaml itself (Hangar, PostgreSQL, a Trivy
// server and the scan adapter; Docker with the compose plugin and internet
// access for the Trivy database and Docker Hub) and removes it afterwards
// (HANGAR_E2E_KEEP=1 keeps it):
//
//	HANGAR_E2E_IMAGE_SCAN_STACK=1 go test -tags e2e -run TestImageScan -timeout 60m -v ./test/e2e/
//
// Against a running Hangar whose scanner.* settings point at a Trivy server
// and adapter instead:
//
//	HANGAR_E2E_URL=http://127.0.0.1:8080 HANGAR_E2E_USER=admin HANGAR_E2E_PASSWORD=... \
//	  go test -tags e2e -run TestImageScan -timeout 60m -v ./test/e2e/
//
// HANGAR_E2E_VULN_IMAGE replaces docker.io/library/alpine:3.10 (e.g. a copy
// in an internal registry).
func TestImageScan(t *testing.T) {
	e := imageScanEnv(t)
	src := os.Getenv("HANGAR_E2E_VULN_IMAGE")
	if src == "" {
		src = "docker.io/library/alpine:3.10"
	}
	repo := fmt.Sprintf("scan-e2e-%d", time.Now().UnixNano()%1000000)
	e.api(t, http.MethodPost, "/api/v1/repositories", map[string]any{"name": repo, "format": "docker", "type": "hosted",
		"blob_store": "default", "config": map[string]any{"hosted": map[string]any{"write_policy": "allow"}, "scan_on_arrival": true}},
		http.StatusCreated)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	srcRef, err := name.ParseReference(src)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(srcRef, remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	var opts []name.Option
	if e.scheme == "http" {
		opts = append(opts, name.Insecure)
	}
	dst := func(tag string) name.Reference {
		r, err := name.ParseReference(e.host+"/"+repo+"/e2e/alpine:"+tag, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	auth := remote.WithAuth(&authn.Basic{Username: e.user, Password: e.pass})
	multi := desc.MediaType.IsIndex()
	if multi {
		idx, err := desc.ImageIndex()
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.WriteIndex(dst("3.10"), idx, remote.WithContext(ctx), auth); err != nil {
			t.Fatalf("push the index: %v", err)
		}
	}
	img, err := remote.Image(srcRef, remote.WithContext(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(dst("3.10-amd64"), img, remote.WithContext(ctx), auth); err != nil {
		t.Fatalf("push the image: %v", err)
	}

	single := e.componentID(t, repo, "3.10-amd64")
	findings := e.waitScanned(t, single)
	if len(findings) == 0 {
		t.Fatalf("%s has no findings", src)
	}
	var osPkgs int
	for _, f := range findings {
		if f.Source != "trivy" || f.Image == nil || f.Severity == "" || f.Installed == "" {
			t.Fatalf("finding = %+v", f)
		}
		if f.Image.Class == "os-pkgs" {
			osPkgs++
			if !strings.HasPrefix(f.Image.OS, "alpine 3.10") || f.Image.Layer == "" {
				t.Fatalf("OS package finding = %+v image %+v", f, f.Image)
			}
		}
		if len(f.Image.Platforms) != 0 {
			t.Fatalf("a single-platform image has platforms %v", f.Image.Platforms)
		}
	}
	if osPkgs == 0 {
		t.Fatal("no OS package findings")
	}
	t.Logf("%s (linux/amd64): %d findings", src, len(findings))

	if multi {
		id := e.componentID(t, repo, "3.10")
		list := e.waitScanned(t, id)
		platforms := map[string]bool{}
		for _, f := range list {
			for _, p := range f.Image.Platforms {
				platforms[p] = true
			}
		}
		if !platforms["linux/amd64"] || len(platforms) < 2 {
			t.Fatalf("platforms of the index findings = %v", platforms)
		}
		t.Logf("index: %d findings on %d platforms", len(list), len(platforms))
	}

	// On demand.
	var res struct {
		Scan struct {
			Status string `json:"status"`
		} `json:"scan"`
		Summary struct {
			Scanned  int `json:"scanned"`
			Findings int `json:"findings"`
		} `json:"summary"`
	}
	e.decode(t, e.api(t, http.MethodPost, "/api/v1/components/"+single+":scan", nil, http.StatusOK), &res)
	if res.Scan.Status != "ok" || res.Summary.Scanned != 1 || res.Summary.Findings != len(findings) {
		t.Fatalf("scan now = %+v", res)
	}

	// The fix plan has a row for the image, the overview counts the findings.
	var plan struct {
		Items []struct {
			Key      string          `json:"key"`
			Findings int             `json:"findings"`
			Image    json.RawMessage `json:"image"`
		} `json:"items"`
	}
	e.decode(t, e.api(t, http.MethodGet, "/api/v1/vulnerabilities/fix-plan?tab=all&repository="+repo, nil, http.StatusOK), &plan)
	if len(plan.Items) != 1 || plan.Items[0].Key != "docker:e2e/alpine" || string(plan.Items[0].Image) == "null" {
		t.Fatalf("fix plan = %+v", plan.Items)
	}
	var overview struct {
		Issues struct {
			OpenFindings int `json:"open_findings"`
		} `json:"issues"`
	}
	e.decode(t, e.api(t, http.MethodGet, "/api/v1/metrics/overview", nil, http.StatusOK), &overview)
	if overview.Issues.OpenFindings < len(findings) {
		t.Fatalf("overview open findings = %d, want at least %d", overview.Issues.OpenFindings, len(findings))
	}
	var status struct {
		Images struct {
			Enabled bool   `json:"enabled"`
			Version string `json:"version"`
		} `json:"images"`
	}
	e.decode(t, e.api(t, http.MethodGet, "/api/v1/scanning/status", nil, http.StatusOK), &status)
	if !status.Images.Enabled || status.Images.Version == "" {
		t.Fatalf("scanning status = %+v", status)
	}
}

type imageScanE2E struct {
	base, scheme, host string
	user, pass         string
}

type e2eFinding struct {
	AdvisoryID string `json:"advisory_id"`
	Package    string `json:"package"`
	Installed  string `json:"installed"`
	Severity   string `json:"severity"`
	Source     string `json:"source"`
	Location   string `json:"location"`
	Image      *struct {
		Class     string   `json:"class"`
		OS        string   `json:"os"`
		Layer     string   `json:"layer"`
		Platforms []string `json:"platforms"`
	} `json:"image"`
}

func imageScanEnv(t *testing.T) *imageScanE2E {
	t.Helper()
	e := &imageScanE2E{}
	if os.Getenv("HANGAR_E2E_IMAGE_SCAN_STACK") == "1" {
		port := os.Getenv("HANGAR_E2E_PORT")
		if port == "" {
			port = "18261"
		}
		compose := func(args ...string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", filepath.Join("testdata", "image-scan", "compose.yaml")}, args...)...)
			cmd.Env = append(os.Environ(), "HANGAR_E2E_PORT="+port)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		if os.Getenv("HANGAR_E2E_KEEP") != "1" {
			t.Cleanup(func() { compose("down", "--volumes", "--remove-orphans") })
		}
		compose("up", "-d", "--build", "--wait")
		e.base, e.user, e.pass = "http://localhost:"+port, "admin", "e2e-admin-password-0123"
	} else {
		e.base = baseURL(t)
		e.user, e.pass = dockerE2ECreds(t)
	}
	u, err := url.Parse(e.base)
	if err != nil {
		t.Fatal(err)
	}
	e.scheme, e.host = u.Scheme, u.Host
	return e
}

func (e *imageScanE2E) api(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(e.user, e.pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	return out
}

func (e *imageScanE2E) decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

// componentID finds the component of tag in repo.
func (e *imageScanE2E) componentID(t *testing.T, repo, tag string) string {
	t.Helper()
	var page struct {
		Items []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"items"`
	}
	e.decode(t, e.api(t, http.MethodGet, "/api/v1/components?repository="+repo+"&name=alpine&limit=100", nil, http.StatusOK), &page)
	for _, c := range page.Items {
		if c.Version == tag {
			return c.ID
		}
	}
	t.Fatalf("no component %s:%s in %+v", repo, tag, page.Items)
	return ""
}

// waitScanned waits for the arrival scan of a component and returns its findings.
func (e *imageScanE2E) waitScanned(t *testing.T, id string) []e2eFinding {
	t.Helper()
	deadline := time.Now().Add(20 * time.Minute)
	for {
		var out struct {
			Items []e2eFinding `json:"items"`
			Scan  struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"scan"`
		}
		e.decode(t, e.api(t, http.MethodGet, "/api/v1/components/"+id+"/findings?limit=500", nil, http.StatusOK), &out)
		switch out.Scan.Status {
		case "ok":
			return out.Items
		case "unsupported":
			t.Fatalf("component %s not scanned: %s", id, out.Scan.Error)
		case "failed":
			// Recorded and retried; show why if it does not recover.
			t.Logf("scan failed (retrying on demand): %s", out.Scan.Error)
			if time.Now().After(deadline) {
				t.Fatalf("scan of %s failed: %s", id, out.Scan.Error)
			}
			time.Sleep(10 * time.Second)
			e.api(t, http.MethodPost, "/api/v1/components/"+id+":scan", nil, http.StatusOK)
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("component %s was not scanned in time (status %q)", id, out.Scan.Status)
		}
		time.Sleep(5 * time.Second)
	}
}
