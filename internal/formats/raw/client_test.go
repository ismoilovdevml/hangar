package raw

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Real-client acceptance tests (docs/FORMATS.md, Raw): curl --upload-file,
// curl -O and wget against the production router backed by in-memory fakes.

func lookClient(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

// run executes a client in dir and returns its combined output.
func run(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_PROXY=*", "no_proxy=*", "HOME="+dir) // no user .curlrc/.wgetrc
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestCurlClient(t *testing.T) {
	curl := lookClient(t, "curl")
	fx := newFixture(t)
	dir := t.TempDir()
	base := fx.srv.URL + "/repository/raw-hosted"
	content := strings.Repeat("hangar raw payload\n", 2000)
	writeFile(t, filepath.Join(dir, "up", "app-1.2.3.tar.gz"), content)
	sha := core.SHA256Of([]byte(content)).Hex()

	// Upload: curl --upload-file streams the file with PUT.
	out, err := run(t, dir, curl, "-sS", "--fail-with-body", "-u", "alice:secret", "-w", "\n%{http_code}",
		"-H", "X-Checksum-Sha256: "+sha, "--upload-file", "up/app-1.2.3.tar.gz", base+"/releases/1.2.3/app-1.2.3.tar.gz")
	if err != nil || !strings.HasSuffix(out, "201") {
		t.Fatalf("curl --upload-file: %v\n%s", err, out)
	}
	// Upload into a directory URL: curl appends the local file name.
	writeFile(t, filepath.Join(dir, "notes.json"), `{"ok":true}`)
	if out, err := run(t, dir, curl, "-sS", "--fail", "-u", "alice:secret", "-T", "notes.json", base+"/docs/"); err != nil {
		t.Fatalf("curl -T dir/: %v\n%s", err, out)
	}
	// A wrong checksum is rejected (curl --fail exits 22).
	if out, err := run(t, dir, curl, "-sS", "--fail", "-u", "alice:secret", "-H", "X-Checksum-Sha256: "+strings.Repeat("0", 64),
		"-T", "notes.json", base+"/docs/bad.json"); err == nil {
		t.Errorf("bad checksum accepted:\n%s", out)
	}
	// Anonymous upload gets 401.
	if out, _ := run(t, dir, curl, "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-T", "notes.json", base+"/docs/anon.json"); out != "401" {
		t.Errorf("anonymous upload = %s", out)
	}

	// Download: curl -O names the file after the URL.
	dl := filepath.Join(dir, "dl")
	if err := os.Mkdir(dl, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, dl, curl, "-sS", "--fail", "-u", "bob:secret", "-O", base+"/releases/1.2.3/app-1.2.3.tar.gz"); err != nil {
		t.Fatalf("curl -O: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(dl, "app-1.2.3.tar.gz")); got != content {
		t.Errorf("curl -O content differs (%d bytes)", len(got))
	}
	if got := readFile(t, filepath.Join(dl, "app-1.2.3.tar.gz")); core.SHA256Of([]byte(got)).Hex() != sha {
		t.Error("sha256 mismatch after download")
	}
	// -J -O takes the name from Content-Disposition.
	jdir := filepath.Join(dir, "dlj")
	if err := os.Mkdir(jdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, jdir, curl, "-sS", "--fail", "-u", "bob:secret", "-J", "-O", base+"/docs/notes.json"); err != nil {
		t.Fatalf("curl -J -O: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(jdir, "notes.json")); got != `{"ok":true}` {
		t.Errorf("curl -J -O = %q", got)
	}
	// Resume a partial download with a Range request (curl -C -).
	partial := filepath.Join(dir, "partial.tgz")
	writeFile(t, partial, content[:1000])
	if out, err := run(t, dir, curl, "-sS", "--fail", "-u", "bob:secret", "-C", "-", "-o", "partial.tgz",
		base+"/releases/1.2.3/app-1.2.3.tar.gz"); err != nil {
		t.Fatalf("curl -C -: %v\n%s", err, out)
	}
	if got := readFile(t, partial); got != content {
		t.Errorf("resumed download differs (%d bytes)", len(got))
	}
	// Conditional download: -z with a future date gets 304, nothing written.
	out, err = run(t, dir, curl, "-sS", "-u", "bob:secret", "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "If-None-Match: \""+sha+"\"", base+"/releases/1.2.3/app-1.2.3.tar.gz")
	if err != nil || out != "304" {
		t.Errorf("If-None-Match = %s %v", out, err)
	}
	// JSON listing.
	out, err = run(t, dir, curl, "-sS", "--fail", "-u", "bob:secret", "-H", "Accept: application/json", base+"/releases/1.2.3/")
	if err != nil {
		t.Fatalf("listing: %v\n%s", err, out)
	}
	var l listing
	if err := json.Unmarshal([]byte(out), &l); err != nil || len(l.Entries) != 1 || l.Entries[0].Name != "app-1.2.3.tar.gz" {
		t.Errorf("listing = %s (%v)", out, err)
	}
	// Delete.
	if out, err := run(t, dir, curl, "-sS", "--fail", "-u", "alice:secret", "-X", "DELETE", base+"/docs/notes.json"); err != nil {
		t.Fatalf("curl -X DELETE: %v\n%s", err, out)
	}
	if out, _ := run(t, dir, curl, "-sS", "-u", "bob:secret", "-o", "/dev/null", "-w", "%{http_code}", base+"/docs/notes.json"); out != "404" {
		t.Errorf("after delete = %s", out)
	}
	// curl collapses "../" itself; --path-as-is sends it and the server refuses it.
	if out, _ := run(t, dir, curl, "-sS", "--path-as-is", "-u", "bob:secret", "-o", "/dev/null", "-w", "%{http_code}",
		base+"/releases/../releases/1.2.3/app-1.2.3.tar.gz"); out != "400" {
		t.Errorf("--path-as-is traversal = %s", out)
	}
}

func TestWgetClient(t *testing.T) {
	wget := lookClient(t, "wget")
	fx := newFixture(t)
	base := fx.srv.URL + "/repository/raw-hosted"
	files := map[string]string{
		"site/index.json":         `{"v":1}`,
		"site/assets/logo.svg":    "<svg/>",
		"site/assets/deep/x.bin":  "binary",
		"site/with space/a b.txt": "spaced",
		"other/not-mirrored":      "outside",
	}
	for p, c := range files {
		if code := fx.put("raw-hosted/"+escapePath(p), c); code != 201 {
			t.Fatalf("PUT %s = %d", p, code)
		}
	}
	dir := t.TempDir()

	// Plain download. The repository needs credentials: wget sends them only
	// after the 401 Basic challenge.
	if out, err := run(t, dir, wget, "-q", "--no-proxy", "--user=bob", "--password=secret", base+"/site/index.json"); err != nil {
		t.Fatalf("wget: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(dir, "index.json")); got != `{"v":1}` {
		t.Errorf("wget content = %q", got)
	}
	// --content-disposition.
	cd := filepath.Join(dir, "cd")
	if err := os.Mkdir(cd, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, cd, wget, "-q", "--no-proxy", "--content-disposition", "--user=bob", "--password=secret",
		base+"/site/assets/deep/x.bin"); err != nil {
		t.Fatalf("wget --content-disposition: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(cd, "x.bin")); got != "binary" {
		t.Errorf("content-disposition download = %q", got)
	}
	// Wrong password fails.
	if out, err := run(t, dir, wget, "-q", "--no-proxy", "--user=bob", "--password=wrong", "-O", "/dev/null", base+"/site/index.json"); err == nil {
		t.Errorf("wget with a wrong password succeeded:\n%s", out)
	}

	// Recursive mirror through the HTML directory listings.
	mirror := filepath.Join(dir, "mirror")
	if err := os.Mkdir(mirror, 0o750); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, mirror, wget, "-q", "--no-proxy", "-r", "-np", "-nH", "--cut-dirs=2", "-e", "robots=off",
		"--reject-regex", `\?cursor=`, "--user=bob", "--password=secret", base+"/site/")
	if err != nil {
		t.Fatalf("wget -r: %v\n%s", err, out)
	}
	var got []string
	_ = filepath.WalkDir(mirror, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != "index.html" {
			rel, _ := filepath.Rel(mirror, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	want := []string{"site/assets/deep/x.bin", "site/assets/logo.svg", "site/index.json", "site/with space/a b.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("wget -r mirrored %v, want %v", got, want)
	}
	for _, p := range want {
		if c := readFile(t, filepath.Join(mirror, p)); c != files[p] {
			t.Errorf("%s = %q", p, c)
		}
	}

	// Upload with wget --method=PUT.
	writeFile(t, filepath.Join(dir, "up.txt"), "wget upload")
	out, err = run(t, dir, wget, "-q", "--no-proxy", "-O", "-", "--method=PUT", "--body-file=up.txt",
		"--user=alice", "--password=secret", base+"/uploads/up.txt")
	if err != nil {
		t.Fatalf("wget --method=PUT: %v\n%s", err, out)
	}
	if code, _, body := fx.do("GET", "raw-hosted/uploads/up.txt", nil, as("bob")); code != 200 || body != "wget upload" {
		t.Errorf("after wget PUT = %d %q", code, body)
	}
}
