package helm_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/archive"
	"github.com/ismoilovdevml/hangar/internal/archive/archivetest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
)

// TestReadChartBoundsDecompression: a chart whose Chart.yaml sits behind
// more uncompressed data than any chart has is refused once the bound is
// reached, instead of being decompressed in full (a gzip bomb: about a
// megabyte here, a few kilobytes with better compression).
func TestReadChartBoundsDecompression(t *testing.T) {
	bomb := archivetest.TarGzBomb("web/big.bin", 300<<20,
		[2]string{"web/Chart.yaml", "apiVersion: v2\nname: web\nversion: 1.0.0\n"})
	_, err := helm.ReadChart(bytes.NewReader(bomb))
	if !errors.Is(err, archive.ErrTooLarge) || !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("ReadChart = %v, want a too-large invalid archive", err)
	}

	// The same layout within the bound still reads.
	small := archivetest.TarGzBomb("web/big.bin", 1<<20,
		[2]string{"web/Chart.yaml", "apiVersion: v2\nname: web\nversion: 1.0.0\n"})
	if m, err := helm.ReadChart(bytes.NewReader(small)); err != nil || m.Name != "web" {
		t.Fatalf("ReadChart of a small archive = %+v, %v", m, err)
	}
}
