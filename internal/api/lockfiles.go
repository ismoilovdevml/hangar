package api

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks/importer"
)

// parseLockfile handles POST /lockfiles?repository=<name>&filename=<name>:
// it reads the coordinates a lockfile (or a plain list) pins for the
// repository's format, for a pre-warm task. The body is the file, raw or
// gzip-compressed (Content-Encoding: gzip), so large lockfiles fit the API
// body limit. Nothing is stored.
func (a *API) parseLockfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionWrite, core.SystemResource(core.SystemTasks)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := r.URL.Query()
	name, filename := q.Get("repository"), q.Get("filename")
	if name == "" || filename == "" {
		a.writeErr(w, r, fmt.Errorf("repository and filename are required: %w", core.ErrInvalid))
		return
	}
	repo, err := a.Deps.Repos.Get(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	var body io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			a.writeErr(w, r, fmt.Errorf("gzip body: %w", core.ErrInvalid))
			return
		}
		defer gz.Close()
		body = gz
	}
	b, err := io.ReadAll(io.LimitReader(body, importer.MaxLockfileBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			err = fmt.Errorf("lockfile larger than %d bytes; send it gzip-compressed: %w", mbe.Limit, core.ErrInvalid)
		} else {
			err = fmt.Errorf("read lockfile: %w: %w", err, core.ErrInvalid)
		}
		a.writeErr(w, r, err)
		return
	}
	res, err := importer.ParseLockfile(repo.Format, filename, b)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}
