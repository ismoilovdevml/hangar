package cargo_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/archive/archivetest"
)

// TestPublishBoundsDecompression: every entry of a .crate is checked, so a
// crate that expands past the bound (a gzip bomb of about a megabyte here)
// is refused once the bound is reached instead of being decompressed in
// full, and nothing is published.
func TestPublishBoundsDecompression(t *testing.T) {
	f := newFixture(t)
	manifest := "[package]\nname = \"bomb\"\nversion = \"1.0.0\"\n"
	crate := archivetest.TarGzBomb("bomb-1.0.0/big.bin", (1<<30)+(1<<20),
		[2]string{"bomb-1.0.0/Cargo.toml", manifest})
	rec := f.publish(t, "cargo-hosted", meta("bomb", "1.0.0", nil), crate)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "larger than") {
		t.Fatalf("publish of a crate bomb = %d %s, want 400 naming the size limit", rec.Code, rec.Body)
	}
	if lines := f.get(t, "/repository/cargo-hosted/index/bo/mb/bomb", "Authorization", f.token); lines.Code != http.StatusNotFound {
		t.Errorf("index of the refused crate = %d %s", lines.Code, lines.Body)
	}
}
