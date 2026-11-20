package helm_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
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
// component up, the chart is not served.
func TestDownloadFailsClosed(t *testing.T) {
	f := newFixture(t)
	lf := &lookupFails{Components: f.env.Components}
	deps := f.env.Deps()
	deps.Components = lf
	f.h = formats.NewHandler(deps, helm.New())
	f.mustUpload(hostedName, chartTgz(t, "web", "1.0.0", ""))
	const p = "/repository/" + hostedName + "/web/1.0.0/web-1.0.0.tgz"
	if rec := f.do(http.MethodGet, p, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	lf.fail.Store(true)
	if rec := f.do(http.MethodGet, p, nil, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("download while the quarantine lookup fails = %d, want 500", rec.Code)
	}
}
