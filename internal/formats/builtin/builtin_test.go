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
	if got := reg.Names(); !slices.Equal(got, []string{"cargo", "docker", "go", "helm", "maven", "npm", "nuget", "pypi", "raw"}) {
		t.Fatalf("names = %v", got)
	}
	env := coretest.NewEnv(t)
	for _, f := range reg.All() {
		repo := env.AddRepo(t, &core.Repository{Name: f.Name() + "-hosted", Format: f.Name(), Type: core.RepoHosted})
		if err := reg.Validate(repo); err != nil {
			t.Errorf("%s: Validate = %v", f.Name(), err)
		}
	}
	// Every format is mounted behind the dispatcher; unknown repositories are 404.
	h := formats.NewHandler(env.Deps(), Formats()...)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repository/missing/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown repository = %d, want 404", rec.Code)
	}
}
