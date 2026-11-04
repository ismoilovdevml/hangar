package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// repositoryInput is schema RepositoryInput (JSON and YAML).
type repositoryInput struct {
	Name        string          `json:"name" yaml:"name"`
	Format      string          `json:"format" yaml:"format"`
	Type        core.RepoType   `json:"type" yaml:"type"`
	Description string          `json:"description,omitempty" yaml:"description,omitempty"`
	BlobStore   string          `json:"blob_store,omitempty" yaml:"blob_store,omitempty"`
	Online      *bool           `json:"online,omitempty" yaml:"online,omitempty"`
	Config      core.RepoConfig `json:"config" yaml:"config"`
}

// repositoryJSON is schema Repository.
type repositoryJSON struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Format         string          `json:"format"`
	Type           core.RepoType   `json:"type"`
	Description    string          `json:"description"`
	BlobStore      string          `json:"blob_store,omitempty"`
	Online         bool            `json:"online"`
	Config         core.RepoConfig `json:"config"`
	URL            string          `json:"url"`
	Status         string          `json:"status"`
	SizeBytes      int64           `json:"size_bytes"`
	ComponentCount int64           `json:"component_count"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type repoCounts struct {
	All    int `json:"all"`
	Hosted int `json:"hosted"`
	Proxy  int `json:"proxy"`
	Group  int `json:"group"`
}

type repositoryList struct {
	page[repositoryJSON]
	Counts repoCounts `json:"counts"`
}

// redactConfig removes write-only secrets before a config leaves the server.
func redactConfig(c core.RepoConfig) core.RepoConfig {
	if c.Proxy != nil {
		p := *c.Proxy
		if p.Auth != nil {
			au := *p.Auth
			au.Password, au.Token = "", ""
			p.Auth = &au
		}
		p.HTTP.Proxy = core.RedactURL(p.HTTP.Proxy)
		p.RemoteURL = core.RedactURL(p.RemoteURL)
		c.Proxy = &p
	}
	return c
}

// baseURL is Deps.BaseURL without a trailing slash.
func (a *API) baseURL() string { return strings.TrimRight(a.Deps.BaseURL, "/") }

// registryHost is the host[:port] docker clients use for repo.
func (a *API) registryHost(repo *core.Repository) string {
	if c := repo.Config.Connector; c != nil && c.Host != "" {
		return c.Host
	}
	u, err := url.Parse(a.baseURL())
	if err != nil || u.Host == "" {
		return a.baseURL()
	}
	if c := repo.Config.Connector; c != nil && c.HTTPPort > 0 {
		return u.Hostname() + ":" + strconv.Itoa(c.HTTPPort)
	}
	return u.Host
}

// repoURL is the URL clients are configured with.
func (a *API) repoURL(repo *core.Repository) string {
	if repo.Format == "docker" {
		if c := repo.Config.Connector; c != nil && (c.Host != "" || c.HTTPPort > 0) {
			return a.registryHost(repo)
		}
		return a.registryHost(repo) + "/" + repo.Name
	}
	return a.baseURL() + "/repository/" + repo.Name + "/"
}

// upstreamStates returns proxy health by repository name (nil without a source).
func (a *API) upstreamStates(ctx context.Context) (map[string]proxy.HealthItem, error) {
	if a.Upstreams == nil {
		return nil, nil
	}
	out := map[string]proxy.HealthItem{}
	cursor := ""
	for {
		p, err := a.Upstreams.List(ctx, cursor, core.MaxPageSize)
		if err != nil {
			return nil, err
		}
		for _, it := range p.Items {
			out[it.Name] = it
		}
		if p.NextCursor == "" || len(p.Items) == 0 {
			return out, nil
		}
		cursor = p.NextCursor
	}
}

func repoStatus(r *core.Repository, up map[string]proxy.HealthItem) string {
	if !r.Online {
		return "offline"
	}
	if r.Type == core.RepoProxy {
		switch up[r.Name].State {
		case proxy.StateSlow:
			return "slow_upstream"
		case proxy.StateBlocked:
			return "blocked"
		}
	}
	return "online"
}

func (a *API) toRepositoryJSON(r *core.Repository, stats map[string]catalog.RepoStats, up map[string]proxy.HealthItem) repositoryJSON {
	st := stats[r.ID]
	return repositoryJSON{
		ID: r.ID, Name: r.Name, Format: r.Format, Type: r.Type, Description: r.Description, BlobStore: r.BlobStore,
		Online: r.Online, Config: redactConfig(r.Config), URL: a.repoURL(r), Status: repoStatus(r, up),
		SizeBytes: st.SizeBytes, ComponentCount: st.ComponentCount, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func (a *API) repoStats(ctx context.Context) (map[string]catalog.RepoStats, error) {
	if a.Catalog == nil {
		return nil, notImplemented("repository statistics")
	}
	return a.Catalog.RepositoryStats(ctx)
}

func (a *API) listRepositories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	typ := core.RepoType(q.Get("type"))
	if typ != "" && !typ.Valid() {
		a.writeErr(w, r, fmt.Errorf("type %q: want hosted, proxy or group: %w", typ, core.ErrInvalid))
		return
	}
	repos, err := a.readableRepos(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	format, name := q.Get("format"), q.Get("q")
	var counts repoCounts
	var matched []*core.Repository
	for _, repo := range repos {
		if (format != "" && repo.Format != format) || (name != "" && !strings.Contains(repo.Name, name)) {
			continue
		}
		counts.All++
		switch repo.Type {
		case core.RepoHosted:
			counts.Hosted++
		case core.RepoProxy:
			counts.Proxy++
		case core.RepoGroup:
			counts.Group++
		}
		if typ == "" || repo.Type == typ {
			matched = append(matched, repo)
		}
	}
	items, next, err := paginate(matched, q.Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	stats, err := a.repoStats(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	up, err := a.upstreamStates(ctx)
	if err != nil {
		a.logger().WarnContext(ctx, "upstream health unavailable", "error", err)
	}
	out := make([]repositoryJSON, 0, len(items))
	for _, repo := range items {
		out = append(out, a.toRepositoryJSON(repo, stats, up))
	}
	WriteJSON(w, http.StatusOK, repositoryList{page: newPage(out, next), Counts: counts})
}

// getReadableRepo resolves {name}, hiding repositories the caller may not read.
func (a *API) getReadableRepo(ctx context.Context, name string) (*core.Repository, error) {
	if a.Deps.Repos == nil {
		return nil, notImplemented("repository service")
	}
	repo, err := a.Deps.Repos.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := a.authorize(ctx, core.ActionRead, core.RepoResource(name)); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return nil, fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
		}
		return nil, err
	}
	return repo, nil
}

func wantsYAML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/yaml") || strings.Contains(accept, "application/x-yaml")
}

func (a *API) getRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo, err := a.getReadableRepo(ctx, chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if wantsYAML(r) {
		online := repo.Online
		b, err := yaml.Marshal(repositoryInput{Name: repo.Name, Format: repo.Format, Type: repo.Type,
			Description: repo.Description, BlobStore: repo.BlobStore, Online: &online, Config: redactConfig(repo.Config)})
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(b)
		return
	}
	stats, err := a.repoStats(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	up, _ := a.upstreamStates(ctx)
	WriteJSON(w, http.StatusOK, a.toRepositoryJSON(repo, stats, up))
}

// decodeRepositoryInput reads a JSON or YAML RepositoryInput.
func decodeRepositoryInput(w http.ResponseWriter, r *http.Request) (repositoryInput, error) {
	var in repositoryInput
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "yaml") {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			return in, fmt.Errorf("read body: %s: %w", err.Error(), core.ErrInvalid)
		}
		if err := yaml.Unmarshal(b, &in); err != nil {
			return in, fmt.Errorf("decode YAML body: %s: %w", err.Error(), core.ErrInvalid)
		}
		return in, nil
	}
	return in, decodeJSON(w, r, &in)
}

func (in repositoryInput) repository() *core.Repository {
	online := true
	if in.Online != nil {
		online = *in.Online
	}
	return &core.Repository{Name: in.Name, Format: in.Format, Type: in.Type, Description: in.Description,
		BlobStore: in.BlobStore, Online: online, Config: in.Config}
}

func (a *API) createRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := decodeRepositoryInput(w, r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeRepoAdmin(ctx, in.Name, "repository.create"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if len(a.Formats) > 0 && !slices.Contains(a.Formats, in.Format) {
		a.writeErr(w, r, fmt.Errorf("format %q is not supported by this server: %w", in.Format, core.ErrInvalid))
		return
	}
	if a.Deps.Repos == nil {
		a.writeErr(w, r, notImplemented("repository service"))
		return
	}
	repo, err := a.Deps.Repos.Create(ctx, in.repository())
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, a.toRepositoryJSON(repo, nil, nil))
}

// keepSecrets carries write-only secrets over from old when the client sent
// the redacted form back unchanged.
func keepSecrets(next, old *core.RepoConfig) {
	if next.Proxy == nil || old.Proxy == nil {
		return
	}
	if na, oa := next.Proxy.Auth, old.Proxy.Auth; na != nil && oa != nil && na.Kind == oa.Kind &&
		na.Username == oa.Username && na.HeaderName == oa.HeaderName {
		if na.Password == "" {
			na.Password = oa.Password
		}
		if na.Token == "" {
			na.Token = oa.Token
		}
	}
	if next.Proxy.HTTP.Proxy != "" && next.Proxy.HTTP.Proxy == core.RedactURL(old.Proxy.HTTP.Proxy) {
		next.Proxy.HTTP.Proxy = old.Proxy.HTTP.Proxy
	}
	if next.Proxy.RemoteURL == core.RedactURL(old.Proxy.RemoteURL) {
		next.Proxy.RemoteURL = old.Proxy.RemoteURL
	}
}

func (a *API) updateRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	in, err := decodeRepositoryInput(w, r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if in.Name != "" && in.Name != name {
		a.writeErr(w, r, fmt.Errorf("name is immutable (path %q, body %q): %w", name, in.Name, core.ErrInvalid))
		return
	}
	in.Name = name
	if err := a.authorizeRepoAdmin(ctx, name, "repository.update"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Repos == nil {
		a.writeErr(w, r, notImplemented("repository service"))
		return
	}
	old, err := a.Deps.Repos.Get(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if (in.Format != "" && in.Format != old.Format) || (in.Type != "" && in.Type != old.Type) {
		a.writeErr(w, r, fmt.Errorf("format and type are immutable: %w", core.ErrInvalid))
		return
	}
	in.Format, in.Type = old.Format, old.Type
	next := in.repository()
	next.ID = old.ID
	keepSecrets(&next.Config, &old.Config)
	repo, err := a.Deps.Repos.Update(ctx, next)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	stats, _ := a.repoStats(ctx)
	WriteJSON(w, http.StatusOK, a.toRepositoryJSON(repo, stats, nil))
}

func (a *API) deleteRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	if err := a.authorizeRepoAdmin(ctx, name, "repository.delete"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Repos == nil {
		a.writeErr(w, r, notImplemented("repository service"))
		return
	}
	if err := a.Deps.Repos.Delete(ctx, name); err != nil {
		a.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
