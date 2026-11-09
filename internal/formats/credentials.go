package formats

import (
	"context"
	"errors"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// verifiedKey carries the credentials the dispatcher already authenticated.
type verifiedKey struct{}

type verified struct {
	creds core.Credentials
	p     *core.Principal
}

// checkCredentials authenticates the Basic or Bearer credentials of r before
// the format routes run. Wrong, expired or revoked credentials answer 401
// with the format's challenge on every route, public ones included: a client
// that sends a bad token to a public service index would otherwise succeed
// there and fail later with an unclear error. Requests without credentials
// pass unchanged. Other failures (a password that must be changed, a database
// error) are left to the format, which reports them as before.
//
// On success the principal is remembered in the request context, so the
// format's own authentication of the same credentials (through
// rememberedAuth) costs no second lookup and is not audited twice.
func (d *dispatcher) checkCredentials(w http.ResponseWriter, r *http.Request, f core.Format) (*http.Request, bool) {
	if d.auth == nil {
		return r, true
	}
	creds := core.CredentialsFromRequest(r)
	if creds.Empty() {
		return r, true
	}
	p, err := d.auth.Authenticate(r.Context(), creds)
	switch {
	case err == nil:
		return r.WithContext(context.WithValue(r.Context(), verifiedKey{}, &verified{creds: creds, p: p})), true
	case errors.Is(err, core.ErrUnauthenticated):
		if c, ok := f.(core.Challenger); ok {
			c.Challenge(w, r, d.deps, err)
		} else {
			w.Header().Set("WWW-Authenticate", `Basic realm="Hangar"`)
			writeError(w, http.StatusUnauthorized, "invalid username, password or token")
		}
		return r, false
	default:
		return r, true
	}
}

// rememberedAuth answers Authenticate from the dispatcher's check when the
// credentials are the ones it verified, and asks next otherwise.
type rememberedAuth struct{ next core.Authenticator }

func (a rememberedAuth) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	if v, ok := ctx.Value(verifiedKey{}).(*verified); ok && !c.Empty() && v.creds == c && v.p != nil {
		p := *v.p
		return &p, nil
	}
	return a.next.Authenticate(ctx, c)
}
