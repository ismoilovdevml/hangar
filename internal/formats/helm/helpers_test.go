package helm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
)

// chartTgz builds a chart archive the way `helm package` lays it out: one
// top-level directory with Chart.yaml, values.yaml, a template and a README.
// chartYAML replaces the generated Chart.yaml when set.
func chartTgz(t testing.TB, name, version, chartYAML string) []byte {
	t.Helper()
	if chartYAML == "" {
		chartYAML = fmt.Sprintf(`apiVersion: v2
name: %s
version: %s
appVersion: "1.16.0"
description: The %s chart
type: application
home: https://charts.example.org/%s
sources:
  - https://git.example.org/acme/%s
keywords: [web, demo]
maintainers:
  - name: Alice
    email: alice@example.org
annotations:
  artifacthub.io/license: Apache-2.0
`, name, version, name, name, name)
	}
	files := map[string]string{
		"Chart.yaml":                chartYAML,
		"values.yaml":               "replicaCount: 1\n",
		"templates/configmap.yaml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\ndata:\n  replicas: {{ .Values.replicaCount | quote }}\n",
		"README.md":                 "# " + name + "\n\nA test chart.\n",
		"charts/sub/Chart.yaml":     "apiVersion: v2\nname: not-me\nversion: 9.9.9\n",
		"templates/NOTES.txt":       "installed\n",
		"templates/_helpers.tpl":    "{{/* helpers */}}\n",
		".helmignore":               "*.swp\n",
		"templates/tests/test.yaml": "# test\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Chart.yaml first, as helm package writes it; the subchart's after.
	order := []string{"Chart.yaml", "values.yaml", "templates/configmap.yaml", "templates/NOTES.txt",
		"templates/_helpers.tpl", "templates/tests/test.yaml", ".helmignore", "README.md", "charts/sub/Chart.yaml"}
	for _, f := range order {
		b := files[f]
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + f, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// provFor returns a provenance document for an archive. The signature is
// not a real one: Hangar stores provenance files, clients verify them.
func provFor(name, version string, tgz []byte) []byte {
	return fmt.Appendf(nil, `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA512

apiVersion: v2
name: %s
version: %s

...
files:
  %s-%s.tgz: sha256:%s
-----BEGIN PGP SIGNATURE-----

wsBcBAEBCgAQBQJnAAAACRAAAAAAAAAAAAAAAA==
=AAAA
-----END PGP SIGNATURE-----
`, name, version, name, version, sha(tgz))
}

type fixture struct {
	t     *testing.T
	env   *coretest.Env
	h     http.Handler
	token string
}

const (
	hostedName = "helm-hosted"
	once       = "helm-once"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: hostedName, Format: helm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllow}}})
	env.AddRepo(t, &core.Repository{Name: once, Format: helm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	token := env.Auth.AddToken("alice")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.Auth.AddUser("reader", "pw")
	env.AuthZ.Grant("reader", "repo:*:read")
	return &fixture{t: t, env: env, h: formats.NewHandler(env.Deps(), helm.New()), token: token}
}

// do sends a request as alice (Basic auth with her token) unless anon.
func (f *fixture) do(method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.SetBasicAuth("alice", f.token)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) anon(method, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// multipartBody builds a ChartMuseum upload body with the given fields.
func multipartBody(t testing.TB, fields map[string][]byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range []string{"chart", "prov"} {
		if b, ok := fields[k]; ok {
			fw, err := mw.CreateFormFile(k, "upload")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write(b)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

func (f *fixture) mustUpload(repo string, tgz []byte) {
	f.t.Helper()
	rec := f.do(http.MethodPost, "/repository/"+repo+"/api/charts", tgz, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("upload to %s = %d %s", repo, rec.Code, rec.Body)
	}
}

// index is the part of index.yaml the tests read.
type index struct {
	APIVersion string `yaml:"apiVersion"`
	Entries    map[string][]struct {
		Name        string    `yaml:"name"`
		Version     string    `yaml:"version"`
		AppVersion  string    `yaml:"appVersion"`
		APIVersion  string    `yaml:"apiVersion"`
		Description string    `yaml:"description"`
		URLs        []string  `yaml:"urls"`
		Digest      string    `yaml:"digest"`
		Created     time.Time `yaml:"created"`
		Maintainers []struct {
			Name string `yaml:"name"`
		} `yaml:"maintainers"`
	} `yaml:"entries"`
	ServerInfo struct {
		ContextPath string `yaml:"contextPath"`
	} `yaml:"serverInfo"`
}

func (f *fixture) index(repo string) index {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/repository/"+repo+"/index.yaml", nil, "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET index.yaml of %s = %d %s", repo, rec.Code, rec.Body)
	}
	var idx index
	if err := yaml.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		f.t.Fatalf("index.yaml: %v\n%s", err, rec.Body)
	}
	return idx
}

func decodeJSON(t testing.TB, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
}

func newRequest(method, path string, body []byte) *http.Request {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	return httptest.NewRequest(method, path, r)
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeYAML(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return yaml.NewDecoder(resp.Body).Decode(v)
}

func joinPath(elem ...string) string { return path.Join(elem...) }

func bytesReader(s string) io.Reader { return bytes.NewReader([]byte(s)) }
