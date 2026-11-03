package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrBlocked is returned when a remote URL or a redirect hop points at an
// address the SSRF guard does not allow. Fetch wraps it with core.ErrUpstream.
var ErrBlocked = errors.New("blocked by outbound address policy")

// Resolver looks up the addresses of a host name. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Guard keeps proxy repositories from being used to reach internal services
// (server-side request forgery). A remote URL, and every redirect hop, must be
// http or https and must not resolve to a loopback, link-local (including the
// 169.254.169.254 cloud metadata endpoint), private (RFC 1918), unique-local
// IPv6, carrier-grade NAT, multicast or otherwise reserved address — unless the
// address or host name is on the allow-list.
//
// Allow-list entries are CIDRs ("10.20.0.0/16"), single IPs ("10.20.1.5"),
// host names ("mirror.corp.example") or domain suffixes starting with a dot
// (".corp.example"). An allow-listed host name is not resolved.
//
// When a remote is reached through an outbound proxy and its name does not
// resolve locally (common in corporate networks where only the proxy has
// public DNS), the request is allowed: the proxy resolves and enforces its
// own policy. Literal IP addresses are always checked.
type Guard struct {
	allowNets  []netip.Prefix
	allowHosts []string // lower-case; ".suffix" matches subdomains
	resolver   Resolver
}

// NewGuard parses the allow-list.
func NewGuard(allow []string) (*Guard, error) {
	g := &Guard{resolver: net.DefaultResolver}
	for _, raw := range allow {
		e := strings.ToLower(strings.TrimSpace(raw))
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			g.allowNets = append(g.allowNets, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			g.allowNets = append(g.allowNets, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		if strings.ContainsAny(e, "/:@ ") {
			return nil, fmt.Errorf("ssrf allow-list entry %q: want CIDR, IP, host or .domain: %w", raw, core.ErrInvalid)
		}
		g.allowHosts = append(g.allowHosts, e)
	}
	return g, nil
}

// ValidateRemote checks a proxy repository's remote URL at create/update time.
// Errors wrap core.ErrInvalid so the API answers 400.
func (g *Guard) ValidateRemote(ctx context.Context, remoteURL string, proxied bool) error {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return fmt.Errorf("remote url: %w", core.ErrInvalid)
	}
	// Resolution failures are not a policy violation: the remote may be
	// temporarily unreachable from here. Fetch checks again on every request.
	if err := g.Check(ctx, u, proxied); errors.Is(err, ErrBlocked) {
		return fmt.Errorf("remote url %s: %w: %w", core.RedactURL(remoteURL), err, core.ErrInvalid)
	}
	return nil
}

// Check verifies that u may be requested. It returns an error wrapping
// ErrBlocked for a policy violation, or a resolution error when the host
// cannot be resolved and the request would not go through a proxy.
func (g *Guard) Check(ctx context.Context, u *url.URL, proxied bool) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q: %w", u.Scheme, ErrBlocked)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("empty host: %w", ErrBlocked)
	}
	if g.hostAllowed(host) {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return g.CheckAddr(a.WithZone(""))
	}
	addrs, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		if proxied {
			return nil
		}
		if err == nil {
			err = errors.New("no addresses")
		}
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if err := g.CheckAddr(a); err != nil {
			return fmt.Errorf("%s: %w", host, err)
		}
	}
	return nil
}

// CheckAddr reports whether a connection to a is allowed. It suits a
// net.Dialer Control hook for checking the address actually dialled.
func (g *Guard) CheckAddr(a netip.Addr) error {
	a = a.Unmap()
	for _, p := range g.allowNets {
		if p.Contains(a) {
			return nil
		}
	}
	if blockedAddr(a) {
		return fmt.Errorf("address %s: %w", a, ErrBlocked)
	}
	return nil
}

func (g *Guard) hostAllowed(host string) bool {
	for _, h := range g.allowHosts {
		if host == h || (strings.HasPrefix(h, ".") && (strings.HasSuffix(host, h) || host == h[1:])) {
			return true
		}
	}
	return false
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this" network
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, broadcast
		"fec0::/10",       // deprecated site-local
		"2001:db8::/32",   // documentation
		"100::/64",        // discard
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64   = netip.MustParsePrefix("64:ff9b::/96")
	sixTo4  = netip.MustParsePrefix("2002::/16")
	teredo  = netip.MustParsePrefix("2001::/32")
	allZero = netip.IPv6Unspecified()
)

func blockedAddr(a netip.Addr) bool {
	if !a.IsValid() || a == allZero {
		return true
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	// IPv6 forms that embed an IPv4 address reach that address.
	if a.Is6() {
		b := a.As16()
		switch {
		case nat64.Contains(a):
			return blockedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case sixTo4.Contains(a):
			return blockedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		case teredo.Contains(a):
			return true
		}
	}
	return false
}
