package pkginfo

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestImageRefs(t *testing.T) {
	md := strings.Join([]string{
		`![Logo](https://cdn.example.com/logo.png)`,
		`[![CI](https://ci.example.com/badge.svg)](https://ci.example.com) [![NuGet](http://img.example.com/v.svg?label=NuGet "title")](https://nuget.example.com)`,
		`<p align="center"><img src="https://img.example.com/a.png?x=1&amp;y=2" width="200"></p>`,
		`<IMG alt=x SRC='images/local.png'>`,
		`![by ref][logo] ![collapsed][] ![shortcut]`,
		`![angle](<docs/my image.png>)`,
		`<!-- ![hidden](https://hidden.example.com/x.png) -->`,
		`\![escaped](https://escaped.example.com/x.png)`,
		`![dup](https://cdn.example.com/logo.png)`,
		``,
		`[logo]: https://ref.example.com/logo.svg`,
		`[collapsed]: <https://ref.example.com/c.png> "Title"`,
		`[shortcut]: ./s.png`,
		`[unused]: https://unused.example.com/x.png`,
	}, "\n")
	want := []string{
		"https://img.example.com/a.png?x=1&y=2",
		"images/local.png",
		"https://cdn.example.com/logo.png",
		"https://ci.example.com/badge.svg",
		"http://img.example.com/v.svg?label=NuGet",
		"https://ref.example.com/logo.svg",
		"https://ref.example.com/c.png",
		"./s.png",
		"docs/my image.png",
	}
	if got := imageRefs(md); !reflect.DeepEqual(got, want) {
		t.Errorf("imageRefs =\n%q\nwant\n%q", got, want)
	}
	if got := decodeEntities("a&amp;b&#38;c&#x26;d&lt;&bogus;&#0;"); got != "a&b&c&d<&bogus;&#0;" {
		t.Errorf("decodeEntities = %q", got)
	}
}

func TestRepositoryRawURLs(t *testing.T) {
	for _, tc := range []struct {
		repo sourceRepo
		want string
	}{
		{sourceRepo{url: "https://github.com/AutoMapper/AutoMapper", commit: "bf76a58"}, "https://raw.githubusercontent.com/AutoMapper/AutoMapper/bf76a58"},
		{sourceRepo{url: "git+https://github.com/acme/ui.git"}, "https://raw.githubusercontent.com/acme/ui/HEAD"},
		{sourceRepo{url: "git@github.com:acme/ui.git"}, "https://raw.githubusercontent.com/acme/ui/HEAD"},
		{sourceRepo{url: "github:acme/ui"}, "https://raw.githubusercontent.com/acme/ui/HEAD"},
		{sourceRepo{url: "acme/ui"}, "https://raw.githubusercontent.com/acme/ui/HEAD"},
		{sourceRepo{url: "https://github.com/acme/mono/tree/develop/packages/x"}, "https://raw.githubusercontent.com/acme/mono/develop"},
		{sourceRepo{url: "https://gitlab.com/group/sub/project.git"}, "https://gitlab.com/group/sub/project/-/raw/HEAD"},
		{sourceRepo{url: "https://gitlab.com/group/project/-/tree/v2"}, "https://gitlab.com/group/project/-/raw/v2"},
		{sourceRepo{url: "https://git.example.com/acme/ui"}, ""},
		{sourceRepo{url: "https://github.com/acme"}, ""},
	} {
		if got := tc.repo.rawBase(); got != tc.want {
			t.Errorf("rawBase(%+v) = %q, want %q", tc.repo, got, tc.want)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/acme/ui/blob/main/docs/logo.png":            "https://raw.githubusercontent.com/acme/ui/main/docs/logo.png",
		"https://github.com/acme/ui/raw/main/logo.png?raw=true":         "https://raw.githubusercontent.com/acme/ui/main/logo.png",
		"https://gitlab.com/group/project/-/blob/main/img/a.png":        "https://gitlab.com/group/project/-/raw/main/img/a.png",
		"https://github.com/acme/ui/actions/workflows/ci.yml/badge.svg": "https://github.com/acme/ui/actions/workflows/ci.yml/badge.svg",
		"https://img.example.com/x.svg?label=a":                         "https://img.example.com/x.svg?label=a",
	} {
		u, _ := url.Parse(in)
		if got := rawFileURL(u); got != want {
			t.Errorf("rawFileURL(%s) = %s, want %s", in, got, want)
		}
	}
}

// The .nupkg holds the README, its images and the icon; images it does
// not hold resolve against the repository at the published commit.
func TestNugetReadmeImagesAndIcon(t *testing.T) {
	e := newEnv(t)
	r := repo("nuget-proxy", "nuget", core.RepoProxy)
	spec := `<?xml version="1.0"?><package><metadata><id>AutoMapper</id><version>14.0.0</version>
		<icon>icon.png</icon><readme>docs\README.md</readme><projectUrl>https://automapper.example.com/</projectUrl>
		<repository type="git" url="https://github.com/AutoMapper/AutoMapper" commit="bf76a58" /></metadata></package>`
	readme := strings.Join([]string{
		`![AutoMapper](https://cdn.example.com/logo.png)`,
		`[![NuGet](http://img.example.com/nuget.svg)](https://nuget.example.com)`,
		`![diagram](images/Diagram.PNG) ![root](/assets/x.png) ![repo only](../src/arch.svg?raw=1)`,
		`![escape](../../../../etc/passwd) ![data](data:image/png;base64,AAAA) ![js](javascript:alert(1))`,
		`![blob](https://github.com/AutoMapper/AutoMapper/blob/main/docs/a.png)`,
	}, "\n")
	c := e.publish(r, &core.Component{Name: "automapper", Version: "14.0.0"},
		file{"v3-flatcontainer/automapper/14.0.0/automapper.14.0.0.nupkg", zipOf(t, map[string]string{
			"AutoMapper.nuspec": spec, "docs/README.md": readme, "icon.png": "\x89PNG\r\n\x1a\nicon",
			"docs/images/diagram.png": "\x89PNG\r\n\x1a\ndiagram", "assets/x.png": "\x89PNG\r\n\x1a\nx",
		})})
	i := e.info(r, c)
	if i.Readme == nil {
		t.Fatalf("no README: %+v", i)
	}
	want := map[string]Image{
		"https://cdn.example.com/logo.png": {URL: "https://cdn.example.com/logo.png"},
		"http://img.example.com/nuget.svg": {URL: "http://img.example.com/nuget.svg"},
		"images/Diagram.PNG":               {Member: "docs/images/diagram.png"},
		"/assets/x.png":                    {Member: "assets/x.png"},
		"../src/arch.svg?raw=1":            {URL: "https://raw.githubusercontent.com/AutoMapper/AutoMapper/bf76a58/src/arch.svg?raw=1"},
		"../../../../etc/passwd":           {URL: "https://raw.githubusercontent.com/AutoMapper/AutoMapper/bf76a58/etc/passwd"},
		"https://github.com/AutoMapper/AutoMapper/blob/main/docs/a.png": {URL: "https://raw.githubusercontent.com/AutoMapper/AutoMapper/main/docs/a.png"},
	}
	if !reflect.DeepEqual(i.Readme.Images, want) {
		t.Errorf("images =\n%v\nwant\n%v", i.Readme.Images, want)
	}
	if i.Icon == nil || i.Icon.Member != "icon.png" {
		t.Errorf("icon = %+v", i.Icon)
	}
	b, err := e.svc.Member(context.Background(), r, c, "docs/images/diagram.png", 1<<20)
	if err != nil || string(b) != "\x89PNG\r\n\x1a\ndiagram" {
		t.Errorf("Member = %q, %v", b, err)
	}
	if _, err := e.svc.Member(context.Background(), r, c, "missing.png", 1<<20); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("missing member: %v", err)
	}
	if _, err := e.svc.Member(context.Background(), r, c, "docs/images/diagram.png", 4); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("oversized member: %v", err)
	}

	// An iconUrl icon is a remote image.
	c2 := e.publish(r, &core.Component{Name: "old", Version: "1.0.0"},
		file{"v3-flatcontainer/old/1.0.0/old.1.0.0.nupkg", zipOf(t, map[string]string{
			"old.nuspec": `<package><metadata><iconUrl>https://icons.example.com/old.png</iconUrl></metadata></package>`})})
	if i := e.info(r, c2); i.Icon == nil || i.Icon.URL != "https://icons.example.com/old.png" {
		t.Errorf("iconUrl icon = %+v", i.Icon)
	}
}

// npm: relative images are files of the tarball (a README from the
// packument is the tarball root's), else in the repository directory.
func TestNpmReadmeImages(t *testing.T) {
	e := newEnv(t)
	r := repo("npm-proxy", "npm", core.RepoProxy)
	e.publish(r, nil, file{"ui-kit/-/packument.json", []byte(`{"name":"ui-kit","description":"UI","license":"MIT",
		"readme":"![shot](./docs/shot.png) ![logo](logo.svg) ![gone](./missing.png)",
		"repository":{"type":"git","url":"git+https://github.com/acme/mono.git","directory":"packages/ui-kit"},
		"versions":{"2.0.0":{}}}`)})
	c := e.publish(r, &core.Component{Name: "ui-kit", Version: "2.0.0"}, file{"ui-kit/-/ui-kit-2.0.0.tgz", tgz(t, map[string]string{
		"package/package.json": `{}`, "package/docs/shot.png": "\x89PNG\r\n\x1a\nshot", "package/logo.svg": "<svg></svg>",
	})})
	i := e.info(r, c)
	want := map[string]Image{
		"./docs/shot.png": {Member: "package/docs/shot.png"},
		"logo.svg":        {Member: "package/logo.svg"},
		"./missing.png":   {URL: "https://raw.githubusercontent.com/acme/mono/HEAD/packages/ui-kit/missing.png"},
	}
	if i.Readme == nil || !reflect.DeepEqual(i.Readme.Images, want) {
		t.Fatalf("npm images = %+v", i.Readme)
	}
	b, err := e.svc.Member(context.Background(), r, c, "package/docs/shot.png", 1<<20)
	if err != nil || string(b) != "\x89PNG\r\n\x1a\nshot" {
		t.Errorf("Member = %q, %v", b, err)
	}

	// Without a tarball or a known repository host, relative images stay unresolved.
	c2 := e.publish(r, &core.Component{Name: "plain", Version: "1.0.0"}, file{"plain/-/meta/1.0.0.json",
		[]byte(`{"readme":"![a](a.png) ![b](https://img.example.com/b.png)","repository":"https://git.example.com/acme/plain"}`)})
	if i := e.info(r, c2); i.Readme == nil || !reflect.DeepEqual(i.Readme.Images, map[string]Image{"https://img.example.com/b.png": {URL: "https://img.example.com/b.png"}}) {
		t.Errorf("plain images = %+v", i.Readme)
	}
}

// PyPI: relative images resolve against the source repository named in
// the project URLs; text READMEs carry no images.
func TestPypiReadmeImages(t *testing.T) {
	e := newEnv(t)
	r := repo("pypi-hosted", "pypi", core.RepoHosted)
	c := e.publish(r, &core.Component{Name: "acme-ml", Version: "1.0", Attributes: map[string]any{
		"metadata": map[string]any{
			"description": "![arch](docs/arch.png)", "description_content_type": "text/markdown",
			"project_urls": []any{"Homepage, https://ml.example.com", "Source, https://gitlab.com/acme/ml"},
		},
	}}, file{"packages/acme-ml/acme_ml-1.0-py3-none-any.whl", []byte("x")})
	if i := e.info(r, c); i.Readme == nil || i.Readme.Images["docs/arch.png"].URL != "https://gitlab.com/acme/ml/-/raw/HEAD/docs/arch.png" {
		t.Errorf("pypi images = %+v", i.Readme)
	}
	c2 := e.publish(r, &core.Component{Name: "rst", Version: "1.0", Attributes: map[string]any{
		"metadata": map[string]any{"description": ".. image:: docs/a.png"},
	}}, file{"packages/rst/rst-1.0-py3-none-any.whl", []byte("x")})
	if i := e.info(r, c2); i.Readme == nil || i.Readme.Images != nil {
		t.Errorf("rst images = %+v", i.Readme)
	}
}
