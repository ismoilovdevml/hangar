package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Browse tree node kinds (schema BrowseEntry.kind).
const (
	nodeFolder  = "folder"
	nodePackage = "package"
	nodeVersion = "version"
	nodeAsset   = "asset"
)

// maxBrowseGroupDepth bounds nested group expansion.
const maxBrowseGroupDepth = 4

// browseComponentJSON is schema BrowseComponent.
type browseComponentJSON struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// browseAssetJSON is schema BrowseAsset.
type browseAssetJSON struct {
	ID               string     `json:"id"`
	Path             string     `json:"path"`
	Kind             string     `json:"kind"`
	ContentType      string     `json:"content_type"`
	SizeBytes        int64      `json:"size_bytes"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastDownloadedAt *time.Time `json:"last_downloaded_at"`
	DownloadCount    int64      `json:"download_count"`
}

// browseEntryJSON is schema BrowseEntry.
type browseEntryJSON struct {
	Name string `json:"name"`
	// Path is the node's path in the browse tree; folders end in "/".
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Repository holds the entry: the repository itself, or for a group the
	// first member (in resolution order) that has it.
	Repository string `json:"repository"`
	// Repositories lists every member that has the entry (groups only).
	Repositories []string             `json:"repositories,omitempty"`
	Component    *browseComponentJSON `json:"component,omitempty"`
	Asset        *browseAssetJSON     `json:"asset,omitempty"`
}

// browsePageJSON is schema BrowsePage.
type browsePageJSON struct {
	Repository string            `json:"repository"`
	Format     string            `json:"format"`
	Path       string            `json:"path"`
	Items      []browseEntryJSON `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// browseRoot is the storage directory a format's browse tree starts at:
// NuGet packages live under the flat container and PyPI files under
// packages/, next to cached upstream documents that are not browsed.
func browseRoot(format string) string {
	switch format {
	case "nuget":
		return "v3-flatcontainer/"
	case "pypi":
		return "packages/"
	}
	return ""
}

// storageDir maps a browse tree directory ("" or ending in "/") to the
// storage directory that holds its children. The tree follows what clients
// see: npm packages show their files directly, without the "-" directory
// tarballs and manifests are stored under.
func storageDir(format, tree string) string {
	root := browseRoot(format)
	if format != "npm" || tree == "" {
		return root + tree
	}
	segs := strings.Split(strings.TrimSuffix(tree, "/"), "/")
	n := 1
	if strings.HasPrefix(segs[0], "@") {
		n = 2
	}
	if len(segs) < n {
		return root + tree
	}
	out := strings.Join(segs[:n], "/") + "/-/"
	if len(segs) > n {
		out += strings.Join(segs[n:], "/") + "/"
	}
	return root + out
}

var pypiSeparators = regexp.MustCompile(`[-_.]+`)

// componentFolders returns the browse tree folders of c's package and of
// its version, for formats whose layout has them ("" otherwise).
func componentFolders(format string, c *catalog.BrowseComponent) (pkg, version string) {
	full := c.Name
	if c.Namespace != "" {
		full = c.Namespace + "/" + c.Name
	}
	switch format {
	case "npm", "docker":
		return full + "/", ""
	case "maven":
		pkg = strings.ReplaceAll(c.Namespace, ".", "/") + "/" + c.Name + "/"
		return pkg, pkg + c.Version + "/"
	case "nuget":
		pkg = strings.ToLower(c.Name) + "/"
		return pkg, pkg + strings.ToLower(c.Version) + "/"
	case "pypi":
		return pypiSeparators.ReplaceAllString(strings.ToLower(c.Name), "-") + "/", ""
	}
	return "", ""
}

// folderKind classifies the browse folder tree using the component found below it.
func folderKind(format, tree string, c *catalog.BrowseComponent) string {
	if c == nil {
		return nodeFolder
	}
	pkg, ver := componentFolders(format, c)
	norm := tree
	if format == "pypi" {
		norm = pypiSeparators.ReplaceAllString(strings.ToLower(tree), "-")
	}
	switch {
	case pkg != "" && strings.EqualFold(norm, pkg):
		return nodePackage
	case ver != "" && strings.EqualFold(norm, ver):
		return nodeVersion
	}
	return nodeFolder
}

// browseFormatsWithPackages are the formats whose folders can be packages
// or versions; raw folders are plain directories and are not annotated.
var browseFormatsWithPackages = map[string]bool{"npm": true, "maven": true, "nuget": true, "pypi": true, "docker": true}

type browseCursor struct {
	K string `json:"k"`
}

func encodeBrowseCursor(key string) string {
	b, _ := json.Marshal(browseCursor{K: key})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeBrowseCursor(s string) (string, error) {
	var c browseCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.K == "" {
		return "", fmt.Errorf("cursor %q: %w", s, core.ErrInvalid)
	}
	return c.K, nil
}

// startAfter is the smallest storage path after the child whose sort key is
// key in directory dir: past a whole folder, or just past a file.
func startAfter(dir, key string) string {
	if name, ok := strings.CutSuffix(key, "/"); ok {
		return dir + name + "0"
	}
	return dir + key + "\x01"
}

// cleanBrowsePath normalises the path query parameter to a tree directory.
func cleanBrowsePath(p string) (string, error) {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "", nil
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" {
			return "", fmt.Errorf("path %q: empty segment: %w", p, core.ErrInvalid)
		}
	}
	return p, nil
}

// browseSources returns the repositories whose content a browse of repo
// shows: repo itself, or a group's members the caller may read, in
// resolution order (nested groups expanded, each repository once).
func (a *API) browseSources(ctx context.Context, repo *core.Repository) ([]*core.Repository, error) {
	var out []*core.Repository
	seen := map[string]bool{}
	var walk func(r *core.Repository, depth int) error
	walk = func(r *core.Repository, depth int) error {
		if seen[r.Name] {
			return nil
		}
		seen[r.Name] = true
		if r.Type != core.RepoGroup {
			out = append(out, r)
			return nil
		}
		if depth >= maxBrowseGroupDepth {
			return nil
		}
		members, err := a.Deps.Repos.Members(ctx, r)
		if err != nil {
			return err
		}
		for _, m := range members {
			ok, err := a.canRead(ctx, m.Name)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := walk(m, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(repo, 0); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *API) browseRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("repository browsing"))
		return
	}
	repo, err := a.getReadableRepo(ctx, chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	tree, err := cleanBrowsePath(q.Get("path"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	after := ""
	if c := q.Get("cursor"); c != "" {
		if after, err = decodeBrowseCursor(c); err != nil {
			a.writeErr(w, r, err)
			return
		}
	}
	sources, err := a.browseSources(ctx, repo)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}

	dir := storageDir(repo.Format, tree)
	start := dir
	if after != "" {
		start = startAfter(dir, after)
	}
	type merged struct {
		key  string
		node catalog.BrowseNode
		in   []string
	}
	byKey := map[string]*merged{}
	var order []*merged
	for _, src := range sources {
		nodes, err := a.Catalog.Browse(ctx, catalog.BrowseQuery{RepoID: src.ID, Prefix: dir, Start: start,
			Limit: limit + 1, Components: browseFormatsWithPackages[repo.Format]})
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		for _, n := range nodes {
			k := n.SortKey()
			if m, ok := byKey[k]; ok {
				m.in = append(m.in, src.Name)
				if m.node.Component == nil {
					m.node.Component = n.Component
				}
				continue
			}
			m := &merged{key: k, node: n, in: []string{src.Name}}
			byKey[k] = m
			order = append(order, m)
		}
	}
	slices.SortFunc(order, func(x, y *merged) int { return strings.Compare(x.key, y.key) })

	out := browsePageJSON{Repository: repo.Name, Format: repo.Format, Path: tree, Items: make([]browseEntryJSON, 0, min(len(order), limit))}
	if len(order) > limit {
		order = order[:limit]
		next := encodeBrowseCursor(order[limit-1].key)
		out.NextCursor = &next
	}
	for _, m := range order {
		e := browseEntryJSON{Name: m.node.Name, Path: tree + m.key, Repository: m.in[0]}
		if repo.Type == core.RepoGroup {
			e.Repositories = m.in
		}
		if c := m.node.Component; c != nil {
			e.Component = &browseComponentJSON{ID: c.ID, Namespace: c.Namespace, Name: c.Name, Version: c.Version}
		}
		if m.node.Folder {
			e.Kind = folderKind(repo.Format, e.Path, m.node.Component)
			if e.Kind == nodeFolder {
				e.Component = nil
			}
		} else {
			e.Kind = nodeAsset
			as := m.node.Asset
			e.Asset = &browseAssetJSON{ID: as.ID, Path: as.Path, Kind: as.Kind, ContentType: as.ContentType, SizeBytes: as.Size,
				CreatedAt: as.CreatedAt, UpdatedAt: as.UpdatedAt, LastDownloadedAt: as.LastDownloadedAt, DownloadCount: as.DownloadCount}
		}
		out.Items = append(out.Items, e)
	}
	WriteJSON(w, http.StatusOK, out)
}

// assetDetailJSON is schema AssetDetail.
type assetDetailJSON struct {
	ID               string               `json:"id"`
	Path             string               `json:"path"`
	Repository       string               `json:"repository"`
	RepositoryType   core.RepoType        `json:"repository_type"`
	Format           string               `json:"format"`
	Kind             string               `json:"kind"`
	ContentType      string               `json:"content_type"`
	SizeBytes        int64                `json:"size_bytes"`
	BlobDigest       string               `json:"blob_digest"`
	Checksums        map[string]string    `json:"checksums"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
	BlobCreatedAt    *time.Time           `json:"blob_created_at"`
	LastDownloadedAt *time.Time           `json:"last_downloaded_at"`
	DownloadCount    int64                `json:"download_count"`
	CachedFromRemote bool                 `json:"cached_from_remote"`
	RemoteURL        string               `json:"remote_url,omitempty"`
	Component        *browseComponentJSON `json:"component,omitempty"`
	DownloadURL      string               `json:"download_url,omitempty"`
}

// readableAsset loads asset {id} and its repository; assets in repositories
// the caller may not read are "not found".
func (a *API) readableAsset(ctx context.Context, id string) (*catalog.AssetDetail, *core.Repository, error) {
	if a.Catalog == nil || a.Deps.Repos == nil {
		return nil, nil, notImplemented("asset details")
	}
	d, err := a.Catalog.Asset(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	repo, err := a.Deps.Repos.GetByID(ctx, d.RepositoryID)
	if err != nil {
		return nil, nil, err
	}
	if err := a.authorize(ctx, core.ActionRead, core.RepoResource(repo.Name)); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return nil, nil, fmt.Errorf("asset %q: %w", id, core.ErrNotFound)
		}
		return nil, nil, err
	}
	return d, repo, nil
}

func (a *API) getAsset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, repo, err := a.readableAsset(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out := assetDetailJSON{ID: d.ID, Path: d.Path, Repository: repo.Name, RepositoryType: repo.Type, Format: repo.Format,
		Kind: d.Kind, ContentType: d.ContentType, SizeBytes: d.Size, BlobDigest: d.BlobDigest, Checksums: map[string]string{},
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, BlobCreatedAt: d.BlobCreatedAt, LastDownloadedAt: d.LastDownloadedAt,
		DownloadCount: d.DownloadCount, CachedFromRemote: repo.Type == core.RepoProxy}
	for k, v := range d.Checksums {
		out.Checksums[k] = v
	}
	if _, ok := out.Checksums["sha256"]; !ok {
		out.Checksums["sha256"] = core.Digest(d.BlobDigest).Hex()
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil {
		out.RemoteURL = core.RedactURL(repo.Config.Proxy.RemoteURL)
	}
	if c := d.Component; c != nil {
		out.Component = &browseComponentJSON{ID: c.ID, Namespace: c.Namespace, Name: c.Name, Version: c.Version}
	}
	if repo.Format != "docker" {
		out.DownloadURL = a.baseURL() + "/repository/" + repo.Name + "/" + d.Path
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) deleteAsset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.Deps.Components == nil {
		a.writeErr(w, r, notImplemented("component service"))
		return
	}
	d, repo, err := a.readableAsset(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	target := "asset:" + repo.Name + "/" + d.Path
	if err := a.authorizeMutation(ctx, core.ActionDelete, core.RepoResource(repo.Name), "asset.delete", target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	// The component service deletes in one transaction, releases the blob
	// reference (unreferenced blobs become eligible for compaction), audits
	// and emits asset.deleted.
	if err := a.Deps.Components.DeleteAsset(ctx, repo, d.Path); err != nil {
		a.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) getComponentInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.PackageInfo == nil {
		a.writeErr(w, r, notImplemented("package details"))
		return
	}
	c, repo, err := a.readableComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	info, err := a.PackageInfo.Info(ctx, repo, c)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, a.componentInfoOut(c.ID, info))
}
