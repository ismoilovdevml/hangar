package auth

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxAllowedCIDRs bounds the networks a token may be restricted to.
const maxAllowedCIDRs = 32

// normalizeCIDRs validates a token's allowed client networks: CIDR
// prefixes or single addresses (a /32 or /128). It returns them masked,
// sorted and de-duplicated; nil for none (no restriction).
func normalizeCIDRs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxAllowedCIDRs {
		return nil, fmt.Errorf("at most %d allowed networks: %w", maxAllowedCIDRs, core.ErrInvalid)
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		var pfx netip.Prefix
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("allowed network %q: want an address or CIDR such as 203.0.113.0/24: %w", raw, core.ErrInvalid)
			}
			pfx = p
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil || a.Zone() != "" {
				return nil, fmt.Errorf("allowed network %q: want an address or CIDR such as 203.0.113.0/24: %w", raw, core.ErrInvalid)
			}
			pfx = netip.PrefixFrom(a, a.BitLen())
		}
		if pfx.Addr().Is4In6() {
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), max(pfx.Bits()-96, 0))
		}
		out = append(out, pfx.Masked().String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// addressAllowed reports whether the client address ip is inside one of
// cidrs. An unknown or malformed address is never allowed.
func addressAllowed(cidrs []string, ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}

// inheritTokenLimits keeps a personal access or service-account token that
// a token-authenticated caller creates within the calling token's own
// limits, so a short-lived or address-restricted token cannot mint a
// longer-lived or unrestricted one: expires must not be later than the
// calling token's expiry, and the allowed networks must lie inside the
// calling token's (none requested means the calling token's). Scopes and
// the read-only flag are checked by planToken. It returns the networks to
// store. Callers signed in with a browser session (no token) are not
// limited: that is how tokens are made in the web UI.
func (s *Service) inheritTokenLimits(ctx context.Context, expires time.Time, cidrs []string) ([]string, error) {
	cur, err := s.CurrentToken(ctx)
	if err != nil || cur == nil {
		return cidrs, err
	}
	if expires.After(cur.ExpiresAt) {
		return nil, fmt.Errorf("token expires_at must not be later than the calling token's (%s): %w",
			cur.ExpiresAt.UTC().Format(time.RFC3339), core.ErrForbidden)
	}
	if len(cur.AllowedCIDRs) == 0 {
		return cidrs, nil
	}
	if len(cidrs) == 0 {
		return normalizeCIDRs(cur.AllowedCIDRs)
	}
	for _, c := range cidrs {
		if !prefixWithin(c, cur.AllowedCIDRs) {
			return nil, fmt.Errorf("allowed network %s is outside the calling token's allowed networks %v: %w",
				c, cur.AllowedCIDRs, core.ErrForbidden)
		}
	}
	return cidrs, nil
}

// prefixWithin reports whether the network child lies entirely inside one
// of parents. Unparsable values are never inside.
func prefixWithin(child string, parents []string) bool {
	c, err := netip.ParsePrefix(child)
	if err != nil {
		return false
	}
	for _, s := range parents {
		p, err := netip.ParsePrefix(s)
		if err == nil && p.Bits() <= c.Bits() && p.Contains(c.Addr()) {
			return true
		}
	}
	return false
}

// readOnlyScopes narrows scopes to reading: every privilege keeps its domain
// and pattern with the read action. nil (the owner's full rights) becomes
// read access to every repository and system area.
func readOnlyScopes(scopes []core.Privilege) []core.Privilege {
	if scopes == nil {
		return []core.Privilege{
			{Domain: core.DomainRepo, Pattern: "*", Action: core.ActionRead},
			{Domain: core.DomainSystem, Pattern: "*", Action: core.ActionRead},
		}
	}
	out := make([]core.Privilege, 0, len(scopes))
	for _, p := range scopes {
		p.Action = core.ActionRead
		out = append(out, p)
	}
	return compactPrivileges(out)
}
