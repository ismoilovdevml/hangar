package cac

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testResolver(env map[string]string, files map[string]string) Resolver {
	return Resolver{
		LookupEnv: func(n string) (string, bool) { v, ok := env[n]; return v, ok },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
	}
}

const header = "apiVersion: hangar/v1\n"

func TestParse(t *testing.T) {
	r := testResolver(map[string]string{"TOKEN": "t0k", "EMPTY": ""}, map[string]string{
		filepath.Join("/cfg", "secrets", "pw"): "p4ss\n",
		filepath.Join("/abs", "pw"):            "abs\r\n",
	})
	for _, tc := range []struct {
		name, doc string
		err       string // substring; "" for success
		check     func(t *testing.T, d *Document)
	}{
		{name: "repository with env secret", doc: header + `kind: Repository
metadata: {name: npm-proxy, labels: {team: web}}
spec:
  format: npm
  type: proxy
  config:
    proxy:
      remote_url: https://registry.npmjs.org
      auth: {kind: bearer, token: {valueFrom: {env: TOKEN}}}
`, check: func(t *testing.T, d *Document) {
			if v, _ := getPath(d.Spec, Path{"config", "proxy", "auth", "token"}); v != "t0k" {
				t.Fatalf("token = %v", v)
			}
			if len(d.Secrets) != 1 || d.Secrets[0].String() != "config.proxy.auth.token" || d.Labels["team"] != "web" {
				t.Fatalf("doc = %+v", d)
			}
		}},
		{name: "file secret relative to the document", doc: header + `kind: OutboundProxy
metadata: {name: corp}
spec: {host: p, password: {valueFrom: {file: secrets/pw}}}
`, check: func(t *testing.T, d *Document) {
			if d.Spec["password"] != "p4ss" {
				t.Fatalf("password = %q", d.Spec["password"])
			}
		}},
		{name: "absolute file secret, CRLF trimmed", doc: header + `kind: OutboundProxy
metadata: {name: corp}
spec: {password: {valueFrom: {file: /abs/pw}}}
`, check: func(t *testing.T, d *Document) {
			if d.Spec["password"] != "abs" {
				t.Fatalf("password = %q", d.Spec["password"])
			}
		}},
		{name: "numbers in the JSON model", doc: header + "kind: BlobStore\nmetadata: {name: s}\nspec: {kind: fs, soft_quota_bytes: 10737418240}\n",
			check: func(t *testing.T, d *Document) {
				if v, _ := d.Spec["soft_quota_bytes"].(interface{ String() string }); v == nil || v.String() != "10737418240" {
					t.Fatalf("quota = %#v", d.Spec["soft_quota_bytes"])
				}
			}},
		{name: "spec.name equal to metadata.name", doc: header + "kind: Role\nmetadata: {name: ci}\nspec: {name: ci, privileges: []}\n",
			check: func(t *testing.T, d *Document) {
				if _, ok := d.Spec["name"]; ok {
					t.Fatal("name kept in spec")
				}
			}},
		{name: "missing env", doc: header + "kind: OutboundProxy\nmetadata: {name: c}\nspec: {password: {valueFrom: {env: NOPE}}}\n",
			err: "spec.password: environment variable NOPE is not set"},
		{name: "empty env", doc: header + "kind: OutboundProxy\nmetadata: {name: c}\nspec: {password: {valueFrom: {env: EMPTY}}}\n",
			err: "EMPTY is empty"},
		{name: "missing file", doc: header + "kind: OutboundProxy\nmetadata: {name: c}\nspec: {password: {valueFrom: {file: nope}}}\n",
			err: "spec.password"},
		{name: "both env and file", doc: header + "kind: OutboundProxy\nmetadata: {name: c}\nspec: {password: {valueFrom: {env: TOKEN, file: x}}}\n",
			err: "exactly one of env"},
		{name: "valueFrom beside other keys", doc: header + "kind: OutboundProxy\nmetadata: {name: c}\nspec: {password: {valueFrom: {env: TOKEN}, x: 1}}\n",
			err: "valueFrom must be the only key"},
		{name: "wrong apiVersion", doc: "apiVersion: v1\nkind: Role\nmetadata: {name: r}\nspec: {}\n", err: `apiVersion "v1"`},
		{name: "unknown kind", doc: header + "kind: User\nmetadata: {name: r}\nspec: {}\n", err: `kind "User"`},
		{name: "no name", doc: header + "kind: Role\nmetadata: {}\nspec: {}\n", err: "metadata.name is required"},
		{name: "no spec", doc: header + "kind: Role\nmetadata: {name: r}\n", err: "spec is required"},
		{name: "unknown top-level key", doc: header + "kind: Role\nmetadata: {name: r}\nspec: {}\nstatus: {}\n", err: "unknown field status"},
		{name: "unknown spec key", doc: header + "kind: Role\nmetadata: {name: r}\nspec: {privilege: []}\n", err: "unknown field spec.privilege"},
		{name: "spec.name differs", doc: header + "kind: Role\nmetadata: {name: r}\nspec: {name: x}\n", err: "differs from metadata.name"},
		{name: "reserved label", doc: header + "kind: Role\nmetadata: {name: r, labels: {hangar/apply-set: x}}\nspec: {}\n", err: "reserved"},
		{name: "label not a string", doc: header + "kind: Role\nmetadata: {name: r, labels: {n: 1}}\nspec: {}\n", err: "must be a string"},
		{name: "bad label key", doc: header + "kind: Role\nmetadata: {name: r, labels: {'-x': a}}\nspec: {}\n", err: "metadata.labels.-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := Parse([]byte(tc.doc), "test.yaml", "/cfg", r)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				if !strings.HasPrefix(err.Error(), "test.yaml:") {
					t.Fatalf("err %v does not name the file", err)
				}
				return
			}
			if err != nil || len(docs) != 1 {
				t.Fatalf("Parse = %v, %v", docs, err)
			}
			tc.check(t, docs[0])
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	role := func(n string) string {
		return header + "kind: Role\nmetadata: {name: " + n + "}\nspec: {privileges: []}\n"
	}
	write("a.yaml", role("a")+"---\n"+role("b")+"---\n")
	write("notes.txt", "not yaml at all: [")
	write(".hidden/x.yaml", role("hidden"))
	write("sub/c.yml", role("c"))

	names := func(docs []*Document) []string {
		var out []string
		for _, d := range docs {
			out = append(out, d.Name)
		}
		return out
	}
	docs, err := Load([]string{dir}, false, nil, Resolver{})
	if err != nil || !slices.Equal(names(docs), []string{"a", "b"}) {
		t.Fatalf("Load = %v, %v", names(docs), err)
	}
	docs, err = Load([]string{dir}, true, nil, Resolver{})
	if err != nil || !slices.Equal(names(docs), []string{"a", "b", "c"}) {
		t.Fatalf("Load -R = %v, %v", names(docs), err)
	}
	docs, err = Load([]string{"-"}, false, strings.NewReader(role("stdin")), Resolver{})
	if err != nil || !slices.Equal(names(docs), []string{"stdin"}) {
		t.Fatalf("Load stdin = %v, %v", names(docs), err)
	}
	write("dup.yaml", role("a"))
	if _, err := Load([]string{dir}, false, nil, Resolver{}); err == nil || !strings.Contains(err.Error(), "Role/a is also declared at") {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := Load([]string{filepath.Join(dir, "missing")}, false, nil, Resolver{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
}
