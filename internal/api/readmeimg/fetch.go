package readmeimg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// accept lists the served image types for the remote's content negotiation.
const accept = "image/avif,image/webp,image/png,image/svg+xml,image/gif,image/jpeg;q=0.9,image/*;q=0.5"

// fetchRemote downloads one image. The URL and every redirect hop must
// pass the SSRF guard and the scheme policy; the request carries no cookies,
// credentials or referrer.
func (s *Service) fetchRemote(ctx context.Context, rawURL string) (*Image, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, redact(rawURL))
	}
	if err := s.checkHop(ctx, u); err != nil {
		return nil, err
	}
	if s.opts.Clients == nil {
		return nil, fmt.Errorf("no outbound HTTP client: %w", core.ErrNotImplemented)
	}
	base, err := s.opts.Clients.ForUpstream(u.Scheme+"://"+u.Host, core.UpstreamHTTP{}, nil)
	if err != nil {
		return nil, err
	}
	client := *base
	client.Jar = nil
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > s.opts.MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", s.opts.MaxRedirects)
		}
		// The client copies headers to the next hop; it would add a Referer.
		req.Header.Del("Referer")
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		return s.checkHop(req.Context(), req.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, redact(rawURL))
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redact(rawURL), unwrapURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: remote answered %d", redact(rawURL), resp.StatusCode)
	}
	if resp.ContentLength > s.opts.MaxBytes {
		return nil, fmt.Errorf("fetch %s: %w (%d bytes, limit %d)", redact(rawURL), ErrTooLarge, resp.ContentLength, s.opts.MaxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redact(rawURL), err)
	}
	if int64(len(body)) > s.opts.MaxBytes {
		return nil, fmt.Errorf("fetch %s: %w (limit %d bytes)", redact(rawURL), ErrTooLarge, s.opts.MaxBytes)
	}
	ct := Sniff(body)
	if ct == "" {
		return nil, fmt.Errorf("fetch %s: %w (declared %q)", redact(rawURL), ErrNotImage, resp.Header.Get("Content-Type"))
	}
	return &Image{ContentType: ct, Body: body}, nil
}

// checkHop applies the scheme policy and the SSRF guard to one URL.
func (s *Service) checkHop(ctx context.Context, u *url.URL) error {
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && s.opts.AllowHTTP:
	default:
		return fmt.Errorf("%w: scheme %q (%s)", ErrNotAllowed, u.Scheme, redact(u.String()))
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in %s", ErrNotAllowed, redact(u.String()))
	}
	if s.opts.Guard == nil {
		return fmt.Errorf("no outbound address policy: %w", core.ErrNotImplemented)
	}
	proxied := false
	if s.opts.Router != nil {
		if p, err := s.opts.Router.ProxyFor(u.String(), core.UpstreamHTTP{}); err == nil && p != nil {
			proxied = true
		}
	}
	return s.opts.Guard.Check(ctx, u, proxied)
}

// unwrapURLError drops the *url.Error wrapper, whose message repeats the URL.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redact shortens a URL for error messages and logs: scheme, host and path.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	return strings.ToValidUTF8(out, "")
}
