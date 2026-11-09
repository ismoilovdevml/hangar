package scanning_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
)

const (
	debID = "DEBIAN-CVE-2023-5678"
	rlID  = "RLSA-2024:6783"
	alID  = "ALSA-2024:6783"
)

// distroAdvisories adds distribution advisories to the fake database.
func distroAdvisories(e *env) {
	affected := func(name, eco, fixed string) []osv.Affected {
		return []osv.Affected{{Package: osv.Package{Name: name, Ecosystem: eco},
			Ranges: []osv.Range{{Type: "ECOSYSTEM", Events: []osv.Event{{Introduced: "0"}, {Fixed: fixed}}}}}}
	}
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	e.db.vulns[debID] = osv.Vuln{ID: debID, Summary: "openssl: excessive time spent in DH checks", Modified: "2026-01-01T00:00:00Z",
		Aliases: []string{"CVE-2023-5678"}, Severity: []osv.Severity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L"}},
		Affected: affected("openssl", "Debian:12", "3.0.11-1~deb12u2")}
	e.db.vulns[rlID] = osv.Vuln{ID: rlID, Summary: "Moderate: openssl security update", Modified: "2026-01-01T00:00:00Z",
		DatabaseSpecific: []byte(`{"severity":"Moderate"}`), Affected: affected("openssl", "Rocky Linux:9", "1:3.0.7-28.el9_4")}
	e.db.vulns[alID] = osv.Vuln{ID: alID, Summary: "Moderate: openssl security update", Modified: "2026-01-01T00:00:00Z",
		Affected: affected("openssl", "AlmaLinux:9", "1:3.0.7-28.el9_4")}
	// Advisories are kept per source package and release.
	e.db.affects["Debian:12/openssl@3.0.11-1~deb12u1"] = []string{debID}
	e.db.affects["Rocky Linux:9/openssl@1:3.0.7-24.el9"] = []string{rlID}
	e.db.affects["AlmaLinux:9/openssl@1:3.0.7-24.el9"] = []string{alID}
}

func (e *env) queried(key string) bool {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	return slices.Contains(e.db.queried, key)
}

func TestDistributionScanning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	distroAdvisories(e)
	store := e.str(`SELECT id::text FROM blob_stores WHERE name = 'default'`)

	// An APT repository: libssl3 is built from the openssl source package.
	aptRepo := e.seed.Repo("apt-hosted", "apt", "hosted", store)
	e.exec(`UPDATE repositories SET config = '{"hosted":{}}' WHERE id = $1`, aptRepo)
	libssl := e.seed.Component(aptRepo, "", "libssl3", "3.0.11-1~deb12u1", time.Now(), 0, "alice")
	e.exec(`UPDATE components SET attributes = '{"source":"openssl"}' WHERE id = $1`, libssl)

	// Without a release it is not scanned, and says how to fix that.
	res, err := e.svc.ScanComponent(ctx, libssl)
	if err != nil || res.Scan.Status != scanning.StatusUnsupported || !strings.Contains(res.Scan.Error, "scanning.distro") {
		t.Fatalf("scan without a release = %+v, %v", res, err)
	}
	repo := &core.Repository{ID: aptRepo, Name: "apt-hosted", Format: "apt", Type: core.RepoHosted}
	if _, err := e.svc.QueueRepositoryScan(ctx, repo); err == nil {
		t.Fatal("repository scan without a release was queued")
	}
	st, err := e.svc.Status(ctx, []string{aptRepo})
	if err != nil || st.Coverage.Unsupported != 1 {
		t.Fatalf("coverage = %+v, %v", st.Coverage, err)
	}

	// The setting names the release: the source package is checked, and
	// the fix is ordered the way dpkg orders versions.
	e.exec(`UPDATE repositories SET config = '{"hosted":{},"scanning":{"distro":"Debian:12"}}' WHERE id = $1`, aptRepo)
	res, err = e.svc.ScanComponent(ctx, libssl)
	if err != nil || res.Scan.Status != scanning.StatusOK || res.Summary.Findings != 1 {
		t.Fatalf("scan = %+v, %v", res, err)
	}
	if !e.queried("Debian:12/openssl@3.0.11-1~deb12u1") {
		t.Fatalf("queried %v", e.db.queried)
	}
	f := e.finding(libssl, debID)
	if f.Package != "openssl" || f.Installed != "3.0.11-1~deb12u1" || f.FixedIn == nil || *f.FixedIn != "3.0.11-1~deb12u2" ||
		f.Severity != "medium" {
		t.Fatalf("finding = %+v", f)
	}
	repo.Config.Scanning = &core.ScanningConfig{Distro: "Debian:12"}
	if n, err := e.svc.QueueRepositoryScan(ctx, repo); err != nil || n != 1 {
		t.Fatalf("repository scan = %d, %v", n, err)
	}
	if st, _ := e.svc.Status(ctx, []string{aptRepo}); st.Coverage.Unsupported != 0 || st.Coverage.Scanned != 1 {
		t.Fatalf("coverage = %+v", st.Coverage)
	}

	// A Debian binNMU: the source version is what advisories know.
	binnmu := e.seed.Component(aptRepo, "", "libssl3", "3.0.11-1~deb12u1+b1", time.Now(), 0, "alice")
	e.exec(`UPDATE components SET attributes = '{"source":"openssl","source_version":"3.0.11-1~deb12u1"}' WHERE id = $1`, binnmu)
	if res, err := e.svc.ScanComponent(ctx, binnmu); err != nil || res.Summary.Findings != 1 {
		t.Fatalf("binNMU scan = %+v, %v", res, err)
	}

	// An RPM proxy of a Rocky Linux 9 mirror: the release comes from the
	// remote, the package name from the source RPM, the version keeps its epoch.
	rpmRepo := e.seed.Repo("rocky-baseos", "rpm", "proxy", store)
	e.exec(`UPDATE repositories SET config = '{"proxy":{"remote_url":"https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/"}}' WHERE id = $1`, rpmRepo)
	libs := e.seed.Component(rpmRepo, "", "openssl-libs", "1:3.0.7-24.el9", time.Now(), 0, "")
	e.exec(`UPDATE components SET attributes = '{"sourcerpm":"openssl-3.0.7-24.el9.src.rpm","arch":"x86_64"}' WHERE id = $1`, libs)
	if res, _ := e.svc.ScanComponent(ctx, libs); res.Scan.Status != scanning.StatusUnsupported {
		t.Fatalf("before detection: %+v", res.Scan)
	}
	if err := e.svc.SyncDistros(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if eco, src, err := e.svc.RepositoryDistro(ctx, rpmRepo); err != nil || eco != "Rocky Linux:9" || src != scanning.DistroFromRemote {
		t.Fatalf("detected = %q %q, %v", eco, src, err)
	}
	// The newly detected release queues a scan of the repository; the
	// setting of the APT repository is recorded too.
	if eco, src, _ := e.svc.RepositoryDistro(ctx, aptRepo); eco != "Debian:12" || src != scanning.DistroFromSetting {
		t.Fatalf("apt release = %q %q", eco, src)
	}
	queued := 0
	for _, j := range e.jobs.Jobs() {
		if j.Type == scanning.JobScanRepository && strings.Contains(string(j.Payload), rpmRepo) {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("repository scans queued for the proxy: %d (%+v)", queued, e.jobs.Jobs())
	}
	// Unchanged: nothing new is queued.
	before := len(e.jobs.Jobs())
	if err := e.svc.SyncDistros(ctx, rpmRepo); err != nil || len(e.jobs.Jobs()) != before {
		t.Fatalf("resync queued jobs: %d -> %d, %v", before, len(e.jobs.Jobs()), err)
	}
	res, err = e.svc.ScanComponent(ctx, libs)
	if err != nil || res.Summary.Findings != 1 || !e.queried("Rocky Linux:9/openssl@1:3.0.7-24.el9") {
		t.Fatalf("rpm scan = %+v, %v (queried %v)", res, err, e.db.queried)
	}
	if f := e.finding(libs, rlID); f.Package != "openssl" || f.FixedIn == nil || *f.FixedIn != "1:3.0.7-28.el9_4" || f.Severity != "medium" {
		t.Fatalf("rpm finding = %+v", f)
	}

	// The fix plan upgrades the installed binaries with the package manager.
	plan, err := e.svc.FixPlan(ctx, scanning.FixPlanQuery{Tab: "all"})
	if err != nil {
		t.Fatal(err)
	}
	cmds := map[string]string{}
	for _, p := range plan.Items {
		for _, u := range p.Upgrades {
			if u.Command != nil {
				cmds[p.Key] = *u.Command
			}
		}
	}
	if cmds["apt:openssl"] != "apt-get install --only-upgrade libssl3" || cmds["rpm:openssl"] != "dnf upgrade openssl-libs" {
		t.Fatalf("upgrade commands = %v", cmds)
	}

	// The setting wins over what the remote says.
	e.exec(`UPDATE repositories SET config = config || '{"scanning":{"distro":"AlmaLinux:9"}}' WHERE id = $1`, rpmRepo)
	if err := e.svc.SyncDistros(ctx, rpmRepo); err != nil {
		t.Fatal(err)
	}
	if eco, src, _ := e.svc.RepositoryDistro(ctx, rpmRepo); eco != "AlmaLinux:9" || src != scanning.DistroFromSetting {
		t.Fatalf("after the setting = %q %q", eco, src)
	}
	if res, err := e.svc.ScanComponent(ctx, libs); err != nil || !e.queried("AlmaLinux:9/openssl@1:3.0.7-24.el9") ||
		res.Summary.Findings != 1 {
		t.Fatalf("setting scan = %+v, %v", res, err)
	}
	if f := e.finding(libs, rlID); f.Status != scanning.FindingFixed {
		t.Fatalf("the Rocky advisory no longer applies: %+v", f)
	}
}

func TestAPTProxyReleaseFromCachedSuites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	store := e.str(`SELECT id::text FROM blob_stores WHERE name = 'default'`)
	repo := e.seed.Repo("debian-proxy", "apt", "proxy", store)
	e.exec(`UPDATE repositories SET config = '{"proxy":{"remote_url":"http://deb.debian.org/debian"}}' WHERE id = $1`, repo)
	digest := e.seed.Blob(store, "release", 10, time.Now())

	sync := func() string {
		t.Helper()
		if err := e.svc.SyncDistros(ctx, repo); err != nil {
			t.Fatal(err)
		}
		eco, _, err := e.svc.RepositoryDistro(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		return eco
	}
	if got := sync(); got != "" {
		t.Fatalf("nothing cached yet: %q", got)
	}
	e.seed.Asset(repo, "", "dists/bookworm/InRelease", digest, 10)
	e.seed.Asset(repo, "", "dists/bookworm-updates/InRelease", digest, 10)
	e.seed.Asset(repo, "", "dists/bookworm/main/binary-amd64/Packages.xz", digest, 10)
	if got := sync(); got != "Debian:12" {
		t.Fatalf("bookworm = %q", got)
	}
	// A second release behind the same proxy: ambiguous, so not guessed.
	e.seed.Asset(repo, "", "dists/trixie/Release", digest, 10)
	if got := sync(); got != "" {
		t.Fatalf("bookworm and trixie = %q", got)
	}
}
