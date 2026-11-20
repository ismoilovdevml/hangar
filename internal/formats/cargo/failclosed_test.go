package cargo_test

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
// component up, the crate is not served.
func TestDownloadFailsClosed(t *testing.T) {
	var lf *lookupFails
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		lf = &lookupFails{Components: env.Components}
		d.Components = lf
	})
	f.mustPublish(t, "cargo-hosted", "serde_like", "1.2.3", nil)
	const p = "/repository/cargo-hosted/api/v1/crates/serde_like/1.2.3/download"
	if rec := f.get(t, p); rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	lf.fail.Store(true)
	if rec := f.get(t, p); rec.Code != http.StatusInternalServerError {
		t.Fatalf("download while the quarantine lookup fails = %d, want 500", rec.Code)
	}
}
