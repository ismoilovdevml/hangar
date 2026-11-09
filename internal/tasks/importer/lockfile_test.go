package importer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseLockfiles(t *testing.T) {
	for _, tc := range []struct {
		format, file, name string
		want               []string
		skipped            int
	}{
		{"npm", "package-lock.json", "", []string{"@acme/ui@0.2.0", "left-pad@1.1.0", "left-pad@1.2.0"}, 2},
		{"npm", "pnpm-lock.yaml", "", []string{"@acme/ui@0.2.0", "left-pad@1.2.0", "react@18.3.1"}, 1},
		{"npm", "yarn.lock", "", []string{"@acme/ui@0.2.0", "left-pad@1.2.0"}, 1},
		{"npm", "yarn-berry.lock", "yarn.lock", []string{"@acme/ui@0.2.0", "left-pad@1.2.0"}, 1},
		{"nuget", "packages.lock.json", "", []string{"Acme.Core@1.1.0", "Acme.Json@3.2.1"}, 1},
		{"pypi", "requirements.txt", "", []string{"acme-tools==1.1.0", "requests==2.31.0"}, 2},
		{"pypi", "uv.lock", "", []string{"acme-tools==1.1.0", "requests==2.31.0"}, 1},
		{"go", "go.sum", "", []string{"example.com/acme/Lib@v1.0.0/go.mod", "example.com/acme/Lib@v1.1.0"}, 0},
		{"cargo", "Cargo.lock", "", []string{"acme-core@1.2.0"}, 2},
		{"maven", "gradle.lockfile", "", []string{"com.acme.tools:cli:2.0", "com.acme:core:1.1"}, 0},
		{"helm", "Chart.lock", "", []string{"web@1.1.0"}, 1},
		{"docker", "images.txt", "", []string{"team/app:1.0", "team/app:1.1"}, 0},
	} {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "lockfiles", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			name := tc.name
			if name == "" {
				name = tc.file
			}
			res, err := ParseLockfile(tc.format, "some/dir/"+name, b)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Coordinates, tc.want) || res.Skipped != tc.skipped {
				t.Fatalf("got %v (skipped %d), want %v (skipped %d)", res.Coordinates, res.Skipped, tc.want, tc.skipped)
			}
		})
	}
	if _, err := ParseLockfile("npm", "list.txt", []byte("left-pad\n")); err == nil {
		t.Fatal("a coordinate without a version was accepted")
	}
	if _, err := ParseLockfile("go", "go.sum", []byte("not a go sum")); err == nil {
		t.Fatal("garbage go.sum accepted")
	}
}

func TestVersionSelection(t *testing.T) {
	vs := func(list ...string) []srcVersion {
		out := make([]srcVersion, len(list))
		for i, v := range list {
			out[i] = srcVersion{Version: v}
		}
		return out
	}
	names := func(in []srcVersion) []string {
		out := make([]string, len(in))
		for i, v := range in {
			out[i] = v.Version
		}
		return out
	}
	for _, tc := range []struct {
		format string
		f      Filters
		in     []string
		want   []string
	}{
		{"npm", Filters{}, []string{"1.10.0", "1.2.0", "1.9.1", "2.0.0-rc.1"}, []string{"1.2.0", "1.9.1", "1.10.0"}},
		{"npm", Filters{Prereleases: true}, []string{"2.0.0", "2.0.0-rc.1", "2.0.0-beta.2"}, []string{"2.0.0-beta.2", "2.0.0-rc.1", "2.0.0"}},
		{"npm", Filters{Versions: "^1.2.0"}, []string{"1.1.0", "1.2.0", "1.9.0", "2.0.0"}, []string{"1.2.0", "1.9.0"}},
		{"npm", Filters{Versions: "~1.2"}, []string{"1.2.0", "1.2.9", "1.3.0"}, []string{"1.2.0", "1.2.9"}},
		{"npm", Filters{Versions: ">=1 <2 || >=3"}, []string{"0.9.0", "1.5.0", "2.1.0", "3.0.0"}, []string{"1.5.0", "3.0.0"}},
		{"npm", Filters{Latest: 2}, []string{"1.0.0", "3.0.0", "2.0.0"}, []string{"2.0.0", "3.0.0"}},
		{"pypi", Filters{}, []string{"1.0", "1.1rc1", "1.1.dev3", "1.1", "1.1.post1"}, []string{"1.0", "1.1", "1.1.post1"}},
		{"pypi", Filters{Versions: "~=1.4", Prereleases: true}, []string{"1.3", "1.4", "1.9", "2.0", "2.0a1"}, []string{"1.4", "1.9"}},
		{"maven", Filters{}, []string{"1.0", "1.1-SNAPSHOT", "1.1-M1", "1.1-RC2", "1.1", "1.0.1"}, []string{"1.0", "1.0.1", "1.1"}},
		{"maven", Filters{Prereleases: true}, []string{"1.1", "1.1-RC2", "1.1-M1", "1.1-alpha-1"}, []string{"1.1-alpha-1", "1.1-M1", "1.1-RC2", "1.1"}},
		{"docker", Filters{Versions: "v1.*"}, []string{"latest", "v1.0", "v1.2", "v2.0"}, []string{"v1.0", "v1.2"}},
		{"apt", Filters{}, []string{"2.10-3", "2.11~rc1-1", "1:1.0-1"}, []string{"2.10-3", "1:1.0-1"}},
		{"go", Filters{}, []string{"v1.2.0", "v1.10.0", "v0.0.0-20200101000000-abcdef123456"}, []string{"v1.2.0", "v1.10.0"}},
	} {
		got := names(selectVersions(tc.format, tc.f, vs(tc.in...)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s %+v: got %v, want %v", tc.format, tc.f, got, tc.want)
		}
	}
	if _, err := parseVersionSpec(">="); err == nil {
		t.Error("operator without version accepted")
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		p, n string
		ok   bool
	}{
		{"*", "team/app", true}, {"@acme/*", "@acme/ui", true}, {"@acme/*", "@other/ui", false},
		{"Acme.*", "acme.core", true}, {"com.acme*:*", "com.acme.tools:cli", true}, {"lib?", "lib1", true}, {"lib?", "lib12", false},
	} {
		if globMatch(tc.p, tc.n) != tc.ok {
			t.Errorf("globMatch(%q, %q) != %v", tc.p, tc.n, tc.ok)
		}
	}
}
