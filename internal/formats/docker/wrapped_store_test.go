package docker_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// wrappedStore hides the store's optional interfaces, as the metrics and
// tracing wrapper does, and exposes the store with Unwrap.
type wrappedStore struct{ core.BlobStore }

func (w wrappedStore) Unwrap() core.BlobStore { return w.BlobStore }

// Regression: with the production blob store wrapper, upload sessions fell
// back to records stored as blobs (never cleaned up) because the temp area
// was not found behind the wrapper.
func TestUploadSessionsThroughWrappedStore(t *testing.T) {
	var mem *coretest.MemBlobStore
	tr := newTestRegWith(t, func(d *core.Deps) {
		mem = d.Blobs.(coretest.SingleStore).Store.(*coretest.MemBlobStore)
		d.Blobs = coretest.SingleStore{Store: wrappedStore{mem}}
	})
	if _, ok := core.TempStoreOf(wrappedStore{mem}); !ok {
		t.Fatal("TempStoreOf does not look through Unwrap")
	}
	data := []byte("wrapped store upload")
	resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	loc := patch(tr, resp.Header.Get("Location"), data, 0)
	if mem.TempLen() != 1 {
		t.Fatalf("session record not in the temp area (%d objects)", mem.TempLen())
	}
	resp, body = tr.do(http.MethodPut, loc+"&digest="+string(core.SHA256Of(data)), nil, alice)
	tr.expect(resp, body, http.StatusCreated)
	resp, body = tr.do(http.MethodGet, loc, nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	if mem.TempLen() != 0 {
		t.Fatalf("temp objects after a single-chunk upload = %d, want 0", mem.TempLen())
	}
}
