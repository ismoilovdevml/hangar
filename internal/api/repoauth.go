package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// authPresence is how an update body carries config.proxy.auth. A missing
// key and an explicit null mean different things to PUT /repositories/{name}.
type authPresence int

const (
	// authOmitted: no auth key; the stored credentials stay as they are.
	authOmitted authPresence = iota
	// authNull: "auth": null; the stored credentials are removed.
	authNull
	// authSent: an auth object; empty secrets keep the stored ones when
	// kind, username and header name are unchanged.
	authSent
)

// jsonAuthPresence reports how the JSON body b carries config.proxy.auth.
// b has already been decoded into a repositoryInput, so it is valid JSON.
func jsonAuthPresence(b []byte) authPresence {
	var probe struct {
		Config struct {
			Proxy map[string]json.RawMessage `json:"proxy"`
		} `json:"config"`
	}
	if json.Unmarshal(b, &probe) != nil {
		return authOmitted
	}
	raw, ok := probe.Config.Proxy["auth"]
	switch {
	case !ok:
		return authOmitted
	case bytes.Equal(bytes.TrimSpace(raw), []byte("null")):
		return authNull
	default:
		return authSent
	}
}

// yamlAuthPresence reports how the YAML body b carries config.proxy.auth;
// "auth: null", "auth: ~" and a bare "auth:" are all null.
func yamlAuthPresence(b []byte) authPresence {
	var probe struct {
		Config struct {
			Proxy map[string]any `yaml:"proxy"`
		} `yaml:"config"`
	}
	if yaml.Unmarshal(b, &probe) != nil {
		return authOmitted
	}
	v, ok := probe.Config.Proxy["auth"]
	switch {
	case !ok:
		return authOmitted
	case v == nil:
		return authNull
	default:
		return authSent
	}
}

// keepSecrets carries the write-only secrets of old over to next, which an
// update body leaves out or sends back redacted:
//
//   - auth omitted: the stored credentials are kept;
//   - auth null: the credentials are removed;
//   - auth sent with an empty password or token and the same kind, username
//     and header name: the stored secret is kept.
//
// Stored credentials are only ever carried to the origin (scheme, host and
// port) they were given for. When the remote URL moves to another origin and
// the body does not resend the secret, the update is refused rather than
// sending the old host's credentials to the new one.
func keepSecrets(next, old *core.RepoConfig, auth authPresence) error {
	if next.Proxy == nil || old.Proxy == nil {
		return nil
	}
	np, op := next.Proxy, old.Proxy
	if np.HTTP.Proxy != "" && np.HTTP.Proxy == core.RedactURL(op.HTTP.Proxy) {
		np.HTTP.Proxy = op.HTTP.Proxy
	}
	if np.RemoteURL == core.RedactURL(op.RemoteURL) {
		np.RemoteURL = op.RemoteURL
	}
	oa := op.Auth
	if oa == nil {
		return nil
	}
	moved := !sameOrigin(np.RemoteURL, op.RemoteURL)
	switch auth {
	case authNull:
		np.Auth = nil
	case authOmitted:
		if moved {
			return movedCredentials(np.RemoteURL, op.RemoteURL)
		}
		a := *oa
		np.Auth = &a
	case authSent:
		na := np.Auth
		if na == nil || na.Kind != oa.Kind || na.Username != oa.Username || na.HeaderName != oa.HeaderName {
			return nil
		}
		if (na.Password == "" && oa.Password != "") || (na.Token == "" && oa.Token != "") {
			if moved {
				return movedCredentials(np.RemoteURL, op.RemoteURL)
			}
		}
		if na.Password == "" {
			na.Password = oa.Password
		}
		if na.Token == "" {
			na.Token = oa.Token
		}
	}
	return nil
}

func movedCredentials(next, old string) error {
	return fmt.Errorf("config.proxy.remote_url moves from %s to %s: the stored upstream credentials are not sent to another host; "+
		"resend config.proxy.auth with its password or token, or set \"auth\": null to remove the credentials: %w",
		originOf(old), originOf(next), core.ErrInvalid)
}

// origin is a URL's scheme, host and port (the default port made explicit),
// lower case. Unparsable URLs have no origin.
func origin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port), true
}

// sameOrigin reports whether a and b have the same scheme, host and port.
func sameOrigin(a, b string) bool {
	oa, ok1 := origin(a)
	ob, ok2 := origin(b)
	return ok1 && ok2 && oa == ob
}

// originOf is the origin for messages: without credentials or path.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}
