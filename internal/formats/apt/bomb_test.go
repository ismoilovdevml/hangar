package apt_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/archive/archivetest"
)

// deb frames members as an ar archive, the way dpkg-deb lays out a .deb.
func deb(members ...[2][]byte) []byte {
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m[0], 1700000000, 0, 0, "100644", len(m[1]))
		out.Write(m[1])
		if len(m[1])%2 == 1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// TestUploadBoundsControlDecompression: a control member whose control
// file sits behind more uncompressed data than any control archive has (a
// gzip bomb of about a quarter megabyte here) is refused once the bound is
// reached instead of being decompressed in full.
func TestUploadBoundsControlDecompression(t *testing.T) {
	f := newFixture(t)
	control := apttest.Package{Name: "bomb", Version: "1", Architecture: "amd64"}.Control()
	bomb := deb(
		[2][]byte{[]byte("debian-binary"), []byte("2.0\n")},
		[2][]byte{[]byte("control.tar.gz"), archivetest.TarGzBomb("./big", 300<<20, [2]string{"./control", control})},
		[2][]byte{[]byte("data.tar.gz"), archivetest.TarGzBomb("./empty", 0)},
	)
	rec := f.do(t, http.MethodPut, "/repository/apt-hosted/upload", bytes.NewReader(bomb))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "larger than") {
		t.Fatalf("upload of a control bomb = %d %s, want 400 naming the size limit", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-hosted/pool/main/b/bomb/bomb_1_amd64.deb", nil); rec.Code != http.StatusNotFound {
		t.Errorf("refused package stored: GET = %d", rec.Code)
	}
}
