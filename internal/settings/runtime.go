package settings

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/ldap"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
)

// AuthRuntime puts sign-in settings into effect: it builds the OIDC
// provider and the LDAP directory for them and hands both to the auth
// service. A provider whose settings did not change is kept, so OIDC
// discovery is not repeated.
type AuthRuntime struct {
	svc      *auth.Service
	oidcOpts oidc.Options
	ldapOpts ldap.Options
	log      *slog.Logger

	mu       sync.Mutex
	oidcCfg  auth.OIDCConfig
	ldapCfg  auth.LDAPConfig
	dir      auth.Directory
	provider atomic.Pointer[oidc.Provider]
}

// NewAuthRuntime starts from what svc has in effect and the OIDC provider
// built for it (nil when OIDC is off). oidcOpts and ldapOpts are used for
// providers built later.
func NewAuthRuntime(svc *auth.Service, current *oidc.Provider, oidcOpts oidc.Options, ldapOpts ldap.Options) *AuthRuntime {
	r := &AuthRuntime{svc: svc, oidcOpts: oidcOpts, ldapOpts: ldapOpts, log: oidcOpts.Logger}
	if r.log == nil {
		r.log = slog.Default()
	}
	r.oidcCfg, r.ldapCfg = svc.External()
	r.dir = svc.Directory()
	r.provider.Store(current)
	return r
}

// Provider returns the OIDC provider in effect (nil when OIDC is off), for
// the sign-in handlers.
func (r *AuthRuntime) Provider() *oidc.Provider { return r.provider.Load() }

// Apply puts o and l into effect. On error nothing changes.
func (r *AuthRuntime) Apply(ctx context.Context, o auth.OIDCConfig, l auth.LDAPConfig) error {
	o, l = o.WithDefaults(), l.WithDefaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	provider, dir := r.provider.Load(), r.dir
	oidcChanged := !reflect.DeepEqual(o, r.oidcCfg) || (o.Enabled && provider == nil)
	if oidcChanged {
		provider = nil
		if o.Enabled {
			p, err := oidc.New(o, r.oidcOpts)
			if err != nil {
				return fmt.Errorf("oidc: %w", err)
			}
			provider = p
		}
	}
	if !reflect.DeepEqual(l, r.ldapCfg) || (l.Enabled && dir == nil) {
		dir = nil
		if l.Enabled {
			d, err := ldap.New(l, r.ldapOpts)
			if err != nil {
				return fmt.Errorf("ldap: %w", err)
			}
			dir = d
		}
	}
	if err := r.svc.SetExternal(ctx, o, l, dir); err != nil {
		return err
	}
	r.oidcCfg, r.ldapCfg, r.dir = o, l, dir
	r.provider.Store(provider)
	if oidcChanged && provider != nil {
		go provider.Warm(context.WithoutCancel(ctx))
	}
	return nil
}
