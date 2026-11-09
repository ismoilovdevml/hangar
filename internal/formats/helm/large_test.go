package helm_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// TestLargeIndex renders an index of thousands of chart versions once and
// serves later requests from the cached document.
func TestLargeIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	f := newFixture(t)
	repo, _ := f.env.Repos.Get(context.Background(), hostedName)
	ctx := coretest.AsUser(context.Background(), "alice")
	d, n, err := f.env.Blob.Put(ctx, bytesReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	const charts, versions = 200, 25
	for c := range charts {
		for v := range versions {
			name, ver := fmt.Sprintf("chart-%03d", c), fmt.Sprintf("1.%d.0", v)
			_, _, err := f.env.Components.Publish(ctx, repo, core.PublishRequest{
				Component: &core.Component{Name: name, Version: ver, Attributes: map[string]any{
					"chart":  map[string]any{"apiVersion": "v2", "name": name, "version": ver, "description": "chart " + name},
					"digest": d.Hex(),
				}},
				Assets: []*core.Asset{{Path: name + "/" + ver + "/" + name + "-" + ver + ".tgz", BlobDigest: d, Size: n}},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Now()
	idx := f.index(hostedName)
	first := time.Since(start)
	if len(idx.Entries) != charts || len(idx.Entries["chart-007"]) != versions || idx.Entries["chart-007"][0].Version != "1.24.0" {
		t.Fatalf("index has %d charts", len(idx.Entries))
	}
	start = time.Now()
	rec := f.do(http.MethodGet, "/repository/"+hostedName+"/index.yaml", nil, "")
	cached := time.Since(start)
	t.Logf("%d versions, %d bytes: first %v, cached %v", charts*versions, rec.Body.Len(), first, cached)
	if cached > first {
		t.Errorf("cached request (%v) slower than the first (%v)", cached, first)
	}
}
