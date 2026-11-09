package maven_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// routingFixture: releases (hosted) holds com.edcom:utils 1.0; "central"
// proxies a public remote over HTTP that also publishes com.edcom:utils 99.0
// (the dependency confusion attack) plus an unrelated org.example:remote;
// "public" groups both.
func routingFixture(t *testing.T) (*fixture, *remote) {
	t.Helper()
	f := newFixture(t)
	rm := newRemote()
	rm.set("com/edcom/utils/maven-metadata.xml", clientMetadata("com.edcom", "utils", "99.0"))
	rm.set("com/edcom/utils/99.0/utils-99.0.jar", emptyJar)
	rm.set("com/edcom/utils/99.0/utils-99.0.pom", pom("com.edcom", "utils", "99.0", "jar"))
	rm.set("org/example/remote/maven-metadata.xml", clientMetadata("org.example", "remote", "3.1"))
	rm.set("org/example/remote/3.1/remote-3.1.jar", emptyJar)
	rm.set("org/example/remote/3.1/remote-3.1.pom", pom("org.example", "remote", "3.1", "jar"))
	srv := httptest.NewServer(rm)
	t.Cleanup(srv.Close)
	f.repo["central"] = f.env.AddRepo(t, &core.Repository{Name: "central", Format: maven.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: srv.URL + "/maven2", MetadataTTL: core.Duration(-1)},
			Format: map[string]any{"version_policy": "release"}}})
	f.addGroup("public", "releases", "central")
	f.mvnDeployRelease("releases", "com.edcom", "utils", "1.0", "hosted-1.0")
	return f, rm
}

func (f *fixture) setRouting(name string, rules *core.RoutingRules) {
	f.t.Helper()
	r := f.repo[name]
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		f.t.Fatal(err)
	}
	f.repo[name] = f.env.Repos.Put(r)
}

// sawPrefix reports whether the remote received a request under prefix.
func (rm *remote) sawPrefix(prefix string) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return slices.ContainsFunc(rm.requests, func(p string) bool { return strings.HasPrefix(p, prefix) })
}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	f, _ := routingFixture(t)
	auth := "reader:r"

	m := parse(t, f.getVerified("public/com/edcom/utils/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0", "99.0"}) || m.Versioning.Release != "99.0" {
		t.Fatalf("unprotected metadata: %+v (the test must bite)", m.Versioning)
	}

	f.setRouting("public", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "releases", Exclusive: []string{"com.edcom.*", "com.edcom"}}}})
	md := f.getVerified("public/com/edcom/utils/maven-metadata.xml", auth)
	m = parse(t, md)
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) || m.Versioning.Release != "1.0" {
		t.Fatalf("protected metadata: %+v", m.Versioning)
	}
	r := f.want(f.do(http.MethodGet, "public/com/edcom/utils/99.0/utils-99.0.jar", auth, ""), http.StatusNotFound, "public jar")
	if h := r.hdr.Get(routing.Header); !strings.Contains(h, `"com.edcom"`) || !strings.Contains(h, `"central"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	if !strings.Contains(r.body, "routing rule") {
		t.Errorf("404 body = %q", r.body)
	}
	if got := f.getVerified("public/com/edcom/utils/1.0/utils-1.0.jar", auth); got != "hosted-1.0" {
		t.Errorf("hosted jar = %q", got)
	}
	// Unrelated public artifacts still come through the proxy member.
	if m := parse(t, f.getVerified("public/org/example/remote/maven-metadata.xml", auth)); !slices.Equal(m.Versioning.Versions, []string{"3.1"}) {
		t.Errorf("public metadata = %+v", m.Versioning)
	}
	f.getVerified("public/org/example/remote/3.1/remote-3.1.jar", auth)

	// The group listing hides the proxy's cached copy of the internal name.
	f.setRouting("public", nil)
	f.getVerified("public/com/edcom/utils/99.0/utils-99.0.jar", auth) // cache it in central
	f.setRouting("public", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "releases", Exclusive: []string{"com.edcom*"}}}})
	lr := f.want(f.do(http.MethodGet, "public/com/edcom/utils/?format=json", auth, ""), http.StatusOK, "listing")
	var l struct {
		Entries []struct{ Name string } `json:"entries"`
	}
	_ = json.Unmarshal([]byte(lr.body), &l)
	for _, e := range l.Entries {
		if e.Name == "99.0/" {
			t.Errorf("listing shows the excluded member's version: %s", lr.body)
		}
	}

	// Excluding every member answers 404 naming the rule.
	f.setRouting("public", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "releases", Block: []string{"com.edcom"}}, {Member: "central", Block: []string{"com.edcom"}}}})
	r = f.want(f.do(http.MethodGet, "public/com/edcom/utils/maven-metadata.xml", auth, ""), http.StatusNotFound, "all excluded")
	if !strings.Contains(r.body, `blocked by routing rule "com.edcom" on repository "public"`) {
		t.Errorf("all excluded body = %q", r.body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	f, rm := routingFixture(t)
	auth := "reader:r"
	// Cache the public jar first: a rule added later must hide it too.
	f.getVerified("central/com/edcom/utils/99.0/utils-99.0.jar", auth)
	rm.mu.Lock()
	rm.requests = nil
	rm.mu.Unlock()

	f.setRouting("central", &core.RoutingRules{Block: []string{"com.edcom"}})
	for _, p := range []string{
		"com/edcom/utils/maven-metadata.xml",
		"com/edcom/utils/maven-metadata.xml.sha1",
		"com/edcom/utils/99.0/utils-99.0.pom",
		"com/edcom/utils/99.0/utils-99.0.jar", // cached before the rule
		"com/edcom/maven-metadata.xml",        // plugin-group metadata of the blocked groupId
	} {
		r := f.do(http.MethodGet, "central/"+p, auth, "")
		if r.code != http.StatusNotFound || !strings.Contains(r.body, `blocked by routing rule "com.edcom" on repository "central"`) {
			t.Errorf("GET %s = %d %q", p, r.code, r.body)
		}
		if r.hdr.Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	// Through the group the hosted version wins and the remote never hears the name.
	m := parse(t, f.getVerified("public/com/edcom/utils/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) {
		t.Errorf("group metadata = %+v", m.Versioning)
	}
	if rm.sawPrefix("com/edcom") {
		t.Error("the internal groupId reached the public remote")
	}
	// The proxy's listing hides the cached blocked artifact (here: all of it).
	if lr := f.do(http.MethodGet, "central/com/edcom/utils/?format=json", auth, ""); lr.code != http.StatusNotFound || strings.Contains(lr.body, "99.0") {
		t.Errorf("listing of a blocked artifact = %d %s", lr.code, lr.body)
	}
	// Allowed names still reach the remote.
	f.getVerified("central/org/example/remote/3.1/remote-3.1.jar", auth)
	if !rm.sawPrefix("org/example/remote") {
		t.Error("allowed artifact never reached the remote")
	}
	// An allow list of groupId:artifactId patterns keeps the rest away.
	f.setRouting("central", &core.RoutingRules{Allow: []string{"org.example:*"}})
	f.getVerified("central/org/example/remote/maven-metadata.xml", auth)
	r := f.want(f.do(http.MethodGet, "central/com/other/lib/1.0/lib-1.0.jar", auth, ""), http.StatusNotFound, "allow miss")
	if !strings.Contains(r.body, "matches no allow pattern") {
		t.Errorf("allow miss body = %q", r.body)
	}
}

// TestRealGradleRouting resolves the dynamic version com.edcom:utils:+
// through the group with the real gradle client: unprotected it picks the
// public 99.0, with an exclusive rule the hosted 1.0.
func TestRealGradleRouting(t *testing.T) {
	gradle, err := exec.LookPath("gradle")
	if err != nil {
		t.Skip("gradle not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped with -short")
	}
	f, _ := routingFixture(t)
	resolve := func() string {
		t.Helper()
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"settings.gradle": "rootProject.name = 'app'\n",
			"build.gradle": `plugins { id 'java' }
repositories {
  maven {
    url = '` + f.srv.URL + `/repository/public/'
    allowInsecureProtocol = true
    credentials { username = 'reader'; password = 'r' }
  }
}
dependencies { implementation 'com.edcom:utils:+' }
tasks.register('printDeps') {
  def cp = configurations.runtimeClasspath
  doLast { cp.files.each { println "DEP " + it.name } }
}
`,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, gradle, "--no-daemon", "--console=plain", "printDeps")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GRADLE_USER_HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gradle: %v\n%s", err, out)
		}
		return grepLines(string(out), "DEP ")
	}
	if got := resolve(); got != "DEP utils-99.0.jar" {
		t.Fatalf("unprotected resolution = %q, want the public 99.0 (the test must bite)", got)
	}
	f.setRouting("public", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "releases", Exclusive: []string{"com.edcom"}}}})
	if got := resolve(); got != "DEP utils-1.0.jar" {
		t.Fatalf("protected resolution = %q, want the hosted 1.0", got)
	}
}
