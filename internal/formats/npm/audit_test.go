package npm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// fakeScanner stands in for the scanner's stored results: every stored
// component is scanned ("ok") unless status says otherwise, with the
// findings set for "name@version".
type fakeScanner struct {
	ts       *testServer
	disabled bool
	mu       sync.Mutex
	status   map[string]string
	findings map[string][]*scanning.Finding
}

func newFakeScanner(ts *testServer) *fakeScanner {
	f := &fakeScanner{ts: ts, status: map[string]string{}, findings: map[string][]*scanning.Finding{}}
	ts.format.SetAdvisories(f)
	return f
}

func (f *fakeScanner) Enabled() bool { return !f.disabled }

func (f *fakeScanner) VersionFindings(ctx context.Context, repoIDs []string, keys []scanning.ComponentKey) ([]*scanning.VersionReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*scanning.VersionReport
	for _, id := range repoIDs {
		for _, k := range keys {
			c, err := f.ts.env.Components.GetComponent(ctx, id, k.Namespace, k.Name, k.Version)
			if err != nil {
				continue
			}
			full := k.Name
			if k.Namespace != "" {
				full = k.Namespace + "/" + k.Name
			}
			st, ok := f.status[full+"@"+k.Version]
			if !ok {
				st = "ok"
			}
			out = append(out, &scanning.VersionReport{ComponentKey: k, RepositoryID: id, ComponentID: c.ID, ScanStatus: st,
				Findings: f.findings[full+"@"+k.Version]})
		}
	}
	return out, nil
}

// lodashAdvisories is the public registry's bulk answer for lodash 4.17.20.
const lodashAdvisories = `{"lodash":[
{"id":1106913,"url":"https://github.com/advisories/GHSA-35jh-r3h4-6jhm","title":"Command Injection in lodash","severity":"high","vulnerable_versions":"<4.17.21","cwe":["CWE-77","CWE-94"],"cvss":{"score":7.2,"vectorString":"CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:U/C:H/I:H/A:H"}},
{"id":1106914,"url":"https://github.com/advisories/GHSA-29mw-wpgm-hmr9","title":"Regular Expression Denial of Service (ReDoS) in lodash","severity":"moderate","vulnerable_versions":">=4.0.0 <4.17.21","cwe":["CWE-400","CWE-1333"],"cvss":{"score":5.3,"vectorString":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L"}}]}`

// auditRemote is a public-registry stand-in: lodash 4.17.20 (packument and
// tarball) and the bulk advisory endpoint, which records what it was asked.
type auditRemote struct {
	t      *testing.T
	srv    *httptest.Server
	tgz    []byte
	status int
	mu     sync.Mutex
	asked  []map[string][]string
}

func newAuditRemote(t *testing.T) *auditRemote {
	a := &auditRemote{t: t, tgz: makeTarball(t, "lodash", "4.17.20")}
	a.srv = httptest.NewServer(a)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *auditRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/-/npm/v1/security/advisories/bulk":
		var body map[string][]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		a.asked = append(a.asked, body)
		status := a.status
		a.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if _, ok := body["lodash"]; !ok {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, lodashAdvisories)
	case r.URL.Path == "/lodash":
		s1 := sha1Hex(a.tgz)
		_, _ = io.WriteString(w, `{"name":"lodash","dist-tags":{"latest":"4.17.20"},"versions":{"4.17.20":{"name":"lodash","version":"4.17.20",
"dist":{"shasum":"`+s1+`","tarball":"`+a.srv.URL+`/lodash/-/lodash-4.17.20.tgz"}}}}`)
	case r.URL.Path == "/lodash/-/lodash-4.17.20.tgz":
		_, _ = w.Write(a.tgz)
	default:
		http.NotFound(w, r)
	}
}

func (a *auditRemote) lastAsked() map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 {
		return nil
	}
	return a.asked[len(a.asked)-1]
}

// auditServer adds npm-public (a proxy of the remote) and npm-all (a group
// of npm-hosted and npm-public) to the test server.
func auditServer(t *testing.T) (*testServer, *auditRemote) {
	ts := newServer(t)
	remote := newAuditRemote(t)
	ts.repos["npm-public"] = ts.env.AddRepo(t, &core.Repository{Name: "npm-public", Format: npm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote.srv.URL},
			Routing: &core.RoutingRules{Block: []string{"@acme/*"}}}})
	ts.repos["npm-all"] = ts.env.AddRepo(t, &core.Repository{Name: "npm-all", Format: npm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-hosted", "npm-public"}, WriteMember: "npm-hosted"}}})
	return ts, remote
}

func postAudit(t *testing.T, ts *testServer, repo, endpoint, body string, gz bool) (int, []byte) {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	hdr := []string{"Content-Type", "application/json"}
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = io.WriteString(w, body)
		_ = w.Close()
		rd = &buf
		hdr = append(hdr, "Content-Encoding", "gzip")
	}
	resp := ts.do(t, http.MethodPost, "/repository/"+repo+"/-/npm/v1/security/"+endpoint, "alice", rd, hdr...)
	return resp.StatusCode, readAll(t, resp)
}

func bulkAnswer(t *testing.T, b []byte) map[string][]map[string]any {
	t.Helper()
	var m map[string][]map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bulk answer %s: %v", b, err)
	}
	return m
}

// Regression (audit always "0 vulnerabilities"): a proxy passes the audit
// to its remote, without the names its routing rules keep from it; a
// remote that cannot answer fails the audit instead of reporting nothing.
func TestAuditThroughProxy(t *testing.T) {
	ts, remote := auditServer(t)
	code, b := postAudit(t, ts, "npm-public", "advisories/bulk", `{"lodash":["4.17.20"],"@acme/secret":["1.0.0"]}`, true)
	if code != http.StatusOK {
		t.Fatalf("bulk = %d %s", code, b)
	}
	got := bulkAnswer(t, b)
	if len(got["lodash"]) != 2 || got["lodash"][0]["severity"] != "high" || got["lodash"][0]["vulnerable_versions"] != "<4.17.21" {
		t.Fatalf("bulk answer = %s", b)
	}
	if asked := remote.lastAsked(); asked["@acme/secret"] != nil || asked["lodash"] == nil {
		t.Fatalf("remote was asked %v, want lodash only", asked)
	}
	// Quick audit (npm's fallback): a full report built from the same answer.
	tree := `{"name":"app","version":"1.0.0","requires":{"lodash":"^4.17.0"},"dependencies":{"lodash":{"version":"4.17.20"},
"wrapper":{"version":"1.0.0","dependencies":{"lodash":{"version":"4.17.21"}}}}}`
	code, b = postAudit(t, ts, "npm-public", "audits/quick", tree, false)
	if code != http.StatusOK {
		t.Fatalf("quick = %d %s", code, b)
	}
	var rep struct {
		Advisories map[string]struct {
			Module   string `json:"module_name"`
			Findings []struct {
				Version string   `json:"version"`
				Paths   []string `json:"paths"`
			} `json:"findings"`
		} `json:"advisories"`
		Metadata struct {
			Vulnerabilities map[string]int `json:"vulnerabilities"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Metadata.Vulnerabilities["high"] != 1 || rep.Metadata.Vulnerabilities["moderate"] != 1 || len(rep.Advisories) != 2 {
		t.Fatalf("quick report = %s", b)
	}
	for _, a := range rep.Advisories {
		if len(a.Findings) != 1 || a.Findings[0].Version != "4.17.20" || a.Findings[0].Paths[0] != "lodash" {
			t.Fatalf("findings = %+v (4.17.21 is fixed)", a.Findings)
		}
	}
	remote.mu.Lock()
	remote.status = http.StatusInternalServerError
	remote.mu.Unlock()
	if code, b := postAudit(t, ts, "npm-public", "advisories/bulk", `{"lodash":["4.17.20"]}`, false); code != http.StatusBadGateway {
		t.Fatalf("remote failing: %d %s, want 502", code, b)
	}
}

// Hosted packages are answered from Hangar's stored findings; without a
// scan result (scanning off, not scanned yet, scan failed) the audit fails
// with 503 rather than answering "no advisories".
func TestAuditHosted(t *testing.T) {
	ts, _ := auditServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.3", "latest")
	body := `{"@acme/ui":["1.0.0","1.0.3"],"@acme/elsewhere":["2.0.0"]}`

	if code, b := postAudit(t, ts, "npm-hosted", "advisories/bulk", body, false); code != http.StatusServiceUnavailable ||
		!strings.Contains(string(b), "scanning is turned off") {
		t.Fatalf("no scanner = %d %s", code, b)
	}
	sc := newFakeScanner(ts)
	sc.status["@acme/ui@1.0.3"] = ""
	if code, b := postAudit(t, ts, "npm-hosted", "advisories/bulk", body, false); code != http.StatusServiceUnavailable ||
		!strings.Contains(string(b), "@acme/ui@1.0.3 (not scanned yet)") {
		t.Fatalf("unscanned = %d %s", code, b)
	}
	delete(sc.status, "@acme/ui@1.0.3")
	cvss := 7.5
	sc.findings["@acme/ui@1.0.0"] = []*scanning.Finding{
		{AdvisoryID: "GHSA-nnnn-4444-5555", Title: "Prototype pollution", Severity: "medium", CVSS: &cvss,
			AffectedVersions: "<1.0.3", Status: "open"},
		{AdvisoryID: "OSV-2026-1", Title: "Accepted", Severity: "critical", AffectedVersions: "all versions", Status: "waived"},
	}
	code, b := postAudit(t, ts, "npm-hosted", "advisories/bulk", body, false)
	if code != http.StatusOK {
		t.Fatalf("scanned = %d %s", code, b)
	}
	got := bulkAnswer(t, b)["@acme/ui"]
	if id, _ := got[0]["id"].(float64); len(got) != 1 || id <= 0 || got[0]["severity"] != "moderate" ||
		got[0]["vulnerable_versions"] != "<1.0.3" || got[0]["url"] != "https://github.com/advisories/GHSA-nnnn-4444-5555" {
		t.Fatalf("hosted advisories = %s", b)
	}
	sc.disabled = true
	if code, _ := postAudit(t, ts, "npm-hosted", "advisories/bulk", `{"@acme/never-published":["1.0.0"]}`, false); code != http.StatusOK {
		t.Fatalf("audit of versions not stored with scanning off = %d, want 200 (nothing to vouch for)", code)
	}
}

// A group merges both answers: hosted findings for its hosted packages, the
// remote's advisories for the rest.
func TestAuditThroughGroup(t *testing.T) {
	ts, remote := auditServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	sc := newFakeScanner(ts)
	sc.findings["@acme/ui@1.0.0"] = []*scanning.Finding{{AdvisoryID: "GHSA-nnnn-4444-5555", Title: "Prototype pollution",
		Severity: "high", AffectedVersions: ">=0, <1.0.3", Status: "open"}}
	code, b := postAudit(t, ts, "npm-all", "advisories/bulk", `{"@acme/ui":["1.0.0"],"lodash":["4.17.20"]}`, true)
	if code != http.StatusOK {
		t.Fatalf("group = %d %s", code, b)
	}
	got := bulkAnswer(t, b)
	if len(got["@acme/ui"]) != 1 || len(got["lodash"]) != 2 {
		t.Fatalf("group answer = %s", b)
	}
	if asked := remote.lastAsked(); asked["@acme/ui"] != nil {
		t.Fatalf("a name the routing rules keep private reached the remote: %v", asked)
	}
	if r := ts.do(t, http.MethodPost, "/repository/npm-all/-/npm/v1/security/advisories/bulk", "", strings.NewReader(`{}`)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous audit = %d", r.StatusCode)
	}
}

// TestRealNpmAudit runs npm audit through a group on a project that
// depends on a vulnerable lodash (from the remote) and a hosted package with
// a stored finding: npm reports both, fails --audit-level=high, and reports
// an unavailable audit when Hangar cannot vouch for a hosted version.
func TestRealNpmAudit(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts, _ := auditServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	sc := newFakeScanner(ts)
	sc.findings["@acme/ui@1.0.0"] = []*scanning.Finding{{AdvisoryID: "GHSA-nnnn-4444-5555", Title: "Prototype pollution in @acme/ui",
		Severity: "critical", AffectedVersions: "<1.0.3", Status: "open"}}
	c := newClient(t, ts, ts.env.Auth.AddToken("alice"))
	group := ts.registry("npm-all")
	dir := writeProject(t, map[string]string{"lodash": "4.17.20", "@acme/ui": "1.0.0"})
	c.must(t, dir, npmBin, "install", "--registry", group, "--no-audit")

	out, err := c.run(t, dir, npmBin, "audit", "--json", "--registry", group)
	if err == nil {
		t.Fatalf("npm audit exited 0 with vulnerable dependencies:\n%s", out)
	}
	var rep struct {
		Vulnerabilities map[string]struct {
			Severity string `json:"severity"`
		} `json:"vulnerabilities"`
		Metadata struct {
			Vulnerabilities map[string]int `json:"vulnerabilities"`
		} `json:"metadata"`
	}
	i := strings.Index(out, "{")
	if i < 0 {
		t.Fatalf("npm audit --json printed no JSON:\n%s", out)
	}
	if err := json.Unmarshal([]byte(out[i:]), &rep); err != nil {
		t.Fatalf("npm audit --json: %v\n%s", err, out)
	}
	if rep.Vulnerabilities["lodash"].Severity != "high" || rep.Vulnerabilities["@acme/ui"].Severity != "critical" ||
		rep.Metadata.Vulnerabilities["critical"] != 1 || rep.Metadata.Vulnerabilities["high"] != 1 {
		t.Fatalf("npm audit = %s", out)
	}
	if out, err := c.run(t, dir, npmBin, "audit", "--audit-level=high", "--registry", group); err == nil || strings.Contains(out, "found 0 vulnerabilities") {
		t.Fatalf("--audit-level=high passed:\n%s", out)
	}

	if pnpm, err := exec.LookPath("pnpm"); err == nil {
		pdir := writeProject(t, map[string]string{"lodash": "4.17.20", "@acme/ui": "1.0.0"})
		c.must(t, pdir, pnpm, "install", "--registry", group, "--store-dir", t.TempDir())
		out, err := c.run(t, pdir, pnpm, "audit", "--registry", group)
		if err == nil || !strings.Contains(out, "critical") || !strings.Contains(out, "GHSA-35jh-r3h4-6jhm") {
			t.Fatalf("pnpm audit:\n%s", out)
		}
	}

	// Not scanned: Hangar refuses the audit, and npm 11 says the audit
	// failed. npm 10 and earlier swallow audit endpoint errors and print
	// "found 0 vulnerabilities" whatever the registry answers.
	sc.mu.Lock()
	sc.status["@acme/ui@1.0.0"] = ""
	sc.mu.Unlock()
	if r := ts.do(t, http.MethodPost, "/repository/npm-all/-/npm/v1/security/advisories/bulk", "alice",
		strings.NewReader(`{"@acme/ui":["1.0.0"]}`)); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("bulk audit of an unscanned hosted version = %d, want 503", r.StatusCode)
	}
	if npmMajor(t, npmBin) < 11 {
		t.Logf("npm %d hides audit endpoint errors; the 503 above is the check", npmMajor(t, npmBin))
		return
	}
	out, err = c.run(t, dir, npmBin, "audit", "--registry", group)
	if err == nil || strings.Contains(out, "found 0 vulnerabilities") || !strings.Contains(out, "audit endpoint returned an error") {
		t.Fatalf("npm audit with an unscanned hosted version:\n%s", out)
	}
}

// npmMajor is the major version of the npm client at bin.
func npmMajor(t *testing.T, bin string) int {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("npm --version: %v", err)
	}
	major, _, _ := strings.Cut(strings.TrimSpace(string(out)), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("npm --version = %q", out)
	}
	return n
}

func sha1Hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}
