package maven

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Checksum algorithms served and accepted as side files, in the order the
// X-Checksum-* response headers are written.
var checksumAlgos = []string{"md5", "sha1", "sha256", "sha512"}

const metadataFile = "maven-metadata.xml"

// pathKind classifies a repository path.
type pathKind int

const (
	// kindOther is a well-formed path that is not part of the Maven layout
	// (archetype-catalog.xml, .index/...).
	kindOther pathKind = iota
	// kindArtifact is groupId/artifactId/version/artifactId-version[-classifier].ext.
	kindArtifact
	// kindMetadata is a maven-metadata.xml at group, artifact or snapshot-version level.
	kindMetadata
	// kindChecksum is <any path>.(md5|sha1|sha256|sha512).
	kindChecksum
)

// mavenPath is a parsed repository path.
type mavenPath struct {
	path string // cleaned, no leading/trailing slash
	kind pathKind

	// kindChecksum: algorithm and the path the checksum describes.
	algo string
	base *mavenPath

	// kindArtifact (all) and kindMetadata (those that apply).
	groupID    string // dotted
	artifactID string
	version    string // base version (directory), e.g. 1.0-SNAPSHOT
	// kindArtifact only.
	fileVersion string // version in the file name: 1.0, 1.0-SNAPSHOT or 1.0-20260101.120000-3
	classifier  string
	extension   string // jar, pom, tar.gz, jar.asc ...
	timestamp   string // yyyyMMdd.HHmmss for timestamped snapshots
	buildNumber int

	// kindMetadata: dir is the directory holding the document.
	dir string
}

// snapshot reports whether the path lies in a SNAPSHOT version.
func (p *mavenPath) snapshot() bool { return p.version != "" && isSnapshot(p.version) }

// Segments of groupId, artifactId, version and file names: a conservative
// character set that is safe in URLs, file systems and HTML.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_.+~-]+$`)

var timestampRE = regexp.MustCompile(`^(\d{8}\.\d{6})-(\d+)`)

// parsePath parses a repository-relative path. It returns ErrInvalid for
// paths with empty, "." or ".." segments or characters outside segmentRE.
// An empty path (the repository root) is not valid here.
func parsePath(raw string) (*mavenPath, error) {
	p := core.CleanPath(raw)
	if p == "" {
		return nil, fmt.Errorf("empty path: %w", core.ErrInvalid)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "." || s == ".." || !segmentRE.MatchString(s) {
			return nil, fmt.Errorf("path %q: invalid segment %q: %w", p, s, core.ErrInvalid)
		}
	}
	mp := &mavenPath{path: p}
	file := segs[len(segs)-1]
	for _, algo := range checksumAlgos {
		if b, ok := strings.CutSuffix(p, "."+algo); ok && !strings.HasSuffix(b, "/") && len(b) > 0 {
			base, err := parsePath(b)
			if err != nil {
				return nil, err
			}
			mp.kind, mp.algo, mp.base = kindChecksum, algo, base
			return mp, nil
		}
	}
	if file == metadataFile {
		mp.kind = kindMetadata
		dirSegs := segs[:len(segs)-1]
		mp.dir = strings.Join(dirSegs, "/")
		n := len(dirSegs)
		switch {
		case n >= 3 && isSnapshot(dirSegs[n-1]):
			mp.groupID = strings.Join(dirSegs[:n-2], ".")
			mp.artifactID = dirSegs[n-2]
			mp.version = dirSegs[n-1]
		case n >= 2:
			mp.groupID = strings.Join(dirSegs[:n-1], ".")
			mp.artifactID = dirSegs[n-1]
		}
		return mp, nil
	}
	if len(segs) >= 4 && parseArtifactFile(mp, segs) {
		mp.kind = kindArtifact
	}
	return mp, nil
}

// parseArtifactFile fills mp from groupId/.../artifactId/version/file and
// reports whether the file name follows the layout for that GAV.
func parseArtifactFile(mp *mavenPath, segs []string) bool {
	n := len(segs)
	file, version, artifactID := segs[n-1], segs[n-2], segs[n-3]
	rest, ok := strings.CutPrefix(file, artifactID+"-")
	if !ok {
		return false
	}
	var fileVersion, ts string
	build := 0
	if r, ok := strings.CutPrefix(rest, version); ok && (r == "" || r[0] == '.' || r[0] == '-') {
		fileVersion, rest = version, r
	} else if isSnapshot(version) {
		prefix := strings.TrimSuffix(version, "SNAPSHOT")
		r, ok := strings.CutPrefix(rest, prefix)
		if !ok {
			return false
		}
		m := timestampRE.FindStringSubmatch(r)
		if m == nil {
			return false
		}
		b, err := strconv.Atoi(m[2])
		if err != nil {
			return false
		}
		ts, build = m[1], b
		fileVersion = prefix + m[0]
		rest = r[len(m[0]):]
	} else {
		return false
	}
	var classifier, ext string
	switch {
	case strings.HasPrefix(rest, "-"):
		c, e, ok := strings.Cut(rest[1:], ".")
		if !ok || c == "" || e == "" {
			return false
		}
		classifier, ext = c, e
	case strings.HasPrefix(rest, "."):
		ext = rest[1:]
	}
	if ext == "" || strings.HasPrefix(ext, ".") || strings.HasSuffix(ext, ".") {
		return false
	}
	mp.groupID = strings.Join(segs[:n-3], ".")
	mp.artifactID = artifactID
	mp.version = version
	mp.fileVersion = fileVersion
	mp.classifier = classifier
	mp.extension = ext
	mp.timestamp = ts
	mp.buildNumber = build
	return true
}

// contentType returns the media type served for a file name.
func contentType(name string) string {
	ext := name
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		ext = name[i+1:]
	}
	switch {
	case strings.HasSuffix(ext, ".asc"):
		return "application/pgp-signature"
	case strings.HasSuffix(ext, ".pom"), strings.HasSuffix(ext, ".xml"):
		return "application/xml"
	case strings.HasSuffix(ext, ".jar"), strings.HasSuffix(ext, ".war"), strings.HasSuffix(ext, ".ear"),
		strings.HasSuffix(ext, ".aar"):
		return "application/java-archive"
	case strings.HasSuffix(ext, ".module"), strings.HasSuffix(ext, ".json"):
		return "application/json"
	case strings.HasSuffix(ext, ".zip"):
		return "application/zip"
	case strings.HasSuffix(ext, ".tar.gz"), strings.HasSuffix(ext, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(ext, ".md5"), strings.HasSuffix(ext, ".sha1"), strings.HasSuffix(ext, ".sha256"),
		strings.HasSuffix(ext, ".sha512"), strings.HasSuffix(ext, ".txt"):
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
