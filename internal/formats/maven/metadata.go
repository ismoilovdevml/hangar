package maven

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Metadata is a maven-metadata.xml document (Maven Repository Metadata
// Model 1.1.0). Namespaced documents are accepted on input; output carries no
// namespace, which every Maven and Gradle version reads.
type Metadata struct {
	XMLName      xml.Name    `xml:"metadata"`
	ModelVersion string      `xml:"modelVersion,attr,omitempty"`
	GroupID      string      `xml:"groupId,omitempty"`
	ArtifactID   string      `xml:"artifactId,omitempty"`
	Version      string      `xml:"version,omitempty"`
	Versioning   *Versioning `xml:"versioning,omitempty"`
	Plugins      []Plugin    `xml:"plugins>plugin,omitempty"`
}

// Versioning is the <versioning> element.
type Versioning struct {
	Latest           string            `xml:"latest,omitempty"`
	Release          string            `xml:"release,omitempty"`
	Snapshot         *Snapshot         `xml:"snapshot,omitempty"`
	Versions         []string          `xml:"versions>version,omitempty"`
	LastUpdated      string            `xml:"lastUpdated,omitempty"`
	SnapshotVersions []SnapshotVersion `xml:"snapshotVersions>snapshotVersion,omitempty"`
}

// Snapshot is the <snapshot> element of a snapshot version's metadata.
type Snapshot struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
	LocalCopy   bool   `xml:"localCopy,omitempty"`
}

// SnapshotVersion is one <snapshotVersion>: the newest file for a classifier/extension.
type SnapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated,omitempty"`
}

// Plugin is one <plugin> of group-level metadata (plugin prefix resolution).
type Plugin struct {
	Name       string `xml:"name,omitempty"`
	Prefix     string `xml:"prefix"`
	ArtifactID string `xml:"artifactId"`
}

// maxMetadataSize bounds metadata documents read from clients, upstreams and members.
const maxMetadataSize = 16 << 20

// lastUpdatedLayout is yyyyMMddHHmmss (UTC).
const lastUpdatedLayout = "20060102150405"

// ParseMetadata decodes a maven-metadata.xml document.
func ParseMetadata(r io.Reader) (*Metadata, error) {
	lr := &io.LimitedReader{R: r, N: maxMetadataSize + 1}
	var m Metadata
	dec := xml.NewDecoder(lr)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("maven-metadata.xml: %w: %w", err, core.ErrInvalid)
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("maven-metadata.xml larger than %d bytes: %w", maxMetadataSize, core.ErrInvalid)
	}
	for i := range m.Plugins {
		m.Plugins[i].Prefix = strings.TrimSpace(m.Plugins[i].Prefix)
	}
	return &m, nil
}

// Marshal renders m with an XML declaration and two-space indentation.
// Empty lists are omitted entirely (encoding/xml would write empty
// <versions/>, <snapshotVersions/> and <plugins/> wrappers for "a>b" tags).
func (m *Metadata) Marshal() []byte {
	type versions struct {
		V []string `xml:"version"`
	}
	type snapshotVersions struct {
		V []SnapshotVersion `xml:"snapshotVersion"`
	}
	type plugins struct {
		V []Plugin `xml:"plugin"`
	}
	type versioning struct {
		Latest           string            `xml:"latest,omitempty"`
		Release          string            `xml:"release,omitempty"`
		Snapshot         *Snapshot         `xml:"snapshot,omitempty"`
		Versions         *versions         `xml:"versions,omitempty"`
		LastUpdated      string            `xml:"lastUpdated,omitempty"`
		SnapshotVersions *snapshotVersions `xml:"snapshotVersions,omitempty"`
	}
	type wire struct {
		XMLName      xml.Name    `xml:"metadata"`
		ModelVersion string      `xml:"modelVersion,attr,omitempty"`
		GroupID      string      `xml:"groupId,omitempty"`
		ArtifactID   string      `xml:"artifactId,omitempty"`
		Version      string      `xml:"version,omitempty"`
		Versioning   *versioning `xml:"versioning,omitempty"`
		Plugins      *plugins    `xml:"plugins,omitempty"`
	}
	w := wire{ModelVersion: m.ModelVersion, GroupID: m.GroupID, ArtifactID: m.ArtifactID, Version: m.Version}
	if v := m.Versioning; v != nil {
		w.Versioning = &versioning{Latest: v.Latest, Release: v.Release, Snapshot: v.Snapshot, LastUpdated: v.LastUpdated}
		if len(v.Versions) > 0 {
			w.Versioning.Versions = &versions{V: v.Versions}
		}
		if len(v.SnapshotVersions) > 0 {
			w.Versioning.SnapshotVersions = &snapshotVersions{V: v.SnapshotVersions}
		}
	}
	if len(m.Plugins) > 0 {
		w.Plugins = &plugins{V: m.Plugins}
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	// Encoding a struct of strings into a bytes.Buffer cannot fail.
	_ = enc.Encode(w)
	b.WriteByte('\n')
	return b.Bytes()
}

// mergeMetadata combines the documents of several repositories (group
// members) for the same path: versions are unioned and re-sorted, latest and
// release recomputed, the newest snapshot and snapshotVersions win, plugins
// are unioned by prefix. docs must not be empty.
func mergeMetadata(docs []*Metadata) *Metadata {
	out := &Metadata{ModelVersion: "1.1.0"}
	versions := map[string]bool{}
	var snap *Snapshot
	svs := map[string]SnapshotVersion{}
	plugins := map[string]Plugin{}
	var lastUpdated string
	hasVersioning := false
	for _, d := range docs {
		if out.GroupID == "" {
			out.GroupID = d.GroupID
		}
		if out.ArtifactID == "" {
			out.ArtifactID = d.ArtifactID
		}
		if out.Version == "" {
			out.Version = d.Version
		}
		for _, p := range d.Plugins {
			if _, ok := plugins[p.Prefix]; !ok {
				plugins[p.Prefix] = p
			}
		}
		v := d.Versioning
		if v == nil {
			continue
		}
		hasVersioning = true
		for _, ver := range v.Versions {
			versions[strings.TrimSpace(ver)] = true
		}
		// Documents without a <versions> list still name their latest/release.
		for _, ver := range []string{v.Latest, v.Release} {
			if ver = strings.TrimSpace(ver); ver != "" {
				versions[ver] = true
			}
		}
		if v.LastUpdated > lastUpdated {
			lastUpdated = v.LastUpdated
		}
		if s := v.Snapshot; s != nil && (snap == nil || newerSnapshot(s, snap)) {
			c := *s
			snap = &c
		}
		for _, sv := range v.SnapshotVersions {
			k := sv.Classifier + "\x00" + sv.Extension
			if old, ok := svs[k]; !ok || sv.Updated > old.Updated {
				svs[k] = sv
			}
		}
	}
	if hasVersioning {
		vs := &Versioning{Snapshot: snap, LastUpdated: lastUpdated}
		for v := range versions {
			vs.Versions = append(vs.Versions, v)
		}
		sortVersions(vs.Versions)
		vs.Latest, vs.Release = latestAndRelease(vs.Versions)
		for _, sv := range svs {
			vs.SnapshotVersions = append(vs.SnapshotVersions, sv)
		}
		sortSnapshotVersions(vs.SnapshotVersions)
		out.Versioning = vs
	}
	for _, p := range plugins {
		out.Plugins = append(out.Plugins, p)
	}
	sort.Slice(out.Plugins, func(i, j int) bool { return out.Plugins[i].Prefix < out.Plugins[j].Prefix })
	return out
}

func newerSnapshot(a, b *Snapshot) bool {
	if a.Timestamp != b.Timestamp {
		return a.Timestamp > b.Timestamp
	}
	return a.BuildNumber > b.BuildNumber
}

// latestAndRelease picks the highest version and the highest non-SNAPSHOT
// version of an ascending list.
func latestAndRelease(sorted []string) (latest, release string) {
	if len(sorted) == 0 {
		return "", ""
	}
	latest = sorted[len(sorted)-1]
	for i := len(sorted) - 1; i >= 0; i-- {
		if !isSnapshot(sorted[i]) {
			return latest, sorted[i]
		}
	}
	return latest, ""
}

func sortSnapshotVersions(svs []SnapshotVersion) {
	sort.Slice(svs, func(i, j int) bool {
		if svs[i].Extension != svs[j].Extension {
			return svs[i].Extension < svs[j].Extension
		}
		return svs[i].Classifier < svs[j].Classifier
	})
}

// artifactMetadata builds the artifact-level document from component
// versions. lastUpdated is the newest component creation time, so the bytes
// (and their checksums) stay identical until the version set changes.
func artifactMetadata(groupID, artifactID string, comps []*core.Component) *Metadata {
	vs := make([]string, 0, len(comps))
	var newest time.Time
	for _, c := range comps {
		vs = append(vs, c.Version)
		if c.CreatedAt.After(newest) {
			newest = c.CreatedAt
		}
	}
	sortVersions(vs)
	latest, release := latestAndRelease(vs)
	return &Metadata{
		ModelVersion: "1.1.0",
		GroupID:      groupID,
		ArtifactID:   artifactID,
		Versioning: &Versioning{
			Latest:      latest,
			Release:     release,
			Versions:    vs,
			LastUpdated: newest.UTC().Format(lastUpdatedLayout),
		},
	}
}

// snapshotMetadata builds a SNAPSHOT version's document from the files in its
// directory. Timestamped files give <snapshot> (the highest build) and one
// <snapshotVersion> per classifier/extension; when only non-unique files
// (artifactId-1.0-SNAPSHOT.jar) exist, <localCopy> tells clients to use them.
// It returns nil when the directory holds no artifact of this version.
func snapshotMetadata(groupID, artifactID, version string, assets []*core.Asset) *Metadata {
	type entry struct {
		sv    SnapshotVersion
		build int
	}
	newest := map[string]entry{}
	var snap *Snapshot
	var lastUpdated time.Time
	found := false
	for _, a := range assets {
		mp, err := parsePath(a.Path)
		if err != nil || mp.kind != kindArtifact || mp.version != version || mp.artifactID != artifactID ||
			mp.groupID != groupID {
			continue
		}
		found = true
		if a.UpdatedAt.After(lastUpdated) {
			lastUpdated = a.UpdatedAt
		}
		k := mp.classifier + "\x00" + mp.extension
		e := entry{sv: SnapshotVersion{Classifier: mp.classifier, Extension: mp.extension, Value: mp.fileVersion}}
		if mp.timestamp != "" {
			e.build = mp.buildNumber
			e.sv.Updated = strings.ReplaceAll(mp.timestamp, ".", "")
			s := &Snapshot{Timestamp: mp.timestamp, BuildNumber: mp.buildNumber}
			if snap == nil || s.BuildNumber > snap.BuildNumber ||
				(s.BuildNumber == snap.BuildNumber && s.Timestamp > snap.Timestamp) {
				snap = s
			}
		} else {
			e.build = -1 // a non-unique file loses against any timestamped one
			e.sv.Updated = a.UpdatedAt.UTC().Format(lastUpdatedLayout)
		}
		if old, ok := newest[k]; !ok || e.build > old.build || (e.build == old.build && e.sv.Updated > old.sv.Updated) {
			newest[k] = e
		}
	}
	if !found {
		return nil
	}
	if snap == nil {
		snap = &Snapshot{LocalCopy: true}
	}
	vs := &Versioning{Snapshot: snap, LastUpdated: lastUpdated.UTC().Format(lastUpdatedLayout)}
	for _, e := range newest {
		vs.SnapshotVersions = append(vs.SnapshotVersions, e.sv)
	}
	sortSnapshotVersions(vs.SnapshotVersions)
	return &Metadata{ModelVersion: "1.1.0", GroupID: groupID, ArtifactID: artifactID, Version: version, Versioning: vs}
}
