package security

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRepublishDefaultPolicy: a hosted repository created through the API
// without a write policy gets allow_once (docker: allow, tags move), and
// publishing different bytes for a released version answers 409 in every
// format instead of replacing the release.
func TestRepublishDefaultPolicy(t *testing.T) {
	h := server(t)
	for _, f := range formatsUnderTest {
		h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{
			"name": f + "-repub", "format": f, "type": "hosted", "blob_store": "default"}, http.StatusCreated)
		var repo struct {
			Config struct {
				Hosted struct {
					WritePolicy string `json:"write_policy"`
				} `json:"hosted"`
			} `json:"config"`
		}
		if err := json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/repositories/"+f+"-repub", nil, http.StatusOK), &repo); err != nil {
			t.Fatal(err)
		}
		want := "allow_once"
		if f == "docker" {
			want = "allow"
		}
		if got := repo.Config.Hosted.WritePolicy; got != want {
			t.Errorf("%s: default write policy = %q, want %q", f, got, want)
		}
	}

	status := func(method, path string, body any, hdr ...string) int {
		t.Helper()
		r, err := h.do(admin, method, path, body, hdr...)
		if err != nil {
			t.Fatal(err)
		}
		return r.status
	}
	t.Run("raw", func(t *testing.T) {
		p := "/repository/raw-repub/rel/app-1.0.tar.gz"
		h.must(admin, http.MethodPut, p, "release bytes", http.StatusCreated)
		if c := status(http.MethodPut, p, "other bytes"); c != http.StatusConflict {
			t.Errorf("re-publish = %d, want 409", c)
		}
		if b := h.must(admin, http.MethodGet, p, nil, http.StatusOK); string(b) != "release bytes" {
			t.Errorf("release replaced: %q", b)
		}
	})
	t.Run("maven", func(t *testing.T) {
		p := "/repository/maven-repub/com/acme/app/1.0/app-1.0.jar"
		h.must(admin, http.MethodPut, p, "jar 1", http.StatusCreated)
		if c := status(http.MethodPut, p, "jar 2"); c != http.StatusConflict {
			t.Errorf("release redeploy = %d, want 409", c)
		}
		s := "/repository/maven-repub/com/acme/app/2.0-SNAPSHOT/app-2.0-SNAPSHOT.jar"
		h.must(admin, http.MethodPut, s, "snap 1", http.StatusCreated)
		h.must(admin, http.MethodPut, s, "snap 2", http.StatusCreated) // SNAPSHOTs stay redeployable
	})
	t.Run("nuget", func(t *testing.T) {
		push := func(pkg []byte) int {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			fw, _ := mw.CreateFormFile("package", "package.nupkg")
			_, _ = fw.Write(pkg)
			_ = mw.Close()
			return status(http.MethodPut, "/repository/nuget-repub/api/v2/package", buf.Bytes(), "Content-Type", mw.FormDataContentType())
		}
		pkg := nupkg(t, "Acme.Repub", "1.0.0", "first")
		if c := push(pkg); c != http.StatusCreated {
			t.Fatalf("push = %d", c)
		}
		for name, p := range map[string][]byte{"same": pkg, "different": nupkg(t, "Acme.Repub", "1.0.0", "second")} {
			if c := push(p); c != http.StatusConflict {
				t.Errorf("re-push of %s bytes = %d, want 409", name, c)
			}
		}
	})
	t.Run("npm", func(t *testing.T) {
		publish := func(content string) int {
			tgz := npmTarball(t, content)
			sum := sha1.Sum(tgz)
			s512 := sha512.Sum512(tgz)
			doc := map[string]any{
				"name": "repub-pkg", "dist-tags": map[string]string{"latest": "1.0.0"},
				"versions": map[string]any{"1.0.0": map[string]any{"name": "repub-pkg", "version": "1.0.0",
					"dist": map[string]any{"shasum": hex.EncodeToString(sum[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
						"tarball": h.base + "/repository/npm-repub/repub-pkg/-/repub-pkg-1.0.0.tgz"}}},
				"_attachments": map[string]any{"repub-pkg-1.0.0.tgz": map[string]any{"content_type": "application/octet-stream",
					"data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz)}},
			}
			return status(http.MethodPut, "/repository/npm-repub/repub-pkg", doc)
		}
		if c := publish("one"); c/100 != 2 {
			t.Fatalf("publish = %d", c)
		}
		if c := publish("two"); c != http.StatusConflict && c != http.StatusForbidden {
			t.Errorf("re-publish = %d, want 409 or 403", c)
		}
	})
	t.Run("pypi twine", func(t *testing.T) { republishTwine(t, h) })
}

// republishTwine uploads a wheel with the real twine, then different bytes
// under the same file name: twine fails on the 409, and --skip-existing
// skips it where twine allows the flag. HANGAR_TEST_TWINE names the twine binary (default: twine on
// PATH); the test skips when it does not run.
func republishTwine(t *testing.T, h *harness) {
	twine := os.Getenv("HANGAR_TEST_TWINE")
	if twine == "" {
		twine = "twine"
	}
	if out, err := exec.Command(twine, "--version").CombinedOutput(); err != nil {
		t.Skipf("twine not usable (%v): %s", err, out)
	}
	dir := t.TempDir()
	run := func(files string, extra ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		args := append([]string{"upload", "--non-interactive", "--disable-progress-bar",
			"--repository-url", h.base + "/repository/pypi-repub/", "-u", adminUser, "-p", adminPassword}, extra...)
		cmd := exec.CommandContext(ctx, twine, append(args, files)...)
		cmd.Env = append(os.Environ(), "TWINE_CERT=", "KEYRING_BACKEND=keyring.backends.null.Keyring")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	whl := func(sub, body string) string {
		p := filepath.Join(dir, sub, "qapkg-1.0-py3-none-any.whl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, wheel(t, body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := whl("a", "A"), whl("b", "B")
	if out, err := run(a); err != nil {
		t.Fatalf("twine upload: %v\n%s", err, out)
	}
	out, err := run(b)
	if err == nil || !strings.Contains(out, "409") {
		t.Errorf("twine re-upload of different bytes: err %v, want a 409 failure\n%s", err, out)
	}
	// twine treats a 409 as "file exists" under --skip-existing. Twine 6
	// and later accept the flag only for the public indexes; for others it
	// stops before uploading, which is the client's choice, not a failure.
	out, err = run(b, "--skip-existing")
	switch {
	case strings.Contains(out, "UnsupportedConfiguration"):
		t.Logf("this twine refuses --skip-existing for private indexes:\n%s", out)
	case err != nil || !strings.Contains(strings.ToLower(out), "skipping"):
		t.Errorf("twine --skip-existing: err %v\n%s", err, out)
	}
	got := h.must(admin, http.MethodGet, "/repository/pypi-repub/packages/qapkg/qapkg-1.0-py3-none-any.whl", nil, http.StatusOK)
	want, _ := os.ReadFile(a)
	if !bytes.Equal(got, want) {
		t.Error("the released wheel was replaced")
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func nupkg(t *testing.T, id, version, description string) []byte {
	return zipBytes(t, map[string]string{id + ".nuspec": `<?xml version="1.0"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>
<id>` + id + `</id><version>` + version + `</version><authors>tests</authors><description>` + description + `</description>
</metadata></package>`})
}

func wheel(t *testing.T, marker string) []byte {
	return zipBytes(t, map[string]string{
		"qapkg/__init__.py":                 "MARKER = '" + marker + "'\n",
		"qapkg-1.0.dist-info/METADATA":      "Metadata-Version: 2.1\nName: qapkg\nVersion: 1.0\nSummary: re-publish test\n",
		"qapkg-1.0.dist-info/WHEEL":         "Wheel-Version: 1.0\nGenerator: hangar-tests\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"qapkg-1.0.dist-info/RECORD":        "",
		"qapkg-1.0.dist-info/top_level.txt": "qapkg\n",
	})
}

// npmTarball is a package tarball holding package/package.json and a file.
func npmTarball(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{
		"package/package.json": `{"name":"repub-pkg","version":"1.0.0"}`,
		"package/index.js":     "module.exports = '" + content + "'\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
