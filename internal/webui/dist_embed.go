//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

// dist is populated from web/dist by `make build-embed` / the Dockerfile.
// The tracked .gitkeep keeps the pattern valid when the UI was not built;
// dist() then reports no UI and the fallback page is served.
//
//go:embed all:dist
var distFS embed.FS

func dist() (fs.FS, bool) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}
