package npm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Media types.
const (
	mediaAbbreviated = "application/vnd.npm.install-v1+json"
	mediaJSON        = "application/json"
)

// manifest is one version document; values are kept as raw JSON so fields
// Hangar does not interpret round-trip unchanged.
type manifest map[string]json.RawMessage

func (m manifest) str(key string) string {
	var s string
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func (m manifest) setStr(key, v string) {
	b, _ := json.Marshal(v)
	m[key] = b
}

func (m manifest) dist() map[string]json.RawMessage {
	d := map[string]json.RawMessage{}
	if raw, ok := m["dist"]; ok {
		_ = json.Unmarshal(raw, &d)
	}
	return d
}

func (m manifest) distStr(key string) string {
	var s string
	if raw, ok := m.dist()[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func (m manifest) setDist(key, v string) {
	d := m.dist()
	b, _ := json.Marshal(v)
	d[key] = b
	m["dist"], _ = json.Marshal(d)
}

func (m manifest) clone() manifest { return maps.Clone(m) }

// packument is a package document. Inside Hangar, version dist.tarball holds
// a path relative to the repository that produced the document
// ("@s/n/-/n-1.0.0.tgz"); render turns it into an absolute URL.
type packument struct {
	Name     string
	DistTags map[string]string
	Time     map[string]string
	Versions map[string]manifest
	Modified string
	// Extra holds every other top-level field (description, readme, users, ...).
	Extra map[string]json.RawMessage
	// origins maps relative tarball paths to the absolute URLs a proxied
	// packument gave for tarballs outside the remote URL (a registry's
	// tarball CDN). Set by relativizeTarballs; never serialised.
	origins map[string]string
}

func newPackument(name string) *packument {
	return &packument{Name: name, DistTags: map[string]string{}, Time: map[string]string{},
		Versions: map[string]manifest{}, Extra: map[string]json.RawMessage{}}
}

// maxDocumentSize bounds packuments Hangar parses (upstream or cached).
const maxDocumentSize = 256 << 20

// parsePackument decodes a packument (full or abbreviated).
func parsePackument(r io.Reader) (*packument, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(io.LimitReader(r, maxDocumentSize))
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("decode packument: %w: %w", err, core.ErrInvalid)
	}
	p := newPackument("")
	for k, v := range top {
		var err error
		switch k {
		case "name":
			err = json.Unmarshal(v, &p.Name)
		case "dist-tags":
			err = json.Unmarshal(v, &p.DistTags)
		case "time":
			// Values are strings, except for legacy "unpublished" objects.
			var t map[string]json.RawMessage
			if err = json.Unmarshal(v, &t); err == nil {
				for tk, tv := range t {
					var s string
					if json.Unmarshal(tv, &s) == nil {
						p.Time[tk] = s
					}
				}
			}
		case "versions":
			err = json.Unmarshal(v, &p.Versions)
		case "modified":
			err = json.Unmarshal(v, &p.Modified)
		case "_id", "_rev", "_attachments":
		default:
			p.Extra[k] = v
		}
		if err != nil {
			return nil, fmt.Errorf("decode packument field %q: %w: %w", k, err, core.ErrInvalid)
		}
	}
	if p.DistTags == nil {
		p.DistTags = map[string]string{}
	}
	if p.Versions == nil {
		p.Versions = map[string]manifest{}
	}
	for v, m := range p.Versions {
		if m == nil {
			delete(p.Versions, v)
		}
	}
	return p, nil
}

func (p *packument) versionList() []string { return slices.Collect(maps.Keys(p.Versions)) }

// normalizeTags drops dist-tags that point at missing versions and makes sure
// "latest" exists whenever there is a version.
func (p *packument) normalizeTags() {
	for t, v := range p.DistTags {
		if _, ok := p.Versions[v]; !ok {
			delete(p.DistTags, t)
		}
	}
	if _, ok := p.DistTags["latest"]; !ok && len(p.Versions) > 0 {
		if v := highestVersion(p.versionList()); v != "" {
			p.DistTags["latest"] = v
		}
	}
}

// rev is a revision string derived from the document content; npm echoes it
// back on unpublish, Hangar does not require it to match.
func (p *packument) rev() string {
	h := sha256.New()
	vs := p.versionList()
	slices.Sort(vs)
	for _, v := range vs {
		_, _ = io.WriteString(h, v+"\x00"+p.Versions[v].str("deprecated")+"\x00")
	}
	tags := slices.Collect(maps.Keys(p.DistTags))
	slices.Sort(tags)
	for _, t := range tags {
		_, _ = io.WriteString(h, t+"="+p.DistTags[t]+"\x00")
	}
	return strconv.Itoa(len(vs)+1) + "-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// hoisted are the fields the full packument copies from the latest version.
var hoisted = []string{"author", "bugs", "contributors", "description", "homepage", "keywords",
	"license", "maintainers", "readme", "readmeFilename", "repository"}

// abbreviatedFields are the per-version fields of the abbreviated packument.
var abbreviatedFields = []string{"name", "version", "dist", "deprecated", "dependencies",
	"acceptDependencies", "optionalDependencies", "devDependencies", "bundleDependencies",
	"bundledDependencies", "peerDependencies", "peerDependenciesMeta", "bin", "directories",
	"engines", "_hasShrinkwrap", "hasInstallScript", "funding", "cpu", "os", "libc"}

// render serialises p. tarballBase is the absolute URL of the serving
// repository, without a trailing slash; relative tarball paths are joined to it.
func (p *packument) render(tarballBase string, abbreviated bool) ([]byte, error) {
	versions := make(map[string]manifest, len(p.Versions))
	for v, m := range p.Versions {
		out := m
		if abbreviated {
			out = manifest{}
			for _, f := range abbreviatedFields {
				if raw, ok := m[f]; ok {
					out[f] = raw
				}
			}
		} else {
			out = m.clone()
		}
		if t := out.distStr("tarball"); t != "" && !strings.Contains(t, "://") {
			out.setDist("tarball", tarballBase+"/"+t)
		}
		versions[v] = out
	}
	top := map[string]any{"name": p.Name, "dist-tags": p.DistTags, "versions": versions}
	if abbreviated {
		mod := p.Modified
		if mod == "" {
			mod = p.Time["modified"]
		}
		if mod != "" {
			top["modified"] = mod
		}
	} else {
		for k, v := range p.Extra {
			top[k] = v
		}
		if latest, ok := p.Versions[p.DistTags["latest"]]; ok {
			for _, f := range hoisted {
				if raw, ok := latest[f]; ok {
					if _, set := p.Extra[f]; !set {
						top[f] = raw
					}
				}
			}
		}
		top["_id"] = p.Name
		top["_rev"] = p.rev()
		if len(p.Time) > 0 {
			top["time"] = p.Time
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(top); err != nil {
		return nil, fmt.Errorf("encode packument: %w", err)
	}
	return buf.Bytes(), nil
}

// renderVersion serialises one version document with an absolute tarball URL.
func renderVersion(m manifest, tarballBase string) ([]byte, error) {
	out := m.clone()
	if t := out.distStr("tarball"); t != "" && !strings.Contains(t, "://") {
		out.setDist("tarball", tarballBase+"/"+t)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("encode version: %w", err)
	}
	return buf.Bytes(), nil
}

// relativizeTarballs rewrites absolute upstream tarball URLs to paths
// relative to the repository: a URL under remote keeps its suffix
// ("https://registry.npmjs.org/@s/n/-/n-1.0.0.tgz" -> "@s/n/-/n-1.0.0.tgz");
// any other URL maps to "<pkg>/-/<file name>".
func (p *packument) relativizeTarballs(n pkgName, remote string) {
	remote = strings.TrimSuffix(remote, "/") + "/"
	for _, m := range p.Versions {
		t := m.distStr("tarball")
		if t == "" {
			continue
		}
		rel := ""
		if s, ok := strings.CutPrefix(t, remote); ok {
			if d, err := decodePath(s); err == nil && strings.HasPrefix(d, n.Full+"/-/") {
				rel = d
			}
		}
		if rel == "" {
			name, _, _ := strings.Cut(t, "?")
			rel = n.Full + "/-/" + name[strings.LastIndex(name, "/")+1:]
			if strings.HasPrefix(t, "https://") || strings.HasPrefix(t, "http://") {
				if p.origins == nil {
					p.origins = map[string]string{}
				}
				p.origins[rel] = t
			}
		}
		m.setDist("tarball", rel)
	}
}

// versionForTarball finds the version whose (relative) tarball path is rel.
func (p *packument) versionForTarball(rel string) (string, manifest) {
	for v, m := range p.Versions {
		if m.distStr("tarball") == rel {
			return v, m
		}
	}
	return "", nil
}

// merge adds q's versions, dist-tags and times that p does not have yet
// (first document wins), for group repositories.
func (p *packument) merge(q *packument) {
	if p.Name == "" {
		p.Name = q.Name
	}
	for v, m := range q.Versions {
		if _, ok := p.Versions[v]; !ok {
			p.Versions[v] = m
		}
	}
	for t, v := range q.DistTags {
		if _, ok := p.DistTags[t]; !ok {
			p.DistTags[t] = v
		}
	}
	for k, v := range q.Time {
		if k == "modified" {
			if v > p.Time[k] {
				p.Time[k] = v
			}
			continue
		}
		if _, ok := p.Time[k]; !ok {
			p.Time[k] = v
		}
	}
	if q.Modified > p.Modified {
		p.Modified = q.Modified
	}
	for k, v := range q.Extra {
		if _, ok := p.Extra[k]; !ok {
			p.Extra[k] = v
		}
	}
}
