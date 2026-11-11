// Package webui serves the single-page web application.
//
// The React app is built by `npm --prefix web run build` into web/dist. A
// release build copies web/dist to internal/webui/dist and compiles with
// `-tags webui`, which embeds it (see `make build-embed` and the Dockerfile).
// Without the tag, or when dist holds no index.html, a small fallback page is
// served instead, so backend-only builds and tests never depend on Node.
package webui

import (
	"bytes"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/version"
)

// Embedded reports whether the web UI is compiled into this binary.
func Embedded() bool {
	_, ok := dist()
	return ok
}

// Handler serves the SPA: real files from the build, and index.html for any
// other path so client-side routes (/repositories, /packages/...) deep-link.
func Handler() http.Handler {
	files, ok := dist()
	if !ok {
		return fallback()
	}
	return spa(files)
}

// encodings are the precompressed variants the web build writes next to a
// file (see web/vite.config.ts), in the order preferred when the client
// accepts several equally.
var encodings = []struct{ token, ext string }{
	{"br", ".br"},
	{"gzip", ".gz"},
}

// spa serves the build in files. A file with precompressed variants is sent
// in the best encoding the client accepts, with Vary: Accept-Encoding.
// Content-hashed assets are cached for a year; index.html is revalidated on
// every load so a new release is picked up at once.
func spa(files fs.FS) http.Handler {
	variants := precompressed(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(files, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite emits content-hashed file names under assets/.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				serveFile(w, r, files, name, variants[name])
				return
			}
			if strings.HasPrefix(name, "assets/") {
				// A chunk of another release (a tab opened before an
				// upgrade): answering with the page would fail as a script
				// with a confusing MIME error instead of a plain 404.
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		serveFile(w, r, files, "index.html", variants["index.html"])
	})
}

// precompressed maps each file of files that has precompressed variants to
// the encodings available for it.
func precompressed(files fs.FS) map[string][]string {
	out := map[string][]string{}
	_ = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, e := range encodings {
			if orig, ok := strings.CutSuffix(name, e.ext); ok {
				if st, err := fs.Stat(files, orig); err == nil && !st.IsDir() {
					out[orig] = append(out[orig], e.token)
				}
			}
		}
		return nil
	})
	return out
}

// serveFile sends name, or its variant in the encoding negotiated from the
// request's Accept-Encoding among available.
func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string, available []string) {
	h := w.Header()
	if len(available) > 0 {
		h.Add("Vary", "Accept-Encoding")
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	}
	enc := negotiate(r.Header.Get("Accept-Encoding"), available)
	if enc == "" {
		f, err := files.Open(name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer func() { _ = f.Close() }()
		rs, ok := f.(io.ReadSeeker)
		if !ok {
			b, err := io.ReadAll(f)
			if err != nil {
				http.Error(w, "read failed", http.StatusInternalServerError)
				return
			}
			rs = bytes.NewReader(b)
		}
		http.ServeContent(w, r, name, time.Time{}, rs)
		return
	}
	// The encoded variant is sent whole: a byte range of compressed data
	// is of no use to a browser, and http.ServeContent would drop the
	// Content-Length of an encoded body.
	var ext string
	for _, e := range encodings {
		if e.token == enc {
			ext = e.ext
		}
	}
	f, err := files.Open(name + ext)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Encoding", enc)
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// negotiate picks the encoding of available the Accept-Encoding header
// prefers: the highest quality value, ties going to the order of encodings.
// It returns "" when the client accepts none of them (send the file as is).
func negotiate(header string, available []string) string {
	if header == "" || len(available) == 0 {
		return ""
	}
	q := map[string]float64{}
	wildcard := -1.0
	for part := range strings.SplitSeq(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		v := 1.0
		for p := range strings.SplitSeq(params, ";") {
			k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil {
					f = 0
				}
				v = f
			}
		}
		if token == "*" {
			wildcard = v
			continue
		}
		q[token] = v
	}
	best, bestQ := "", 0.0
	for _, e := range encodings {
		if !slices.Contains(available, e.token) {
			continue
		}
		v, ok := q[e.token]
		if !ok {
			v = max(wildcard, 0)
		}
		if v > bestQ {
			best, bestQ = e.token, v
		}
	}
	return best
}

var fallbackTmpl = template.Must(template.New("fallback").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Hangar</title>
<style>
  :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
  body { max-width: 40rem; margin: 4rem auto; padding: 0 1rem; line-height: 1.5; }
  code { font-family: ui-monospace, monospace; }
</style>
</head>
<body>
<h1>Hangar {{.Version}}</h1>
<p>The server is running. This binary was built without the web UI.</p>
<ul>
  <li>REST API: <code>/api/v1/</code></li>
  <li>Health: <a href="/healthz"><code>/healthz</code></a>, <a href="/readyz"><code>/readyz</code></a></li>
  <li>Repositories: <code>/repository/&lt;name&gt;/</code>, OCI: <code>/v2/</code></li>
</ul>
<p>Build with the UI: <code>make build-embed</code>.</p>
</body>
</html>
`))

func fallback() http.Handler {
	var buf bytes.Buffer
	_ = fallbackTmpl.Execute(&buf, version.Get())
	page := buf.Bytes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})
}
