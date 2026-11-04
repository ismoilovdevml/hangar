package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// How a proxy repository's remote answers for content it does not have
// differs between registries:
//
//   - 404 with NAME_UNKNOWN / MANIFEST_UNKNOWN / BLOB_UNKNOWN (the spec);
//   - 401 UNAUTHORIZED even with a valid anonymous token (Docker Hub: the
//     repository might be private, so it never says "unknown");
//   - 403 DENIED (GHCR, Quay, ECR for repositories the credentials cannot see);
//   - 400 NAME_INVALID for names the remote does not accept at all.
//
// All of them mean "this remote does not serve that name": the proxy answers
// MANIFEST_UNKNOWN / BLOB_UNKNOWN and a group moves on to its next member.
// Pushing a new image through a group that contains a Docker Hub proxy relies
// on this: the client's existence probes (HEAD manifests/<tag>, HEAD
// blobs/<digest>) must answer 404, not 502.

// errRemoteDenied marks a remote that refused access to a name (401/403). It
// wraps core.ErrNotFound.
var errRemoteDenied = fmt.Errorf("remote does not serve this name (unknown repository or access denied): %w", core.ErrNotFound)

// errRateLimited marks a remote answering 429. It wraps core.ErrUpstream and
// is answered as TOOMANYREQUESTS.
var errRateLimited = fmt.Errorf("remote registry rate limit exceeded: %w", core.ErrUpstream)

// remoteErrorCode returns the first OCI error code in a remote's error body
// ("" for HEAD responses or bodies that are not the OCI envelope).
func remoteErrorCode(resp *core.UpstreamResponse) string {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return ""
	}
	var e ociErrors
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e); err != nil || len(e.Errors) == 0 {
		return ""
	}
	return e.Errors[0].Code
}

// upstreamStatusErr turns a remote's unexpected status (and the OCI error
// code of its body, if any) into an error; see the comment at the top of
// this file.
func upstreamStatusErr(what string, status int, code string) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%s: remote answered 404: %w", what, core.ErrNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: remote answered %d: %w", what, status, errRemoteDenied)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", what, errRateLimited)
	case http.StatusBadRequest:
		switch code {
		case codeNameInvalid, codeNameUnknown, codeManifestUnknown, codeBlobUnknown:
			return fmt.Errorf("%s: remote rejected the name: %w", what, core.ErrNotFound)
		}
	}
	return fmt.Errorf("%s: remote answered %d: %w", what, status, core.ErrUpstream)
}

// unknownName reports whether an error says the remote does not know the
// repository name at all (as opposed to one tag or blob of a known name).
func unknownName(status int, code string) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden ||
		code == codeNameUnknown || code == codeNameInvalid
}

// Defaults for the per-process cache of names a remote does not serve.
const (
	// defaultUnknownNameTTL applies when the proxy has no negative_cache_ttl.
	// Short: it only spares the remote the burst of probes a push through a
	// group sends for every layer of a new image.
	defaultUnknownNameTTL = 30 * time.Second
	maxUnknownNames       = 10000
)

// nameCache remembers (proxy repository, remote name) pairs the remote
// answered "unknown" or "denied" for, so a push of a new image through a
// group does not send one remote request per layer probe.
type nameCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry
}

func nameKey(repo *core.Repository, remoteName string) string { return repo.Name + "\x00" + remoteName }

func (c *nameCache) unknown(repo *core.Repository, remoteName string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := nameKey(repo, remoteName)
	exp, ok := c.entries[k]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(c.entries, k)
		return false
	}
	return true
}

func (c *nameCache) add(repo *core.Repository, remoteName string, now time.Time) {
	ttl := defaultUnknownNameTTL
	if p := repo.Config.Proxy; p != nil && p.NegativeCacheTTL > 0 {
		ttl = p.NegativeCacheTTL.D()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]time.Time{}
	}
	if len(c.entries) >= maxUnknownNames {
		for k, exp := range c.entries {
			if !now.Before(exp) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxUnknownNames {
			clear(c.entries)
		}
	}
	c.entries[nameKey(repo, remoteName)] = now.Add(ttl)
}

// forget drops every entry of the repository named repo (its configuration changed).
func (c *nameCache) forget(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, repo+"\x00") {
			delete(c.entries, k)
		}
	}
}

// errNameUnknownCached is returned while a name is in the nameCache.
var errNameUnknownCached = fmt.Errorf("remote recently answered that it does not serve this name: %w", core.ErrNotFound)

// remoteMiss classifies a remote's non-200 answer for remoteName, remembers
// names the remote does not serve at all, and returns the error to report.
func (h *handler) remoteMiss(repo *core.Repository, remoteName, what string, resp *core.UpstreamResponse) error {
	code := remoteErrorCode(resp)
	err := upstreamStatusErr(what, resp.StatusCode, code)
	if unknownName(resp.StatusCode, code) {
		h.unknown.add(repo, remoteName, h.now())
		if errors.Is(err, errRemoteDenied) && repo.Config.Proxy != nil && repo.Config.Proxy.Auth != nil {
			// With credentials configured a 401/403 may also mean they are wrong.
			h.log.Warn("docker: remote refused a name; check the proxy credentials if the image exists upstream",
				"repository", repo.Name, "name", remoteName, "status", resp.StatusCode)
		}
	}
	return err
}
