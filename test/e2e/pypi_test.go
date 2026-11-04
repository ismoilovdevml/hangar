//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PyPI acceptance (docs/FORMATS.md, "PyPI"): twine upload into a hosted
// repository, pip / uv install through a group of hosted + pypi.org proxy,
// pip download, yank visible to pip, and poetry resolution when installed.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories) and python3; HANGAR_E2E_BLOB_STORE defaults to "default".

const pypiTinyProject = `[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[project]
name = "hangar-e2e-tiny"
version = "%s"
requires-python = ">=3.8"

[tool.setuptools]
packages = ["hangar_e2e_tiny"]
`

func TestPyPI(t *testing.T) {
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "pypi-e2e-hosted-"+suffix, "pypi-e2e-proxy-"+suffix, "pypi-e2e-group-"+suffix

	api := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}
	for _, r := range []map[string]any{
		{"name": hosted, "format": "pypi", "type": "hosted", "blob_store": store, "config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}},
		{"name": proxy, "format": "pypi", "type": "proxy", "blob_store": store, "config": map[string]any{"proxy": map[string]any{"remote_url": "https://pypi.org"}}},
		{"name": group, "format": "pypi", "type": "group", "config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}}}},
	} {
		if code, body := api(http.MethodPost, "/api/v1/repositories", r); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", r["name"], code, body)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{group, proxy, hosted} {
			api(http.MethodDelete, "/api/v1/repositories/"+n, nil)
		}
	})

	work := t.TempDir()
	venv := filepath.Join(work, "venv")
	vpy := filepath.Join(venv, "bin", "python")
	env := []string{"PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PYTHON_KEYRING_BACKEND=keyring.backends.null.Keyring"}
	pypiRun(t, work, env, python, "-m", "venv", venv)
	pypiRun(t, work, env, vpy, "-m", "pip", "install", "-q", "twine", "build", "setuptools>=61", "wheel")

	proj := filepath.Join(work, "proj")
	_ = os.MkdirAll(filepath.Join(proj, "hangar_e2e_tiny"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "pyproject.toml"), []byte(fmt.Sprintf(pypiTinyProject, "0.1.0")), 0o600)
	_ = os.WriteFile(filepath.Join(proj, "hangar_e2e_tiny", "__init__.py"), []byte("VALUE = 7\n"), 0o600)
	pypiRun(t, proj, env, vpy, "-m", "build", "--no-isolation", "--sdist", "--wheel", "--outdir", "dist")
	dists, _ := filepath.Glob(filepath.Join(proj, "dist", "*"))

	u, _ := url.Parse(base)
	u.User = url.UserPassword("__token__", token)
	index := u.String() + "/repository/" + group + "/simple/"

	t.Run("twine upload", func(t *testing.T) {
		twine := []string{"-m", "twine", "upload", "--non-interactive", "--disable-progress-bar",
			"--repository-url", base + "/repository/" + hosted + "/", "-u", "__token__", "-p", token}
		pypiRun(t, proj, env, vpy, append(append([]string{}, twine...), dists...)...)
		// allow_once: uploading the same files again does not fail
		pypiRun(t, proj, env, vpy, append(append(append([]string{}, twine...), "--skip-existing"), dists...)...)
	})
	t.Run("pip install via group", func(t *testing.T) {
		pypiRun(t, work, env, vpy, "-m", "pip", "install", "--no-cache-dir", "--index-url", index, "hangar-e2e-tiny==0.1.0", "six")
		if out := pypiRun(t, work, nil, vpy, "-c", "import hangar_e2e_tiny, six; print(hangar_e2e_tiny.VALUE)"); strings.TrimSpace(out) != "7" {
			t.Fatalf("import = %q", out)
		}
	})
	t.Run("pip download", func(t *testing.T) {
		dl := filepath.Join(work, "dl")
		pypiRun(t, work, env, vpy, "-m", "pip", "download", "--no-cache-dir", "--no-deps", "-d", dl, "--index-url", index, "hangar-e2e-tiny==0.1.0", "requests")
		if got, _ := filepath.Glob(filepath.Join(dl, "*")); len(got) != 2 {
			t.Fatalf("downloaded %v", got)
		}
	})
	t.Run("uv pip install", func(t *testing.T) {
		uv, err := exec.LookPath("uv")
		if err != nil {
			t.Skip("uv not installed")
		}
		uvenv := filepath.Join(work, "uvenv")
		pypiRun(t, work, nil, uv, "venv", "--python", python, uvenv)
		pypiRun(t, work, []string{"UV_NO_CACHE=1"}, uv, "pip", "install", "--python", filepath.Join(uvenv, "bin", "python"), "--index-url", index, "hangar-e2e-tiny==0.1.0", "six")
	})
	t.Run("yank", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/repository/"+hosted+"/yank/hangar-e2e-tiny/0.1.0", strings.NewReader(`{"reason":"e2e"}`))
		req.SetBasicAuth("__token__", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("yank: %v %v", resp, err)
		}
		_ = resp.Body.Close()
		out := pypiRun(t, work, env, vpy, "-m", "pip", "download", "--no-cache-dir", "--no-deps", "-d", filepath.Join(work, "dl2"), "--index-url", index, "hangar-e2e-tiny==0.1.0")
		if !strings.Contains(out, "yanked") {
			t.Errorf("pip did not report the yank:\n%s", out)
		}
	})
	t.Run("poetry", func(t *testing.T) {
		poetry, err := exec.LookPath("poetry")
		if err != nil {
			t.Skip("poetry not installed")
		}
		app := filepath.Join(work, "app")
		_ = os.MkdirAll(app, 0o755)
		_ = os.WriteFile(filepath.Join(app, "pyproject.toml"), []byte(`[project]
name = "app"
version = "0.0.1"
requires-python = ">=3.8"
dependencies = ["hangar-e2e-tiny==0.1.0", "six"]

[[tool.poetry.source]]
name = "hangar"
url = "`+base+"/repository/"+group+`/simple/"
priority = "primary"
`), 0o600)
		penv := append([]string{"POETRY_HTTP_BASIC_HANGAR_USERNAME=__token__", "POETRY_HTTP_BASIC_HANGAR_PASSWORD=" + token}, env...)
		pypiRun(t, app, penv, poetry, "lock")
	})
}

func pypiRun(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, out)
	}
	return string(out)
}
