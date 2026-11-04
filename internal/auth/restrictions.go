package auth

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

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
