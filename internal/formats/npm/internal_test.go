package npm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParseName(t *testing.T) {
	tests := []struct {
		in          string
		scope, base string
		ok          bool
	}{
		{"lodash", "", "lodash", true},
		{"@acme/ui-kit", "@acme", "ui-kit", true},
		{"JSONStream", "", "JSONStream", true},
		{"a.b_c~d", "", "a.b_c~d", true},
		{"", "", "", false},
		{".hidden", "", "", false},
		{"_private", "", "", false},
		{"@acme", "", "", false},
		{"@/x", "", "", false},
		{"@acme/", "", "", false},
		{"a/b", "", "", false},
		{"node_modules", "", "", false},
		{"sp ace", "", "", false},
		{"../etc", "", "", false},
		{strings.Repeat("a", 215), "", "", false},
	}
	for _, tt := range tests {
		n, err := parseName(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("parseName(%q) err = %v, want ok=%v", tt.in, err, tt.ok)
			continue
		}
		if tt.ok && (n.Scope != tt.scope || n.Base != tt.base || n.Full != tt.in) {
			t.Errorf("parseName(%q) = %+v", tt.in, n)
		}
		if !tt.ok && !errors.Is(err, core.ErrInvalid) {
			t.Errorf("parseName(%q) err = %v, want ErrInvalid", tt.in, err)
		}
	}
}

func TestSplitPackage(t *testing.T) {
	tests := []struct {
		raw, name, rest string
	}{
		{"lodash", "lodash", ""},
		{"@acme%2fui", "@acme/ui", ""},
		{"@acme%2Fui", "@acme/ui", ""},
		{"@acme/ui", "@acme/ui", ""},
		{"%40acme%2fui", "@acme/ui", ""},
		{"@acme/ui/-/ui-1.0.0.tgz", "@acme/ui", "-|ui-1.0.0.tgz"},
		{"lodash/-/lodash-4.17.21.tgz", "lodash", "-|lodash-4.17.21.tgz"},
		{"lodash/latest", "lodash", "latest"},
		{"lodash/-rev/3-abc", "lodash", "-rev|3-abc"},
	}
	for _, tt := range tests {
		p, err := decodePath(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		n, rest, err := splitPackage(p)
		if err != nil || n.Full != tt.name || strings.Join(rest, "|") != tt.rest {
			t.Errorf("splitPackage(%q) = %q, %q, %v", tt.raw, n.Full, rest, err)
		}
	}
}

func TestVersionFromFile(t *testing.T) {
	n := pkgName{Full: "@acme/ui", Scope: "@acme", Base: "ui"}
	for in, want := range map[string]string{
		"ui-1.2.3.tgz":            "1.2.3",
		"@acme/ui-1.0.0-rc.1.tgz": "1.0.0-rc.1",
		"ui-latest.tgz":           "",
		"other-1.0.0.tgz":         "",
		"ui-1.0.0.zip":            "",
	} {
		if got := versionFromFile(n, in); got != want {
			t.Errorf("versionFromFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSemver(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.10.0", "2.0.0+build.5"}
	for i := 0; i+1 < len(order); i++ {
		a, err1 := parseSemver(order[i])
		b, err2 := parseSemver(order[i+1])
		if err1 != nil || err2 != nil {
			t.Fatalf("parse %q/%q: %v %v", order[i], order[i+1], err1, err2)
		}
		if a.compare(b) != -1 || b.compare(a) != 1 || a.compare(a) != 0 {
			t.Errorf("%s should sort before %s", order[i], order[i+1])
		}
	}
	for _, bad := range []string{"", "1", "1.0", "01.0.0", "1.0.0-", "1.0.0-a..b", "v1.0.0", "1.0.x"} {
		if _, err := parseSemver(bad); err == nil {
			t.Errorf("parseSemver(%q) accepted", bad)
		}
	}
	if got := highestVersion([]string{"1.0.0", "2.0.0-rc.1", "1.5.0", "junk"}); got != "1.5.0" {
		t.Errorf("highestVersion = %q, want 1.5.0 (releases beat prereleases)", got)
	}
	if got := highestVersion([]string{"2.0.0-rc.1", "2.0.0-rc.2"}); got != "2.0.0-rc.2" {
		t.Errorf("highestVersion prerelease only = %q", got)
	}
}

func TestValidTag(t *testing.T) {
	for tag, want := range map[string]bool{"latest": true, "beta": true, "next-2": true,
		"1.0.0": false, "v1.2.3": false, "": false, "a/b": false, "with space": false} {
		if got := validTag(tag); got != want {
			t.Errorf("validTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestParsePublishBody(t *testing.T) {
	data := []byte("tarball bytes \x00\xff with binary")
	b64 := base64.StdEncoding.EncodeToString(data)
	collect := func(got *[]byte) onDataFunc {
		return func(_ string, r io.Reader) (any, error) {
			b, err := io.ReadAll(invalidOnError{base64.NewDecoder(base64.StdEncoding, invalidOnError{r})})
			*got = b
			return len(b), err
		}
	}
	tests := []struct {
		name    string
		body    string
		wantErr error
		check   func(t *testing.T, b *publishBody, got []byte)
	}{
		{
			name: "attachments last",
			body: `{"name":"x","versions":{"1.0.0":{"name":"x","readme":"a \"quoted\" {brace}"}},` +
				`"_attachments":{"x-1.0.0.tgz":{"content_type":"application/octet-stream","data":"` + b64 + `","length":` +
				itoa(len(data)) + `}}}`,
			check: func(t *testing.T, b *publishBody, got []byte) {
				if !bytes.Equal(got, data) {
					t.Fatalf("data = %q", got)
				}
				if len(b.Attachments) != 1 || b.Attachments[0].Length != int64(len(data)) || b.Attachments[0].Result != len(data) {
					t.Fatalf("attachments = %+v", b.Attachments[0])
				}
				if string(b.Fields["name"]) != `"x"` || !strings.Contains(string(b.Fields["versions"]), `\"quoted\"`) {
					t.Fatalf("fields = %v", b.Fields)
				}
			},
		},
		{
			name: "attachments first, escaped slashes, whitespace",
			body: "{ \"_attachments\" : { \"x-1.0.0.tgz\" : { \"length\": 1, \"data\" : \"" +
				strings.ReplaceAll(b64, "/", `\/`) + "\" } } ,\n \"name\" : \"x\" }",
			check: func(t *testing.T, b *publishBody, got []byte) {
				if !bytes.Equal(got, data) || string(b.Fields["name"]) != `"x"` {
					t.Fatalf("got %q fields %v", got, b.Fields)
				}
			},
		},
		{
			name: "non-tarball attachment is skipped",
			body: `{"_attachments":{"x-1.0.0.sigstore":{"data":"{\"json\":true}"}},"name":"x"}`,
			check: func(t *testing.T, b *publishBody, got []byte) {
				if got != nil || len(b.Attachments) != 1 || b.Attachments[0].Result != nil {
					t.Fatalf("sigstore attachment was streamed: %+v", b.Attachments)
				}
			},
		},
		{name: "empty object", body: `{}`, check: func(t *testing.T, b *publishBody, _ []byte) {
			if len(b.Fields) != 0 || len(b.Attachments) != 0 {
				t.Fatal("expected nothing")
			}
		}},
		{name: "truncated", body: `{"name":"x","versions":{`, wantErr: core.ErrInvalid},
		{name: "not an object", body: `["x"]`, wantErr: core.ErrInvalid},
		{name: "trailing data", body: `{"a":1} {}`, wantErr: core.ErrInvalid},
		{name: "invalid literal", body: `{"a":nope}`, wantErr: core.ErrInvalid},
		{name: "data not a string", body: `{"_attachments":{"x.tgz":{"data":12}}}`, wantErr: core.ErrInvalid},
		{name: "unicode escape in data", body: `{"_attachments":{"x.tgz":{"data":"A"}}}`, wantErr: core.ErrInvalid},
		{name: "metadata too large", body: `{"readme":"` + strings.Repeat("a", maxMetadataSize) + `"}`, wantErr: errTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []byte
			b, err := parsePublishBody(strings.NewReader(tt.body), collect(&got))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, b, got)
		})
	}
}

func itoa(n int) string {
	var b []byte
	for n > 0 || len(b) == 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCanonicalTarball(t *testing.T) {
	scoped := pkgName{Full: "@acme/ui", Scope: "@acme", Base: "ui"}
	plain := pkgName{Full: "lodash", Base: "lodash"}
	for _, tt := range []struct {
		n             pkgName
		in, want, alt string
	}{
		{scoped, "@acme/ui/-/@acme/ui-1.0.0.tgz", "@acme/ui/-/ui-1.0.0.tgz", ""},
		{scoped, "@acme/ui/-/ui-1.0.0.tgz", "@acme/ui/-/ui-1.0.0.tgz", "@acme/ui/-/@acme/ui-1.0.0.tgz"},
		{scoped, "@acme/ui/-/@acme/ui-2.0.0-rc.1.tgz", "@acme/ui/-/ui-2.0.0-rc.1.tgz", ""},
		{scoped, "@acme/ui/-/@acme/other-1.0.0.tgz", "@acme/ui/-/@acme/other-1.0.0.tgz", ""},
		{scoped, "@acme/ui/-/meta/1.0.0.json", "@acme/ui/-/meta/1.0.0.json", ""},
		{plain, "lodash/-/lodash-4.17.21.tgz", "lodash/-/lodash-4.17.21.tgz", ""},
	} {
		if got := canonicalTarball(tt.n, tt.in); got != tt.want {
			t.Errorf("canonicalTarball(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := scopedFileTarball(tt.n, tt.in); got != tt.alt {
			t.Errorf("scopedFileTarball(%q) = %q, want %q", tt.in, got, tt.alt)
		}
	}
}

func TestRelativizeTarballs(t *testing.T) {
	n := pkgName{Full: "@types/node", Scope: "@types", Base: "node"}
	p := newPackument(n.Full)
	p.Versions["1.0.0"] = manifest{"dist": []byte(`{"tarball":"https://registry.npmjs.org/@types/node/-/node-1.0.0.tgz"}`)}
	p.Versions["2.0.0"] = manifest{"dist": []byte(`{"tarball":"https://cdn.example.com/files/node-2.0.0.tgz?sig=1"}`)}
	p.Versions["3.0.0"] = manifest{"dist": []byte(`{"tarball":"https://registry.npmjs.org/@types%2fnode/-/node-3.0.0.tgz"}`)}
	// Remotes that name scoped tarballs "@s/n/-/@s/n-<v>.tgz".
	p.Versions["4.0.0"] = manifest{"dist": []byte(`{"tarball":"https://registry.npmjs.org/@types/node/-/@types/node-4.0.0.tgz"}`)}
	p.Versions["5.0.0"] = manifest{"dist": []byte(`{"tarball":"https://registry.npmjs.org/@types%2fnode/-/@types%2fnode-5.0.0.tgz"}`)}
	p.relativizeTarballs(n, "https://registry.npmjs.org/")
	for v, want := range map[string]string{
		"1.0.0": "@types/node/-/node-1.0.0.tgz",
		"2.0.0": "@types/node/-/node-2.0.0.tgz",
		"3.0.0": "@types/node/-/node-3.0.0.tgz",
		"4.0.0": "@types/node/-/node-4.0.0.tgz",
		"5.0.0": "@types/node/-/node-5.0.0.tgz",
	} {
		if got := p.Versions[v].distStr("tarball"); got != want {
			t.Errorf("%s tarball = %q, want %q", v, got, want)
		}
	}
	if got := p.origins["@types/node/-/node-4.0.0.tgz"]; got != "https://registry.npmjs.org/@types/node/-/@types/node-4.0.0.tgz" {
		t.Errorf("origin of a renamed tarball = %q", got)
	}
	if _, ok := p.origins["@types/node/-/node-1.0.0.tgz"]; ok {
		t.Error("a tarball under the remote with the conventional name needs no origin")
	}
	out, err := p.render("https://hangar.test/repository/npm-proxy", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tarball":"https://hangar.test/repository/npm-proxy/@types/node/-/node-1.0.0.tgz"`) {
		t.Fatalf("render = %s", out)
	}
}
