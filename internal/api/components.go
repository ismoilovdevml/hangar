package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/search"
)

type publisherJSON struct {
	Kind        string `json:"kind"` // user, ci, service_account
	DisplayName string `json:"display_name"`
}

// publishedComponent is schema PublishedComponent.
type publishedComponent struct {
	ID               string              `json:"id"`
	Format           string              `json:"format"`
	Namespace        string              `json:"namespace"`
	Name             string              `json:"name"`
	Version          string              `json:"version"`
	Repository       string              `json:"repository"`
	Publisher        publisherJSON       `json:"publisher"`
	PublishedAt      time.Time           `json:"published_at"`
	Scan             catalog.ScanSummary `json:"scan"`
	DownloadCount    int64               `json:"download_count"`
	LastDownloadedAt *time.Time          `json:"last_downloaded_at"`
}

func toPublished(c *catalog.ComponentRow) publishedComponent {
	name := c.PublishedBy
	if name == "" {
		name = "system"
	}
	return publishedComponent{
		ID: c.ID, Format: c.Format, Namespace: c.Namespace, Name: c.Name, Version: c.Version, Repository: c.Repository,
		Publisher: publisherJSON{Kind: c.PublisherKind, DisplayName: name}, PublishedAt: c.CreatedAt, Scan: c.Scan,
		DownloadCount: c.DownloadCount, LastDownloadedAt: c.LastDownloadedAt,
	}
}

// componentDetail is schema ComponentDetail.
type componentDetail struct {
	publishedComponent
	SizeBytes        int64             `json:"size_bytes"`
	Digest           string            `json:"digest,omitempty"`
	Checksums        map[string]string `json:"checksums"`
	InstallCommand   string            `json:"install_command"`
	Signed           bool              `json:"signed"`
	Platforms        []string          `json:"platforms,omitempty"`
	Quarantined      bool              `json:"quarantined"`
	QuarantineReason string            `json:"quarantine_reason,omitempty"`
	BaseImage        string            `json:"base_image,omitempty"`
	SourceURL        string            `json:"source_url,omitempty"`
	License          string            `json:"license,omitempty"`
	Attributes       map[string]any    `json:"attributes,omitempty"`
}

// assetJSON is schema Asset.
type assetJSON struct {
	ID               string            `json:"id"`
	Path             string            `json:"path"`
	Kind             core.AssetKind    `json:"kind"`
	BlobDigest       core.Digest       `json:"blob_digest"`
	ContentType      string            `json:"content_type"`
	SizeBytes        int64             `json:"size_bytes"`
	Checksums        map[string]string `json:"checksums,omitempty"`
	DownloadURL      string            `json:"download_url,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	LastDownloadedAt *time.Time        `json:"last_downloaded_at"`
}

func attrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// installCommand renders the client command for c in its repository.
func (a *API) installCommand(repo *core.Repository, c *catalog.ComponentRow) string {
	url := a.repoURL(repo)
	full := c.Name
	if c.Namespace != "" {
		full = c.Namespace + "/" + c.Name
	}
	switch c.Format {
	case "npm":
		return fmt.Sprintf("npm install %s@%s --registry %s", full, c.Version, url)
	case "maven":
		return fmt.Sprintf("mvn dependency:get -Dartifact=%s:%s:%s -DremoteRepositories=%s", c.Namespace, c.Name, c.Version, url)
	case "pypi":
		return fmt.Sprintf("pip install %s==%s --index-url %ssimple/", c.Name, c.Version, url)
	case "docker":
		return fmt.Sprintf("docker pull %s/%s:%s", url, full, c.Version)
	case "helm":
		return fmt.Sprintf("helm pull %s --version %s --repo %s", c.Name, c.Version, url)
	case "go":
		return fmt.Sprintf("GOPROXY=%s go get %s@%s", url, full, c.Version)
	case "nuget":
		return fmt.Sprintf("dotnet add package %s --version %s --source %sindex.json", c.Name, c.Version, url)
	case "cargo":
		return fmt.Sprintf("cargo add %s@%s --registry %s", c.Name, c.Version, repo.Name)
	case "apt":
		return fmt.Sprintf("apt-get install %s=%s", c.Name, c.Version)
	default:
		return fmt.Sprintf("curl -fLO %s%s", url, strings.TrimPrefix(full, "/"))
	}
}

func (a *API) listComponents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("component listing"))
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	p, err := a.Catalog.ListComponents(ctx, catalog.ComponentFilter{RepoIDs: ids, Repository: q.Get("repository"),
		Format: q.Get("format"), Name: q.Get("name"), Sort: q.Get("sort"), Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]publishedComponent, 0, len(p.Items))
	for _, c := range p.Items {
		items = append(items, toPublished(c))
	}
	WriteJSON(w, http.StatusOK, newPage(items, p.NextCursor))
}

// readableComponent loads {id} through the component service and checks
// read access to its repository; unreadable components are "not found".
func (a *API) readableComponent(ctx context.Context, id string) (*core.Component, *core.Repository, error) {
	if a.Deps.Components == nil || a.Deps.Repos == nil {
		return nil, nil, notImplemented("component service")
	}
	if !catalog.ValidID(id) {
		return nil, nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	c, err := a.Deps.Components.GetComponentByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	repo, err := a.Deps.Repos.GetByID(ctx, c.RepositoryID)
	if err != nil {
		return nil, nil, err
	}
	if err := a.authorize(ctx, core.ActionRead, core.RepoResource(repo.Name)); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return nil, nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
		}
		return nil, nil, err
	}
	return c, repo, nil
}

// catalogComponent is readableComponent served from the catalog.
func (a *API) catalogComponent(ctx context.Context, id string) (*catalog.ComponentDetail, *core.Repository, error) {
	if a.Catalog == nil || a.Deps.Repos == nil {
		return nil, nil, notImplemented("component details")
	}
	d, err := a.Catalog.Component(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	repo, err := a.Deps.Repos.GetByID(ctx, d.RepositoryID)
	if err != nil {
		return nil, nil, err
	}
	if err := a.authorize(ctx, core.ActionRead, core.RepoResource(repo.Name)); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return nil, nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
		}
		return nil, nil, err
	}
	return d, repo, nil
}

func (a *API) getComponent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, repo, err := a.catalogComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out := componentDetail{
		publishedComponent: toPublished(&d.ComponentRow), SizeBytes: d.SizeBytes, Digest: d.Digest, Checksums: d.Checksums,
		InstallCommand: a.installCommand(repo, &d.ComponentRow), Quarantined: d.Quarantined,
		QuarantineReason: attrString(d.Attributes, "quarantine_reason"), BaseImage: attrString(d.Attributes, "base_image"),
		SourceURL: attrString(d.Attributes, "source_url"), License: attrString(d.Attributes, "license"), Attributes: d.Attributes,
	}
	out.Signed, _ = d.Attributes["signed"].(bool)
	if ps, ok := d.Attributes["platforms"].([]any); ok {
		for _, p := range ps {
			if s, ok := p.(string); ok {
				out.Platforms = append(out.Platforms, s)
			}
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) deleteComponent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	c, repo, err := a.readableComponent(ctx, id)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	target := "component:" + repo.Name + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
	if err := a.authorizeMutation(ctx, core.ActionDelete, core.RepoResource(repo.Name), "component.delete", target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.Deps.Components.DeleteComponent(ctx, repo, id); err != nil {
		a.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listComponentAssets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	c, repo, err := a.readableComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, ComponentID: c.ID,
		Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]assetJSON, 0, len(p.Items))
	for _, as := range p.Items {
		j := assetJSON{ID: as.ID, Path: as.Path, Kind: as.Kind, BlobDigest: as.BlobDigest, ContentType: as.ContentType,
			SizeBytes: as.Size, Checksums: as.Checksums, CreatedAt: as.CreatedAt.UTC(), LastDownloadedAt: as.LastDownloadedAt}
		if repo.Format != "docker" {
			j.DownloadURL = a.baseURL() + "/repository/" + repo.Name + "/" + as.Path
		}
		items = append(items, j)
	}
	WriteJSON(w, http.StatusOK, newPage(items, p.NextCursor))
}

func (a *API) listComponentVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	c, repo, err := a.catalogComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	ns := c.Namespace
	p, err := a.Catalog.ListComponents(ctx, catalog.ComponentFilter{RepoIDs: []string{repo.ID}, Name: c.Name, Namespace: &ns,
		Sort: catalog.SortPublishedDesc, Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]publishedComponent, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, toPublished(it))
	}
	WriteJSON(w, http.StatusOK, newPage(items, p.NextCursor))
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.Search == nil {
		a.writeErr(w, r, notImplemented("search"))
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	res, err := a.Search.Search(ctx, search.Query{Q: q.Get("q"), Sort: q.Get("sort"), RepoIDs: ids, Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	type result struct {
		page[search.Hit]
		Total  int           `json:"total"`
		Facets search.Facets `json:"facets"`
	}
	WriteJSON(w, http.StatusOK, result{page: newPage(res.Items, res.NextCursor), Total: res.Total, Facets: res.Facets})
}
