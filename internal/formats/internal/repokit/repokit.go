// Package repokit holds what the Linux package repository formats (apt and
// rpm) share: request authorization and errors, path checks, group member
// resolution, serving stored assets and generated documents, publishing
// regenerated metadata, signing with the repository key, and the proxy
// cache for remote mirrors (metadata with a TTL, content forever). Every
// format resolves the target of a write with WriteTarget.
package repokit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/quarantine"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

// MaxGroupDepth bounds nested group resolution.
const MaxGroupDepth = 8

// Base carries the per-server state a format handler builds on.
type Base struct {
	Deps   core.Deps
	Log    *slog.Logger
	Format string
	Signer signing.Signer
	Now    func() time.Time

	fill  singleflight.Group
	locks sync.Map // in-process locks when Deps.Locks is nil

	sigMu sync.Mutex
	sigs  map[string][]byte // signatures by kind/document digest/key fingerprint
}

// NewBase returns a Base for format.
func NewBase(format string, deps core.Deps, signer signing.Signer) *Base {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Base{Deps: deps, Log: log.With("format", format), Format: format, Signer: signer, Now: time.Now, sigs: map[string][]byte{}}
}

// RepoURL is the absolute client URL of repo's root, with a trailing slash.
func (b *Base) RepoURL(repo *core.Repository) string {
	return strings.TrimSuffix(b.Deps.BaseURL, "/") + "/repository/" + repo.Name + "/"
}

// Authorize authenticates r and checks action on repo. On failure it has
// answered (401 with a Basic challenge, which apt and dnf answer with the
// credentials of auth.conf.d / the .repo file, or 403).
func (b *Base) Authorize(w http.ResponseWriter, r *http.Request, repo *core.Repository, action core.Action) (*http.Request, bool) {
	ctx := r.Context()
	p, err := b.Deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))
	if err == nil {
		err = b.Deps.AuthZ.Authorize(ctx, p, action, core.RepoResource(repo.Name))
	}
	if err != nil {
		b.Fail(w, r, err)
		return r, false
	}
	ctx = core.WithPrincipal(ctx, p)
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return r.WithContext(ctx), true
}

// Fail answers with a plain-text message (apt and dnf print it).
func (b *Base) Fail(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, b.Log, err) {
		return
	}
	err = routing.Annotate(r.Context(), err)
	code := core.HTTPStatus(err)
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="hangar"`)
		msg = "authentication required"
	case code == http.StatusForbidden && !errors.Is(err, core.ErrQuarantined):
		msg = "access denied"
	case code == http.StatusTooManyRequests:
		b.Log.WarnContext(r.Context(), "upstream rate limited", "path", r.URL.Path, "err", err)
		msg = "upstream rate limited, retry later"
		core.SetRetryAfter(w.Header(), err)
	case code == http.StatusBadGateway:
		b.Log.WarnContext(r.Context(), "upstream failed", "path", r.URL.Path, "err", err)
		msg = "upstream unavailable"
	case code >= http.StatusInternalServerError && code != http.StatusNotImplemented && code != http.StatusServiceUnavailable:
		b.Log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg) //nolint:gosec // text/plain with nosniff
}

// CleanPath validates a repository-relative request path: non-empty
// segments of printable characters, no "." or ".." segments, no
// backslashes. It returns the path without leading or trailing slashes.
func CleanPath(p string) (string, error) {
	if len(p) > 1024 {
		return "", fmt.Errorf("path too long: %w", core.ErrInvalid)
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return "", fmt.Errorf("invalid path %q: %w", p, core.ErrInvalid)
		}
		for _, c := range s {
			if c < 0x21 || c > 0x7e || c == '\\' || c == '"' || c == '<' || c == '>' || c == '`' {
				return "", fmt.Errorf("invalid character in path %q: %w", p, core.ErrInvalid)
			}
		}
	}
	return strings.Join(segs, "/"), nil
}

// WriteTarget resolves the repository an upload to repo is stored in: repo
// itself when hosted, a group's hosted write member, or nil (read-only).
func (b *Base) WriteTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	return WriteTarget(ctx, b.Deps.Repos, b.Format, repo)
}

// WriteTarget resolves the repository a write addressed to repo is stored
// in, for every format: repo itself when hosted; a group's write member,
// which must be an online hosted repository of format (an offline one
// fails with core.ErrOffline, any other with a *WriteMemberError); nil when
// repo takes no writes (a proxy, a group without a write member). Formats
// answer nil with their protocol's read-only error. Authorization is the
// caller's: the principal needs write on the target and, through a group,
// on the group.
func WriteTarget(ctx context.Context, repos core.RepositoryService, format string, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, nil
		}
		m, err := repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, err
		}
		if m.Type != core.RepoHosted || m.Format != format {
			return nil, &WriteMemberError{Group: repo.Name, Member: m.Name, Format: format}
		}
		if !m.Online {
			return nil, fmt.Errorf("write member %q of %q: %w", m.Name, repo.Name, core.ErrOffline)
		}
		return m, nil
	}
	return nil, nil
}

// WriteMemberError is a group write member that is not a hosted repository
// of the group's format. It is core.ErrInvalid.
type WriteMemberError struct {
	Group, Member, Format string
}

func (e *WriteMemberError) Error() string {
	return fmt.Sprintf("group %q write member %q is not a hosted %s repository", e.Group, e.Member, e.Format)
}

// Unwrap makes errors.Is(err, core.ErrInvalid) hold.
func (e *WriteMemberError) Unwrap() error { return core.ErrInvalid }

// AuthorizeWrite resolves the write target of repo and checks the
// principal may write to it (and, for a group, to the group). It answers
// 405 for read-only repositories.
func (b *Base) AuthorizeWrite(w http.ResponseWriter, r *http.Request, repo *core.Repository) (*http.Request, *core.Repository, bool) {
	target, err := b.WriteTarget(r.Context(), repo)
	if err != nil {
		b.Fail(w, r, err)
		return r, nil, false
	}
	if target == nil {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = fmt.Fprintf(w, "%s repository %s does not accept uploads\n", repo.Type, repo.Name)
		return r, nil, false
	}
	r, ok := b.Authorize(w, r, target, core.ActionWrite)
	if !ok {
		return r, nil, false
	}
	if target != repo {
		if err := b.Deps.AuthZ.Authorize(r.Context(), core.PrincipalFrom(r.Context()), core.ActionWrite, core.RepoResource(repo.Name)); err != nil {
			b.Fail(w, r, err)
			return r, nil, false
		}
	}
	return r, target, true
}

// Leaf is a hosted or proxy member of a group with the group memberships
// it was reached through (outermost first), whose routing rules apply to it.
type Leaf struct {
	Repo *core.Repository
	Via  []Link
}

// Link is one group membership on the way to a Leaf.
type Link struct {
	Group, Member *core.Repository
}

// Block returns the rule on the way to l that keeps it from serving the
// package name, or nil.
func (l Leaf) Block(name string) *core.RoutingBlock {
	for _, k := range l.Via {
		if b := k.Group.MemberRoutingBlock(k.Member.Name, name); b != nil {
			return b
		}
	}
	return nil
}

// Allows reports whether the routing rules let l serve the package name,
// recording an exclusion.
func (l Leaf) Allows(ctx context.Context, name string) bool {
	if b := l.Block(name); b != nil {
		routing.Record(ctx, l.Repo.Format, b)
		return false
	}
	return true
}

// RoutingKey fingerprints the routing rules that apply to leaves (those of
// the groups on the way, and a proxy leaf's own), for cache keys of merged
// documents: a rule change regenerates them.
func RoutingKey(leaves []Leaf) string {
	var sb strings.Builder
	seen := map[string]bool{}
	for _, l := range leaves {
		if l.Proxy() && !l.Repo.Config.Routing.Empty() {
			js, _ := json.Marshal(l.Repo.Config.Routing)
			sb.WriteString(l.Repo.ID + "=" + string(js) + "\n")
		}
		for _, k := range l.Via {
			if seen[k.Group.ID] || k.Group.Config.Routing.Empty() {
				continue
			}
			seen[k.Group.ID] = true
			js, _ := json.Marshal(k.Group.Config.Routing)
			sb.WriteString(k.Group.ID + "=" + string(js) + "\n")
		}
	}
	return sb.String()
}

// Excludes reports, without recording it, whether the routing rules keep
// l from serving name: a group's on the way, or a proxy leaf's own. Merging
// a remote's index checks every package it lists, tens of thousands; an
// exclusion is recorded (metric, audit) when a download is refused.
func (l Leaf) Excludes(name string) bool {
	return l.Block(name) != nil || (l.Proxy() && l.Repo.RoutingBlock(name) != nil)
}

// Proxy reports whether l is a proxy repository: it holds what its remote
// serves, read through its own cache, not only what is stored.
func (l Leaf) Proxy() bool { return l.Repo.Type == core.RepoProxy }

// Leaves expands a group into its hosted and proxy members in resolution
// order (nested groups depth-first; repeats, other formats and offline
// members skipped), each with the memberships it was reached through, for
// routing rules.
func (b *Base) Leaves(ctx context.Context, group *core.Repository) ([]Leaf, error) {
	var out []Leaf
	seen := map[string]bool{}
	var walk func(r *core.Repository, via []Link, depth int) error
	walk = func(r *core.Repository, via []Link, depth int) error {
		if depth > MaxGroupDepth {
			return fmt.Errorf("group %q nests deeper than %d levels: %w", group.Name, MaxGroupDepth, core.ErrInvalid)
		}
		members, err := b.Deps.Repos.Members(ctx, r)
		if err != nil {
			return err
		}
		for _, m := range members {
			if seen[m.ID] || m.Format != b.Format || !m.Online {
				continue
			}
			seen[m.ID] = true
			path := make([]Link, len(via)+1)
			copy(path, via)
			path[len(via)] = Link{Group: r, Member: m}
			switch m.Type {
			case core.RepoGroup:
				if err := walk(m, path, depth+1); err != nil {
					return err
				}
			default:
				out = append(out, Leaf{Repo: m, Via: path})
			}
		}
		return nil
	}
	if err := walk(group, nil, 1); err != nil {
		return nil, err
	}
	return out, nil
}

// ProxyFetch returns a proxy member's copy of a file through the member's
// own proxy path (its cache, else its remote), as a request to the member
// would, so the member's routing rules and cache apply.
type ProxyFetch func(ctx context.Context, member *core.Repository) (*core.Asset, error)

// FindInLeaves returns the first leaf holding path that the routing rules
// let serve the package name ("" applies no rule): a hosted leaf holds what
// it stores, a proxy leaf what fetch returns (nil skips proxy leaves). A
// proxy leaf that fails other than with not-found (its remote is down) is
// passed over, and its error returned when no later leaf holds path. When
// the rules (the groups' or a proxy's own) exclude every leaf the error is
// the first *core.RoutingBlock. A failed lookup in a hosted leaf is returned
// at once.
func (b *Base) FindInLeaves(ctx context.Context, leaves []Leaf, path, name string, fetch ProxyFetch) (*core.Asset, *core.Repository, error) {
	var (
		first   *core.RoutingBlock
		lastErr error
		allowed int
	)
	for _, l := range leaves {
		if name != "" {
			if bl := l.Block(name); bl != nil {
				routing.Record(ctx, l.Repo.Format, bl)
				if first == nil {
					first = bl
				}
				continue
			}
		}
		if l.Proxy() {
			if fetch == nil {
				continue
			}
			a, err := fetch(ctx, l.Repo)
			switch {
			case err == nil:
				return a, l.Repo, nil
			case errors.Is(err, core.ErrRoutingBlocked):
				if bl, ok := core.AsRoutingBlock(err); ok && first == nil {
					first = bl
				}
			case errors.Is(err, core.ErrNotFound):
				allowed++
			default:
				allowed++
				lastErr = err
			}
			continue
		}
		allowed++
		a, err := b.Deps.Components.GetAsset(ctx, l.Repo.ID, path)
		if err == nil {
			return a, l.Repo, nil
		}
		if !errors.Is(err, core.ErrNotFound) {
			return nil, nil, err
		}
	}
	switch {
	case lastErr != nil:
		return nil, nil, lastErr
	case allowed == 0 && first != nil:
		return nil, nil, first
	}
	return nil, nil, fmt.Errorf("%s: %w", path, core.ErrNotFound)
}

// QuarantinedPaths returns the paths of repoID's assets that belong to a
// quarantined component. A proxy's merged indexes leave them out, since a
// cached package is the proxy's only record of a remote package's
// quarantine.
func (b *Base) QuarantinedPaths(ctx context.Context, repoID string) (map[string]bool, error) {
	comps, err := quarantine.Components(ctx, b.Deps.Components, repoID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for id := range comps {
		cursor := ""
		for {
			page, err := b.Deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: repoID, ComponentID: id, Limit: core.MaxPageSize, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			for _, a := range page.Items {
				out[a.Path] = true
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return out, nil
}

// AllAssets lists every asset of repoID under prefix.
func (b *Base) AllAssets(ctx context.Context, repoID, prefix string) ([]*core.Asset, error) {
	var out []*core.Asset
	cursor := ""
	for {
		page, err := b.Deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: repoID, PathPrefix: prefix, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Quarantined returns the ids of repoID's quarantined components.
func (b *Base) Quarantined(ctx context.Context, repoID string) (map[string]bool, error) {
	out := map[string]bool{}
	cursor := ""
	for {
		page, err := b.Deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repoID, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if c.Quarantined {
				out[c.ID] = true
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// CheckQuarantine fails with core.ErrQuarantined when a's component is quarantined.
// A failed lookup is returned: the check fails closed (quarantine.Blocked).
func (b *Base) CheckQuarantine(ctx context.Context, a *core.Asset) error {
	c, err := quarantine.Blocked(ctx, b.Deps.Components, a)
	if err != nil {
		return err
	}
	if c != nil {
		return fmt.Errorf("%s %s: %w", c.Name, c.Version, core.ErrQuarantined)
	}
	return nil
}

// Lock takes the cluster-wide lock key (an in-process lock without Deps.Locks).
func (b *Base) Lock(ctx context.Context, key string) (func(), error) {
	if b.Deps.Locks != nil {
		return b.Deps.Locks.Lock(ctx, b.Format+":"+key)
	}
	v, _ := b.locks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}

// ReadAsset reads a stored asset of repo, at most limit bytes.
func (b *Base) ReadAsset(ctx context.Context, repo *core.Repository, a *core.Asset, limit int64) ([]byte, error) {
	bs, err := b.Deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, fmt.Errorf("blob of %s: %w", a.Path, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", a.Path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes: %w", a.Path, limit, core.ErrInvalid)
	}
	return data, nil
}

// File is a generated metadata document.
type File struct {
	Data        []byte
	ContentType string
}

// Sum is the hex sha256 of f's content.
func (f File) Sum() string {
	s := sha256.Sum256(f.Data)
	return hex.EncodeToString(s[:])
}

// Sync makes the metadata assets of repo under prefix equal files (paths
// relative to the repository): changed and new files are published in one
// request, files no longer generated are deleted unless keep says to keep
// them for now. Unchanged files are left alone. It reports whether
// anything was written.
func (b *Base) Sync(ctx context.Context, repo *core.Repository, prefix string, files map[string]File, keep func(*core.Asset) bool) (bool, error) {
	existing, err := b.AllAssets(ctx, repo.ID, prefix)
	if err != nil {
		return false, err
	}
	have := map[string]*core.Asset{}
	for _, a := range existing {
		if a.Kind == core.AssetMetadata {
			have[a.Path] = a
		}
	}
	bs, err := b.Deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return false, err
	}
	var assets []*core.Asset
	for p, f := range files {
		if a := have[p]; a != nil && a.BlobDigest.Hex() == f.Sum() {
			continue
		}
		d, n, err := bs.Put(ctx, bytes.NewReader(f.Data))
		if err != nil {
			return false, fmt.Errorf("store %s: %w", p, err)
		}
		assets = append(assets, &core.Asset{Path: p, Kind: core.AssetMetadata, BlobDigest: d, Size: n, ContentType: f.ContentType})
	}
	changed := len(assets) > 0
	if changed {
		if _, _, err := b.Deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: assets}); err != nil {
			return false, err
		}
	}
	for p, a := range have {
		if _, ok := files[p]; ok || (keep != nil && keep(a)) {
			continue
		}
		if err := b.Deps.Components.DeleteAsset(ctx, repo, p); err != nil && !errors.Is(err, core.ErrNotFound) {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// Signature kinds for Sign.
const (
	SignClear    = "clear"
	SignDetached = "detached"
)

// maxSignatureCache bounds the cached signatures; the cache is cleared when full.
const maxSignatureCache = 4096

// Sign signs doc with repo's key: a cleartext-signed copy (SignClear) or an
// armored detached signature (SignDetached), made at time at. Signatures are
// cached by document, key and kind, so repeated metadata requests do not
// sign again.
func (b *Base) Sign(ctx context.Context, repo *core.Repository, kind string, doc []byte, at time.Time) ([]byte, error) {
	if b.Signer == nil {
		return nil, fmt.Errorf("metadata signing is not configured on this server: %w", core.ErrNotImplemented)
	}
	key, err := b.Signer.KeyFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(doc)
	ck := kind + "/" + key.Fingerprint() + "/" + hex.EncodeToString(sum[:])
	b.sigMu.Lock()
	sig, ok := b.sigs[ck]
	b.sigMu.Unlock()
	if ok {
		return sig, nil
	}
	// A signature older than its key does not verify (gpg reports a time
	// conflict), as happens for a document from before a key rotation.
	if at.Before(key.CreatedAt()) {
		at = b.Now()
	}
	if kind == SignClear {
		sig, err = key.ClearSign(doc, at)
	} else {
		sig, err = key.DetachSign(doc, at)
	}
	if err != nil {
		return nil, err
	}
	b.sigMu.Lock()
	if len(b.sigs) >= maxSignatureCache {
		clear(b.sigs)
	}
	b.sigs[ck] = sig
	b.sigMu.Unlock()
	return sig, nil
}

// ServePublicKey answers public.key (armored) and public.gpg (binary) with
// repo's signing key.
func (b *Base) ServePublicKey(w http.ResponseWriter, r *http.Request, repo *core.Repository, binary bool) {
	if repo.Type == core.RepoProxy {
		b.Fail(w, r, fmt.Errorf("proxy repository %s passes its remote's signatures through and has no key of its own; "+
			"use the remote's archive key: %w", repo.Name, core.ErrNotFound))
		return
	}
	if b.Signer == nil {
		b.Fail(w, r, fmt.Errorf("metadata signing is not configured on this server: %w", core.ErrNotImplemented))
		return
	}
	key, err := b.Signer.KeyFor(r.Context(), repo)
	if err != nil {
		b.Fail(w, r, err)
		return
	}
	var data []byte
	ct := "application/pgp-keys"
	if binary {
		data, err = key.PublicBinary()
		ct = "application/octet-stream"
	} else {
		data, err = key.PublicArmored()
	}
	if err != nil {
		b.Fail(w, r, err)
		return
	}
	w.Header().Set("X-Key-Fingerprint", key.Fingerprint())
	b.ServeBytes(w, r, data, ct, key.CreatedAt())
}

// ServeBytes answers with generated content (Range, HEAD and conditional
// requests included). Metadata must be revalidated by caches.
func (b *Base) ServeBytes(w http.ResponseWriter, r *http.Request, data []byte, contentType string, modTime time.Time) {
	sum := sha256.Sum256(data)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", modTime, bytes.NewReader(data))
}

// ServeAsset streams a stored asset of owner. Immutable content may be
// cached by clients; metadata must be revalidated. Successful GETs of
// content are recorded as downloads.
func (b *Base) ServeAsset(w http.ResponseWriter, r *http.Request, owner *core.Repository, a *core.Asset) {
	ctx := r.Context()
	bs, err := b.Deps.Blobs.ForRepository(ctx, owner)
	if err != nil {
		b.Fail(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		b.Fail(w, r, fmt.Errorf("blob of %s in %s: %w: %w", a.Path, owner.Name, err, errIntegrity))
		return
	}
	defer rc.Close()
	h := w.Header()
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("ETag", `"`+a.BlobDigest.Hex()+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	if a.Kind == core.AssetContent {
		h.Set("Cache-Control", "max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	mod := a.UpdatedAt
	if t, ok := AttrTime(a, AttrUpstreamLastModified); ok {
		mod = t
	}
	sw := &statusWriter{ResponseWriter: w}
	http.ServeContent(sw, r, "", mod, rc)
	if r.Method == http.MethodGet && a.Kind == core.AssetContent && (sw.status == http.StatusOK || sw.status == 0) {
		b.Deps.Components.RecordDownload(ctx, a)
	}
}

// errIntegrity marks an asset whose blob is missing: a server fault, not a client 404.
var errIntegrity = errors.New("stored content missing")

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// StringList reads an attribute holding a list of strings ([]string, or
// []any after a JSON round trip).
func StringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// AttrString reads a string attribute.
func AttrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// AttrInt reads a numeric attribute (int64, or float64 after a JSON round trip).
func AttrInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

// AttrTime reads an RFC 3339 time attribute of a.
func AttrTime(a *core.Asset, k string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, AttrString(a.Attributes, k))
	return t, err == nil
}

// Tally starts counting proxy cache hits and misses for the request.
func Tally(ctx context.Context) (context.Context, *proxy.CacheTally) {
	return proxy.WithCacheTally(ctx)
}

// RequestPath returns the decoded wildcard path of r ("*"), validated with
// CleanPath and prefixed with prefix (the route's fixed part, e.g. "pool/").
func RequestPath(r *http.Request, prefix string) (string, error) {
	p := chi.URLParam(r, "*")
	if r.URL.RawPath != "" {
		dec, err := url.PathUnescape(p)
		if err != nil {
			return "", fmt.Errorf("path %q: bad percent-encoding: %w", p, core.ErrInvalid)
		}
		p = dec
	}
	return CleanPath(prefix + p)
}
