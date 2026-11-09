package trivy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
)

const (
	token  = "adapter-token-0123456789"
	digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	report = `{"SchemaVersion":2,"ArtifactName":"x","Metadata":{"OS":{"Family":"debian","Name":"12.5"}},"Results":[
		{"Target":"x (debian 12.5)","Class":"os-pkgs","Type":"debian","Vulnerabilities":[{"VulnerabilityID":"CVE-2024-1",
		"PkgName":"libc6","InstalledVersion":"2.36-9","FixedVersion":"2.36-9+deb12u4","Severity":"HIGH","SeveritySource":"debian",
		"CVSS":{"nvd":{"V3Score":7.5,"V3Vector":"CVSS:3.1/AV:N"},"redhat":{"V40Score":8.1,"V40Vector":"CVSS:4.0/AV:N"}}}]}]}`
)

type recorder struct {
	mu    sync.Mutex
	args  [][]string
	envs  [][]string
	out   string
	fail  string
	block chan struct{}
	busy  atomic.Int32
	peak  atomic.Int32
}

func (r *recorder) run(ctx context.Context, args, env []string, stdout, stderr io.Writer) error {
	n := r.busy.Add(1)
	defer r.busy.Add(-1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	r.mu.Lock()
	r.args, r.envs = append(r.args, args), append(r.envs, env)
	block, fail, out := r.block, r.fail, r.out
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail != "" {
		_, _ = io.WriteString(stderr, "2026-11-09T10:00:00Z\tINFO\tstarting\n2026-11-09T10:00:01Z\tFATAL\t"+fail+"\n")
		return errors.New("exit status 1")
	}
	if len(args) > 0 && args[0] == "--version" {
		_, _ = io.WriteString(stdout, "Version: 0.76.1\nVulnerability DB:\n")
		return nil
	}
	_, err := io.WriteString(stdout, out)
	return err
}

func newAdapter(t *testing.T, r *recorder, concurrency int) *httptest.Server {
	t.Helper()
	a, err := trivy.NewAdapter(trivy.AdapterOptions{Token: token, Run: r.run, CacheDir: "/cache", Concurrency: concurrency,
		Env: []string{"HTTPS_PROXY=http://proxy:3128", "TRIVY_PASSWORD=leak", "HANGAR_SCANNER_TOKEN=" + token, "TRIVY_JAVA_DB_REPOSITORY=mirror.example.com/java-db"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, auth string, body any) (*http.Response, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+trivy.ScanPath, strings.NewReader(string(b)))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestAdapterRunsTrivyClient(t *testing.T) {
	r := &recorder{out: report}
	srv := newAdapter(t, r, 1)
	req := trivy.ScanRequest{Image: "hangar:8080/docker-hosted/team/app@" + digest, RegistryToken: "reg-token",
		Insecure: true, Server: "http://trivy:4954", ServerToken: "srv-token", TimeoutSeconds: 120}
	resp, body := post(t, srv.URL, token, req)
	if resp.StatusCode != http.StatusOK || body != report {
		t.Fatalf("answer %d %s", resp.StatusCode, body)
	}
	args := strings.Join(r.args[0], " ")
	want := "image --server http://trivy:4954 --image-src remote --scanners vuln --format json --list-all-pkgs=false --quiet " +
		"--timeout 120s --cache-dir /cache --insecure hangar:8080/docker-hosted/team/app@" + digest
	if args != want {
		t.Fatalf("args\n%s\nwant\n%s", args, want)
	}
	env := strings.Join(r.envs[0], "\n")
	for _, w := range []string{"HTTPS_PROXY=http://proxy:3128", "TRIVY_REGISTRY_TOKEN=reg-token", "TRIVY_TOKEN=srv-token",
		"TRIVY_DISABLE_TELEMETRY=true", "TRIVY_JAVA_DB_REPOSITORY=mirror.example.com/java-db"} {
		if !strings.Contains(env, w) {
			t.Fatalf("env lacks %s:\n%s", w, env)
		}
	}
	// Inherited credentials never reach Trivy; neither does the adapter token.
	if strings.Contains(env, "TRIVY_PASSWORD") || strings.Contains(env, token) {
		t.Fatalf("env leaks:\n%s", env)
	}
}

func TestAdapterRejects(t *testing.T) {
	r := &recorder{out: report}
	srv := newAdapter(t, r, 1)
	ok := trivy.ScanRequest{Image: "hangar:8080/docker-hosted/app@" + digest, RegistryToken: "t", Server: "http://trivy:4954"}
	if resp, _ := post(t, srv.URL, "", ok); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv.URL, "wrong-token-0123456789", ok); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	for _, image := range []string{
		"hangar:8080/docker-hosted/app:1.0",                  // a tag, not a digest
		"--config=/etc/passwd/x@" + digest,                   // an option
		"hangar:8080/docker-hosted/app@sha256:abc",           // short digest
		"hangar:8080/Docker/app@" + digest,                   // upper case path
		"hangar:8080/app@" + digest + " --format=table",      // extra words
		"http://hangar:8080/docker-hosted/app@" + digest,     // a URL
		"hangar:8080/docker-hosted/app@" + digest + "\nfoo",  // newline
		"hangar:8080/docker-hosted/../app@" + digest,         // dots
		"hangar:8080/docker-hosted/app/@" + digest,           // empty component
		"hangar:8080/docker-hosted/app@sha512:" + digest[7:], // other algorithm
	} {
		bad := ok
		bad.Image = image
		if resp, body := post(t, srv.URL, token, bad); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("image %q: %d %s", image, resp.StatusCode, body)
		}
	}
	bad := ok
	bad.Server = "file:///etc"
	if resp, _ := post(t, srv.URL, token, bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("server: %d", resp.StatusCode)
	}
	bad = ok
	bad.RegistryToken = ""
	if resp, _ := post(t, srv.URL, token, bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no registry token: %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv.URL, token, map[string]any{"image": ok.Image, "args": []string{"--x"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}
	if len(r.args) != 0 {
		t.Fatalf("trivy ran for a rejected request: %v", r.args)
	}
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestAdapterFailureAndConcurrency(t *testing.T) {
	r := &recorder{fail: "unable to pull image: token reg-secret rejected"}
	srv := newAdapter(t, r, 1)
	req := trivy.ScanRequest{Image: "hangar:8080/docker-hosted/app@" + digest, RegistryToken: "reg-secret", Server: "http://trivy:4954"}
	resp, body := post(t, srv.URL, token, req)
	var e trivy.ErrorResponse
	_ = json.Unmarshal([]byte(body), &e)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(e.Error, "unable to pull image") ||
		strings.Contains(e.Error, "reg-secret") || strings.Contains(e.Error, "starting") {
		t.Fatalf("failure = %d %q", resp.StatusCode, e.Error)
	}

	// One scan at a time: the second waits for the first.
	r2 := &recorder{out: report, block: make(chan struct{})}
	srv2 := newAdapter(t, r2, 1)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := post(t, srv2.URL, token, req)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(r2.block)
	wg.Wait()
	if p := r2.peak.Load(); p != 1 {
		t.Fatalf("peak concurrency %d", p)
	}
	if v, err := trivy.Check(context.Background(), r2.run, nil); err != nil || v != "Version: 0.76.1" {
		t.Fatalf("check = %q, %v", v, err)
	}
}

func TestClientAgainstAdapter(t *testing.T) {
	r := &recorder{out: report}
	srv := newAdapter(t, r, 2)
	version := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Version":"0.76.1","VulnerabilityDB":{"Version":2,"UpdatedAt":"2026-11-09T06:00:00Z","NextUpdate":"2026-11-10T06:00:00Z"}}`)
	}))
	defer version.Close()
	c, err := trivy.New(trivy.Options{ServerURL: version.URL + "/", ServerToken: "s", AdapterURL: srv.URL, AdapterToken: token, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.Scan(context.Background(), "hangar:8080/docker-hosted/app@"+digest, "reg", false)
	if err != nil {
		t.Fatal(err)
	}
	v := rep.Results[0].Vulnerabilities[0]
	score, vector := v.Rating()
	// The severity source (debian) has no score: NVD's v3 is taken.
	if rep.Metadata.OS.String() != "debian 12.5" || v.Level() != "high" || score == nil || *score != 7.5 || vector != "CVSS:3.1/AV:N" {
		t.Fatalf("report = %+v score %v %s", rep, score, vector)
	}
	if !strings.Contains(strings.Join(r.args[0], " "), "--server "+version.URL+" ") || strings.Contains(strings.Join(r.args[0], " "), "--insecure") {
		t.Fatalf("args = %v", r.args[0])
	}
	info, err := c.Version(context.Background())
	if err != nil || info.Version != "0.76.1" || info.VulnerabilityDB == nil || info.VulnerabilityDB.UpdatedAt == nil {
		t.Fatalf("version = %+v, %v", info, err)
	}

	// Adapter errors and broken reports surface as errors.
	r.mu.Lock()
	r.fail = "unknown image"
	r.mu.Unlock()
	if _, err := c.Scan(context.Background(), "hangar:8080/docker-hosted/app@"+digest, "reg", false); err == nil ||
		!strings.Contains(err.Error(), "unknown image") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %v", err)
	}
	if _, err := trivy.ParseReport([]byte(`{"SchemaVersion":1}`)); err == nil {
		t.Fatal("schema 1 accepted")
	}
	bad, _ := trivy.New(trivy.Options{ServerURL: version.URL, AdapterURL: srv.URL, AdapterToken: "wrong-token-0123456789"})
	if _, err := bad.Scan(context.Background(), "hangar:8080/docker-hosted/app@"+digest, "reg", false); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong adapter token = %v", err)
	}
	for _, o := range []trivy.Options{
		{AdapterURL: srv.URL, AdapterToken: token},
		{ServerURL: version.URL, AdapterToken: token},
		{ServerURL: version.URL, AdapterURL: srv.URL},
		{ServerURL: "trivy:4954", AdapterURL: srv.URL, AdapterToken: token},
	} {
		if _, err := trivy.New(o); err == nil {
			t.Fatalf("options %+v accepted", o)
		}
	}
}

func TestRatingAndFixedVersions(t *testing.T) {
	v := trivy.Vulnerability{SeveritySource: "redhat", Severity: "critical", FixedVersion: "2.15.0, 2.12.2,",
		CVSS: map[string]trivy.CVSS{"nvd": {V3Score: 9.8}, "redhat": {V40Score: 9.3, V40Vector: "CVSS:4.0/X", V3Score: 8.1}}}
	if s, vec := v.Rating(); s == nil || *s != 9.3 || vec != "CVSS:4.0/X" {
		t.Fatalf("rating = %v %s", s, vec)
	}
	if v.Level() != "critical" || len(v.FixedVersions()) != 2 {
		t.Fatalf("level %s fixed %v", v.Level(), v.FixedVersions())
	}
	none := trivy.Vulnerability{Severity: "UNKNOWN", CVSS: map[string]trivy.CVSS{"ghsa": {V2Score: 5}}}
	if s, _ := none.Rating(); s == nil || *s != 5 || none.Level() != "unknown" {
		t.Fatalf("fallback rating %v", s)
	}
	if s, _ := (&trivy.Vulnerability{}).Rating(); s != nil {
		t.Fatal("rating without CVSS")
	}
}
