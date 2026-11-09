package maven_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestQuarantinedVersionsNotAdvertised: maven-metadata.xml leaves out the
// versions quarantined in the repository serving it (hosted, proxy and the
// group merging both), with latest and release recomputed, and the build
// list of a quarantined SNAPSHOT is not served.
func TestQuarantinedVersionsNotAdvertised(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	rm.set("org/example/remote/1.0/remote-1.0.jar", "jar 1.0")
	rm.set("org/example/remote/2.0/remote-2.0.jar", "jar 2.0")
	rm.set("org/example/remote/maven-metadata.xml",
		"<metadata><groupId>org.example</groupId><artifactId>remote</artifactId><versioning><latest>2.0</latest><release>2.0</release>"+
			"<versions><version>1.0</version><version>2.0</version></versions><lastUpdated>20260101000000</lastUpdated></versioning></metadata>")
	f.addProxy("central", rm, core.ProxyConfig{}, map[string]any{"version_policy": "mixed"})
	f.addGroup("public", "releases", "snapshots", "central")
	auth := "reader:r"
	quarantine := func(repo, g, a, v string) {
		t.Helper()
		c, err := f.env.Components.GetComponent(t.Context(), f.repo[repo].ID, g, a, v)
		if err != nil {
			t.Fatalf("component %s %s:%s:%s: %v", repo, g, a, v, err)
		}
		f.env.Components.SetQuarantined(c.ID, true)
	}

	// Proxy: 2.0 is cached, then quarantined.
	f.want(f.do(http.MethodGet, "central/org/example/remote/2.0/remote-2.0.jar", auth, ""), http.StatusOK, "cache 2.0")
	quarantine("central", "org.example", "remote", "2.0")
	for _, repo := range []string{"central", "public"} {
		m := parse(t, f.getVerified(repo+"/org/example/remote/maven-metadata.xml", auth))
		if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) || m.Versioning.Latest != "1.0" || m.Versioning.Release != "1.0" {
			t.Errorf("%s metadata = %+v", repo, m.Versioning)
		}
	}

	// Hosted: a release and a SNAPSHOT, both quarantined.
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "jar-1.0")
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.1", "jar-1.1")
	quarantine("releases", "com.acme", "lib", "1.1")
	m := parse(t, f.getVerified("releases/com/acme/lib/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) || m.Versioning.Release != "1.0" {
		t.Errorf("hosted release metadata = %+v", m.Versioning)
	}
	f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "2.0-SNAPSHOT", "20260101.120000", "snap")
	quarantine("snapshots", "com.acme", "lib", "2.0-SNAPSHOT")
	for _, repo := range []string{"snapshots", "public"} {
		f.want(f.do(http.MethodGet, repo+"/com/acme/lib/2.0-SNAPSHOT/maven-metadata.xml", auth, ""), http.StatusForbidden,
			repo+" metadata of a quarantined SNAPSHOT")
	}
	if r := f.do(http.MethodGet, "snapshots/com/acme/lib/maven-metadata.xml", auth, ""); r.code != http.StatusNotFound {
		t.Errorf("artifact metadata with only a quarantined SNAPSHOT = %d %s", r.code, r.body)
	}
}
