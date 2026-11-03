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
	"io/fs"
	"net/http"
	"path"
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
	fileServer := http.FileServerFS(files)
	index, _ := fs.ReadFile(files, "index.html")
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
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
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
