package nuget_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
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
// component up, the package is not served.
func TestDownloadFailsClosed(t *testing.T) {
	f := newFixture(t)
	lf := &lookupFails{Components: f.env.Components}
	deps := f.env.Deps()
	deps.Components = lf
	f.h = formats.NewHandler(deps, nuget.New())
	f.mustPush("nuget-hosted", "Acme.Lib", "1.0.0", "")
	const p = "/repository/nuget-hosted/v3-flatcontainer/acme.lib/1.0.0/acme.lib.1.0.0.nupkg"
	if rec := f.get(p); rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	lf.fail.Store(true)
	if rec := f.get(p); rec.Code != http.StatusInternalServerError {
		t.Fatalf("download while the quarantine lookup fails = %d, want 500", rec.Code)
	}
}
