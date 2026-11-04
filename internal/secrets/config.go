package secrets

import (
	"fmt"
	"net/url"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// The secrets of a repository configuration, as JSON paths in
// repositories.config. URL fields hold their secret in the password part of
// the user information (https://user:secret@host/).
var (
	repoValuePaths = [][]string{{"proxy", "auth", "password"}, {"proxy", "auth", "token"}}
	repoURLPaths   = [][]string{{"proxy", "remote_url"}, {"proxy", "http", "proxy"}}
)

// Blob store secrets: a plaintext key (never stored) and its sealed field in
// blob_stores.config.
var storeFields = []struct{ plain, sealed string }{
	{"secret_access_key", "secret_access_key_sealed"},
	{"session_token", "session_token_sealed"},
}

// sealValue seals v unless it is empty or already sealed. A value that is
// already sealed must open with k, so a client cannot plant a ciphertext it
// made up or copied from elsewhere.
func (k *Keyring) sealValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if k == nil {
		return "", ErrNoKey
	}
	if IsSealed(v) {
		if err := k.Check(v); err != nil {
			return "", fmt.Errorf("%w: %w", err, core.ErrInvalid)
		}
		return v, nil
	}
	return k.SealString(v)
}

// mapURLPassword applies fn to the password in raw's user information and
// returns the rebuilt URL. URLs without a password are returned unchanged.
func mapURLPassword(raw string, fn func(string) (string, error)) (string, error) {
	if raw == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, nil //nolint:nilerr // not a URL with credentials: nothing to seal; validation reports bad URLs
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" {
		return raw, nil
	}
	next, err := fn(pw)
	if err != nil {
		return "", err
	}
	if next == pw {
		return raw, nil
	}
	u.User = url.UserPassword(u.User.Username(), next)
	return u.String(), nil
}

// SealRepoConfig seals, in place, every plaintext secret of cfg: the proxy
// upstream password and token (bearer token or header value) and passwords in
// the remote URL and the outbound proxy URL. Already sealed values are kept
// after checking that they open. With a nil keyring any secret is refused
// with ErrNoKey. Repository services call it before storing a configuration.
func (k *Keyring) SealRepoConfig(cfg *core.RepoConfig) error {
	p := cfg.Proxy
	if p == nil {
		return nil
	}
	var err error
	if a := p.Auth; a != nil {
		if a.Password, err = k.sealValue(a.Password); err != nil {
			return fmt.Errorf("proxy auth password: %w", err)
		}
		if a.Token, err = k.sealValue(a.Token); err != nil {
			return fmt.Errorf("proxy auth token: %w", err)
		}
	}
	if p.RemoteURL, err = mapURLPassword(p.RemoteURL, k.sealValue); err != nil {
		return fmt.Errorf("proxy remote_url password: %w", err)
	}
	if p.HTTP.Proxy, err = mapURLPassword(p.HTTP.Proxy, k.sealValue); err != nil {
		return fmt.Errorf("proxy http.proxy password: %w", err)
	}
	return nil
}

// OpenProxyConfig returns a copy of cfg with every secret in plaintext, for
// building the outbound client. Values stored before encryption was
// configured are returned as they are. cfg itself is not modified.
func (k *Keyring) OpenProxyConfig(cfg *core.ProxyConfig) (*core.ProxyConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	out := *cfg
	var err error
	if cfg.Auth != nil {
		a := *cfg.Auth
		if a.Password, err = k.Reveal(a.Password); err != nil {
			return nil, fmt.Errorf("proxy auth password: %w", err)
		}
		if a.Token, err = k.Reveal(a.Token); err != nil {
			return nil, fmt.Errorf("proxy auth token: %w", err)
		}
		out.Auth = &a
	}
	if out.RemoteURL, err = mapURLPassword(cfg.RemoteURL, k.Reveal); err != nil {
		return nil, fmt.Errorf("proxy remote_url password: %w", err)
	}
	if out.HTTP.Proxy, err = mapURLPassword(cfg.HTTP.Proxy, k.Reveal); err != nil {
		return nil, fmt.Errorf("proxy http.proxy password: %w", err)
	}
	return &out, nil
}

// OpenStoreConfig returns a copy of a blob_stores.config document with its
// sealed fields opened into the plaintext fields the storage driver reads
// (secret_access_key_sealed -> secret_access_key). cfg is not modified.
func (k *Keyring) OpenStoreConfig(cfg map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(cfg))
	for key, v := range cfg {
		out[key] = v
	}
	for _, f := range storeFields {
		raw, present := out[f.sealed]
		if !present {
			continue
		}
		delete(out, f.sealed)
		s, _ := raw.(string)
		if s == "" {
			continue
		}
		pt, err := k.Reveal(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.sealed, err)
		}
		out[f.plain] = pt
	}
	return out, nil
}

// lookup returns the string at path in a decoded JSON document.
func lookup(doc map[string]any, path []string) (string, bool) {
	cur := doc
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			s, ok := v.(string)
			return s, ok
		}
		if cur, ok = v.(map[string]any); !ok {
			return "", false
		}
	}
	return "", false
}

// store sets the string at path; every parent must already exist.
func store(doc map[string]any, path []string, v string) {
	cur := doc
	for _, p := range path[:len(path)-1] {
		cur = cur[p].(map[string]any)
	}
	cur[path[len(path)-1]] = v
}

// valueFunc transforms one secret value; field names it for reports.
type valueFunc func(field, v string) (string, error)

// mapRepoDoc applies fn to every secret of a repositories.config document and
// reports whether anything changed.
func mapRepoDoc(doc map[string]any, fn valueFunc) (bool, error) {
	changed := false
	for _, p := range repoValuePaths {
		v, ok := lookup(doc, p)
		if !ok || v == "" {
			continue
		}
		next, err := fn(joinPath(p), v)
		if err != nil {
			return false, err
		}
		if next != v {
			store(doc, p, next)
			changed = true
		}
	}
	for _, p := range repoURLPaths {
		v, ok := lookup(doc, p)
		if !ok || v == "" {
			continue
		}
		field := joinPath(p)
		next, err := mapURLPassword(v, func(pw string) (string, error) { return fn(field, pw) })
		if err != nil {
			return false, err
		}
		if next != v {
			store(doc, p, next)
			changed = true
		}
	}
	return changed, nil
}

// mapStoreDoc moves plaintext blob store secrets into their sealed fields
// (sealing them with seal) and applies fn to sealed fields.
func mapStoreDoc(doc map[string]any, seal func(string) (string, error), fn valueFunc) (bool, error) {
	changed := false
	for _, f := range storeFields {
		if raw, ok := doc[f.plain]; ok {
			s, _ := raw.(string)
			if s != "" {
				sealed, err := seal(s)
				if err != nil {
					return false, fmt.Errorf("%s: %w", f.plain, err)
				}
				doc[f.sealed] = sealed
			}
			delete(doc, f.plain)
			changed = true
			continue
		}
		s, ok := doc[f.sealed].(string)
		if !ok || s == "" {
			continue
		}
		next, err := fn(f.sealed, s)
		if err != nil {
			return false, err
		}
		if next != s {
			doc[f.sealed] = next
			changed = true
		}
	}
	return changed, nil
}

func joinPath(p []string) string {
	out := p[0]
	for _, s := range p[1:] {
		out += "." + s
	}
	return out
}
