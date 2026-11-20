package pypi_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// lookupFails fails component lookups while fail is set, as an
// unreachable database does.
type lookupFails struct {
	*coretest.Components
	fail atomic.Bool
}

func (l *lookupFails) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	if l.fail.Load() {
		return nil, errors.New("database unreachable")
	}
	return l.Components.GetComponentByID(ctx, id)
}

// TestDownloadFailsClosed: when the quarantine check cannot look the
// component up, the file is not served.
func TestDownloadFailsClosed(t *testing.T) {
	var lf *lookupFails
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		lf = &lookupFails{Components: env.Components}
		d.Components = lf
	})
	whl := buildWheel(t, "Tiny.Pkg", "0.1.0", ">=3.8")
	f.upload(t, "/repository/pypi-hosted/", "Tiny.Pkg", "0.1.0", whl)
	p := "/repository/pypi-hosted/packages/tiny-pkg/" + whl.filename
	if rec := f.get(t, p, "", "bob", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	lf.fail.Store(true)
	if rec := f.get(t, p, "", "bob", "secret"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("download while the quarantine lookup fails = %d, want 500", rec.Code)
	}
}
