package pkginfo

import (
	"context"
	"reflect"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestGoModuleInfo(t *testing.T) {
	e := newEnv(t)
	r := repo("go-hosted", "go", core.RepoHosted)
	mod, ver := "github.com/Acme/lib/v2", "v2.1.0"
	p := mod + "@" + ver + "/"
	z := zipOf(t, map[string]string{
		p + "go.mod":         "module " + mod + "\n",
		p + "README.md":      "# lib\n\n![logo](docs/logo.png)\n",
		p + "README.txt":     "plain",
		p + "docs/logo.png":  "png",
		p + "LICENSE":        "                                 Apache License\n                           Version 2.0, January 2004\n",
		p + "sub/LICENSE":    "MIT License",
		p + "sub/README.md":  "nested",
		p + "LICENSE-THIRDS": "other",
	})
	c := e.publish(r, &core.Component{Name: mod, Version: ver}, file{mod + "/@v/" + ver + "/" + ver + ".zip", z})
	i := e.info(r, c)
	if i.Readme == nil || i.Readme.Content != "# lib\n\n![logo](docs/logo.png)" || i.Readme.Source != "README.md in the module zip" {
		t.Fatalf("readme = %+v", i.Readme)
	}
	if !reflect.DeepEqual(i.Licenses, []string{"Apache-2.0"}) || i.Homepage != "https://github.com/Acme/lib" {
		t.Errorf("info = %+v", i)
	}
	if img := i.Readme.Images["docs/logo.png"]; img.Member != p+"docs/logo.png" {
		t.Errorf("readme image = %+v", i.Readme.Images)
	}
	if b, err := e.svc.Member(context.Background(), r, c, p+"docs/logo.png", 1<<20); err != nil || string(b) != "png" {
		t.Errorf("member = %q, %v", b, err)
	}

	// An unknown licence is named by its file; modules elsewhere have no home page.
	c2 := e.publish(r, &core.Component{Name: "example.com/x", Version: "v1.0.0"},
		file{"example.com/x/@v/v1.0.0/v1.0.0.zip", zipOf(t, map[string]string{"example.com/x@v1.0.0/COPYING": "custom terms"})})
	if i := e.info(r, c2); !reflect.DeepEqual(i.Licenses, []string{"License file: COPYING"}) || i.Homepage != "" || i.Readme != nil {
		t.Errorf("info = %+v", i)
	}
}

func TestLicenseID(t *testing.T) {
	for _, tc := range []struct{ name, head, want string }{
		{"LICENSE-MIT", "", "MIT"},
		{"LICENSE.apache", "", "Apache-2.0"},
		{"LICENSE", "Copyright (c) 2009 The Go Authors. All rights reserved.\n\nRedistribution and use in source and binary forms, with or without\nmodification... Neither the name of Google Inc.", "BSD-3-Clause"},
		{"LICENSE", "Mozilla Public License Version 2.0", "MPL-2.0"},
		{"COPYING", "GNU GENERAL PUBLIC LICENSE\n Version 3, 29 June 2007", "GPL-3.0"},
		{"LICENSE", "ISC License\n\nCopyright", "ISC"},
		{"LICENSE", "whatever", ""},
	} {
		if got := licenseID(tc.name, tc.head); got != tc.want {
			t.Errorf("licenseID(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	for mod, want := range map[string][2]string{
		"github.com/acme/x":          {"https://github.com/acme/x", ""},
		"github.com/acme/x/v3":       {"https://github.com/acme/x", ""},
		"github.com/acme/x/sub/v2":   {"https://github.com/acme/x", "sub"},
		"gitlab.com/group/sub/proj":  {"https://gitlab.com/group/sub/proj", ""},
		"golang.org/x/text":          {"", ""},
		"github.com/acme":            {"", ""},
		"bitbucket.org/team/repo/pk": {"https://bitbucket.org/team/repo", "pk"},
	} {
		if u, d := goSourceRepo(mod); u != want[0] || d != want[1] {
			t.Errorf("goSourceRepo(%s) = %q %q, want %v", mod, u, d, want)
		}
	}
}
