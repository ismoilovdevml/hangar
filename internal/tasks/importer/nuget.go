package importer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NuGet: the V3 service index (registration pages for versions, publish
// dates and listing state, the flat container for packages, the search
// service for listing), or a V2 feed (FindPackagesById). Packages are pushed
// with PUT api/v2/package; unlisted versions are unlisted again.

func init() {
	register("nuget", func(j *job) formatAdapter { return &nugetAdapter{j: j} })
	warmers["nuget"] = warmNuGet
}

type nugetAdapter struct {
	j    *job
	once sync.Once
	err  error
	// V3 resources (empty for a V2 feed)
	flat, reg, search string
	v2                string
}

type nugetLeaf struct {
	content   string
	listed    bool
	hash      string // sha512 hex
	published time.Time
}

func (a *nugetAdapter) init(ctx context.Context) error {
	a.once.Do(func() {
		var idx struct {
			Resources []struct {
				ID   string `json:"@id"`
				Type string `json:"@type"`
			} `json:"resources"`
		}
		ref := ""
		if !strings.HasSuffix(a.j.src.base.Path, ".json") {
			ref = "index.json"
		}
		err := a.j.src.json(ctx, ref, &idx)
		if err != nil {
			if errors.Is(err, errNotFound) || strings.Contains(err.Error(), "expected JSON") {
				a.v2 = strings.TrimSuffix(a.j.src.base.String(), "/") + "/"
				return
			}
			a.err = err
			return
		}
		for _, r := range idx.Resources {
			id := r.ID
			if !strings.HasSuffix(id, "/") {
				id += "/"
			}
			switch {
			case strings.HasPrefix(r.Type, "PackageBaseAddress"):
				a.flat = id
			case strings.HasPrefix(r.Type, "RegistrationsBaseUrl") && (a.reg == "" || strings.Contains(r.Type, "3.6.0")):
				a.reg = id
			case strings.HasPrefix(r.Type, "SearchQueryService"):
				a.search = strings.TrimSuffix(id, "/")
			}
		}
		if a.flat == "" {
			a.err = errors.New("the service index has no PackageBaseAddress resource")
		}
	})
	return a.err
}

func (a *nugetAdapter) list(ctx context.Context) ([]string, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	if a.search == "" {
		return nil, errNoListing
	}
	var out []string
	seen := map[string]bool{}
	for skip := 0; skip < MaxListedPackages; {
		var page struct {
			TotalHits int `json:"totalHits"`
			Data      []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		q := url.Values{"q": {""}, "skip": {strconv.Itoa(skip)}, "take": {"1000"}, "prerelease": {"true"}, "semVerLevel": {"2.0.0"}}
		if err := a.j.src.json(ctx, a.search+"?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, d := range page.Data {
			if !seen[strings.ToLower(d.ID)] {
				seen[strings.ToLower(d.ID)] = true
				out = append(out, d.ID)
			}
		}
		skip += len(page.Data)
		if len(page.Data) == 0 || skip >= page.TotalHits {
			break
		}
	}
	return out, nil
}

func (a *nugetAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	if a.v2 != "" {
		return a.versionsV2(ctx, name)
	}
	lid := strings.ToLower(name)
	if a.reg != "" {
		out, err := a.registration(ctx, lid)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, errNotFound) || ctx.Err() != nil {
			return nil, err
		}
	}
	var flat struct {
		Versions []string `json:"versions"`
	}
	if err := a.j.src.json(ctx, a.flat+url.PathEscape(lid)+"/index.json", &flat); err != nil {
		return nil, err
	}
	out := make([]srcVersion, 0, len(flat.Versions))
	for _, v := range flat.Versions {
		out = append(out, srcVersion{Version: v, Data: nugetLeaf{listed: true}})
	}
	return out, nil
}

type nugetRegPage struct {
	ID    string `json:"@id"`
	Items []struct {
		CatalogEntry struct {
			Version     string `json:"version"`
			Published   string `json:"published"`
			Listed      *bool  `json:"listed"`
			PackageHash string `json:"packageHash"`
			HashAlgo    string `json:"packageHashAlgorithm"`
		} `json:"catalogEntry"`
		PackageContent string `json:"packageContent"`
	} `json:"items"`
}

func (a *nugetAdapter) registration(ctx context.Context, lid string) ([]srcVersion, error) {
	var idx struct {
		Items []nugetRegPage `json:"items"`
	}
	if err := a.j.src.json(ctx, a.reg+url.PathEscape(lid)+"/index.json", &idx); err != nil {
		return nil, err
	}
	var out []srcVersion
	for _, page := range idx.Items {
		if page.Items == nil && page.ID != "" {
			if err := a.j.src.json(ctx, page.ID, &page); err != nil {
				return nil, err
			}
		}
		for _, it := range page.Items {
			ce := it.CatalogEntry
			leaf := nugetLeaf{content: it.PackageContent, listed: ce.Listed == nil || *ce.Listed}
			pub, err := time.Parse(time.RFC3339, ce.Published)
			if err == nil && pub.Year() > 1900 {
				// 1900-01-01 marks an unlisted package in the V3 protocol.
				leaf.published = pub
			} else if err == nil && pub.Year() <= 1900 {
				leaf.listed = false
			}
			if strings.EqualFold(ce.HashAlgo, "SHA512") {
				if raw, err := base64.StdEncoding.DecodeString(ce.PackageHash); err == nil {
					leaf.hash = hex.EncodeToString(raw)
				}
			}
			out = append(out, srcVersion{Version: ce.Version, Published: leaf.published, Data: leaf})
		}
	}
	return out, nil
}

// versionsV2 reads a V2 feed's FindPackagesById() (Atom), following next links.
func (a *nugetAdapter) versionsV2(ctx context.Context, name string) ([]srcVersion, error) {
	type entry struct {
		Content struct {
			Src string `xml:"src,attr"`
		} `xml:"content"`
		Version   string `xml:"properties>Version"`
		Published string `xml:"properties>Published"`
		Listed    string `xml:"properties>Listed"`
		Hash      string `xml:"properties>PackageHash"`
		HashAlgo  string `xml:"properties>PackageHashAlgorithm"`
	}
	type feed struct {
		Entries []entry `xml:"entry"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
	}
	ref := a.v2 + "FindPackagesById()?" + "id=" + url.QueryEscape("'"+name+"'") + "&semVerLevel=2.0.0"
	var out []srcVersion
	for page := 0; ref != "" && page < 1000; page++ {
		b, _, err := a.j.src.bytes(ctx, ref, http.Header{"Accept": {"application/atom+xml"}})
		if err != nil {
			return nil, err
		}
		var f feed
		if err := xml.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("V2 feed of %s: %w", name, err)
		}
		for _, e := range f.Entries {
			leaf := nugetLeaf{content: e.Content.Src, listed: !strings.EqualFold(e.Listed, "false")}
			if t, err := time.Parse(time.RFC3339, e.Published); err == nil && t.Year() > 1900 {
				leaf.published = t
			} else if err == nil {
				leaf.listed = false
			} else if t, err := time.Parse("2006-01-02T15:04:05.999", e.Published); err == nil && t.Year() > 1900 {
				leaf.published = t
			}
			if strings.EqualFold(e.HashAlgo, "SHA512") {
				if raw, err := base64.StdEncoding.DecodeString(e.Hash); err == nil {
					leaf.hash = hex.EncodeToString(raw)
				}
			}
			out = append(out, srcVersion{Version: e.Version, Published: leaf.published, Data: leaf})
		}
		ref = ""
		for _, l := range f.Links {
			if l.Rel == "next" {
				ref = l.Href
			}
		}
	}
	return out, nil
}

// normalizeNuGet is NuGet's normalized version: lower case, no build
// metadata, no leading zeros, at least three parts and no zero fourth part.
func normalizeNuGet(v string) string {
	v = strings.ToLower(v)
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	for i, p := range parts {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			parts[i] = strconv.FormatInt(n, 10)
		}
	}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	if len(parts) == 4 && parts[3] == "0" {
		parts = parts[:3]
	}
	out := strings.Join(parts, ".")
	if pre != "" {
		out += "-" + pre
	}
	return out
}

func (a *nugetAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	leaf, _ := v.Data.(nugetLeaf)
	lid, lver := strings.ToLower(name), normalizeNuGet(v.Version)
	have, err := a.j.existingAssets(ctx, "", lid, lver)
	if err != nil {
		return 0, err
	}
	if have != nil && leaf.hash != "" {
		if hasContent(have, sums{"sha512": leaf.hash}) {
			return skipped, nil
		}
		return conflict, nil
	}
	ref := leaf.content
	if ref == "" {
		ref = a.flat + url.PathEscape(lid) + "/" + url.PathEscape(lver) + "/" + url.PathEscape(lid+"."+lver) + ".nupkg"
	}
	dl, err := a.j.fetch(ctx, ref, nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if have != nil {
		if hasContent(have, sums{"sha256": dl.sha256, "sha512": dl.sha512}) {
			return skipped, nil
		}
		return conflict, nil
	}
	body, err := dl.reader()
	if err != nil {
		return 0, err
	}
	resp, err := a.j.put(ctx, http.MethodPut, "api/v2/package", http.Header{"Content-Type": {"application/octet-stream"}}, body, dl.size, v.Published)
	if err != nil {
		return 0, err
	}
	o, err := uploadOutcome(resp, name+" "+v.Version)
	if err != nil || o != imported || leaf.listed {
		return o, err
	}
	r, err := a.j.put(ctx, http.MethodDelete, "api/v2/package/"+url.PathEscape(name)+"/"+url.PathEscape(v.Version), nil, nil, 0, time.Time{})
	if err != nil {
		return o, err
	}
	if !r.ok() {
		a.j.log.Warnf("%s %s: imported, but unlisting it failed: %s", name, v.Version, r.message())
	}
	return o, nil
}

func (a *nugetAdapter) coords(name string, v srcVersion) []string {
	return []string{name + "@" + v.Version}
}

// warmNuGet fetches the version list and the package from the flat container.
func warmNuGet(ctx context.Context, w *warmJob, c coord) error {
	lid, lver := strings.ToLower(c.name), normalizeNuGet(c.version)
	if _, err := w.get(ctx, "v3-flatcontainer/"+url.PathEscape(lid)+"/index.json", nil, 0); err != nil {
		return err
	}
	_, err := w.get(ctx, "v3-flatcontainer/"+url.PathEscape(lid)+"/"+url.PathEscape(lver)+"/"+url.PathEscape(lid+"."+lver)+".nupkg", nil, 0)
	return err
}
