package fs

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestTempStore(t *testing.T) {
	coretest.RunTempStoreTests(t, func(t *testing.T) core.TempStore {
		s, err := New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
