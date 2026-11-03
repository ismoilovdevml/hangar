package builtin

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

func TestBuiltinFormatsRegisterAndMount(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); !slices.Equal(got, []string{"docker", "maven", "npm", "raw"}) {
		t.Fatalf("names = %v", got)
	}
	env := coretest.NewEnv(t)
	for _, f := range reg.All() {
		repo := env.AddRepo(t, &core.Repository{Name: f.Name() + "-hosted", Format: f.Name(), Type: core.RepoHosted})
		if err := reg.Validate(repo); err != nil {
			t.Errorf("%s: Validate = %v", f.Name(), err)
		}
	}
	h := formats.NewHandler(env.Deps(), Formats()...)
	for _, path := range []string{"/repository/raw-hosted/a/b.txt", "/repository/npm-hosted/@acme%2fui", "/v2/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s = %d, want 501 from the stub", path, rec.Code)
		}
	}
}
