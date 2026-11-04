package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/pkginfo"
	"github.com/ismoilovdevml/hangar/internal/api/readmeimg"
)

// ReadmeImages signs README image URLs and fetches remote images
// (implemented by readmeimg.Service).
type ReadmeImages interface {
	Mode() readmeimg.Mode
	Sign(componentID, kind, source string) string
	Verify(componentID, kind, source, sig string) bool
	NormalizeURL(raw string) (string, bool)
	Fetch(ctx context.Context, rawURL string) (*readmeimg.Image, error)
}

// CSPImage sandboxes images served from README sources: an SVG opened on
// its own runs no script and loads nothing.
const CSPImage = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox"

// readmeImageCache is how long browsers keep a README image. Package files
// never change; remote images are refreshed by the server once a day.
const (
	remoteImageMaxAge = 24 * 60 * 60
	memberImageMaxAge = 7 * 24 * 60 * 60
)

// componentInfoJSON is schema ComponentInfo in api/openapi.yaml.
type componentInfoJSON struct {
	Description string      `json:"description,omitempty"`
	Licenses    []string    `json:"licenses"`
	Keywords    []string    `json:"keywords"`
	Homepage    string      `json:"homepage,omitempty"`
	Readme      *readmeJSON `json:"readme,omitempty"`
	IconURL     string      `json:"icon_url,omitempty"`
	// ReadmeImages is ui.readme_images: proxy, direct or off.
	ReadmeImages string `json:"readme_images"`
}

// readmeJSON is schema Readme.
type readmeJSON struct {
	Format    string            `json:"format"`
	Content   string            `json:"content"`
	Source    string            `json:"source"`
	Truncated bool              `json:"truncated"`
	Images    map[string]string `json:"images"`
}

// componentInfoOut turns the extracted facts into the API answer: every
// image source becomes a URL the web UI may load under its content security
// policy, or is left out (the UI shows its alternative text).
func (a *API) componentInfoOut(componentID string, info *pkginfo.Info) componentInfoJSON {
	out := componentInfoJSON{Description: info.Description, Licenses: info.Licenses, Keywords: info.Keywords,
		Homepage: info.Homepage, ReadmeImages: string(readmeimg.ModeOff)}
	if a.ReadmeImages != nil {
		out.ReadmeImages = string(a.ReadmeImages.Mode())
	}
	if r := info.Readme; r != nil {
		out.Readme = &readmeJSON{Format: r.Format, Content: r.Content, Source: r.Source, Truncated: r.Truncated, Images: map[string]string{}}
		for ref, img := range r.Images {
			if u := a.imageURL(componentID, img); u != "" {
				out.Readme.Images[ref] = u
			}
		}
	}
	if info.Icon != nil {
		out.IconURL = a.imageURL(componentID, *info.Icon)
	}
	return out
}

// imageURL is where the browser loads img from: a signed same-origin URL
// (files in the package; remote images in proxy mode), the remote URL
// itself (direct mode), or "" (off, or not loadable).
func (a *API) imageURL(componentID string, img pkginfo.Image) string {
	ri := a.ReadmeImages
	if ri == nil {
		return ""
	}
	signed := func(kind, source string) string {
		return Prefix + "/components/" + url.PathEscape(componentID) + "/readme-images/" + ri.Sign(componentID, kind, source) +
			"?" + url.Values{kind: {source}}.Encode()
	}
	if img.Member != "" {
		return signed(readmeimg.KindMember, img.Member)
	}
	u, ok := ri.NormalizeURL(img.URL)
	if !ok {
		return ""
	}
	switch ri.Mode() {
	case readmeimg.ModeProxy:
		return signed(readmeimg.KindURL, u)
	case readmeimg.ModeDirect:
		return u
	}
	return ""
}

// getReadmeImage serves one README image: a file of the package, or a
// remote image fetched through the server. The signature proves the
// server itself issued the URL for this component; the caller needs read
// access to the component's repository like for the README itself.
func (a *API) getReadmeImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.ReadmeImages == nil || a.PackageInfo == nil {
		a.writeErr(w, r, notImplemented("README images"))
		return
	}
	c, repo, err := a.readableComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	var kind, source string
	switch {
	case q.Has(readmeimg.KindURL) && !q.Has(readmeimg.KindMember):
		kind, source = readmeimg.KindURL, q.Get(readmeimg.KindURL)
	case q.Has(readmeimg.KindMember) && !q.Has(readmeimg.KindURL):
		kind, source = readmeimg.KindMember, q.Get(readmeimg.KindMember)
	default:
		WriteError(w, http.StatusBadRequest, "invalid_argument", "exactly one of the query parameters u and p is required")
		return
	}
	if !a.ReadmeImages.Verify(c.ID, kind, source, chi.URLParam(r, "sig")) {
		WriteError(w, http.StatusForbidden, "invalid_signature", "the image URL is not signed by this server")
		return
	}
	var (
		ct     string
		body   []byte
		maxAge int
	)
	switch kind {
	case readmeimg.KindURL:
		if a.ReadmeImages.Mode() != readmeimg.ModeProxy {
			WriteError(w, http.StatusNotFound, "not_found", "remote README images are not loaded through this server (ui.readme_images)")
			return
		}
		img, err := a.ReadmeImages.Fetch(ctx, source)
		if err != nil {
			a.logger().DebugContext(ctx, "readme image unavailable", "component", c.ID, "error", err)
			WriteError(w, http.StatusBadGateway, "image_unavailable", "the image could not be loaded from its host")
			return
		}
		ct, body, maxAge = img.ContentType, img.Body, remoteImageMaxAge
	default:
		b, err := a.PackageInfo.Member(ctx, repo, c, source, readmeimg.DefaultMaxBytes)
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		if ct = readmeimg.Sniff(b); ct == "" {
			WriteError(w, http.StatusUnsupportedMediaType, "not_an_image", "the file is not a PNG, JPEG, GIF, WebP, AVIF or SVG image")
			return
		}
		body, maxAge = b, memberImageMaxAge
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ct)
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	hdr.Set("Content-Security-Policy", CSPImage)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
	hdr.Set("Cache-Control", "private, max-age="+strconv.Itoa(maxAge))
	hdr.Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) //nolint:gosec // sniffed image type, nosniff and a sandbox policy: never a document that runs
}
