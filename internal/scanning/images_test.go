package scanning_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
)

const (
	indexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	amdDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	armDigest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	chartDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// fakeRegistry serves the manifests of a multi-platform index and a Helm
// chart, and hands out tokens per repository.
type fakeRegistry struct {
	mu     sync.Mutex
	tokens []string
}

func (r *fakeRegistry) Manifests(_ context.Context, repository, image, digest string) ([]scanning.ImageManifest, error) {
	switch digest {
	case indexDigest:
		// arm64 listed first: the scanner puts linux/amd64 first anyway.
		return []scanning.ImageManifest{{Digest: armDigest, Platform: "linux/arm64"}, {Digest: amdDigest, Platform: "linux/amd64"}}, nil
	case amdDigest:
		return []scanning.ImageManifest{{Digest: amdDigest}}, nil
	case chartDigest:
		return nil, fmt.Errorf("%w: a Helm chart: not a container image", scanning.ErrNotImage)
	}
	return nil, fmt.Errorf("manifest %s of %s/%s: %w", digest, repository, image, core.ErrNotFound)
}

func (r *fakeRegistry) PullToken(repository, image string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := "tok-" + repository + "-" + strings.ReplaceAll(image, "/", "-")
	r.tokens = append(r.tokens, t)
	return t, nil
}

// fakeTrivy is the Trivy client behind a real adapter: it answers with the
// fixture report per platform digest.
type fakeTrivy struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []fakeCall
	reports map[string][]byte // digest -> report
	fail    string            // non-empty: print this and exit 1
}

type fakeCall struct {
	args []string
	env  []string
}

func (f *fakeTrivy) run(_ context.Context, args, env []string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{args: args, env: env})
	fail := f.fail
	f.mu.Unlock()
	if fail != "" {
		_, _ = io.WriteString(stderr, "2026-11-09T10:00:00Z\tFATAL\t"+fail+"\n")
		return errors.New("exit status 1")
	}
	image := args[len(args)-1]
	_, digest, _ := strings.Cut(image, "@")
	f.mu.Lock()
	report, ok := f.reports[digest]
	f.mu.Unlock()
	if !ok {
		_, _ = io.WriteString(stderr, "FATAL\tunable to find the specified image\n")
		return errors.New("exit status 1")
	}
	_, err := stdout.Write(report)
	return err
}

func (f *fakeTrivy) set(digest string, report []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports[digest] = report
}

func (f *fakeTrivy) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fixture loads the report and lets edit change its decoded form.
func fixture(t *testing.T, digest string, edit func(*trivy.Report)) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/trivy-report.json")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "sha256:PLATFORM_DIGEST", digest))
	if edit == nil {
		return raw
	}
	var r trivy.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	edit(&r)
	out, _ := json.Marshal(r)
	return out
}

// withoutLang drops the language package results (the arm64 image has no jar).
func withoutLang(r *trivy.Report) {
	r.Results = slices.DeleteFunc(r.Results, func(res trivy.Result) bool { return res.Class == trivy.ClassLang })
}

type imageEnv struct {
	*env
	reg       *fakeRegistry
	trivy     *fakeTrivy
	serverURL string
	chart     string
}

func newImageEnv(t *testing.T, platforms string) *imageEnv {
	t.Helper()
	e := &imageEnv{env: newEnv(t), reg: &fakeRegistry{}}
	e.trivy = &fakeTrivy{t: t, reports: map[string][]byte{
		amdDigest: fixture(t, amdDigest, nil),
		armDigest: fixture(t, armDigest, withoutLang),
	}}
	adapter, err := trivy.NewAdapter(trivy.AdapterOptions{Token: "adapter-token-0123456789", Run: e.trivy.run, Env: []string{"HTTPS_PROXY=http://proxy.example.com:3128"}})
	if err != nil {
		t.Fatal(err)
	}
	adapterSrv := httptest.NewServer(adapter)
	t.Cleanup(adapterSrv.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Version":"0.76.1","VulnerabilityDB":{"Version":2,"UpdatedAt":"2026-11-09T06:00:00Z"}}`)
	}))
	t.Cleanup(server.Close)
	e.serverURL = server.URL
	client, err := trivy.New(trivy.Options{ServerURL: server.URL, AdapterURL: adapterSrv.URL, AdapterToken: "adapter-token-0123456789",
		Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = scanning.New(scanning.Options{Pool: e.pool, Advisories: nopAdvisories{}, Enabled: true, Events: e.events,
		Jobs: e.jobs, Audit: e.audit, Images: client, Registry: e.reg, RegistryHost: "hangar.test:8080", RegistryInsecure: true,
		ImagePlatforms: platforms})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE repositories SET config = '{"hosted":{},"scan_on_arrival":true,"quarantine_critical":true}' WHERE id = $1`, e.dockerRepo)
	e.exec(`UPDATE components SET attributes = jsonb_build_object('digest', $2::text, 'platforms', '["linux/arm64","linux/amd64"]'::jsonb) WHERE id = $1`,
		e.image, indexDigest)
	e.chart = e.seed.Component(e.dockerRepo, "charts", "web", "0.1.0", time.Now(), 0, "alice")
	e.exec(`UPDATE components SET attributes = jsonb_build_object('digest', $2::text) WHERE id = $1`, e.chart, chartDigest)
	return e
}

// nopAdvisories is an OSV database with no advisories.
type nopAdvisories struct{}

func (nopAdvisories) QueryBatch(_ context.Context, q []osv.Query) ([][]osv.VulnRef, error) {
	return make([][]osv.VulnRef, len(q)), nil
}

func (nopAdvisories) Vuln(context.Context, string) (*osv.Vuln, error) { return nil, core.ErrNotFound }

func (e *imageEnv) byPackage(componentID string) map[string]*scanning.Finding {
	e.t.Helper()
	l := e.findings(scanning.FindingQuery{ComponentID: componentID, Statuses: []string{"open", "assigned", "waived", "fixed"}})
	out := map[string]*scanning.Finding{}
	for _, f := range l.Items {
		full, err := e.svc.Finding(context.Background(), f.ID, nil)
		if err != nil {
			e.t.Fatal(err)
		}
		out[f.AdvisoryID+" "+f.Package] = full
	}
	return out
}

func TestImageScanMultiPlatform(t *testing.T) {
	e := newImageEnv(t, scanning.PlatformsAll)
	ctx := context.Background()

	res, err := e.svc.ScanComponent(ctx, e.image)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scan.Status != scanning.StatusOK || res.Summary.Scanned != 1 || res.Summary.Findings != 5 || res.Summary.New != 5 ||
		res.Summary.Quarantined != 1 {
		t.Fatalf("scan = %+v", res)
	}
	// One Trivy run per platform manifest, by digest, default platform first.
	if n := e.trivy.callCount(); n != 2 {
		t.Fatalf("trivy runs = %d", n)
	}
	first := e.trivy.calls[0]
	if img := first.args[len(first.args)-1]; img != "hangar.test:8080/docker-hosted/team/api@"+amdDigest {
		t.Fatalf("first image = %s", img)
	}
	args := strings.Join(first.args, " ")
	for _, want := range []string{"image --server " + e.serverURL, "--image-src remote", "--scanners vuln", "--format json", "--insecure"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q lack %q", args, want)
		}
	}
	env := strings.Join(first.env, "\n")
	if !strings.Contains(env, "TRIVY_REGISTRY_TOKEN=tok-docker-hosted-team-api") || !strings.Contains(env, "HTTPS_PROXY=http://proxy.example.com:3128") ||
		strings.Contains(args, "tok-docker-hosted") {
		t.Fatalf("env = %q", env)
	}

	got := e.byPackage(e.image)
	if len(got) != 5 {
		t.Fatalf("findings = %v", got)
	}
	apk := got["CVE-2021-36159 apk-tools"]
	if apk == nil || apk.Severity != "critical" || apk.CVSS == nil || *apk.CVSS != 9.1 || apk.Installed != "2.10.6-r0" ||
		apk.FixedIn == nil || *apk.FixedIn != "2.10.7-r0" || apk.Source != "trivy" || apk.Location != "" || apk.Image == nil ||
		apk.Image.Class != "os-pkgs" || apk.Image.OS != "alpine 3.10.9" || apk.Image.Type != "alpine" ||
		!slices.Equal(apk.Image.Platforms, []string{"linux/amd64", "linux/arm64"}) ||
		apk.Image.Layer != "sha256:396c31837116ac290458afcb928f68b6cc1c7bdd6963fc72f52f365a2a89c1b5" ||
		apk.References[0].URL != "https://avd.aquasec.com/nvd/cve-2021-36159" || apk.Details == "" || apk.AdvisoryPublishedAt == nil {
		t.Fatalf("apk-tools = %+v image %+v", apk, apk.Image)
	}
	// One advisory in two packages is two findings.
	if got["CVE-2021-3711 libssl1.1"] == nil || got["CVE-2021-3711 libcrypto1.1"] == nil {
		t.Fatal("CVE-2021-3711 not reported for both packages")
	}
	if c := got["CVE-2021-3711 libcrypto1.1"]; c.CVSS == nil || *c.CVSS != 9.8 {
		t.Fatalf("libcrypto CVSS = %v", c.CVSS)
	}
	log4j := got["CVE-2021-44228 org.apache.logging.log4j:log4j-core"]
	if log4j == nil || log4j.Location != "app/lib/log4j-core-2.14.1.jar" || log4j.FixedIn == nil || *log4j.FixedIn != "2.15.0" ||
		log4j.CVSS == nil || *log4j.CVSS != 10 || log4j.Image.Class != "lang-pkgs" || log4j.Image.OS != "" ||
		!slices.Equal(log4j.Image.Platforms, []string{"linux/amd64"}) || len(log4j.FixedVersions) != 3 {
		t.Fatalf("log4j = %+v", log4j)
	}
	busybox := got["CVE-2023-42363 busybox"]
	if busybox == nil || busybox.FixedIn != nil || busybox.Severity != "medium" || busybox.Image.FixStatus != "affected" {
		t.Fatalf("busybox = %+v", busybox)
	}
	// Search covers the location of library findings.
	if l := e.findings(scanning.FindingQuery{Q: "log4j-core-2.14.1.jar"}); l.Total != 1 {
		t.Fatalf("search by path = %d", l.Total)
	}
	// quarantine_critical applies to images.
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.image) != "true" {
		t.Fatal("image not quarantined")
	}
	if e.str(`SELECT scanner FROM component_scans WHERE component_id = $1`, e.image) != "trivy" {
		t.Fatal("scanner not recorded")
	}

	// A rescan that no longer reports apk-tools fixes it; the rest stays.
	e.trivy.set(amdDigest, fixture(t, amdDigest, func(r *trivy.Report) {
		r.Results[0].Vulnerabilities = r.Results[0].Vulnerabilities[1:]
	}))
	e.trivy.set(armDigest, fixture(t, armDigest, func(r *trivy.Report) {
		withoutLang(r)
		r.Results[0].Vulnerabilities = r.Results[0].Vulnerabilities[1:]
	}))
	res, err = e.svc.ScanComponent(ctx, e.image)
	if err != nil || res.Summary.Findings != 4 || res.Summary.New != 0 {
		t.Fatalf("rescan = %+v, %v", res, err)
	}
	got = e.byPackage(e.image)
	if got["CVE-2021-36159 apk-tools"].Status != "fixed" || got["CVE-2021-3711 libssl1.1"].Status != "open" {
		t.Fatalf("after rescan apk %s libssl %s", got["CVE-2021-36159 apk-tools"].Status, got["CVE-2021-3711 libssl1.1"].Status)
	}

	// A failing scan keeps the findings and records why, without the token.
	e.trivy.mu.Lock()
	e.trivy.fail = "unable to pull: GET http://hangar.test:8080/v2/: token tok-docker-hosted-team-api rejected"
	e.trivy.mu.Unlock()
	res, err = e.svc.ScanComponent(ctx, e.image)
	if err != nil || res.Scan.Status != scanning.StatusFailed || !strings.Contains(res.Scan.Error, "unable to pull") ||
		strings.Contains(res.Scan.Error, "tok-docker-hosted") || !strings.Contains(res.Scan.Error, "linux/amd64") {
		t.Fatalf("failed scan = %+v, %v", res, err)
	}
	if l := e.findings(scanning.FindingQuery{ComponentID: e.image}); l.Total != 4 {
		t.Fatalf("findings after a failure = %d", l.Total)
	}

	// Status reports the Trivy server.
	st, err := e.svc.Status(ctx, nil)
	if err != nil || st.Images == nil || !st.Images.Enabled || st.Images.Version != "0.76.1" || st.Images.DBUpdatedAt == nil ||
		!slices.Contains(st.SupportedFormats, "docker") {
		t.Fatalf("status = %+v images %+v, %v", st, st.Images, err)
	}
}

func TestImageScanDefaultPlatformAndArtifacts(t *testing.T) {
	e := newImageEnv(t, scanning.PlatformsDefault)
	ctx := context.Background()
	res, err := e.svc.ScanComponent(ctx, e.image)
	if err != nil || res.Summary.Findings != 5 || e.trivy.callCount() != 1 {
		t.Fatalf("scan = %+v, %v, runs %d", res, err, e.trivy.callCount())
	}
	if f := e.byPackage(e.image)["CVE-2021-36159 apk-tools"]; !slices.Equal(f.Image.Platforms, []string{"linux/amd64"}) {
		t.Fatalf("platforms = %v", f.Image.Platforms)
	}
	// A Helm chart in a docker repository is not scanned and says why.
	res, err = e.svc.ScanComponent(ctx, e.chart)
	if err != nil || res.Scan.Status != scanning.StatusUnsupported || !strings.Contains(res.Scan.Error, "Helm chart") {
		t.Fatalf("chart = %+v, %v", res, err)
	}
	cov, err := e.svc.Status(ctx, nil)
	if err != nil || cov.Coverage.Unsupported < 1 {
		t.Fatalf("coverage = %+v, %v", cov.Coverage, err)
	}
}

func TestImageRepositoryScanSharesDigests(t *testing.T) {
	e := newImageEnv(t, scanning.PlatformsAll)
	ctx := context.Background()
	// "latest" points at the same index as "1.0".
	latest := e.seed.Component(e.dockerRepo, "team", "api", "latest", time.Now(), 0, "alice")
	e.exec(`UPDATE components SET attributes = jsonb_build_object('digest', $2::text) WHERE id = $1`, latest, indexDigest)
	e.jobs.RunInline = true
	if err := e.svc.HandleJobs(e.jobs, nil); err != nil {
		t.Fatal(err)
	}
	repo := &core.Repository{ID: e.dockerRepo, Name: "docker-hosted", Format: "docker"}
	n, err := e.svc.QueueRepositoryScan(ctx, repo)
	if err != nil || n != 3 {
		t.Fatalf("queued %d, %v", n, err)
	}
	if runs := e.trivy.callCount(); runs != 2 {
		t.Fatalf("trivy runs = %d, want one per platform digest", runs)
	}
	for _, id := range []string{e.image, latest} {
		if cs, _ := e.svc.ComponentScan(ctx, id, "docker"); cs.Status != scanning.StatusOK {
			t.Fatalf("%s scan = %+v", id, cs)
		}
	}

	// The fix plan has one row per image: the tags, the packages inside
	// and a Dockerfile line for the OS packages with a fix.
	plan, err := e.svc.FixPlan(ctx, scanning.FixPlanQuery{Tab: scanning.TabAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("plan items = %d", len(plan.Items))
	}
	p := plan.Items[0]
	if p.Key != "docker:team/api" || p.Format != "docker" || !slices.Equal(slices.Sorted(slices.Values(p.Versions)), []string{"1.0", "latest"}) || p.Findings != 10 ||
		p.MaxSeverity != "critical" || p.Image == nil || p.Image.OS != "alpine 3.10.9" || len(p.Upgrades) != 1 ||
		p.Upgrades[0].To != nil || p.Upgrades[0].Fixes != 8 || p.Upgrades[0].Unfixed != 2 || p.Fixable != 8 {
		t.Fatalf("plan = %+v", p)
	}
	if p.Image.Command == nil || *p.Image.Command != "RUN apk upgrade --no-cache apk-tools libcrypto1.1 libssl1.1" {
		t.Fatalf("command = %v", p.Image.Command)
	}
	var log4j *scanning.ImagePackage
	for i := range p.Image.Packages {
		if p.Image.Packages[i].Class == scanning.ClassLibrary {
			log4j = &p.Image.Packages[i]
		}
	}
	if log4j == nil || log4j.Location != "app/lib/log4j-core-2.14.1.jar" || log4j.FixedIn == nil || *log4j.FixedIn != "2.15.0" ||
		log4j.Findings != 2 {
		t.Fatalf("library = %+v", log4j)
	}
	if it := p.Items[0]; it.Package == "" || it.Version == "" || it.Image == nil {
		t.Fatalf("plan finding = %+v", it)
	}

	// Assigning the image assigns every finding of every tag.
	e.exec(`INSERT INTO users (username, display_name, kind) VALUES ('alice', 'Alice', 'user') ON CONFLICT DO NOTHING`)
	ch, err := e.svc.AssignPackage(ctx, nil, "docker:team/api", "alice")
	if err != nil || len(ch.Findings) != 10 {
		t.Fatalf("assign = %+v, %v", ch, err)
	}
}
