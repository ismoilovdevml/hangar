package maven

import (
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		path string
		want mavenPath // path and base are not compared
	}{
		{"com/acme/lib/1.0/lib-1.0.jar", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "lib",
			version: "1.0", fileVersion: "1.0", extension: "jar"}},
		{"com/acme/lib/1.0/lib-1.0-sources.jar", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "lib",
			version: "1.0", fileVersion: "1.0", classifier: "sources", extension: "jar"}},
		{"com/acme/lib/1.0/lib-1.0.tar.gz", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "lib",
			version: "1.0", fileVersion: "1.0", extension: "tar.gz"}},
		{"com/acme/lib/1.0/lib-1.0-bin.tar.gz", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "lib",
			version: "1.0", fileVersion: "1.0", classifier: "bin", extension: "tar.gz"}},
		{"com/acme/lib/1.0/lib-1.0.jar.asc", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "lib",
			version: "1.0", fileVersion: "1.0", extension: "jar.asc"}},
		{"com/acme/my-lib/2.1.0/my-lib-2.1.0.pom", mavenPath{kind: kindArtifact, groupID: "com.acme", artifactID: "my-lib",
			version: "2.1.0", fileVersion: "2.1.0", extension: "pom"}},
		{"com/acme/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar", mavenPath{kind: kindArtifact, groupID: "com.acme",
			artifactID: "lib", version: "1.0-SNAPSHOT", fileVersion: "1.0-SNAPSHOT", extension: "jar"}},
		{"com/acme/lib/1.0-SNAPSHOT/lib-1.0-20260101.120000-3.jar", mavenPath{kind: kindArtifact, groupID: "com.acme",
			artifactID: "lib", version: "1.0-SNAPSHOT", fileVersion: "1.0-20260101.120000-3", extension: "jar",
			timestamp: "20260101.120000", buildNumber: 3}},
		{"com/acme/lib/1.0-SNAPSHOT/lib-1.0-20260101.120000-12-javadoc.jar", mavenPath{kind: kindArtifact,
			groupID: "com.acme", artifactID: "lib", version: "1.0-SNAPSHOT", fileVersion: "1.0-20260101.120000-12",
			classifier: "javadoc", extension: "jar", timestamp: "20260101.120000", buildNumber: 12}},
		{"org/x/y/z/a/1/a-1.module", mavenPath{kind: kindArtifact, groupID: "org.x.y.z", artifactID: "a",
			version: "1", fileVersion: "1", extension: "module"}},
		{"com/acme/lib/maven-metadata.xml", mavenPath{kind: kindMetadata, groupID: "com.acme", artifactID: "lib",
			dir: "com/acme/lib"}},
		{"com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", mavenPath{kind: kindMetadata, groupID: "com.acme",
			artifactID: "lib", version: "1.0-SNAPSHOT", dir: "com/acme/lib/1.0-SNAPSHOT"}},
		{"com/maven-metadata.xml", mavenPath{kind: kindMetadata, dir: "com"}},
		{"archetype-catalog.xml", mavenPath{kind: kindOther}},
		{"com/acme/lib/1.0/other-1.0.jar", mavenPath{kind: kindOther}},             // wrong artifactId
		{"com/acme/lib/1.0/lib-1.1.jar", mavenPath{kind: kindOther}},               // wrong version
		{"com/acme/lib/1.0/lib-1.0", mavenPath{kind: kindOther}},                   // no extension
		{"com/acme/lib/1.0-SNAPSHOT/lib-1.0-2026.jar", mavenPath{kind: kindOther}}, // bad timestamp
		{"lib/1.0/lib-1.0.jar", mavenPath{kind: kindOther}},                        // no groupId
	}
	for _, tt := range tests {
		got, err := parsePath(tt.path)
		if err != nil {
			t.Errorf("parsePath(%q): %v", tt.path, err)
			continue
		}
		got.path, got.base = "", nil
		if *got != tt.want {
			t.Errorf("parsePath(%q) =\n %+v\nwant\n %+v", tt.path, *got, tt.want)
		}
	}
}

func TestParsePathChecksum(t *testing.T) {
	for _, algo := range checksumAlgos {
		mp, err := parsePath("com/acme/lib/1.0/lib-1.0.jar." + algo)
		if err != nil {
			t.Fatal(err)
		}
		if mp.kind != kindChecksum || mp.algo != algo || mp.base.kind != kindArtifact || mp.base.path != "com/acme/lib/1.0/lib-1.0.jar" {
			t.Errorf("%s: got %+v base %+v", algo, mp, mp.base)
		}
	}
	mp, err := parsePath("com/acme/lib/maven-metadata.xml.sha1")
	if err != nil || mp.kind != kindChecksum || mp.base.kind != kindMetadata {
		t.Fatalf("metadata checksum: %+v %v", mp, err)
	}
}

func TestParsePathInvalid(t *testing.T) {
	for _, p := range []string{"", "/", "com/../etc/passwd", "com/./x", "com//x.jar", "com/a b/x.jar", "com/a\\b/x", "com/<x>/y"} {
		if _, err := parsePath(p); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("parsePath(%q) err = %v, want ErrInvalid", p, err)
		}
	}
}

func TestCoordinates(t *testing.T) {
	f := New()
	c := f.Coordinates(&core.Asset{Path: "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20260101.120000-3-sources.jar"})
	if c.Namespace != "com.acme" || c.Name != "lib" || c.Version != "1.0-SNAPSHOT" ||
		c.Extra["classifier"] != "sources" || c.Extra["extension"] != "jar" || c.Extra["snapshot_version"] != "1.0-20260101.120000-3" {
		t.Fatalf("Coordinates = %+v", c)
	}
	if c := f.Coordinates(&core.Asset{Path: "archetype-catalog.xml"}); c.Name != "archetype-catalog.xml" || c.Namespace != "" {
		t.Fatalf("Coordinates(other) = %+v", c)
	}
}

func TestValidate(t *testing.T) {
	f := New()
	base := func(cfg map[string]any) *core.Repository {
		return &core.Repository{Name: "m", Format: Name, Type: core.RepoHosted, Config: core.RepoConfig{Format: cfg}}
	}
	for _, ok := range []map[string]any{nil, {"version_policy": "release"}, {"version_policy": "snapshot", "layout_policy": "permissive"}, {"version_policy": "mixed"}} {
		if err := f.Validate(base(ok)); err != nil {
			t.Errorf("Validate(%v): %v", ok, err)
		}
	}
	for _, bad := range []map[string]any{{"version_policy": "nightly"}, {"layout_policy": "loose"}, {"versionPolicy": "release"}, {"version_policy": 3}} {
		if err := f.Validate(base(bad)); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Validate(%v) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := f.Validate(&core.Repository{Name: "m", Format: "npm"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("foreign format: %v", err)
	}
	g := &core.Repository{Name: "g", Format: Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"a"}, WriteMember: "a"}}}
	if err := f.Validate(g); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("group write member: %v", err)
	}
}
