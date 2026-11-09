package importer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// npm: the registry API. Listing uses /-/v1/search (per pattern prefix) or
// /-/all; versions and metadata come from the full packument; a version is
// published with the npm publish document (PUT /<name>), dist-tags are
// carried over afterwards.

func init() {
	register("npm", func(j *job) formatAdapter { return &npmAdapter{j: j, tags: map[string]map[string]string{}} })
	warmers["npm"] = warmNpm
}

type npmAdapter struct {
	j    *job
	mu   sync.Mutex
	tags map[string]map[string]string // dist-tags by package
}

// npmEscape is the URL form of a package name: "@scope%2fname".
func npmEscape(name string) string {
	return strings.Replace(url.PathEscape(name), "%2F", "%2f", 1)
}

func npmScope(name string) (scope, base string) {
	if strings.HasPrefix(name, "@") {
		if s, b, ok := strings.Cut(name, "/"); ok {
			return s, b
		}
	}
	return "", name
}

func (a *npmAdapter) list(ctx context.Context) ([]string, error) {
	// The CouchDB-era listing, still served by some registries.
	var all map[string]json.RawMessage
	if err := a.j.src.json(ctx, "-/all", &all); err == nil && len(all) > 0 {
		out := make([]string, 0, len(all))
		for k := range all {
			if k != "_updated" {
				out = append(out, k)
			}
		}
		return out, nil
	} else if err != nil && ctx.Err() != nil {
		return nil, err
	}
	texts := []string{}
	for _, p := range a.j.patterns {
		if i := strings.IndexAny(p, "*?"); i >= 0 {
			p = p[:i]
		}
		texts = append(texts, strings.TrimSuffix(p, "/"))
	}
	if len(texts) == 0 {
		texts = []string{""}
	}
	seen := map[string]bool{}
	var out []string
	for _, text := range texts {
		for from := 0; ; {
			var page struct {
				Objects []struct {
					Package struct {
						Name string `json:"name"`
					} `json:"package"`
				} `json:"objects"`
				Total int `json:"total"`
			}
			q := url.Values{"text": {text}, "size": {"250"}, "from": {strconv.Itoa(from)}}
			if err := a.j.src.json(ctx, "-/v1/search?"+q.Encode(), &page); err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				if from == 0 && len(out) == 0 {
					return nil, errNoListing
				}
				return nil, err
			}
			for _, o := range page.Objects {
				if n := o.Package.Name; n != "" && !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
			from += len(page.Objects)
			if len(page.Objects) == 0 || from >= page.Total || from >= MaxListedPackages {
				break
			}
		}
	}
	return out, nil
}

type npmPackument struct {
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
	Time     map[string]string          `json:"time"`
}

func (a *npmAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	var doc npmPackument
	if err := a.j.src.json(ctx, npmEscape(name), &doc); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.tags[name] = doc.DistTags
	a.mu.Unlock()
	out := make([]srcVersion, 0, len(doc.Versions))
	for v, m := range doc.Versions {
		sv := srcVersion{Version: v, Data: m}
		if t, err := time.Parse(time.RFC3339, doc.Time[v]); err == nil {
			sv.Published = t
		}
		out = append(out, sv)
	}
	return out, nil
}

type npmDist struct {
	Tarball   string `json:"tarball"`
	Shasum    string `json:"shasum"`
	Integrity string `json:"integrity"`
}

func npmSums(d npmDist) sums {
	s := sums{"sha1": strings.ToLower(d.Shasum)}
	for _, tok := range strings.Fields(d.Integrity) {
		algo, b64, ok := strings.Cut(tok, "-")
		if !ok {
			continue
		}
		b64, _, _ = strings.Cut(b64, "?")
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil && (algo == "sha512" || algo == "sha256" || algo == "sha1") {
			s[algo] = hex.EncodeToString(raw)
		}
	}
	return s
}

func (a *npmAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	raw, _ := v.Data.(json.RawMessage)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("version document: %w", err)
	}
	var dist npmDist
	_ = json.Unmarshal(m["dist"], &dist)
	if dist.Tarball == "" {
		return 0, errors.New("version document has no dist.tarball")
	}
	scope, base := npmScope(name)
	have, err := a.j.existingAssets(ctx, scope, base, v.Version)
	if err != nil {
		return 0, err
	}
	if have != nil {
		if hasContent(have, npmSums(dist)) {
			return skipped, nil
		}
		return conflict, nil
	}
	dl, err := a.j.fetch(ctx, dist.Tarball, nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	// Fields npm adds on the server side are not part of a publish.
	for _, k := range []string{"_npmUser", "_id", "_resolved", "_from", "_nodeVersion", "_npmVersion", "_hasShrinkwrap", "_npmOperationalInternal", "maintainers"} {
		delete(m, k)
	}
	if dist.Integrity == "" {
		dist.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(mustHex(dl.sha512))
	}
	distDoc := map[string]any{"shasum": dl.sha1, "integrity": dist.Integrity, "tarball": dist.Tarball}
	m["dist"], _ = json.Marshal(distDoc)
	head, err := json.Marshal(map[string]any{
		"_id": name, "name": name,
		"versions":  map[string]any{v.Version: m},
		"dist-tags": map[string]string{},
	})
	if err != nil {
		return 0, err
	}
	file := base + "-" + v.Version + ".tgz"
	// {...head without the closing brace, "_attachments":{file:{...,"data":"<base64>"}}}
	prefix := string(head[:len(head)-1]) + `,"_attachments":{` + jsonString(file) +
		`:{"content_type":"application/octet-stream","length":` + strconv.FormatInt(dl.size, 10) + `,"data":"`
	suffix := `"}}}`
	r, err := dl.reader()
	if err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	go func() {
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		_, err := io.WriteString(pw, prefix)
		if err == nil {
			_, err = io.Copy(enc, r)
		}
		if err == nil {
			err = enc.Close()
		}
		if err == nil {
			_, err = io.WriteString(pw, suffix)
		}
		pw.CloseWithError(err)
	}()
	size := int64(len(prefix)) + int64(base64.StdEncoding.EncodedLen(int(dl.size))) + int64(len(suffix))
	resp, err := a.j.put(ctx, http.MethodPut, npmEscape(name), http.Header{"Content-Type": {"application/json"}}, pr, size, v.Published)
	_ = pr.Close()
	if err != nil {
		return 0, err
	}
	return uploadOutcome(resp, name+"@"+v.Version)
}

func (a *npmAdapter) finish(ctx context.Context, name string, all []srcVersion) error {
	a.mu.Lock()
	tags := a.tags[name]
	delete(a.tags, name)
	a.mu.Unlock()
	scope, base := npmScope(name)
	for tag, v := range tags {
		if c, err := a.j.s.opts.Components.GetComponent(ctx, a.j.repo.ID, scope, base, v); err != nil || c == nil {
			continue // that version was not imported
		}
		body := jsonString(v)
		r, err := a.j.put(ctx, http.MethodPut, "-/package/"+npmEscape(name)+"/dist-tags/"+url.PathEscape(tag),
			http.Header{"Content-Type": {"application/json"}}, strings.NewReader(body), int64(len(body)), time.Time{})
		if err != nil {
			return err
		}
		if !r.ok() {
			return fmt.Errorf("dist-tag %s: %s", tag, r.message())
		}
	}
	return nil
}

func (a *npmAdapter) coords(name string, v srcVersion) []string {
	return []string{name + "@" + v.Version}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// warmNpm fetches the packument npm install asks for, then the tarball.
func warmNpm(ctx context.Context, w *warmJob, c coord) error {
	var doc struct {
		Versions map[string]struct {
			Dist npmDist `json:"dist"`
		} `json:"versions"`
	}
	hdr := http.Header{"Accept": {"application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8"}}
	if err := w.jsonDoc(ctx, npmEscape(c.name), hdr, &doc); err != nil {
		return err
	}
	ver, ok := doc.Versions[c.version]
	if !ok || ver.Dist.Tarball == "" {
		return fmt.Errorf("version %s is not in the registry: %w", c.version, errNotFound)
	}
	return w.fetchLink(ctx, repoPath(w.repo.Name, ""), ver.Dist.Tarball)
}
