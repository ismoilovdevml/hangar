package maven_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
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
// component up, the artifact is not served.
func TestDownloadFailsClosed(t *testing.T) {
	f := newFixture(t)
	lf := &lookupFails{Components: f.env.Components}
	deps := f.env.Deps()
	deps.Components = lf
	f.srv = httptest.NewServer(formats.NewHandler(deps, maven.New()))
	t.Cleanup(f.srv.Close)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "jar-1.0")
	const p = "releases/com/acme/lib/1.0/lib-1.0.jar"
	f.want(f.do(http.MethodGet, p, "reader:r", ""), http.StatusOK, "download")
	lf.fail.Store(true)
	f.want(f.do(http.MethodGet, p, "reader:r", ""), http.StatusInternalServerError, "download while the quarantine lookup fails")
}

// TestQuarantinedSnapshotNotServedByProxy: a proxy caches non-unique
// SNAPSHOT files as metadata assets (they may change upstream); they still
// belong to their component and are refused once it is quarantined, from
// the proxy and through a group.
func TestQuarantinedSnapshotNotServedByProxy(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	const jar = "org/example/snap/1.0-SNAPSHOT/snap-1.0-SNAPSHOT.jar"
	rm.set(jar, "snapshot jar")
	f.addProxy("central", rm, core.ProxyConfig{}, map[string]any{"version_policy": "mixed"})
	f.addGroup("public", "central")
	auth := "reader:r"

	f.want(f.do(http.MethodGet, "central/"+jar, auth, ""), http.StatusOK, "cache the SNAPSHOT")
	a, err := f.env.Components.GetAsset(t.Context(), f.repo["central"].ID, jar)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != core.AssetMetadata || a.ComponentID == "" {
		t.Fatalf("cached SNAPSHOT = kind %q component %q; the test relies on a metadata asset of a component", a.Kind, a.ComponentID)
	}
	f.env.Components.SetQuarantined(a.ComponentID, true)
	for _, repo := range []string{"central", "public"} {
		f.want(f.do(http.MethodGet, repo+"/"+jar, auth, ""), http.StatusForbidden, repo+" quarantined SNAPSHOT")
	}
}
