package nuget

import (
	"maps"
	"strconv"
	"strings"
	"time"
)

// Registration paging, as nuget.org does it: below inlineLimit versions every
// page is inlined in the index; from inlineLimit on, pages of pageSize are
// linked and fetched separately.
const (
	pageSize    = 64
	inlineLimit = 128
)

// unlistedPublished is the published date older clients read as "unlisted".
const unlistedPublished = "1900-01-01T00:00:00+00:00"

// regDoc renders registration documents for one package in one hive of the
// repository the client addressed.
type regDoc struct {
	h       *handler
	base    string // repository URL with trailing slash
	hive    string
	lid     string
	entries []entry // ascending
}

func (d *regDoc) hiveURL() string  { return d.base + "v3/" + d.hive + "/" }
func (d *regDoc) indexURL() string { return d.hiveURL() + d.lid + "/index.json" }
func (d *regDoc) leafURL(e entry) string {
	return d.hiveURL() + d.lid + "/" + e.ver.Lower() + ".json"
}
func (d *regDoc) contentURL(lid string, e entry) string {
	lver := e.ver.Lower()
	return d.base + "v3-flatcontainer/" + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"
}

func (d *regDoc) pages() [][]entry {
	var out [][]entry
	for i := 0; i < len(d.entries); i += pageSize {
		out = append(out, d.entries[i:min(i+pageSize, len(d.entries))])
	}
	return out
}

func (d *regDoc) pageURL(p []entry) string {
	return d.hiveURL() + d.lid + "/page/" + p[0].ver.Lower() + "/" + p[len(p)-1].ver.Lower() + ".json"
}

func (d *regDoc) pageObject(p []entry, inline bool) map[string]any {
	o := map[string]any{
		"@id":   d.pageURL(p),
		"count": len(p),
		"lower": p[0].ver.Normalized(),
		"upper": p[len(p)-1].ver.Normalized(),
	}
	if inline {
		items := make([]any, len(p))
		for i, e := range p {
			items[i] = d.leafObject(e)
		}
		o["@type"] = "catalog:CatalogPage"
		o["items"] = items
		o["parent"] = d.indexURL()
	}
	return o
}

// index renders the registration index.
func (d *regDoc) index() map[string]any {
	pages := d.pages()
	inline := len(d.entries) < inlineLimit
	items := make([]any, len(pages))
	for i, p := range pages {
		items[i] = d.pageObject(p, inline)
	}
	return map[string]any{
		"@id":   d.indexURL(),
		"@type": []string{"catalog:CatalogRoot", "PackageRegistration", "catalog:Permalink"},
		"count": len(pages),
		"items": items,
	}
}

// page renders the page with the given lowercased bounds, or nil.
func (d *regDoc) page(lower, upper string) map[string]any {
	for _, p := range d.pages() {
		if p[0].ver.Lower() == lower && p[len(p)-1].ver.Lower() == upper {
			return d.pageObject(p, true)
		}
	}
	return nil
}

// leaf renders the standalone registration leaf of a version, or nil.
func (d *regDoc) leaf(lver string) map[string]any {
	for _, e := range d.entries {
		if e.ver.Lower() == lver {
			return map[string]any{
				"@id":            d.leafURL(e),
				"@type":          []string{"Package", "http://schema.nuget.org/catalog#Permalink"},
				"catalogEntry":   d.leafURL(e),
				"listed":         e.listed,
				"packageContent": d.contentURL(d.lid, e),
				"published":      d.published(e),
				"registration":   d.indexURL(),
			}
		}
	}
	return nil
}

func (d *regDoc) published(e entry) string {
	if !e.listed {
		return unlistedPublished
	}
	if e.published.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	return e.published.UTC().Format(time.RFC3339Nano)
}

// leafObject renders a leaf as it appears inside a page.
func (d *regDoc) leafObject(e entry) map[string]any {
	content := d.contentURL(d.lid, e)
	return map[string]any{
		"@id":            d.leafURL(e),
		"@type":          "Package",
		"catalogEntry":   d.catalogObject(e, content),
		"packageContent": content,
		"registration":   d.indexURL(),
	}
}

// catalogObject renders the catalog entry, replacing every URL that would
// point elsewhere (upstream catalog, upstream content) with Hangar URLs.
func (d *regDoc) catalogObject(e entry, content string) map[string]any {
	c := maps.Clone(e.catalog)
	if c == nil {
		c = map[string]any{}
	}
	c["@id"] = d.leafURL(e)
	c["@type"] = "PackageDetails"
	c["packageContent"] = content
	c["listed"] = e.listed
	c["published"] = d.published(e)
	c["version"] = e.ver.Full()
	if _, ok := c["id"]; !ok {
		c["id"] = d.lid
	}
	delete(c, "catalog:commitId")
	delete(c, "catalog:commitTimeStamp")
	groups, _ := c["dependencyGroups"].([]any)
	out := make([]any, 0, len(groups))
	for gi, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		gm = maps.Clone(gm)
		gm["@id"] = d.leafURL(e) + "#dependencygroup/" + strconv.Itoa(gi)
		gm["@type"] = "PackageDependencyGroup"
		deps, _ := gm["dependencies"].([]any)
		nd := make([]any, 0, len(deps))
		for _, dep := range deps {
			dm, ok := dep.(map[string]any)
			if !ok {
				continue
			}
			dm = maps.Clone(dm)
			id, _ := dm["id"].(string)
			if id == "" {
				continue
			}
			dm["@id"] = d.leafURL(e) + "#dependencygroup/" + strconv.Itoa(gi) + "/" + strings.ToLower(id)
			dm["@type"] = "PackageDependency"
			dm["registration"] = d.hiveURL() + strings.ToLower(id) + "/index.json"
			if r, ok := dm["range"].(string); ok {
				dm["range"] = normalizeRange(r)
			}
			nd = append(nd, dm)
		}
		gm["dependencies"] = nd
		out = append(out, gm)
	}
	if len(out) > 0 {
		c["dependencyGroups"] = out
	} else {
		delete(c, "dependencyGroups")
	}
	return c
}
