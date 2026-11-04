package maven_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func groupFixture(t *testing.T) (*fixture, *remote) {
	t.Helper()
	f := newFixture(t)
	rm := newRemote()
	rm.set("com/acme/lib/maven-metadata.xml", clientMetadata("com.acme", "lib", "0.9", "2.0"))
	rm.set("com/acme/lib/2.0/lib-2.0.jar", "central-2.0")
	rm.set("com/acme/lib/1.0/lib-1.0.jar", "central-1.0")
	f.addProxy("central", rm, core.ProxyConfig{MetadataTTL: core.Duration(-1)}, map[string]any{"version_policy": "release"})
	f.addGroup("public", "releases", "snapshots", "central")
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "hosted-1.0")
	f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "2.1-SNAPSHOT", "20260105.101010", "snap")
	return f, rm
}

// TestGroupResolution replays `mvn install` resolving through a group:
// artifact-level metadata merged from every member, files from the first
// member that has them.
func TestGroupResolution(t *testing.T) {
	f, rm := groupFixture(t)
	auth := "reader:r"
	md := f.getVerified("public/com/acme/lib/maven-metadata.xml", auth)
	m := parse(t, md)
	if !slices.Equal(m.Versioning.Versions, []string{"0.9", "1.0", "2.0", "2.1-SNAPSHOT"}) ||
		m.Versioning.Latest != "2.1-SNAPSHOT" || m.Versioning.Release != "2.0" {
		t.Fatalf("merged metadata:\n%s", md)
	}
	for algo, want := range checksums(md) {
		if r := f.do(http.MethodGet, "public/com/acme/lib/maven-metadata.xml."+algo, auth, ""); r.body != want {
			t.Errorf("merged metadata .%s = %q, want %q", algo, r.body, want)
		}
	}
	// Member order: releases (hosted) shadows central for 1.0.
	if got := f.getVerified("public/com/acme/lib/1.0/lib-1.0.jar", auth); got != "hosted-1.0" {
		t.Fatalf("1.0 from %q", got)
	}
	if rm.count("com/acme/lib/1.0/lib-1.0.jar") != 0 {
		t.Fatal("remote asked for an artifact a hosted member has")
	}
	// Only in the proxy member.
	if got := f.getVerified("public/com/acme/lib/2.0/lib-2.0.jar", auth); got != "central-2.0" {
		t.Fatalf("2.0 from %q", got)
	}
	// Snapshot metadata and timestamped files through the group.
	sm := parse(t, f.getVerified("public/com/acme/lib/2.1-SNAPSHOT/maven-metadata.xml", auth))
	if sm.Versioning.Snapshot.BuildNumber != 1 {
		t.Fatalf("snapshot metadata %+v", sm.Versioning)
	}
	f.getVerified("public/com/acme/lib/2.1-SNAPSHOT/lib-2.1-20260105.101010-1.jar", auth)
	f.want(f.do(http.MethodGet, "public/com/acme/lib/9.9/lib-9.9.jar", auth, ""), http.StatusNotFound, "missing everywhere")

	// Group listing is the union of the members.
	r := f.want(f.do(http.MethodGet, "public/com/acme/lib/?format=json", auth, ""), http.StatusOK, "group listing")
	var l struct {
		Entries []struct{ Name string } `json:"entries"`
	}
	_ = json.Unmarshal([]byte(r.body), &l)
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if !slices.Contains(names, "1.0/") || !slices.Contains(names, "2.0/") || !slices.Contains(names, "2.1-SNAPSHOT/") {
		t.Fatalf("group listing %v", names)
	}
}

func TestGroupMemberFailure(t *testing.T) {
	f, rm := groupFixture(t)
	rm.mu.Lock()
	rm.down = true
	rm.mu.Unlock()
	auth := "reader:r"
	// Hosted members still answer; the remote's metadata is left out.
	m := parse(t, f.getVerified("public/com/acme/lib/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0", "2.1-SNAPSHOT"}) {
		t.Fatalf("metadata with remote down %+v", m.Versioning)
	}
	f.getVerified("public/com/acme/lib/1.0/lib-1.0.jar", auth)
	// Only the failed member could have had it: report the upstream failure.
	f.want(f.do(http.MethodGet, "public/com/acme/lib/3.0/lib-3.0.jar", auth, ""), http.StatusBadGateway, "remote down")
}

func TestNestedGroup(t *testing.T) {
	f, _ := groupFixture(t)
	f.addGroup("outer", "mixed", "public")
	f.mvnDeployRelease("mixed", "com.acme", "lib", "1.5", "mixed-1.5")
	m := parse(t, f.getVerified("outer/com/acme/lib/maven-metadata.xml", "reader:r"))
	if !slices.Equal(m.Versioning.Versions, []string{"0.9", "1.0", "1.5", "2.0", "2.1-SNAPSHOT"}) {
		t.Fatalf("nested group metadata %+v", m.Versioning)
	}
	f.getVerified("outer/com/acme/lib/2.0/lib-2.0.jar", "reader:r")
	// A group that contains itself terminates.
	f.addGroup("loop", "loop", "releases")
	f.getVerified("loop/com/acme/lib/1.0/lib-1.0.jar", "reader:r")
	f.want(f.do(http.MethodGet, "loop/com/acme/lib/7.0/lib-7.0.jar", "reader:r", ""), http.StatusNotFound, "self-member loop")
}
