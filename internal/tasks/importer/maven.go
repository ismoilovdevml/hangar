package importer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Maven: the Maven 2 repository layout. Artifacts are "groupId:artifactId";
// their versions come from maven-metadata.xml. Patterns and an empty list
// need a source that serves directory listings (crawled from the groups
// the patterns name). A version's files come from its directory listing,
// the snapshot metadata, or, without either, the POM's packaging plus the
// usual -sources, -javadoc, .module and .asc companions. Files are deployed
// with PUT, as Maven does.

func init() {
	register("maven", func(j *job) formatAdapter { return &mavenAdapter{j: j} })
	warmers["maven"] = warmMaven
}

// Crawl bounds.
const (
	mavenMaxDepth = 16
	mavenMaxDirs  = 50_000
)

type mavenAdapter struct{ j *job }

func splitGA(name string) (group, artifact string, err error) {
	g, a, ok := strings.Cut(name, ":")
	if !ok || g == "" || a == "" || strings.ContainsAny(name, "/\\ ") {
		return "", "", fmt.Errorf("maven name %q: want groupId:artifactId: %w", name, core.ErrInvalid)
	}
	return g, a, nil
}

func gaPath(group, artifact string) string {
	return strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/"
}

func (a *mavenAdapter) listing(ctx context.Context, dir string) ([]string, error) {
	return a.j.listing(ctx, dir)
}

// listing returns the entries of a directory listing of the source (names,
// "/" suffix for directories), or nil when the source serves none.
func (j *job) listing(ctx context.Context, dir string) ([]string, error) {
	u, err := j.src.resolve(dir)
	if err != nil {
		return nil, err
	}
	body, hdr, err := j.src.bytes(ctx, dir, http.Header{"Accept": {"text/html"}})
	if err != nil {
		return nil, err
	}
	if !strings.Contains(hdr.Get("Content-Type"), "html") {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range anchorRE.FindAllSubmatch(body, -1) {
		attrs := map[string]string{}
		for _, at := range attrRE.FindAllStringSubmatch(string(m[1]), -1) {
			attrs[strings.ToLower(at[1])] = at[2] + at[3] + at[4]
		}
		href := attrs["href"]
		if href == "" || strings.HasPrefix(href, "?") || strings.HasPrefix(href, "#") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		abs := u.ResolveReference(ref)
		if !j.src.sameHost(abs) || !strings.HasPrefix(abs.Path, u.Path) || abs.Path == u.Path {
			continue // parent, sibling or external link
		}
		rest := strings.TrimPrefix(abs.Path, u.Path)
		if strings.Count(strings.TrimSuffix(rest, "/"), "/") > 0 {
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, rest)
		}
	}
	return out, nil
}

func (a *mavenAdapter) list(ctx context.Context) ([]string, error) {
	starts := []string{""}
	if len(a.j.patterns) > 0 {
		starts = starts[:0]
		for _, p := range a.j.patterns {
			g, _, _ := strings.Cut(p, ":")
			if i := strings.IndexAny(g, "*?"); i >= 0 {
				g = g[:i]
				if j := strings.LastIndexByte(g, '.'); j >= 0 {
					g = g[:j+1]
				} else {
					g = ""
				}
			}
			g = strings.TrimSuffix(g, ".")
			if g == "" {
				starts = append(starts, "")
			} else {
				starts = append(starts, strings.ReplaceAll(g, ".", "/")+"/")
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	visited := 0
	type dir struct {
		path  string
		depth int
	}
	for _, s := range dedupe(starts) {
		queue := []dir{{s, 0}}
		for len(queue) > 0 {
			d := queue[0]
			queue = queue[1:]
			if visited++; visited > mavenMaxDirs {
				return nil, fmt.Errorf("more than %d directories at the source; narrow the patterns: %w", mavenMaxDirs, core.ErrInvalid)
			}
			entries, err := a.listing(ctx, d.path)
			if err != nil {
				if d.path == s && d.depth == 0 && (errors.Is(err, errNotFound) || errors.Is(err, core.ErrForbidden)) && len(out) == 0 {
					return nil, errNoListing
				}
				if ctx.Err() != nil {
					return nil, err
				}
				continue
			}
			if entries == nil && d.depth == 0 {
				return nil, errNoListing
			}
			if slices.Contains(entries, "maven-metadata.xml") && d.path != "" {
				p := strings.Split(strings.TrimSuffix(d.path, "/"), "/")
				if len(p) >= 2 {
					ga := strings.Join(p[:len(p)-1], ".") + ":" + p[len(p)-1]
					if !seen[ga] {
						seen[ga] = true
						out = append(out, ga)
					}
				}
				continue // version directories below
			}
			if d.depth >= mavenMaxDepth {
				continue
			}
			for _, e := range entries {
				if strings.HasSuffix(e, "/") && !strings.HasPrefix(e, ".") {
					queue = append(queue, dir{d.path + e, d.depth + 1})
				}
			}
		}
	}
	return out, nil
}

type mavenMetadata struct {
	Versioning struct {
		Versions         []string `xml:"versions>version"`
		SnapshotVersions []struct {
			Classifier string `xml:"classifier"`
			Extension  string `xml:"extension"`
			Value      string `xml:"value"`
		} `xml:"snapshotVersions>snapshotVersion"`
	} `xml:"versioning"`
}

func (a *mavenAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	g, art, err := splitGA(name)
	if err != nil {
		return nil, err
	}
	b, _, err := a.j.src.bytes(ctx, gaPath(g, art)+"maven-metadata.xml", nil)
	if err != nil {
		return nil, err
	}
	var md mavenMetadata
	if err := xml.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("maven-metadata.xml of %s: %w", name, err)
	}
	out := make([]srcVersion, 0, len(md.Versioning.Versions))
	for _, v := range md.Versioning.Versions {
		out = append(out, srcVersion{Version: v})
	}
	return out, nil
}

type mavenPOM struct {
	Packaging string `xml:"packaging"`
}

// mainExtension is the extension of the main artifact for a packaging ("" for pom).
func mainExtension(packaging string) string {
	switch packaging {
	case "", "jar", "bundle", "maven-plugin", "eclipse-plugin", "hk2-jar", "ejb", "takari-jar":
		return "jar"
	case "pom":
		return ""
	}
	return packaging
}

func isChecksumFile(name string) bool {
	for _, ext := range []string{".md5", ".sha1", ".sha256", ".sha512"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return strings.HasPrefix(name, "maven-metadata")
}

// files lists the file names of a version.
func (a *mavenAdapter) files(ctx context.Context, art, v, dir string) ([]string, error) {
	if strings.HasSuffix(v, "-SNAPSHOT") {
		if b, _, err := a.j.src.bytes(ctx, dir+"maven-metadata.xml", nil); err == nil {
			var md mavenMetadata
			if xml.Unmarshal(b, &md) == nil && len(md.Versioning.SnapshotVersions) > 0 {
				var out []string
				for _, sv := range md.Versioning.SnapshotVersions {
					f := art + "-" + sv.Value
					if sv.Classifier != "" {
						f += "-" + sv.Classifier
					}
					out = append(out, f+"."+sv.Extension)
				}
				return out, nil
			}
		}
	}
	if entries, err := a.listing(ctx, dir); err == nil && len(entries) > 0 {
		var out []string
		for _, e := range entries {
			if !strings.HasSuffix(e, "/") && !isChecksumFile(e) {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	} else if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	base := art + "-" + v
	pom, _, err := a.j.src.bytes(ctx, dir+base+".pom", nil)
	if err != nil {
		return nil, err
	}
	var p mavenPOM
	_ = xml.Unmarshal(pom, &p)
	out := []string{base + ".pom", base + ".module"}
	if ext := mainExtension(strings.TrimSpace(p.Packaging)); ext != "" {
		out = append(out, base+"."+ext, base+"-sources.jar", base+"-javadoc.jar")
	}
	for _, f := range slices.Clone(out) {
		out = append(out, f+".asc")
	}
	return out, nil
}

func (a *mavenAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	g, art, err := splitGA(name)
	if err != nil {
		return 0, err
	}
	dir := gaPath(g, art) + v.Version + "/"
	files, err := a.files(ctx, art, v.Version, dir)
	if err != nil {
		return 0, err
	}
	// The POM first: Maven deploys it with the main artifact, and its
	// date is the version's publish date.
	slices.SortStableFunc(files, func(x, y string) int {
		return boolInt(!strings.HasSuffix(x, ".pom")) - boolInt(!strings.HasSuffix(y, ".pom"))
	})
	have, err := a.j.existingAssets(ctx, g, art, v.Version)
	if err != nil {
		return 0, err
	}
	result := skipped
	var published time.Time
	copied := 0
	for _, f := range files {
		o, at, err := a.copyFile(ctx, dir+f, have, published)
		if errors.Is(err, errNotFound) {
			continue // a companion this version does not have
		}
		if err != nil {
			return 0, fmt.Errorf("%s: %w", f, err)
		}
		copied++
		if published.IsZero() {
			published = at
		}
		switch {
		case o == imported:
			result = imported
		case o == conflict && result == skipped:
			result = conflict
		}
	}
	if copied == 0 {
		return 0, fmt.Errorf("no files of %s %s at the source: %w", name, v.Version, errNotFound)
	}
	return result, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (a *mavenAdapter) copyFile(ctx context.Context, rel string, have []*core.Asset, published time.Time) (outcome, time.Time, error) {
	var asset *core.Asset
	for _, x := range have {
		if x.Path == rel {
			asset = x
		}
	}
	if asset != nil {
		if sha1, _, err := a.j.src.bytes(ctx, rel+".sha1", nil); err == nil {
			sum := strings.ToLower(strings.TrimSpace(strings.Fields(string(sha1) + " ")[0]))
			if hasContent([]*core.Asset{asset}, sums{"sha1": sum}) {
				return skipped, time.Time{}, nil
			}
		}
	}
	dl, err := a.j.fetch(ctx, rel, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer dl.Close()
	if asset != nil {
		if hasContent([]*core.Asset{asset}, sums{"sha256": dl.sha256, "sha1": dl.sha1}) {
			return skipped, dl.modified, nil
		}
		return conflict, dl.modified, nil
	}
	at := published
	if at.IsZero() {
		at = dl.modified
	}
	body, err := dl.reader()
	if err != nil {
		return 0, time.Time{}, err
	}
	hdr := http.Header{"X-Checksum-Sha1": {dl.sha1}, "Content-Type": {"application/octet-stream"}}
	resp, err := a.j.put(ctx, http.MethodPut, rel, hdr, body, dl.size, at)
	if err != nil {
		return 0, time.Time{}, err
	}
	o, err := uploadOutcome(resp, rel)
	return o, dl.modified, err
}

func (a *mavenAdapter) coords(name string, v srcVersion) []string { return []string{name + ":" + v.Version} }

// warmMaven fetches the POM and the main artifact.
func warmMaven(ctx context.Context, w *warmJob, c coord) error {
	g, art, err := splitGA(c.name)
	if err != nil {
		return err
	}
	dir := gaPath(g, art) + c.version + "/"
	base := art + "-" + c.version
	r, err := w.get(ctx, dir+base+".pom", nil, 1<<20)
	if err != nil {
		return err
	}
	ext := c.ext
	if ext == "" {
		var p mavenPOM
		_ = xml.Unmarshal(r.Body, &p)
		ext = mainExtension(strings.TrimSpace(p.Packaging))
	}
	if ext == "" || ext == "pom" {
		return nil
	}
	file := base
	if c.classifier != "" {
		file += "-" + c.classifier
	}
	_, err = w.get(ctx, dir+file+"."+ext, nil, 0)
	return err
}
