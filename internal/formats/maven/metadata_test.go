package maven

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParseMetadataNamespaced(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://maven.apache.org/METADATA/1.1.0" modelVersion="1.1.0">
  <groupId>com.acme</groupId>
  <artifactId>lib</artifactId>
  <versioning>
    <latest>1.1</latest>
    <release>1.1</release>
    <versions><version>1.0</version><version>1.1</version></versions>
    <lastUpdated>20260101120000</lastUpdated>
  </versioning>
</metadata>`
	m, err := ParseMetadata(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if m.GroupID != "com.acme" || m.Versioning == nil || !slices.Equal(m.Versioning.Versions, []string{"1.0", "1.1"}) {
		t.Fatalf("parsed %+v", m)
	}
	if _, err := ParseMetadata(strings.NewReader("<metadata><groupId>")); err == nil {
		t.Fatal("truncated document accepted")
	}
}

func TestArtifactMetadata(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	comps := []*core.Component{
		{Version: "1.10", CreatedAt: t0},
		{Version: "2.0-SNAPSHOT", CreatedAt: t0.Add(time.Hour)},
		{Version: "1.9", CreatedAt: t0.Add(-time.Hour)},
	}
	m := artifactMetadata("com.acme", "lib", comps)
	v := m.Versioning
	if !slices.Equal(v.Versions, []string{"1.9", "1.10", "2.0-SNAPSHOT"}) || v.Latest != "2.0-SNAPSHOT" || v.Release != "1.10" ||
		v.LastUpdated != "20260102040405" {
		t.Fatalf("versioning %+v", v)
	}
	// Deterministic bytes for the same state.
	if !bytes.Equal(m.Marshal(), artifactMetadata("com.acme", "lib", comps).Marshal()) {
		t.Fatal("artifact metadata is not deterministic")
	}
	out := string(m.Marshal())
	for _, want := range []string{`<?xml version="1.0" encoding="UTF-8"?>`, `<metadata modelVersion="1.1.0">`,
		"<groupId>com.acme</groupId>", "<version>1.10</version>", "<lastUpdated>20260102040405</lastUpdated>"} {
		if !strings.Contains(out, want) {
			t.Errorf("metadata missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"<snapshotVersions", "<plugins", "<snapshot>"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("metadata has empty %s:\n%s", unwanted, out)
		}
	}
}

func TestSnapshotMetadata(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dir := "com/acme/lib/1.0-SNAPSHOT/"
	assets := []*core.Asset{
		{Path: dir + "lib-1.0-20260101.120000-1.jar", UpdatedAt: t0},
		{Path: dir + "lib-1.0-20260101.120000-1.pom", UpdatedAt: t0},
		{Path: dir + "lib-1.0-20260102.090000-2.jar", UpdatedAt: t0.Add(time.Hour)},
		{Path: dir + "lib-1.0-20260102.090000-2.pom", UpdatedAt: t0.Add(time.Hour)},
		{Path: dir + "lib-1.0-20260102.090000-2.jar.asc", UpdatedAt: t0.Add(time.Hour)},
		{Path: dir + "lib-1.0-20260101.120000-1-sources.jar", UpdatedAt: t0},
		{Path: dir + "unrelated.txt", UpdatedAt: t0.Add(5 * time.Hour)},
	}
	m := snapshotMetadata("com.acme", "lib", "1.0-SNAPSHOT", assets)
	if m == nil {
		t.Fatal("nil metadata")
	}
	v := m.Versioning
	if m.Version != "1.0-SNAPSHOT" || v.Snapshot == nil || v.Snapshot.Timestamp != "20260102.090000" || v.Snapshot.BuildNumber != 2 {
		t.Fatalf("snapshot %+v / %+v", m, v.Snapshot)
	}
	want := []SnapshotVersion{
		{Extension: "jar", Value: "1.0-20260102.090000-2", Updated: "20260102090000"},
		{Classifier: "sources", Extension: "jar", Value: "1.0-20260101.120000-1", Updated: "20260101120000"},
		{Extension: "jar.asc", Value: "1.0-20260102.090000-2", Updated: "20260102090000"},
		{Extension: "pom", Value: "1.0-20260102.090000-2", Updated: "20260102090000"},
	}
	if !slices.Equal(v.SnapshotVersions, want) {
		t.Fatalf("snapshotVersions\n %+v\nwant\n %+v", v.SnapshotVersions, want)
	}
	if v.LastUpdated != "20260102040405" {
		t.Fatalf("lastUpdated %s", v.LastUpdated)
	}

	// Only non-unique files: clients are told to use the base-version file.
	nu := snapshotMetadata("com.acme", "lib", "1.0-SNAPSHOT", []*core.Asset{{Path: dir + "lib-1.0-SNAPSHOT.jar", UpdatedAt: t0}})
	if nu == nil || nu.Versioning.Snapshot == nil || !nu.Versioning.Snapshot.LocalCopy ||
		nu.Versioning.SnapshotVersions[0].Value != "1.0-SNAPSHOT" {
		t.Fatalf("non-unique snapshot metadata %+v", nu)
	}
	if snapshotMetadata("com.acme", "lib", "1.0-SNAPSHOT", nil) != nil {
		t.Fatal("empty directory produced metadata")
	}
}

func TestMergeMetadata(t *testing.T) {
	a := &Metadata{GroupID: "g", ArtifactID: "a", Versioning: &Versioning{Versions: []string{"1.0", "1.2"}, LastUpdated: "20260101000000"}}
	b := &Metadata{GroupID: "g", ArtifactID: "a", Version: "1.1", // old documents carry <version> at artifact level
		Versioning: &Versioning{Latest: "1.3-SNAPSHOT", Versions: []string{"1.1", "1.0"}, LastUpdated: "20260201000000"}}
	m := mergeMetadata([]*Metadata{a, b})
	v := m.Versioning
	if !slices.Equal(v.Versions, []string{"1.0", "1.1", "1.2", "1.3-SNAPSHOT"}) || v.Latest != "1.3-SNAPSHOT" || v.Release != "1.2" ||
		v.LastUpdated != "20260201000000" {
		t.Fatalf("merged %+v", v)
	}

	s1 := &Metadata{Version: "1.0-SNAPSHOT", Versioning: &Versioning{Snapshot: &Snapshot{Timestamp: "20260101.000000", BuildNumber: 4},
		SnapshotVersions: []SnapshotVersion{{Extension: "jar", Value: "1.0-20260101.000000-4", Updated: "20260101000000"}}}}
	s2 := &Metadata{Version: "1.0-SNAPSHOT", Versioning: &Versioning{Snapshot: &Snapshot{Timestamp: "20260102.000000", BuildNumber: 1},
		SnapshotVersions: []SnapshotVersion{{Extension: "jar", Value: "1.0-20260102.000000-1", Updated: "20260102000000"}}}}
	sm := mergeMetadata([]*Metadata{s1, s2})
	if sm.Versioning.Snapshot.Timestamp != "20260102.000000" || sm.Versioning.SnapshotVersions[0].Value != "1.0-20260102.000000-1" {
		t.Fatalf("merged snapshot %+v", sm.Versioning)
	}

	p := mergeMetadata([]*Metadata{
		{Plugins: []Plugin{{Prefix: "foo", ArtifactID: "foo-maven-plugin"}}},
		{Plugins: []Plugin{{Prefix: "bar", ArtifactID: "bar-maven-plugin"}, {Prefix: "foo", ArtifactID: "other"}}},
	})
	if p.Versioning != nil || len(p.Plugins) != 2 || p.Plugins[0].Prefix != "bar" || p.Plugins[1].ArtifactID != "foo-maven-plugin" {
		t.Fatalf("merged plugins %+v", p)
	}
}
