package core_test

import (
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestRepositoryDistroSetting(t *testing.T) {
	repo := func(format, distro string) *core.Repository {
		return &core.Repository{Name: "dist", Format: format, Type: core.RepoHosted, BlobStore: "default",
			Config: core.RepoConfig{Scanning: &core.ScanningConfig{Distro: distro}}}
	}
	for _, ok := range []struct{ format, distro string }{
		{"apt", "Debian:12"}, {"apt", "Ubuntu:22.04:LTS"}, {"rpm", "Rocky Linux:9"}, {"rpm", "AlmaLinux:9"},
		{"rpm", "Red Hat:enterprise_linux:9::baseos"}, {"rpm", "openSUSE:Leap 15.6"}, {"apt", ""}, {"npm", ""},
	} {
		if err := repo(ok.format, ok.distro).Validate(); err != nil {
			t.Errorf("%s %q: %v", ok.format, ok.distro, err)
		}
	}
	for _, bad := range []struct{ format, distro string }{
		{"npm", "Debian:12"}, {"apt", "Rocky Linux:9"}, {"rpm", "Debian:12"}, {"apt", "Debian"}, {"apt", "Debian:"},
		{"apt", " Debian:12"}, {"apt", "Debian:12\n"}, {"apt", "Debian;12"},
	} {
		if err := repo(bad.format, bad.distro).Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s %q: err = %v, want ErrInvalid", bad.format, bad.distro, err)
		}
	}
}
